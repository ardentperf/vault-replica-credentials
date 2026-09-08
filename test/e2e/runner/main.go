// The E2E actor owns database provisioning and SQL. No controller package
// depends on this program or obtains its PostgreSQL credentials.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var sensitive = []string{"e2e-dev-root-token", "dummy-password", "dummy-user"}
var credentialFields = regexp.MustCompile(`(?i)("(?:password|username|user|token|root-token|(?:last)?promotionToken|(?:last)?demotionToken|lease_id|leaseID|currentLeaseID|primary_conninfo)"\s*:\s*)"[^"\n]*"`)
var generatedUser = regexp.MustCompile(`v-(?:token|root)-[a-zA-Z0-9_.-]+`)
var secretAssignment = regexp.MustCompile(`(?i)((?:password|root token|token)\s*[:=]\s*)\S+`)
var leasePath = regexp.MustCompile(`database/creds/[a-zA-Z0-9_./-]+`)

func redact(s string) string {
	for _, v := range sensitive {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	s = credentialFields.ReplaceAllString(s, `${1}"[REDACTED]"`)
	s = generatedUser.ReplaceAllString(s, "[REDACTED]")
	s = leasePath.ReplaceAllString(s, "[REDACTED]")
	return secretAssignment.ReplaceAllString(s, `${1}[REDACTED]`)
}
func random() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type actor struct {
	ctx                      context.Context
	artifact, token, address string
	http                     *http.Client
	counter                  int
	forwards                 []*exec.Cmd
	ports                    map[string]int
}

func (a *actor) artifactWrite(name string, data []byte) {
	_ = os.WriteFile(filepath.Join(a.artifact, name), []byte(redact(string(data))), 0600)
}
func (a *actor) command(input []byte, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s failed: %s", name, redact(string(out)))
	}
	return out, nil
}
func (a *actor) k(region string, input []byte, args ...string) ([]byte, error) {
	return a.command(input, "kubectl", append([]string{"--context", "kind-k8s-" + region, "--request-timeout=30s"}, args...)...)
}
func (a *actor) apply(region string, obj any) error {
	b, _ := json.Marshal(obj)
	_, err := a.k(region, b, "apply", "--server-side", "-f", "-")
	return err
}
func (a *actor) get(region, ns, resource, name string) (map[string]any, error) {
	out, err := a.k(region, nil, "-n", ns, "get", resource, name, "-o", "json")
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	err = json.Unmarshal(out, &obj)
	return obj, err
}
func (a *actor) patch(region, ns, resource, name string, body any) error {
	b, _ := json.Marshal(body)
	_, err := a.k(region, b, "-n", ns, "patch", resource, name, "--type=merge", "--patch-file=/dev/stdin")
	return err
}
func (a *actor) wait(label string, timeout time.Duration, fn func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(a.ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var last error
	for {
		ok, err := fn()
		if ok && err == nil {
			return nil
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s timed out: %v", label, last)
		case <-ticker.C:
		}
	}
}
func (a *actor) vault(method, path string, body any) (map[string]any, error) {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req, _ := http.NewRequestWithContext(a.ctx, method, a.address+"/v1/"+path, bytes.NewReader(payload))
	req.Header.Set("X-Vault-Token", a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, errors.New("Vault transport failed")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Vault %s status %d: %s", path, resp.StatusCode, redact(string(raw)))
	}
	var result map[string]any
	if len(raw) > 0 {
		err = json.Unmarshal(raw, &result)
	}
	return result, err
}
func obj(kind, ns, name string, spec map[string]any) map[string]any {
	v := "v1"
	if kind == "Deployment" {
		v = "apps/v1"
	}
	return map[string]any{"apiVersion": v, "kind": kind, "metadata": map[string]any{"namespace": ns, "name": name}, "spec": spec}
}
func nested(m map[string]any, keys ...string) any {
	var v any = m
	for _, key := range keys {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = mm[key]
	}
	return v
}
func str(m map[string]any, keys ...string) string { v, _ := nested(m, keys...).(string); return v }

func main() {
	sensitive = append(sensitive, os.Getenv("VAULT_ROOT_TOKEN"))
	if len(os.Args) > 1 && os.Args[1] == "--redact" {
		b, _ := io.ReadAll(os.Stdin)
		fmt.Print(redact(string(b)))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Minute)
	defer cancel()
	a := &actor{ctx: ctx, artifact: os.Getenv("ARTIFACT_DIR"), token: os.Getenv("VAULT_ROOT_TOKEN"), address: os.Getenv("VAULT_ADDR"), http: &http.Client{Timeout: 15 * time.Second}, ports: map[string]int{"us": 19090, "eu": 19091}}
	err := a.infrastructure()
	if err == nil && !(len(os.Args) > 1 && os.Args[1] == "--infrastructure") {
		err = a.suite()
	}
	for _, cmd := range a.forwards {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, redact(err.Error()))
		os.Exit(1)
	}
	fmt.Println("E2E acceptance passed")
}

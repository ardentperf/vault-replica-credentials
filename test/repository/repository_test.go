package repository

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func root() string { return "../.." }
func TestPinnedE2EToolsTakePrecedence(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(), "test/e2e/setup.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `export PATH="${ROOT_DIR}/.tools:${PATH}"`) {
		t.Fatal("ambient CNPG plugin can override pinned release")
	}
}
func TestDependencyInventory(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(root(), "renovate.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		CustomManagers []struct {
			MatchStrings []string `json:"matchStrings"`
		} `json:"customManagers"`
	}
	if json.Unmarshal(raw, &cfg) != nil || len(cfg.CustomManagers) == 0 {
		t.Fatal("missing custom manager")
	}
	pattern := strings.ReplaceAll(cfg.CustomManagers[0].MatchStrings[0], "(?<", "(?P<")
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatal(err)
	}
	dependencies := map[string]bool{}
	for _, path := range []string{"test/e2e/versions.sh", "scripts/install-tools.sh", "scripts/renovate-check.sh"} {
		b, err := os.ReadFile(filepath.Join(root(), path))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range re.FindAllStringSubmatch(string(b), -1) {
			dependencies[match[re.SubexpIndex("depName")]] = true
		}
	}
	for _, dep := range []string{"cloudnative-pg/cloudnative-pg", "hashicorp/vault", "ghcr.io/cloudnative-pg/postgresql", "prom/prometheus", "kindest/node", "kubernetes-sigs/kind", "kubernetes/kubernetes", "nektos/act", "catthehacker/ubuntu", "github.com/yannh/kubeconform", "golang.org/x/vuln", "ghcr.io/renovatebot/renovate"} {
		if !dependencies[dep] {
			t.Errorf("Renovate misses %s", dep)
		}
	}
}

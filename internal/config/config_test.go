package config

import (
	"testing"
	"time"
)

func TestLoadRequiresBoundedWatchNamespace(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "missing", raw: ""},
		{name: "empty item", raw: "reporting,"},
		{name: "wildcard", raw: "*"},
		{name: "unrestricted alias", raw: "all"},
		{name: "invalid name", raw: "REPORTING"},
		{name: "duplicate", raw: "reporting, reporting"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnvironment()
			if tt.name == "missing" {
				delete(env, "WATCH_NAMESPACE")
			} else {
				env["WATCH_NAMESPACE"] = tt.raw
			}
			if _, err := Load(mapLookup(env)); err == nil {
				t.Fatalf("Load() error = nil, want fail-closed configuration error")
			}
		})
	}
}

func TestLoadTrimsAndPreservesNamespaceOrder(t *testing.T) {
	env := validEnvironment()
	env["WATCH_NAMESPACE"] = " reporting,analytics "

	got, err := Load(mapLookup(env))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{"reporting", "analytics"}
	if len(got.WatchNamespaces) != len(want) {
		t.Fatalf("WatchNamespaces = %#v, want %#v", got.WatchNamespaces, want)
	}
	for i := range want {
		if got.WatchNamespaces[i] != want[i] {
			t.Errorf("WatchNamespaces[%d] = %q, want %q", i, got.WatchNamespaces[i], want[i])
		}
	}
	if got.Workflow.PasswordPropagationDelay != 5*time.Second {
		t.Errorf("PasswordPropagationDelay = %s, want 5s", got.Workflow.PasswordPropagationDelay)
	}
	if got.SystemNamespace != SystemNamespace {
		t.Errorf("SystemNamespace = %q, want %q", got.SystemNamespace, SystemNamespace)
	}
}

func TestLoadRequiresTLSVaultAddress(t *testing.T) {
	env := validEnvironment()
	env["VAULT_ADDR"] = "http://vault.example"
	if _, err := Load(mapLookup(env)); err == nil {
		t.Fatal("Load() error = nil, want TLS validation error")
	}
}

func TestLoadAllowsExplicitInsecureHTTPForE2E(t *testing.T) {
	env := validEnvironment()
	env["VAULT_ADDR"] = "http://vault.example:8200"
	env["VAULT_ALLOW_INSECURE_HTTP"] = "true"

	got, err := Load(mapLookup(env))
	if err != nil {
		t.Fatalf("Load() error = %v, want explicit E2E HTTP to be accepted", err)
	}
	if !got.Vault.AllowInsecureHTTP {
		t.Fatal("AllowInsecureHTTP = false, want true")
	}
}

func TestLoadRequiresVaultTokenWithoutEchoingIt(t *testing.T) {
	env := validEnvironment()
	delete(env, "VAULT_TOKEN")
	if _, err := Load(mapLookup(env)); err == nil {
		t.Fatal("Load() error = nil, want missing token error")
	}
}

func TestLoadRejectsUnsafeAndMalformedRuntimeValues(t *testing.T) {
	tests := map[string]string{
		"VAULT_REQUEST_TIMEOUT": "0s",
		"VAULT_RETRY_ATTEMPTS":  "zero",
		"WORKERS":               "0",
		"LEADER_ELECTION":       "sometimes",
		"STATE_MAX_BYTES":       "-1",
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			env := validEnvironment()
			env[name] = value
			if _, err := Load(mapLookup(env)); err == nil {
				t.Fatalf("Load() accepted %s=%q", name, value)
			}
		})
	}
}

func validEnvironment() map[string]string {
	return map[string]string{
		"WATCH_NAMESPACE": "reporting",
		"VAULT_ADDR":      "https://vault.example",
		"VAULT_TOKEN":     "test-only-token",
	}
}

func mapLookup(values map[string]string) Lookup {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

package kubernetes

import (
	"testing"

	"github.com/ardentperf/vault-replica-credentials/internal/config"
)

func TestCacheOptionsKeepSystemStateSeparateFromTargetSecrets(t *testing.T) {
	cfg := config.Config{
		WatchNamespaces: []string{"reporting", "analytics"},
		SystemNamespace: config.SystemNamespace,
	}
	options := CacheOptions(cfg)

	if len(options.DefaultNamespaces) != 2 {
		t.Fatalf("DefaultNamespaces = %#v, want two target namespaces", options.DefaultNamespaces)
	}
	secret, ok := options.ByObject[secretCacheObject]
	if !ok {
		t.Fatal("Secret cache override is missing")
	}
	if len(secret.Namespaces) != 1 {
		t.Fatalf("Secret namespaces = %#v, want only %q", secret.Namespaces, config.SystemNamespace)
	}
	if _, ok := secret.Namespaces[config.SystemNamespace]; !ok {
		t.Fatalf("Secret namespaces = %#v, want %q", secret.Namespaces, config.SystemNamespace)
	}
	if _, ok := secret.Namespaces["reporting"]; ok {
		t.Fatal("target namespace was added to the Secret cache")
	}
	if _, ok := options.ByObject[leaseCacheObject]; !ok {
		t.Fatal("Lease cache override is missing")
	}
}

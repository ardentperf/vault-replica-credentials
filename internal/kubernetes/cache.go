// Package kubernetes contains manager-facing Kubernetes setup helpers.
package kubernetes

import (
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
	"github.com/ardentperf/vault-replica-credentials/internal/config"
)

var (
	secretCacheObject client.Object = &corev1.Secret{}
	leaseCacheObject  client.Object = &coordinationv1.Lease{}
)

// NewScheme returns the Kubernetes scheme used by the manager. No CNPG
// controller or Cluster resource is created by this package.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	scheme.AddKnownTypeWithName(cnpg.ClusterGVK, cnpg.NewCluster())
	scheme.AddKnownTypeWithName(cnpg.ClusterGVK.GroupVersion().WithKind("ClusterList"), cnpg.NewClusterList())
	metav1.AddToGroupVersion(scheme, cnpg.ClusterGVK.GroupVersion())
	return scheme, nil
}

// CacheOptions scopes the cache exactly as required by DESIGN.md:
// Cluster and Pod informers fall under the approved namespaces, while Secret
// and Lease informers are restricted to cnpg-system. Restricting Secret this
// way prevents target credential Secret watches.
func CacheOptions(cfg config.Config) cache.Options {
	targetNamespaces := make(map[string]cache.Config, len(cfg.WatchNamespaces))
	for _, namespace := range cfg.WatchNamespaces {
		targetNamespaces[namespace] = cache.Config{}
	}
	systemNamespace := map[string]cache.Config{cfg.SystemNamespace: {}}

	return cache.Options{
		DefaultNamespaces: targetNamespaces,
		ByObject: map[client.Object]cache.ByObject{
			secretCacheObject: {
				Namespaces: systemNamespace,
			},
			leaseCacheObject: {
				Namespaces: systemNamespace,
			},
		},
	}
}

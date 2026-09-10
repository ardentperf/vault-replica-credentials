package controller

import (
	"context"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
)

// SetupWithManager registers only the CRD and Pod inputs required for the
// workflow. There is deliberately no Secret, Event, or Node watch.
func (r *Reconciler) SetupWithManager(mgr manager.Manager) error {
	workers := r.config.Runtime.Workers
	if workers < 1 {
		workers = 1
	}
	if err := ctrl.NewControllerManagedBy(mgr).
		For(cnpg.NewClusterObject(), builder.WithPredicates(clusterPredicate())).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapPod), builder.WithPredicates(podPredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: workers}).
		Complete(r); err != nil {
		return err
	}
	return mgr.Add(NewOrphanSweeper(r))
}

func clusterPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(update event.UpdateEvent) bool {
			oldObject, oldOK := update.ObjectOld.(*unstructured.Unstructured)
			newObject, newOK := update.ObjectNew.(*unstructured.Unstructured)
			return !oldOK || !newOK || cnpg.RelevantChange(oldObject, newObject)
		},
	}
}

func podPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return true },
	}
}

func (r *Reconciler) mapPod(ctx context.Context, object client.Object) []reconcile.Request {
	pod, ok := object.(*corev1.Pod)
	if !ok || pod.Namespace == "" || pod.Name == "" {
		return nil
	}
	clusters := &unstructured.UnstructuredList{}
	clusters.SetGroupVersionKind(schema.GroupVersionKind{Group: cnpg.ClusterGVK.Group, Version: cnpg.ClusterGVK.Version, Kind: "ClusterList"})
	if err := r.client.List(ctx, clusters, client.InNamespace(pod.Namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, 1)
	for index := range clusters.Items {
		info, err := cnpg.Inspect(&clusters.Items[index])
		if err == nil && info.Replica && info.PrimaryPod == pod.Name {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&clusters.Items[index])})
		}
	}
	return requests
}

// OrphanSweeper is manager-owned work that uses direct API reads and requires
// two in-memory consecutive absences before revoking a lease. Namespace scope
// is checked before every read so a removed namespace is never treated as a
// deletion.
type OrphanSweeper struct {
	reconciler *Reconciler
	mu         sync.Mutex
	absences   map[string]int
}

func NewOrphanSweeper(reconciler *Reconciler) *OrphanSweeper {
	return &OrphanSweeper{reconciler: reconciler, absences: map[string]int{}}
}

func (s *OrphanSweeper) Start(ctx context.Context) error {
	interval := s.reconciler.config.Workflow.OrphanSweepInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.Sweep(ctx); err != nil {
				// A managed runnable must keep trying after transient API/Vault
				// failures. Reconciliation and metrics expose the resulting work.
				continue
			}
		}
	}
}

func (s *OrphanSweeper) NeedLeaderElection() bool { return true }

func (s *OrphanSweeper) Sweep(ctx context.Context) error {
	store, err := s.reconciler.state.Load(ctx)
	if err != nil {
		return err
	}
	s.reconciler.observe(store)
	for key, entry := range store.Clusters {
		namespace, name, ok := strings.Cut(key, "/")
		if !ok || !s.watched(namespace) {
			continue
		}
		object := cnpg.NewClusterObject()
		err := s.reconciler.apiReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object)
		if err == nil {
			s.reset(key)
			if string(object.GetUID()) != entry.ClusterUID {
				if err := s.reconciler.cleanupEntry(ctx, key, entry); err != nil {
					return err
				}
			}
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		if s.noteAbsence(key) >= s.reconciler.config.Workflow.OrphanAbsenceSweeps {
			if err := s.reconciler.cleanupEntry(ctx, key, entry); err != nil {
				return err
			}
			s.reset(key)
		}
	}
	return nil
}

func (s *OrphanSweeper) watched(namespace string) bool {
	for _, candidate := range s.reconciler.config.WatchNamespaces {
		if candidate == namespace {
			return true
		}
	}
	return false
}

func (s *OrphanSweeper) noteAbsence(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.absences[key]++
	return s.absences[key]
}

func (s *OrphanSweeper) reset(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.absences, key)
}

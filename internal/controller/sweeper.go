package controller

import (
	"context"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ardentperf/vault-replica-credentials/internal/cnpg"
)

// Sweeper provides deletion cleanup without finalizers. Absence counters are
// intentionally process-local and therefore fail safe across restarts.
type Sweeper struct {
	Reader           client.Reader
	Reconciler       *Reconciler
	Namespaces       map[string]struct{}
	Interval         time.Duration
	RequiredAbsences int

	mu       sync.Mutex
	absences map[string]int
}

// NewSweeper validates the bounded watched namespace set.
func NewSweeper(reader client.Reader, reconciler *Reconciler, namespaces []string, interval time.Duration, requiredAbsences int) (*Sweeper, error) {
	if reader == nil || reconciler == nil || len(namespaces) == 0 || interval <= 0 || requiredAbsences < 2 {
		return nil, errInvalidSweeper
	}
	allowed := make(map[string]struct{}, len(namespaces))
	for _, namespace := range namespaces {
		allowed[namespace] = struct{}{}
	}
	return &Sweeper{Reader: reader, Reconciler: reconciler, Namespaces: allowed, Interval: interval, RequiredAbsences: requiredAbsences, absences: map[string]int{}}, nil
}

var errInvalidSweeper = &configurationError{"orphan sweeper configuration is invalid"}

type configurationError struct{ message string }

func (err *configurationError) Error() string { return err.message }

// NeedLeaderElection ensures only the active manager performs cleanup.
func (*Sweeper) NeedLeaderElection() bool { return true }

// Start runs one startup sweep, then the configured periodic sweep.
func (sweeper *Sweeper) Start(ctx context.Context) error {
	if err := sweeper.SweepOnce(ctx); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "orphan sweep failed; cleanup will be retried")
	}
	ticker := time.NewTicker(sweeper.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := sweeper.SweepOnce(ctx); err != nil {
				ctrl.LoggerFrom(ctx).Error(err, "orphan sweep failed; cleanup will be retried")
			}
		}
	}
}

// SweepOnce uses fresh API reads and requires consecutive confirmed 404s.
func (sweeper *Sweeper) SweepOnce(ctx context.Context) error {
	store, err := sweeper.Reconciler.readState(ctx)
	if err != nil {
		return err
	}
	for key := range store.Clusters {
		namespace, name, ok := splitClusterKey(key)
		if !ok {
			continue
		}
		if _, watched := sweeper.Namespaces[namespace]; !watched {
			// An out-of-scope namespace is never evidence of deletion.
			sweeper.reset(key)
			continue
		}
		cluster := cnpg.NewCluster()
		err := sweeper.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cluster)
		if err == nil {
			sweeper.reset(key)
			continue
		}
		if !apierrors.IsNotFound(err) {
			sweeper.reset(key)
			continue
		}
		if sweeper.increment(key) < sweeper.RequiredAbsences {
			continue
		}
		if err := sweeper.Reconciler.CleanupKey(ctx, key); err != nil {
			return err
		}
		sweeper.reset(key)
	}
	return nil
}

func (sweeper *Sweeper) increment(key string) int {
	sweeper.mu.Lock()
	defer sweeper.mu.Unlock()
	sweeper.absences[key]++
	return sweeper.absences[key]
}

func (sweeper *Sweeper) reset(key string) {
	sweeper.mu.Lock()
	delete(sweeper.absences, key)
	sweeper.mu.Unlock()
}

func splitClusterKey(key string) (string, string, bool) {
	parts := strings.Split(key, "/")
	returnValue := len(parts) == 2 && parts[0] != "" && parts[1] != ""
	if !returnValue {
		return "", "", false
	}
	return parts[0], parts[1], true
}

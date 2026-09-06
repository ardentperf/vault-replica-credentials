// Package config contains the process configuration for the operator.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	SystemNamespace = "cnpg-system"

	DefaultStateSecretName = "vault-replica-controller-state"
	DefaultStateSecretKey  = "state.json"

	DefaultVaultMinLeaseDuration = 768 * time.Hour
	DefaultLeaseSafetyMargin     = 24 * time.Hour

	DefaultPasswordPropagationDelay = 5 * time.Second
	DefaultVerificationDelay        = 2 * time.Minute
	DefaultIssueStageTimeout        = time.Minute
	DefaultPasswordUsernameTimeout  = time.Minute
	DefaultReconnectStageTimeout    = 5 * time.Minute
	DefaultOrphanSweepInterval      = 5 * time.Minute
	DefaultOrphanAbsenceSweeps      = 2
	DefaultStateMaxBytes            = 256 * 1024

	DefaultMetricsBindAddress     = ":8080"
	DefaultHealthProbeBindAddress = ":8081"
	DefaultLeaderElectionID       = "vault-replica-controller"
	DefaultWorkerCount            = 1
)

// Config is the complete process configuration needed by the manager and the
// future reconciliation implementation. Loading it does not make external
// API calls.
type Config struct {
	WatchNamespaces []string

	SystemNamespace string
	StateSecretName string
	StateSecretKey  string

	Vault    VaultConfig
	Workflow WorkflowConfig
	Runtime  RuntimeConfig
}

// VaultConfig describes the future Vault client boundary. Authentication is
// intentionally represented as deployment configuration; this scaffold does
// not select or execute a Vault authentication method.
type VaultConfig struct {
	Address           string
	AllowInsecureHTTP bool
	MinLease          time.Duration
	SafetyMargin      time.Duration
}

// WorkflowConfig contains persisted-timestamp and stage-deadline settings
// from DESIGN.md. No workflow is run by the initial scaffold.
type WorkflowConfig struct {
	PasswordPropagationDelay time.Duration
	VerificationDelay        time.Duration
	IssueStageTimeout        time.Duration
	PasswordUsernameTimeout  time.Duration
	ReconnectStageTimeout    time.Duration
	OrphanSweepInterval      time.Duration
	OrphanAbsenceSweeps      int
	StateMaxBytes            int
}

// RuntimeConfig contains manager operational settings.
type RuntimeConfig struct {
	MetricsBindAddress     string
	HealthProbeBindAddress string
	LeaderElectionID       string
	LeaderElection         bool
	Workers                int
}

// Lookup is the environment lookup function used by Load. It is a named
// type so tests can supply a deterministic environment without mutating the
// process environment.
type Lookup func(string) (string, bool)

// Load reads and validates operator configuration from the environment.
// WATCH_NAMESPACE and the Vault address/role are required. Invalid namespace
// configuration fails closed and never becomes a cluster-wide watch.
func Load(lookup Lookup) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}

	watchNamespaces, err := parseWatchNamespaces(required(lookup, "WATCH_NAMESPACE"))
	if err != nil {
		return Config{}, err
	}

	vaultAddress, err := requiredValue(lookup, "VAULT_ADDR")
	if err != nil {
		return Config{}, err
	}
	allowInsecureHTTP, err := boolOr(lookup, "VAULT_ALLOW_INSECURE_HTTP", false)
	if err != nil {
		return Config{}, err
	}
	if err := validateVaultAddress(vaultAddress, allowInsecureHTTP); err != nil {
		return Config{}, err
	}

	stateSecretName := valueOr(lookup, "STATE_SECRET_NAME", DefaultStateSecretName)
	if errs := validation.IsDNS1123Subdomain(stateSecretName); len(errs) > 0 {
		return Config{}, fmt.Errorf("STATE_SECRET_NAME %q is invalid: %s", stateSecretName, strings.Join(errs, "; "))
	}
	stateSecretKey := strings.TrimSpace(valueOr(lookup, "STATE_SECRET_KEY", DefaultStateSecretKey))
	if stateSecretKey == "" {
		return Config{}, fmt.Errorf("STATE_SECRET_KEY must not be empty")
	}

	minLease, err := positiveDuration(lookup, "VAULT_MIN_LEASE_DURATION", DefaultVaultMinLeaseDuration)
	if err != nil {
		return Config{}, err
	}
	safetyMargin, err := positiveDuration(lookup, "VAULT_LEASE_SAFETY_MARGIN", DefaultLeaseSafetyMargin)
	if err != nil {
		return Config{}, err
	}
	if safetyMargin >= minLease {
		return Config{}, fmt.Errorf("VAULT_LEASE_SAFETY_MARGIN must be less than VAULT_MIN_LEASE_DURATION")
	}

	workflow, err := loadWorkflow(lookup)
	if err != nil {
		return Config{}, err
	}
	runtime, err := loadRuntime(lookup)
	if err != nil {
		return Config{}, err
	}

	return Config{
		WatchNamespaces: watchNamespaces,
		SystemNamespace: SystemNamespace,
		StateSecretName: stateSecretName,
		StateSecretKey:  stateSecretKey,
		Vault: VaultConfig{
			Address:           vaultAddress,
			AllowInsecureHTTP: allowInsecureHTTP,
			MinLease:          minLease,
			SafetyMargin:      safetyMargin,
		},
		Workflow: workflow,
		Runtime:  runtime,
	}, nil
}

func loadWorkflow(lookup Lookup) (WorkflowConfig, error) {
	passwordDelay, err := positiveDuration(lookup, "PASSWORD_PROPAGATION_DELAY", DefaultPasswordPropagationDelay)
	if err != nil {
		return WorkflowConfig{}, err
	}
	verificationDelay, err := positiveDuration(lookup, "VERIFICATION_DELAY", DefaultVerificationDelay)
	if err != nil {
		return WorkflowConfig{}, err
	}
	issueTimeout, err := positiveDuration(lookup, "ISSUE_STAGE_TIMEOUT", DefaultIssueStageTimeout)
	if err != nil {
		return WorkflowConfig{}, err
	}
	passwordUsernameTimeout, err := positiveDuration(lookup, "PASSWORD_USERNAME_STAGE_TIMEOUT", DefaultPasswordUsernameTimeout)
	if err != nil {
		return WorkflowConfig{}, err
	}
	reconnectTimeout, err := positiveDuration(lookup, "RECONNECT_STAGE_TIMEOUT", DefaultReconnectStageTimeout)
	if err != nil {
		return WorkflowConfig{}, err
	}
	orphanInterval, err := positiveDuration(lookup, "ORPHAN_SWEEP_INTERVAL", DefaultOrphanSweepInterval)
	if err != nil {
		return WorkflowConfig{}, err
	}
	absenceSweeps, err := positiveInt(lookup, "ORPHAN_ABSENCE_SWEEPS", DefaultOrphanAbsenceSweeps)
	if err != nil {
		return WorkflowConfig{}, err
	}
	stateMaxBytes, err := positiveInt(lookup, "STATE_MAX_BYTES", DefaultStateMaxBytes)
	if err != nil {
		return WorkflowConfig{}, err
	}

	return WorkflowConfig{
		PasswordPropagationDelay: passwordDelay,
		VerificationDelay:        verificationDelay,
		IssueStageTimeout:        issueTimeout,
		PasswordUsernameTimeout:  passwordUsernameTimeout,
		ReconnectStageTimeout:    reconnectTimeout,
		OrphanSweepInterval:      orphanInterval,
		OrphanAbsenceSweeps:      absenceSweeps,
		StateMaxBytes:            stateMaxBytes,
	}, nil
}

func loadRuntime(lookup Lookup) (RuntimeConfig, error) {
	workers, err := positiveInt(lookup, "WORKERS", DefaultWorkerCount)
	if err != nil {
		return RuntimeConfig{}, err
	}
	leaderElection, err := boolOr(lookup, "LEADER_ELECTION", true)
	if err != nil {
		return RuntimeConfig{}, err
	}
	return RuntimeConfig{
		MetricsBindAddress:     valueOr(lookup, "METRICS_BIND_ADDRESS", DefaultMetricsBindAddress),
		HealthProbeBindAddress: valueOr(lookup, "HEALTH_PROBE_BIND_ADDRESS", DefaultHealthProbeBindAddress),
		LeaderElectionID:       valueOr(lookup, "LEADER_ELECTION_ID", DefaultLeaderElectionID),
		LeaderElection:         leaderElection,
		Workers:                workers,
	}, nil
}

func parseWatchNamespaces(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("WATCH_NAMESPACE is required and must not be empty")
	}

	parts := strings.Split(raw, ",")
	namespaces := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		namespace := strings.TrimSpace(part)
		if namespace == "" {
			return nil, fmt.Errorf("WATCH_NAMESPACE contains an empty namespace")
		}
		if namespace == "*" || namespace == "all" {
			return nil, fmt.Errorf("WATCH_NAMESPACE value %q is unrestricted", namespace)
		}
		if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
			return nil, fmt.Errorf("WATCH_NAMESPACE namespace %q is invalid: %s", namespace, strings.Join(errs, "; "))
		}
		if _, duplicate := seen[namespace]; duplicate {
			return nil, fmt.Errorf("WATCH_NAMESPACE contains duplicate namespace %q", namespace)
		}
		seen[namespace] = struct{}{}
		namespaces = append(namespaces, namespace)
	}
	return namespaces, nil
}

func validateVaultAddress(raw string, allowInsecureHTTP bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("VAULT_ADDR must be an absolute URL with a host")
	}
	if parsed.Scheme != "https" && !(allowInsecureHTTP && parsed.Scheme == "http") {
		return fmt.Errorf("VAULT_ADDR must use https for TLS-validated Vault communication")
	}
	if parsed.User != nil {
		return fmt.Errorf("VAULT_ADDR must not include user credentials")
	}
	return nil
}

func required(lookup Lookup, name string) string {
	value, _ := lookup(name)
	return value
}

func requiredValue(lookup Lookup, name string) (string, error) {
	value := strings.TrimSpace(required(lookup, name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func valueOr(lookup Lookup, name, fallback string) string {
	if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func positiveDuration(lookup Lookup, name string, fallback time.Duration) (time.Duration, error) {
	raw := valueOr(lookup, name, fallback.String())
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %q", name, raw)
	}
	return duration, nil
}

func positiveInt(lookup Lookup, name string, fallback int) (int, error) {
	raw := valueOr(lookup, name, strconv.Itoa(fallback))
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, raw)
	}
	return value, nil
}

func boolOr(lookup Lookup, name string, fallback bool) (bool, error) {
	raw := valueOr(lookup, name, strconv.FormatBool(fallback))
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", name, raw)
	}
	return value, nil
}
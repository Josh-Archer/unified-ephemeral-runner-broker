package model

import "time"

type BackendName string

const (
	BackendARC            BackendName = "arc"
	BackendCodeBuild      BackendName = "codebuild"
	BackendLambda         BackendName = "lambda"
	BackendCloudRun       BackendName = "cloud-run"
	BackendAzureFunctions BackendName = "azure-functions"
	BackendAzureVM        BackendName = "azure-vm"
	BackendEC2            BackendName = "ec2"
	BackendGCE            BackendName = "gce"
	BackendDesktop        BackendName = "desktop"
)

type PoolName string

const (
	PoolFull PoolName = "full"
	PoolLite PoolName = "lite"
)

type CapabilityProfile string

const (
	CapabilityFull CapabilityProfile = "full"
	CapabilityLite CapabilityProfile = "lite"
)

type AllocationState string

const (
	StateReserved    AllocationState = "reserved"
	StateReady       AllocationState = "ready"
	StateWarm        AllocationState = "warm"
	StatePending     AllocationState = "pending"
	StateCanceled    AllocationState = "canceled"
	StateExpired     AllocationState = "expired"
	StateFailed      AllocationState = "failed"
	StateCompleted   AllocationState = "completed"
	StateQuarantined AllocationState = "quarantined"
)

type PriorityClass string

const (
	PriorityClassNormal PriorityClass = "normal"
	PriorityClassHigh   PriorityClass = "high"
	// Optional lane names commonly used with fairShare.priorityClasses /
	// fairShare.softReserves (for example deploy > pr > smoke). Configure weights
	// and soft reserves explicitly; these constants are documentation aids only.
	PriorityClassDeploy PriorityClass = "deploy"
	PriorityClassPR     PriorityClass = "pr"
	PriorityClassSmoke  PriorityClass = "smoke"
)

type GitHubScope struct {
	Type              string `yaml:"type" json:"type"`
	Organization      string `yaml:"organization" json:"organization"`
	Owner             string `yaml:"owner,omitempty" json:"owner,omitempty"`
	Repository        string `yaml:"repository,omitempty" json:"repository,omitempty"`
	RunnerGroupPrefix string `yaml:"runnerGroupPrefix" json:"runnerGroupPrefix"`
}

type GitHubAuth struct {
	Mode      string `yaml:"mode" json:"mode"`
	SecretRef string `yaml:"secretRef" json:"secretRef"`
}

type GitHubConfig struct {
	Auth  GitHubAuth  `yaml:"auth" json:"auth"`
	Scope GitHubScope `yaml:"scope" json:"scope"`
}

// OIDCPolicyConfig restricts which GitHub Actions identities may allocate
// runners. Empty lists are permissive (any authenticated subject is allowed).
// When either list is non-empty, the caller's identity must match at least one
// entry across the configured lists (union of repository and owner allowlists).
//
// Entries are exact matches, or simple trailing wildcards:
//   - "owner/repo" exact repository
//   - "owner/*"    any repository under owner
//   - "owner"      exact owner (for allowedOwners)
type OIDCPolicyConfig struct {
	AllowedRepositories []string `yaml:"allowedRepositories,omitempty" json:"allowedRepositories,omitempty"`
	AllowedOwners       []string `yaml:"allowedOwners,omitempty" json:"allowedOwners,omitempty"`
}

type BrokerAPIConfig struct {
	OIDCAudience string           `yaml:"oidcAudience" json:"oidcAudience"`
	OIDCPolicy   OIDCPolicyConfig `yaml:"oidcPolicy,omitempty" json:"oidcPolicy,omitempty"`
}

type StateStoreConfig struct {
	// Type selects the store backend: memory (default), file, or postgres.
	// memory and file are single-replica development/restart-recovery options.
	// postgres is the supported multi-replica shared store.
	Type string `yaml:"type,omitempty" json:"type,omitempty"`
	// Path is required when type is file.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// DSN is an optional direct PostgreSQL connection string when type is postgres.
	DSN string `yaml:"dsn,omitempty" json:"dsn,omitempty"`
	// DSNEnv names the environment variable that holds the PostgreSQL DSN when
	// type is postgres and DSN is empty. Defaults to UECB_STATE_STORE_DSN.
	DSNEnv string `yaml:"dsnEnv,omitempty" json:"dsnEnv,omitempty"`
}

// HAConfig controls multi-replica coordination. When empty, HA is implied by
// stateStore.type=postgres.
type HAConfig struct {
	// Enabled forces HA coordination (leader election). Defaults to true when
	// stateStore.type is postgres.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// LeaseTTL is how long a background-work leader lease is held before renewal.
	LeaseTTL time.Duration `yaml:"leaseTTL,omitempty" json:"leaseTTL,omitempty"`
	// Identity overrides the process identity used for leader election
	// (defaults to HOSTNAME / pod name).
	Identity string `yaml:"identity,omitempty" json:"identity,omitempty"`
}

type AdmissionQueueConfig struct {
	Enabled     bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	RetryAfter  time.Duration `yaml:"retryAfter,omitempty" json:"retryAfter,omitempty"`
	MaxAttempts int           `yaml:"maxAttempts,omitempty" json:"maxAttempts,omitempty"`
}

type BrokerRuntimeConfig struct {
	DefaultPool          PoolName      `yaml:"defaultPool" json:"defaultPool"`
	DefaultJobTimeout    time.Duration `yaml:"defaultJobTimeout" json:"defaultJobTimeout"`
	AllowUnauthenticated bool          `yaml:"allowUnauthenticated" json:"allowUnauthenticated"`
	// OrphanCleanup is the broker-side stuck-allocation reaper. Active
	// reserved/ready allocations past expires_at (+ optional gracePeriod) are
	// marked terminal so maxRunners capacity is released when finalize never runs.
	OrphanCleanup struct {
		Enabled       bool          `yaml:"enabled" json:"enabled"`
		QuarantineTTL time.Duration `yaml:"quarantineTTL" json:"quarantineTTL"`
		// GracePeriod is extra time after job_timeout (allocation expires_at)
		// before the reaper marks the allocation terminal and releases capacity.
		// Zero (default) reaps immediately when expires_at is reached.
		GracePeriod time.Duration `yaml:"gracePeriod,omitempty" json:"gracePeriod,omitempty"`
	} `yaml:"orphanCleanup" json:"orphanCleanup"`
	API          BrokerAPIConfig      `yaml:"api" json:"api"`
	StateStore   StateStoreConfig     `yaml:"stateStore,omitempty" json:"stateStore,omitempty"`
	HA           HAConfig             `yaml:"ha,omitempty" json:"ha,omitempty"`
	Queue        AdmissionQueueConfig `yaml:"queue,omitempty" json:"queue,omitempty"`
	TierRouting   TierRoutingConfig    `yaml:"tierRouting,omitempty" json:"tierRouting,omitempty"`
	LiveCapacity  LiveCapacityConfig   `yaml:"liveCapacity,omitempty" json:"liveCapacity,omitempty"`
	QualityAware  QualityAwareConfig   `yaml:"qualityAware,omitempty" json:"qualityAware,omitempty"`
}

// LiveCapacityConfig controls optional provider-reported capacity routing.
// When disabled, the broker uses only local scheduler accounting and
// configured maxRunners. When enabled, backends that implement Capacity()
// (SDK adapters, built-in arc/desktop, and HTTP-dispatch capacity_url) are
// polled out of band and exhausted providers are filtered before scheduler
// selection.
type LiveCapacityConfig struct {
	Enabled          bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	RefreshInterval  time.Duration `yaml:"refreshInterval,omitempty" json:"refreshInterval,omitempty"`
	StaleAfter       time.Duration `yaml:"staleAfter,omitempty" json:"staleAfter,omitempty"`
	ProbeTimeout     time.Duration `yaml:"probeTimeout,omitempty" json:"probeTimeout,omitempty"`
	FailureMode      string        `yaml:"failureMode,omitempty" json:"failureMode,omitempty"`
	RefreshOnStartup bool          `yaml:"refreshOnStartup,omitempty" json:"refreshOnStartup,omitempty"`
}

// QualityAwareConfig controls optional quality-aware auto backend selection.
// When enabled, unpinned allocations rank eligible backends by free slots,
// historical success rate, p95 ready latency, and recent capacity errors.
// Pins, capability filters, tier, live capacity, and admission still apply
// first; provision failure still falls back to remaining eligible backends.
type QualityAwareConfig struct {
	Enabled    bool                  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Window     time.Duration         `yaml:"window,omitempty" json:"window,omitempty"`
	MinSamples int                   `yaml:"minSamples,omitempty" json:"minSamples,omitempty"`
	Weights    QualityAwareWeights   `yaml:"weights,omitempty" json:"weights,omitempty"`
}

// QualityAwareWeights scales score components. Zero values use defaults when
// every weight is zero; explicit zeros are otherwise preserved only if at least
// one weight is non-zero (see quality.Weights.Normalize).
type QualityAwareWeights struct {
	FreeSlots      float64 `yaml:"freeSlots,omitempty" json:"freeSlots,omitempty"`
	SuccessRate    float64 `yaml:"successRate,omitempty" json:"successRate,omitempty"`
	Latency        float64 `yaml:"latency,omitempty" json:"latency,omitempty"`
	CapacityErrors float64 `yaml:"capacityErrors,omitempty" json:"capacityErrors,omitempty"`
}

type TierRoutingConfig struct {
	Enabled          bool                          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	RefreshInterval  time.Duration                 `yaml:"refreshInterval,omitempty" json:"refreshInterval,omitempty"`
	StaleAfter       time.Duration                 `yaml:"staleAfter,omitempty" json:"staleAfter,omitempty"`
	FailureMode      string                        `yaml:"failureMode,omitempty" json:"failureMode,omitempty"`
	FallbackBackends []BackendName                 `yaml:"fallbackBackends,omitempty" json:"fallbackBackends,omitempty"`
	Prometheus       TierPrometheusConfig          `yaml:"prometheus,omitempty" json:"prometheus,omitempty"`
	Providers        map[string]TierProviderConfig `yaml:"providers,omitempty" json:"providers,omitempty"`
	ProviderRules    []ProviderTierRuleConfig      `yaml:"providerRules,omitempty" json:"providerRules,omitempty"`
	RefreshOnStartup bool                          `yaml:"refreshOnStartup,omitempty" json:"refreshOnStartup,omitempty"`
}

type TierPrometheusConfig struct {
	URL       string        `yaml:"url,omitempty" json:"url,omitempty"`
	Timeout   time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	SecretRef string        `yaml:"secretRef,omitempty" json:"secretRef,omitempty"`
}

type TierProviderConfig struct {
	Provider         string            `yaml:"provider,omitempty" json:"provider,omitempty"`
	Mode             string            `yaml:"mode,omitempty" json:"mode,omitempty"`
	SecretRef        string            `yaml:"secretRef,omitempty" json:"secretRef,omitempty"`
	AccountID        string            `yaml:"accountId,omitempty" json:"accountId,omitempty"`
	SubscriptionID   string            `yaml:"subscriptionId,omitempty" json:"subscriptionId,omitempty"`
	ProjectID        string            `yaml:"projectId,omitempty" json:"projectId,omitempty"`
	BillingAccountID string            `yaml:"billingAccountId,omitempty" json:"billingAccountId,omitempty"`
	BudgetName       string            `yaml:"budgetName,omitempty" json:"budgetName,omitempty"`
	Region           string            `yaml:"region,omitempty" json:"region,omitempty"`
	Labels           map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

type CircuitBreakerConfig struct {
	Enabled                  bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	FailureThreshold         int           `yaml:"failureThreshold,omitempty" json:"failureThreshold,omitempty"`
	EvaluationWindow         time.Duration `yaml:"evaluationWindow,omitempty" json:"evaluationWindow,omitempty"`
	OpenDuration             time.Duration `yaml:"openDuration,omitempty" json:"openDuration,omitempty"`
	ProbeInterval            time.Duration `yaml:"probeInterval,omitempty" json:"probeInterval,omitempty"`
	ProbeTimeout             time.Duration `yaml:"probeTimeout,omitempty" json:"probeTimeout,omitempty"`
	RecoverySuccessThreshold int           `yaml:"recoverySuccessThreshold,omitempty" json:"recoverySuccessThreshold,omitempty"`
	HalfOpenMaxRequests      int           `yaml:"halfOpenMaxRequests,omitempty" json:"halfOpenMaxRequests,omitempty"`
	TripReasons              []string      `yaml:"tripReasons,omitempty" json:"tripReasons,omitempty"`
}

type RateLimitConfig struct {
	Enabled  bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Permits  int           `yaml:"permits,omitempty" json:"permits,omitempty"`
	Interval time.Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
	Burst    int           `yaml:"burst,omitempty" json:"burst,omitempty"`
}

// BudgetConfig is an optional per-backend allocation budget guardrail.
// When enabled, the broker tracks successful ready allocations against daily
// and/or monthly operator-set quotas (UTC calendar windows) and marks the
// backend ineligible once either limit is exceeded. This is orthogonal to
// tierRouting (provider billing APIs) and rateLimit (short cold-launch windows).
//
// Counters are local by default and can be shared across replicas via the
// admission state document. Pluggable external sources (metrics/cloud APIs)
// may override reported usage when wired into the budget tracker.
type BudgetConfig struct {
	Enabled               bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	MaxAllocationsDaily   int  `yaml:"maxAllocationsDaily,omitempty" json:"maxAllocationsDaily,omitempty"`
	MaxAllocationsMonthly int  `yaml:"maxAllocationsMonthly,omitempty" json:"maxAllocationsMonthly,omitempty"`
}

type DesktopConfig struct {
	Address   string `yaml:"address" json:"address"`
	CheckPort int    `yaml:"checkPort" json:"checkPort"`
}

type BackendConfig struct {
	Enabled        bool                 `yaml:"enabled" json:"enabled"`
	Healthy        bool                 `yaml:"healthy" json:"healthy"`
	MaxRunners     int                  `yaml:"maxRunners" json:"maxRunners"`
	WarmMin        int                  `yaml:"warmMin" json:"warmMin"`
	WarmMax        int                  `yaml:"warmMax" json:"warmMax"`
	WarmTTL        time.Duration        `yaml:"warmTTL,omitempty" json:"warmTTL,omitempty"`
	// WarmSchedule optionally gates warmMin/warmMax to calendar windows so cold
	// cloud backends (Lambda, Cloud Run, Azure Functions, …) can pre-warm only
	// during known CI periods and stay at zero otherwise for cost control.
	WarmSchedule   *WarmScheduleConfig  `yaml:"warmSchedule,omitempty" json:"warmSchedule,omitempty"`
	Weight         int                  `yaml:"weight,omitempty" json:"weight,omitempty"`
	// CostClass ranks relative expense for scheduler tie-breaking when free
	// capacity is equal. Lower is cheaper. Zero means use the built-in default
	// for the backend name (cluster-local first, then free-tier cloud, then VMs).
	CostClass      int                  `yaml:"costClass,omitempty" json:"costClass,omitempty"`
	MaxJobDuration time.Duration        `yaml:"maxJobDuration,omitempty" json:"maxJobDuration,omitempty"`
	Capabilities   []string             `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	RunnerLabel    string               `yaml:"runnerLabel,omitempty" json:"runnerLabel,omitempty"`
	Template       string               `yaml:"template,omitempty" json:"template,omitempty"`
	SecretRef      string               `yaml:"secretRef,omitempty" json:"secretRef,omitempty"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuitBreaker,omitempty" json:"circuitBreaker,omitempty"`
	RateLimit      RateLimitConfig      `yaml:"rateLimit,omitempty" json:"rateLimit,omitempty"`
	Budget         BudgetConfig         `yaml:"budget,omitempty" json:"budget,omitempty"`
	TierRules      []TierRuleConfig     `yaml:"tierRules,omitempty" json:"tierRules,omitempty"`
	Desktop        *DesktopConfig       `yaml:"desktop,omitempty" json:"desktop,omitempty"`
}

// WarmScheduleConfig controls when warm pool targets apply. With no windows,
// warmMin/warmMax apply continuously. With one or more windows, warm capacity
// is maintained only while "now" falls inside a matching window; outside all
// windows the effective warm target is zero (idle warm runners are recycled).
type WarmScheduleConfig struct {
	// Timezone is an IANA name such as "America/New_York". Empty means UTC.
	Timezone string `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	// Windows are OR-ed: any matching window activates warm capacity.
	Windows []WarmWindowConfig `yaml:"windows,omitempty" json:"windows,omitempty"`
}

// WarmWindowConfig is a local-time interval when warm capacity is desired.
// Start and End use 24h "HH:MM" or "HH:MM:SS". When End is before or equal to
// Start, the window crosses midnight (e.g. 22:00–06:00).
type WarmWindowConfig struct {
	// Days lists weekday names (mon..sun). Empty means every day.
	Days []string `yaml:"days,omitempty" json:"days,omitempty"`
	Start string  `yaml:"start" json:"start"`
	End   string  `yaml:"end" json:"end"`
	// Optional per-window overrides of backend warmMin/warmMax.
	WarmMin *int `yaml:"warmMin,omitempty" json:"warmMin,omitempty"`
	WarmMax *int `yaml:"warmMax,omitempty" json:"warmMax,omitempty"`
}

type TierRuleConfig struct {
	Name               string        `yaml:"name,omitempty" json:"name,omitempty"`
	ProviderRef        string        `yaml:"providerRef,omitempty" json:"providerRef,omitempty"`
	Dimension          string        `yaml:"dimension,omitempty" json:"dimension,omitempty"`
	UsageQuery         string        `yaml:"usageQuery,omitempty" json:"usageQuery,omitempty"`
	BurnRateQuery      string        `yaml:"burnRateQuery,omitempty" json:"burnRateQuery,omitempty"`
	LimitSources       []string      `yaml:"limitSources,omitempty" json:"limitSources,omitempty"`
	Combine            string        `yaml:"combine,omitempty" json:"combine,omitempty"`
	SoftLimitRatio     float64       `yaml:"softLimitRatio,omitempty" json:"softLimitRatio,omitempty"`
	HardLimitRatio     float64       `yaml:"hardLimitRatio,omitempty" json:"hardLimitRatio,omitempty"`
	MinRemainingCredit float64       `yaml:"minRemainingCredit,omitempty" json:"minRemainingCredit,omitempty"`
	ProjectionWindow   time.Duration `yaml:"projectionWindow,omitempty" json:"projectionWindow,omitempty"`
	Action             string        `yaml:"action,omitempty" json:"action,omitempty"`
}

type ProviderTierRuleConfig struct {
	Name               string        `yaml:"name,omitempty" json:"name,omitempty"`
	ProviderRef        string        `yaml:"providerRef,omitempty" json:"providerRef,omitempty"`
	Backends           []BackendName `yaml:"backends,omitempty" json:"backends,omitempty"`
	UsageQuery         string        `yaml:"usageQuery,omitempty" json:"usageQuery,omitempty"`
	BurnRateQuery      string        `yaml:"burnRateQuery,omitempty" json:"burnRateQuery,omitempty"`
	SoftLimitRatio     float64       `yaml:"softLimitRatio,omitempty" json:"softLimitRatio,omitempty"`
	HardLimitRatio     float64       `yaml:"hardLimitRatio,omitempty" json:"hardLimitRatio,omitempty"`
	MinRemainingCredit float64       `yaml:"minRemainingCredit,omitempty" json:"minRemainingCredit,omitempty"`
	ProjectionWindow   time.Duration `yaml:"projectionWindow,omitempty" json:"projectionWindow,omitempty"`
	Action             string        `yaml:"action,omitempty" json:"action,omitempty"`
}

type FairShareConfig struct {
	Enabled         bool           `yaml:"enabled" json:"enabled"`
	UsageWindow     time.Duration  `yaml:"usageWindow,omitempty" json:"usageWindow,omitempty"`
	StarvationAfter time.Duration  `yaml:"starvationAfter,omitempty" json:"starvationAfter,omitempty"`
	// PriorityClasses maps priority_class / lane names to positive weights.
	// Higher weight ranks better under fair-share scoring and is treated as a
	// higher lane for soft-reserve protection (for example deploy:3, pr:2, smoke:1).
	PriorityClasses map[string]int `yaml:"priorityClasses,omitempty" json:"priorityClasses,omitempty"`
	// SoftReserves holds slots per priority class (or lane) on each backend so
	// lower-weight classes cannot fill maxRunners and starve higher lanes.
	// A request only sees soft reserves for classes with strictly higher weight
	// as unavailable capacity: effectiveMax = maxRunners - sum(higher softReserves).
	// Soft reserves never exceed hard maxRunners for the protected class itself.
	SoftReserves map[string]int `yaml:"softReserves,omitempty" json:"softReserves,omitempty"`
	// Quotas are optional hard caps on concurrent active allocations per tenant
	// (repo, workflow label, or other queue identity passed as tenant).
	Quotas map[string]int `yaml:"quotas,omitempty" json:"quotas,omitempty"`
}

type PoolConfig struct {
	Name              PoolName                      `yaml:"name" json:"name"`
	Labels            []string                      `yaml:"labels" json:"labels"`
	Scheduler         string                        `yaml:"scheduler" json:"scheduler"`
	FairShare         FairShareConfig               `yaml:"fairShare,omitempty" json:"fairShare,omitempty"`
	CapabilityProfile CapabilityProfile             `yaml:"capabilityProfile" json:"capabilityProfile"`
	Backends          map[BackendName]BackendConfig `yaml:"backends" json:"backends"`
}

type BrokerConfig struct {
	GitHub GitHubConfig        `yaml:"github" json:"github"`
	Broker BrokerRuntimeConfig `yaml:"broker" json:"broker"`
	Pools  []PoolConfig        `yaml:"pools" json:"pools"`
}

type AllocationRequest struct {
	Pool                 PoolName      `json:"pool"`
	Backend              *BackendName  `json:"backend,omitempty"`
	JobTimeout           time.Duration `json:"job_timeout"`
	Tenant               string        `json:"tenant,omitempty"`
	PriorityClass        string        `json:"priority_class,omitempty"`
	Labels               []string      `json:"labels,omitempty"`
	RequiredCapabilities []string      `json:"required_capabilities,omitempty"`
	ExcludedCapabilities []string      `json:"excluded_capabilities,omitempty"`
}

type AllocationStatus struct {
	ID              string      `json:"allocation_id"`
	CorrelationID   string      `json:"correlation_id,omitempty"`
	Pool            PoolName    `json:"pool"`
	SelectedBackend BackendName `json:"selected_backend"`
	RunnerLabel     string      `json:"runner_label"`
	RequestedLabels []string    `json:"requested_labels,omitempty"`
	Tenant          string      `json:"tenant,omitempty"`
	PriorityClass   string      `json:"priority_class,omitempty"`
	// Subject/Repository/Owner bind the allocating OIDC principal for ownership checks.
	Subject    string             `json:"subject,omitempty"`
	Repository string             `json:"repository,omitempty"`
	Owner      string             `json:"owner,omitempty"`
	ExpiresAt  time.Time          `json:"expires_at"`
	RetryAfter time.Time          `json:"retry_after,omitempty"`
	Attempts   int                `json:"attempts,omitempty"`
	State      AllocationState    `json:"state"`
	Error      string             `json:"error,omitempty"`
	Metadata   map[string]string  `json:"metadata,omitempty"`
	Request    *AllocationRequest `json:"request,omitempty"`
}

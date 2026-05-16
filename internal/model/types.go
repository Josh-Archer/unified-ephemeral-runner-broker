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

type BrokerAPIConfig struct {
	OIDCAudience string `yaml:"oidcAudience" json:"oidcAudience"`
}

type StateStoreConfig struct {
	Type string `yaml:"type,omitempty" json:"type,omitempty"`
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
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
	OrphanCleanup        struct {
		Enabled       bool          `yaml:"enabled" json:"enabled"`
		QuarantineTTL time.Duration `yaml:"quarantineTTL" json:"quarantineTTL"`
	} `yaml:"orphanCleanup" json:"orphanCleanup"`
	API         BrokerAPIConfig      `yaml:"api" json:"api"`
	StateStore  StateStoreConfig     `yaml:"stateStore,omitempty" json:"stateStore,omitempty"`
	Queue       AdmissionQueueConfig `yaml:"queue,omitempty" json:"queue,omitempty"`
	TierRouting TierRoutingConfig    `yaml:"tierRouting,omitempty" json:"tierRouting,omitempty"`
}

type TierRoutingConfig struct {
	Enabled          bool                          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	RefreshInterval  time.Duration                 `yaml:"refreshInterval,omitempty" json:"refreshInterval,omitempty"`
	StaleAfter       time.Duration                 `yaml:"staleAfter,omitempty" json:"staleAfter,omitempty"`
	FailureMode      string                        `yaml:"failureMode,omitempty" json:"failureMode,omitempty"`
	FallbackBackends []BackendName                 `yaml:"fallbackBackends,omitempty" json:"fallbackBackends,omitempty"`
	Prometheus       TierPrometheusConfig          `yaml:"prometheus,omitempty" json:"prometheus,omitempty"`
	Providers        map[string]TierProviderConfig `yaml:"providers,omitempty" json:"providers,omitempty"`
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

type BackendConfig struct {
	Enabled        bool                 `yaml:"enabled" json:"enabled"`
	Healthy        bool                 `yaml:"healthy" json:"healthy"`
	MaxRunners     int                  `yaml:"maxRunners" json:"maxRunners"`
	WarmMin        int                  `yaml:"warmMin" json:"warmMin"`
	WarmMax        int                  `yaml:"warmMax" json:"warmMax"`
	WarmTTL        time.Duration        `yaml:"warmTTL,omitempty" json:"warmTTL,omitempty"`
	Weight         int                  `yaml:"weight,omitempty" json:"weight,omitempty"`
	MaxJobDuration time.Duration        `yaml:"maxJobDuration,omitempty" json:"maxJobDuration,omitempty"`
	Capabilities   []string             `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	RunnerLabel    string               `yaml:"runnerLabel,omitempty" json:"runnerLabel,omitempty"`
	Template       string               `yaml:"template,omitempty" json:"template,omitempty"`
	SecretRef      string               `yaml:"secretRef,omitempty" json:"secretRef,omitempty"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuitBreaker,omitempty" json:"circuitBreaker,omitempty"`
	RateLimit      RateLimitConfig      `yaml:"rateLimit,omitempty" json:"rateLimit,omitempty"`
	TierRules      []TierRuleConfig     `yaml:"tierRules,omitempty" json:"tierRules,omitempty"`
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

type FairShareConfig struct {
	Enabled         bool           `yaml:"enabled" json:"enabled"`
	UsageWindow     time.Duration  `yaml:"usageWindow,omitempty" json:"usageWindow,omitempty"`
	StarvationAfter time.Duration  `yaml:"starvationAfter,omitempty" json:"starvationAfter,omitempty"`
	PriorityClasses map[string]int `yaml:"priorityClasses,omitempty" json:"priorityClasses,omitempty"`
	Quotas          map[string]int `yaml:"quotas,omitempty" json:"quotas,omitempty"`
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
	ID              string             `json:"allocation_id"`
	CorrelationID   string             `json:"correlation_id,omitempty"`
	Pool            PoolName           `json:"pool"`
	SelectedBackend BackendName        `json:"selected_backend"`
	RunnerLabel     string             `json:"runner_label"`
	RequestedLabels []string           `json:"requested_labels,omitempty"`
	Tenant          string             `json:"tenant,omitempty"`
	PriorityClass   string             `json:"priority_class,omitempty"`
	ExpiresAt       time.Time          `json:"expires_at"`
	RetryAfter      time.Time          `json:"retry_after,omitempty"`
	Attempts        int                `json:"attempts,omitempty"`
	State           AllocationState    `json:"state"`
	Error           string             `json:"error,omitempty"`
	Metadata        map[string]string  `json:"metadata,omitempty"`
	Request         *AllocationRequest `json:"request,omitempty"`
}

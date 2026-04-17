package model

import "time"

type BackendName string

const (
	BackendARC            BackendName = "arc"
	BackendLambda         BackendName = "lambda"
	BackendCloudRun       BackendName = "cloud-run"
	BackendAzureFunctions BackendName = "azure-functions"
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
	StateReserved AllocationState = "reserved"
	StateReady    AllocationState = "ready"
	StateCanceled AllocationState = "canceled"
	StateExpired  AllocationState = "expired"
	StateFailed   AllocationState = "failed"
)

type GitHubScope struct {
	Type              string `yaml:"type" json:"type"`
	Organization      string `yaml:"organization" json:"organization"`
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

type BrokerRuntimeConfig struct {
	DefaultPool          PoolName        `yaml:"defaultPool" json:"defaultPool"`
	DefaultJobTimeout    time.Duration   `yaml:"defaultJobTimeout" json:"defaultJobTimeout"`
	AllowUnauthenticated bool            `yaml:"allowUnauthenticated" json:"allowUnauthenticated"`
	API                  BrokerAPIConfig `yaml:"api" json:"api"`
}

type BackendConfig struct {
	Enabled        bool          `yaml:"enabled" json:"enabled"`
	Healthy        bool          `yaml:"healthy" json:"healthy"`
	MaxRunners     int           `yaml:"maxRunners" json:"maxRunners"`
	MaxJobDuration time.Duration `yaml:"maxJobDuration,omitempty" json:"maxJobDuration,omitempty"`
	Template       string        `yaml:"template,omitempty" json:"template,omitempty"`
	SecretRef      string        `yaml:"secretRef,omitempty" json:"secretRef,omitempty"`
}

type PoolConfig struct {
	Name              PoolName                      `yaml:"name" json:"name"`
	Labels            []string                      `yaml:"labels" json:"labels"`
	Scheduler         string                        `yaml:"scheduler" json:"scheduler"`
	CapabilityProfile CapabilityProfile             `yaml:"capabilityProfile" json:"capabilityProfile"`
	Backends          map[BackendName]BackendConfig `yaml:"backends" json:"backends"`
}

type BrokerConfig struct {
	GitHub GitHubConfig        `yaml:"github" json:"github"`
	Broker BrokerRuntimeConfig `yaml:"broker" json:"broker"`
	Pools  []PoolConfig        `yaml:"pools" json:"pools"`
}

type AllocationRequest struct {
	Pool       PoolName      `json:"pool"`
	Backend    *BackendName  `json:"backend,omitempty"`
	JobTimeout time.Duration `json:"job_timeout"`
	Labels     []string      `json:"labels,omitempty"`
}

type AllocationStatus struct {
	ID              string            `json:"allocation_id"`
	Pool            PoolName          `json:"pool"`
	SelectedBackend BackendName       `json:"selected_backend"`
	RunnerLabel     string            `json:"runner_label"`
	RequestedLabels []string          `json:"requested_labels,omitempty"`
	ExpiresAt       time.Time         `json:"expires_at"`
	State           AllocationState   `json:"state"`
	Error           string            `json:"error,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

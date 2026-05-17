package config

import (
	"os"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"gopkg.in/yaml.v3"
)

func Default() model.BrokerConfig {
	return model.BrokerConfig{
		GitHub: model.GitHubConfig{
			Auth: model.GitHubAuth{
				Mode:      "github-app",
				SecretRef: "uecb-github-app",
			},
			Scope: model.GitHubScope{
				Type:              "organization",
				Organization:      "my-org",
				RunnerGroupPrefix: "uecb",
			},
		},
		Broker: model.BrokerRuntimeConfig{
			DefaultPool:       model.PoolFull,
			DefaultJobTimeout: 15 * time.Minute,
			OrphanCleanup: struct {
				Enabled       bool          `yaml:"enabled" json:"enabled"`
				QuarantineTTL time.Duration `yaml:"quarantineTTL" json:"quarantineTTL"`
			}{
				Enabled:       false,
				QuarantineTTL: 15 * time.Minute,
			},
			API: model.BrokerAPIConfig{
				OIDCAudience: "uecb-broker",
			},
			StateStore: model.StateStoreConfig{
				Type: "memory",
			},
			Queue: model.AdmissionQueueConfig{
				Enabled:     false,
				RetryAfter:  30 * time.Second,
				MaxAttempts: 3,
			},
			TierRouting: model.TierRoutingConfig{
				Enabled:          false,
				RefreshInterval:  5 * time.Minute,
				StaleAfter:       15 * time.Minute,
				FailureMode:      "pass-through-round-robin",
				RefreshOnStartup: true,
				Prometheus: model.TierPrometheusConfig{
					Timeout: 2 * time.Second,
				},
			},
		},
		Pools: []model.PoolConfig{
			{
				Name:      model.PoolFull,
				Labels:    []string{"self-hosted", "linux", "x64", "uecb", "uecb-full"},
				Scheduler: "round-robin",
				FairShare: model.FairShareConfig{
					Enabled: false,
					PriorityClasses: map[string]int{
						string(model.PriorityClassNormal): 1,
						string(model.PriorityClassHigh):   2,
					},
				},
				CapabilityProfile: model.CapabilityFull,
				Backends: map[model.BackendName]model.BackendConfig{
					model.BackendARC: {
						Enabled:      true,
						Healthy:      true,
						MaxRunners:   4,
						Weight:       1,
						Capabilities: []string{"cluster-local", "docker", "region:local"},
						Template:     "arc-full",
					},
				},
			},
			{
				Name:      model.PoolLite,
				Labels:    []string{"self-hosted", "linux", "x64", "uecb", "uecb-lite"},
				Scheduler: "round-robin",
				FairShare: model.FairShareConfig{
					Enabled: false,
					PriorityClasses: map[string]int{
						string(model.PriorityClassNormal): 1,
						string(model.PriorityClassHigh):   2,
					},
				},
				CapabilityProfile: model.CapabilityLite,
				Backends: map[model.BackendName]model.BackendConfig{
					model.BackendARC: {
						Enabled:      true,
						Healthy:      true,
						MaxRunners:   2,
						Weight:       1,
						Capabilities: []string{"cluster-local", "docker", "region:local"},
						Template:     "arc-lite",
					},
					model.BackendCodeBuild: {
						Enabled:        false,
						Healthy:        true,
						MaxRunners:     3,
						Weight:         1,
						MaxJobDuration: 14 * time.Minute,
						Capabilities:   []string{"docker", "region:aws-us-east-1"},
						SecretRef:      "uecb-codebuild",
					},
					model.BackendLambda: {
						Enabled:        false,
						Healthy:        true,
						MaxRunners:     3,
						Weight:         1,
						MaxJobDuration: 14 * time.Minute,
						Capabilities:   []string{"region:aws-us-east-1"},
						SecretRef:      "uecb-lambda",
					},
					model.BackendCloudRun: {
						Enabled:        false,
						Healthy:        true,
						MaxRunners:     2,
						Weight:         1,
						MaxJobDuration: 30 * time.Minute,
						Capabilities:   []string{"region:gcp-us-central1"},
						SecretRef:      "uecb-cloud-run",
					},
					model.BackendAzureFunctions: {
						Enabled:        false,
						Healthy:        true,
						MaxRunners:     2,
						Weight:         1,
						MaxJobDuration: 25 * time.Minute,
						Capabilities:   []string{"region:azure-eastus"},
						SecretRef:      "uecb-azure-functions",
					},
					model.BackendAzureVM: {
						Enabled:        false,
						Healthy:        true,
						MaxRunners:     1,
						Weight:         1,
						MaxJobDuration: 6 * time.Hour,
						Capabilities:   []string{"docker", "privileged", "vm", "cloud:azure", "region:azure-eastus"},
						RunnerLabel:    "replace-with-private-azure-vm-runner-label",
					},
					model.BackendEC2: {
						Enabled:        false,
						Healthy:        true,
						MaxRunners:     1,
						Weight:         1,
						MaxJobDuration: 6 * time.Hour,
						Capabilities:   []string{"docker", "privileged", "vm", "cloud:aws", "region:aws-us-east-1"},
						SecretRef:      "uecb-ec2",
					},
					model.BackendGCE: {
						Enabled:        false,
						Healthy:        true,
						MaxRunners:     1,
						Weight:         1,
						MaxJobDuration: 6 * time.Hour,
						Capabilities:   []string{"docker", "privileged", "vm", "cloud:gcp", "region:gcp-us-central1"},
						SecretRef:      "uecb-gce",
					},
				},
			},
		},
	}
}

func Load(path string) (model.BrokerConfig, error) {
	if path == "" {
		return Default(), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return model.BrokerConfig{}, err
	}

	cfg := Default()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return model.BrokerConfig{}, err
	}
	if err := Validate(cfg); err != nil {
		return model.BrokerConfig{}, err
	}
	return cfg, nil
}

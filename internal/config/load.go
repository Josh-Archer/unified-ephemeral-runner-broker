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
			API: model.BrokerAPIConfig{
				OIDCAudience: "uecb-broker",
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
						Capabilities:   []string{"region:aws-us-east-1"},
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
	return cfg, nil
}

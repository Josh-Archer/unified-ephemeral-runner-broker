package arc

import (
	"context"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type Backend struct {
	cfg model.BrokerConfig
}

func New(cfg model.BrokerConfig) *Backend {
	return &Backend{cfg: cfg}
}

func (b *Backend) Name() model.BackendName {
	return model.BackendARC
}

func (b *Backend) Provision(_ context.Context, request model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	runnerLabel := b.runnerLabel(allocation.Pool, allocation.ID)
	return backend.ProvisionedRunner{
		RunnerLabel: runnerLabel,
		Metadata: map[string]string{
			"pool":            string(request.Pool),
			"capability":      "full",
			"provisioner":     "arc-job",
			"lifecycle":       "ephemeral",
			"runner_label":    runnerLabel,
			"supports_docker": "true",
		},
	}, nil
}

func (b *Backend) runnerLabel(poolName model.PoolName, allocationID string) string {
	if cfg, ok := b.backendConfig(poolName); ok {
		if runnerLabel := strings.TrimSpace(cfg.RunnerLabel); runnerLabel != "" {
			return runnerLabel
		}
		if template := strings.TrimSpace(cfg.Template); template != "" {
			return template
		}
	}

	return backend.DefaultRunnerLabel(model.BackendARC, allocationID)
}

func (b *Backend) backendConfig(poolName model.PoolName) (model.BackendConfig, bool) {
	for _, pool := range b.cfg.Pools {
		if pool.Name != poolName {
			continue
		}
		cfg, ok := pool.Backends[model.BackendARC]
		return cfg, ok
	}
	return model.BackendConfig{}, false
}

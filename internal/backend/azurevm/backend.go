package azurevm

import (
	"context"
	"fmt"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type Backend struct {
	cfg model.BrokerConfig
}

func New(cfg model.BrokerConfig, clients ...RunnerClient) backend.Backend {
	b := &Backend{cfg: cfg}
	for _, pool := range cfg.Pools {
		if target, ok := pool.Backends[model.BackendAzureVM]; ok && target.AzureVM != nil {
			m := &managedBackend{Backend: b}
			if len(clients) > 0 {
				m.client = clients[0]
			}
			return m
		}
	}
	return b
}

func (b *Backend) Name() model.BackendName {
	return model.BackendAzureVM
}

func (b *Backend) Provision(_ context.Context, _ model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	cfg, ok := b.backendConfig(allocation.Pool)
	if !ok {
		return backend.ProvisionedRunner{}, fmt.Errorf("backend %s is not configured for pool %s", model.BackendAzureVM, allocation.Pool)
	}

	runnerLabel := strings.TrimSpace(cfg.RunnerLabel)
	if runnerLabel == "" {
		runnerLabel = strings.TrimSpace(cfg.Template)
	}
	if runnerLabel == "" {
		return backend.ProvisionedRunner{}, fmt.Errorf("backend %s requires runnerLabel or template", model.BackendAzureVM)
	}

	return backend.ProvisionedRunner{
		RunnerLabel: runnerLabel,
		Metadata: map[string]string{
			"provider":     string(model.BackendAzureVM),
			"runner_label": runnerLabel,
		},
	}, nil
}

func (b *Backend) backendConfig(poolName model.PoolName) (model.BackendConfig, bool) {
	for _, pool := range b.cfg.Pools {
		if pool.Name != poolName {
			continue
		}
		cfg, ok := pool.Backends[model.BackendAzureVM]
		return cfg, ok
	}
	return model.BackendConfig{}, false
}

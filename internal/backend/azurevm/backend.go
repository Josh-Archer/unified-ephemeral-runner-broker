package azurevm

import (
	"context"
	"fmt"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/externaldispatch"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
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

// UsesEphemeralDispatch reports an unmanaged azure-vm pool that points at an
// external launcher (secretRef), the same contract as ec2 and gce. Managed
// named-runner pools stay on the lease adapter even if a secret is also set.
func UsesEphemeralDispatch(cfg model.BrokerConfig) bool {
	sawManaged := false
	sawSecret := false
	for _, pool := range cfg.Pools {
		target, ok := pool.Backends[model.BackendAzureVM]
		if !ok {
			continue
		}
		if target.AzureVM != nil {
			sawManaged = true
		}
		if strings.TrimSpace(target.SecretRef) != "" {
			sawSecret = true
		}
	}
	return sawSecret && !sawManaged
}

// NewFromRuntime selects the azure-vm adapter. Ephemeral pools dispatch a VM
// and can be held warm. Managed pools lease named machines. Static-label pools
// return the configured runner label and are not warm-provisionable.
func NewFromRuntime(cfg model.BrokerConfig, secrets runtime.SecretReader, clients ...RunnerClient) backend.Backend {
	if UsesEphemeralDispatch(cfg) {
		return externaldispatch.New(model.BackendAzureVM, cfg, secrets)
	}
	return New(cfg, clients...)
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

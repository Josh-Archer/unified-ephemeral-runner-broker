package desktop

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

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
	return model.BackendDesktop
}

func (b *Backend) Provision(_ context.Context, request model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	cfg, _ := b.backendConfig(allocation.Pool)

	if cfg.Desktop != nil && cfg.Desktop.Address != "" && cfg.Desktop.CheckPort > 0 {
		address := fmt.Sprintf("%s:%d", cfg.Desktop.Address, cfg.Desktop.CheckPort)
		conn, err := net.DialTimeout("tcp", address, 2*time.Second)
		if err != nil {
			return backend.ProvisionedRunner{}, backend.NewAllocationError(fmt.Errorf("desktop is offline"), backend.ErrBackendCapacityExhausted, true)
		}
		conn.Close()
	}

	runnerLabel := b.runnerLabel(allocation.Pool, allocation.ID)
	return backend.ProvisionedRunner{
		RunnerLabel: runnerLabel,
		Metadata: map[string]string{
			"pool":         string(request.Pool),
			"provisioner":  "desktop",
			"runner_label": runnerLabel,
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

	return backend.DefaultRunnerLabel(model.BackendDesktop, allocationID)
}

func (b *Backend) backendConfig(poolName model.PoolName) (model.BackendConfig, bool) {
	for _, pool := range b.cfg.Pools {
		if pool.Name != poolName {
			continue
		}
		cfg, ok := pool.Backends[model.BackendDesktop]
		return cfg, ok
	}
	return model.BackendConfig{}, false
}

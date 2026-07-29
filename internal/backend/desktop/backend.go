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

// DialFunc opens a TCP connection for optional desktop host probes.
type DialFunc func(network, address string, timeout time.Duration) (net.Conn, error)

type Backend struct {
	cfg  model.BrokerConfig
	dial DialFunc
}

func New(cfg model.BrokerConfig) *Backend {
	return &Backend{cfg: cfg}
}

// WithDialer overrides the host probe dialer (tests).
func (b *Backend) WithDialer(dial DialFunc) *Backend {
	b.dial = dial
	return b
}

func (b *Backend) Name() model.BackendName {
	return model.BackendDesktop
}

func (b *Backend) Provision(_ context.Context, request model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	cfg, _ := b.backendConfig(allocation.Pool)

	if online, err := b.hostOnline(cfg); err != nil {
		return backend.ProvisionedRunner{}, err
	} else if !online {
		return backend.ProvisionedRunner{}, backend.NewAllocationError(fmt.Errorf("desktop is offline"), backend.ErrBackendCapacityExhausted, true)
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

// Capacity reports desktop free slots using the same shape as HTTP capacity JSON.
//
// Uses configured maxRunners as the scale ceiling. When desktop.address and
// desktop.checkPort are set, an offline host is reported as exhausted
// (ActiveRunners == MaxRunners, free slots 0) rather than a probe error so
// live-capacity routing can skip the backend consistently with cloud feeds.
func (b *Backend) Capacity(_ context.Context) (backend.CapacityStatus, error) {
	_, backendCfg, err := b.firstConfiguredPool()
	if err != nil {
		return backend.CapacityStatus{}, err
	}

	maxRunners := backendCfg.MaxRunners
	if maxRunners <= 0 {
		// Desktop hosts are typically single-runner; default the scale to 1
		// when operators omit maxRunners so Capacity() still publishes a
		// usable reading.
		maxRunners = 1
	}

	online, err := b.hostOnline(backendCfg)
	if err != nil {
		return backend.CapacityStatus{}, err
	}
	if !online {
		// Exhausted shape: free_slots = 0 (active fills the ceiling).
		return backend.CapacityStatus{
			MaxRunners:    maxRunners,
			ActiveRunners: maxRunners,
		}, nil
	}

	return backend.CapacityStatus{MaxRunners: maxRunners}, nil
}

func (b *Backend) hostOnline(cfg model.BackendConfig) (bool, error) {
	if cfg.Desktop == nil || strings.TrimSpace(cfg.Desktop.Address) == "" || cfg.Desktop.CheckPort <= 0 {
		// No host probe configured: treat as available at configured scale.
		return true, nil
	}

	address := fmt.Sprintf("%s:%d", strings.TrimSpace(cfg.Desktop.Address), cfg.Desktop.CheckPort)
	dial := b.dial
	if dial == nil {
		dial = func(network, address string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout(network, address, timeout)
		}
	}
	conn, err := dial("tcp", address, 2*time.Second)
	if err != nil {
		return false, nil
	}
	_ = conn.Close()
	return true, nil
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

func (b *Backend) firstConfiguredPool() (model.PoolConfig, model.BackendConfig, error) {
	for _, pool := range b.cfg.Pools {
		if cfg, ok := pool.Backends[model.BackendDesktop]; ok && cfg.Enabled {
			return pool, cfg, nil
		}
	}
	for _, pool := range b.cfg.Pools {
		if cfg, ok := pool.Backends[model.BackendDesktop]; ok {
			return pool, cfg, nil
		}
	}
	return model.PoolConfig{}, model.BackendConfig{}, fmt.Errorf("backend %s is not configured", model.BackendDesktop)
}

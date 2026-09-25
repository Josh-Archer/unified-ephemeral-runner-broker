package desktop

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// DialFunc opens a TCP connection for optional desktop host probes.
type DialFunc func(network, address string, timeout time.Duration) (net.Conn, error)

// ActiveCounter reports the number of scheduler-accounted active allocations
// for a pool and backend. This interface is satisfied directly by store.Store.
type ActiveCounter = backend.ActiveCounter

type Backend struct {
	cfg             model.BrokerConfig
	dial            DialFunc
	mu              sync.Mutex
	active          map[string]struct{}
	activeCounter   backend.ActiveCounter
	activeCountFunc func() int
}

func New(cfg model.BrokerConfig) *Backend {
	return &Backend{
		cfg:    cfg,
		active: make(map[string]struct{}),
	}
}

// WithDialer overrides the host probe dialer (tests).
func (b *Backend) WithDialer(dial DialFunc) *Backend {
	b.dial = dial
	return b
}

// SetActiveCounter configures a durable active allocation counter.
func (b *Backend) SetActiveCounter(counter backend.ActiveCounter) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.activeCounter = counter
}

// WithActiveCounter configures a durable active allocation counter and returns the backend.
func (b *Backend) WithActiveCounter(counter backend.ActiveCounter) *Backend {
	b.SetActiveCounter(counter)
	return b
}

// WithActiveCountFunc overrides the active runner count source (tests).
func (b *Backend) WithActiveCountFunc(fn func() int) *Backend {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.activeCountFunc = fn
	return b
}

// ActiveCountFunc returns a count function summing counter.CountActive(pool, BackendDesktop)
// across all pools in cfg where the desktop backend is configured.
func ActiveCountFunc(cfg model.BrokerConfig, counter backend.ActiveCounter) func() int {
	if counter == nil {
		return nil
	}
	return func() int {
		var total int
		for _, pool := range cfg.Pools {
			if _, ok := pool.Backends[model.BackendDesktop]; ok {
				total += counter.CountActive(pool.Name, model.BackendDesktop)
			}
		}
		return total
	}
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

	b.mu.Lock()
	if b.active == nil {
		b.active = make(map[string]struct{})
	}
	key := strings.TrimSpace(allocation.ID)
	if key == "" {
		key = runnerLabel
	}
	if key != "" {
		b.active[key] = struct{}{}
	}
	b.mu.Unlock()

	return backend.ProvisionedRunner{
		RunnerLabel: runnerLabel,
		Metadata: map[string]string{
			"pool":         string(request.Pool),
			"provisioner":  "desktop",
			"runner_label": runnerLabel,
		},
	}, nil
}

// Cleanup releases runner capacity when an allocation terminates.
func (b *Backend) Cleanup(_ context.Context, allocation model.AllocationStatus) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != nil {
		if id := strings.TrimSpace(allocation.ID); id != "" {
			delete(b.active, id)
		}
		if runnerLabel := strings.TrimSpace(allocation.RunnerLabel); runnerLabel != "" {
			delete(b.active, runnerLabel)
		}
	}
	return nil
}

// Capacity reports desktop free slots using the same shape as HTTP capacity JSON.
//
// Uses configured maxRunners as the scale ceiling. When desktop.address and
// desktop.checkPort are set, an offline host is reported as exhausted
// (ActiveRunners == MaxRunners, free slots 0) rather than a probe error so
// live-capacity routing can skip the backend consistently with cloud feeds.
// When online, active runners reflect current in-flight desktop allocations.
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

	b.mu.Lock()
	fn := b.activeCountFunc
	counter := b.activeCounter
	inMemoryCount := len(b.active)
	b.mu.Unlock()

	var activeRunners int
	if fn != nil {
		activeRunners = fn()
	} else if counter != nil {
		for _, pool := range b.cfg.Pools {
			if _, ok := pool.Backends[model.BackendDesktop]; ok {
				activeRunners += counter.CountActive(pool.Name, model.BackendDesktop)
			}
		}
	} else {
		activeRunners = inMemoryCount
	}
	if activeRunners < 0 {
		activeRunners = 0
	}

	if !online {
		// Exhausted shape: free_slots = 0 (active fills the ceiling).
		active := maxRunners
		if activeRunners > active {
			active = activeRunners
		}
		return backend.CapacityStatus{
			MaxRunners:    maxRunners,
			ActiveRunners: active,
		}, nil
	}

	return backend.CapacityStatus{
		MaxRunners:    maxRunners,
		ActiveRunners: activeRunners,
	}, nil
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

package desktop

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
)

func desktopConfig(maxRunners int, address string, checkPort int) model.BrokerConfig {
	return model.BrokerConfig{
		Pools: []model.PoolConfig{
			{
				Name: model.PoolLite,
				Backends: map[model.BackendName]model.BackendConfig{
					model.BackendDesktop: {
						Enabled:    true,
						Healthy:    true,
						MaxRunners: maxRunners,
						Desktop: &model.DesktopConfig{
							Address:   address,
							CheckPort: checkPort,
						},
					},
				},
			},
		},
	}
}

type stubConn struct{}

func (stubConn) Read([]byte) (int, error)         { return 0, errors.New("unused") }
func (stubConn) Write([]byte) (int, error)        { return 0, errors.New("unused") }
func (stubConn) Close() error                     { return nil }
func (stubConn) LocalAddr() net.Addr              { return nil }
func (stubConn) RemoteAddr() net.Addr             { return nil }
func (stubConn) SetDeadline(time.Time) error      { return nil }
func (stubConn) SetReadDeadline(time.Time) error  { return nil }
func (stubConn) SetWriteDeadline(time.Time) error { return nil }

func TestCapacityOnlineReportsFreeScale(t *testing.T) {
	cfg := desktopConfig(2, "desktop.local", 22)
	b := New(cfg).WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
		if address != "desktop.local:22" {
			t.Fatalf("unexpected probe address %q", address)
		}
		return stubConn{}, nil
	})

	status, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 2 || status.ActiveRunners != 0 {
		t.Fatalf("unexpected capacity %+v", status)
	}
	if free := backend.FreeSlots(status); free != 2 {
		t.Fatalf("expected 2 free slots, got %d", free)
	}
}

func TestCapacityOfflineReportsExhaustion(t *testing.T) {
	cfg := desktopConfig(1, "desktop.local", 22)
	b := New(cfg).WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
		return nil, errors.New("connection refused")
	})

	status, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 1 || status.ActiveRunners != 1 {
		t.Fatalf("expected exhausted shape max=1 active=1, got %+v", status)
	}
	if free := backend.FreeSlots(status); free != 0 {
		t.Fatalf("expected 0 free slots when host offline, got %d", free)
	}
}

func TestCapacityDefaultsMaxRunnersToOne(t *testing.T) {
	cfg := desktopConfig(0, "", 0)
	status, err := New(cfg).Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 1 || backend.FreeSlots(status) != 1 {
		t.Fatalf("expected default maxRunners=1 free, got %+v free=%d", status, backend.FreeSlots(status))
	}
}

func TestCapacityNoProbeUsesConfiguredScale(t *testing.T) {
	cfg := desktopConfig(3, "", 0)
	status, err := New(cfg).Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 3 || backend.FreeSlots(status) != 3 {
		t.Fatalf("unexpected capacity %+v free=%d", status, backend.FreeSlots(status))
	}
}

func TestProvisionOfflineIsCapacityExhausted(t *testing.T) {
	cfg := desktopConfig(1, "desktop.local", 22)
	b := New(cfg).WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
		return nil, errors.New("connection refused")
	})

	_, err := b.Provision(context.Background(), model.AllocationRequest{
		Pool: model.PoolLite,
	}, model.AllocationStatus{
		ID:   "desk-1",
		Pool: model.PoolLite,
	})
	if !backend.IsCapacityExhausted(err) {
		t.Fatalf("expected capacity exhausted, got %v", err)
	}
}

func TestProvisionOnlineReturnsLabel(t *testing.T) {
	cfg := desktopConfig(1, "desktop.local", 22)
	cfg.Pools[0].Backends[model.BackendDesktop] = model.BackendConfig{
		Enabled:     true,
		MaxRunners:  1,
		RunnerLabel: "desktop-runner",
		Desktop: &model.DesktopConfig{
			Address:   "desktop.local",
			CheckPort: 22,
		},
	}
	b := New(cfg).WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
		return stubConn{}, nil
	})

	provisioned, err := b.Provision(context.Background(), model.AllocationRequest{
		Pool: model.PoolLite,
	}, model.AllocationStatus{
		ID:   "desk-2",
		Pool: model.PoolLite,
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if provisioned.RunnerLabel != "desktop-runner" {
		t.Fatalf("unexpected label %q", provisioned.RunnerLabel)
	}
}

func TestCapacityOnlineReportsActiveRunners(t *testing.T) {
	cfg := desktopConfig(2, "desktop.local", 22)
	b := New(cfg).WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
		return stubConn{}, nil
	})

	// Initially 0 active runners, 2 free slots.
	status, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.ActiveRunners != 0 || backend.FreeSlots(status) != 2 {
		t.Fatalf("expected 0 active 2 free, got %+v", status)
	}

	// Provision first runner: active becomes 1, free becomes 1.
	_, err = b.Provision(context.Background(), model.AllocationRequest{Pool: model.PoolLite}, model.AllocationStatus{
		ID:   "desk-1",
		Pool: model.PoolLite,
	})
	if err != nil {
		t.Fatalf("provision 1: %v", err)
	}

	status, err = b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.ActiveRunners != 1 || backend.FreeSlots(status) != 1 {
		t.Fatalf("expected 1 active 1 free, got %+v free=%d", status, backend.FreeSlots(status))
	}

	// Provision second runner: busy / exhausted shape (active=2, free=0).
	_, err = b.Provision(context.Background(), model.AllocationRequest{Pool: model.PoolLite}, model.AllocationStatus{
		ID:   "desk-2",
		Pool: model.PoolLite,
	})
	if err != nil {
		t.Fatalf("provision 2: %v", err)
	}

	status, err = b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.ActiveRunners != 2 || backend.FreeSlots(status) != 0 {
		t.Fatalf("expected 2 active 0 free, got %+v free=%d", status, backend.FreeSlots(status))
	}

	// Cleanup first runner: active returns to 1, free returns to 1.
	if err := b.Cleanup(context.Background(), model.AllocationStatus{ID: "desk-1"}); err != nil {
		t.Fatalf("cleanup 1: %v", err)
	}

	status, err = b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.ActiveRunners != 1 || backend.FreeSlots(status) != 1 {
		t.Fatalf("expected 1 active 1 free, got %+v free=%d", status, backend.FreeSlots(status))
	}

	// Cleanup second runner: active returns to 0, free returns to 2.
	if err := b.Cleanup(context.Background(), model.AllocationStatus{ID: "desk-2"}); err != nil {
		t.Fatalf("cleanup 2: %v", err)
	}

	status, err = b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.ActiveRunners != 0 || backend.FreeSlots(status) != 2 {
		t.Fatalf("expected 0 active 2 free, got %+v free=%d", status, backend.FreeSlots(status))
	}
}

func TestCapacityOnlineWithActiveCountFunc(t *testing.T) {
	cfg := desktopConfig(3, "desktop.local", 22)
	b := New(cfg).
		WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
			return stubConn{}, nil
		}).
		WithActiveCountFunc(func() int {
			return 2
		})

	status, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 3 || status.ActiveRunners != 2 {
		t.Fatalf("expected max=3 active=2, got %+v", status)
	}
	if free := backend.FreeSlots(status); free != 1 {
		t.Fatalf("expected 1 free slot, got %d", free)
	}
}

func TestCapacityImplementsCapacityBackend(t *testing.T) {
	var _ backend.CapacityBackend = New(desktopConfig(1, "", 0))
}

func TestCleanupImplementsCleanupBackend(t *testing.T) {
	var _ backend.CleanupBackend = New(desktopConfig(1, "", 0))
}

func TestDesktopImplementsActiveCountReceiver(t *testing.T) {
	var _ backend.ActiveCountReceiver = New(desktopConfig(1, "", 0))
}

func TestCapacityOnline_RestartWithActiveStoreAllocations(t *testing.T) {
	cfg := desktopConfig(3, "desktop.local", 22)
	memStore := store.NewMemory()

	// Existing store already has an active desktop allocation (e.g. from before broker restart).
	now := time.Now().UTC()
	err := memStore.Save(model.AllocationStatus{
		ID:              "desk-existing-1",
		Pool:            model.PoolLite,
		SelectedBackend: model.BackendDesktop,
		State:           model.StateReady,
		ExpiresAt:       now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("save existing allocation: %v", err)
	}

	// New desktop backend instance (simulating broker restart with clean in-memory state).
	b := New(cfg).
		WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
			return stubConn{}, nil
		}).
		WithActiveCounter(memStore)

	// In-memory map is empty on restart.
	b.mu.Lock()
	inMemCount := len(b.active)
	b.mu.Unlock()
	if inMemCount != 0 {
		t.Fatalf("expected empty in-memory map on new backend instance, got %d", inMemCount)
	}

	// Authoritative store count must report ActiveRunners=1, FreeSlots=2.
	status, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.ActiveRunners != 1 {
		t.Fatalf("expected 1 active runner on restart, got %+v", status)
	}
	if free := backend.FreeSlots(status); free != 2 {
		t.Fatalf("expected 2 free slots on restart, got %d", free)
	}

	// When allocation finishes in store, capacity updates accordingly.
	memStore.MarkState("desk-existing-1", model.StateCompleted, time.Now().UTC(), "job finished")
	status, err = b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity after completion: %v", err)
	}
	if status.ActiveRunners != 0 {
		t.Fatalf("expected 0 active runners after completion, got %+v", status)
	}
	if free := backend.FreeSlots(status); free != 3 {
		t.Fatalf("expected 3 free slots after completion, got %d", free)
	}
}

func TestCapacityOnline_SumsConfiguredPools(t *testing.T) {
	cfg := desktopConfig(5, "desktop.local", 22)
	// Add a second pool with desktop configured
	cfg.Pools = append(cfg.Pools, model.PoolConfig{
		Name: model.PoolFull,
		Backends: map[model.BackendName]model.BackendConfig{
			model.BackendDesktop: {
				Enabled:    true,
				MaxRunners: 5,
			},
		},
	})
	// Add a third pool WITHOUT desktop configured
	cfg.Pools = append(cfg.Pools, model.PoolConfig{
		Name: "other-pool",
		Backends: map[model.BackendName]model.BackendConfig{
			model.BackendARC: {
				Enabled:    true,
				MaxRunners: 5,
			},
		},
	})

	memStore := store.NewMemory()
	now := time.Now().UTC()
	// Active in PoolLite
	_ = memStore.Save(model.AllocationStatus{
		ID:              "desk-lite",
		Pool:            model.PoolLite,
		SelectedBackend: model.BackendDesktop,
		State:           model.StateReady,
		ExpiresAt:       now.Add(10 * time.Minute),
	})
	// Active in PoolFull
	_ = memStore.Save(model.AllocationStatus{
		ID:              "desk-full",
		Pool:            model.PoolFull,
		SelectedBackend: model.BackendDesktop,
		State:           model.StateReserved,
		ExpiresAt:       now.Add(10 * time.Minute),
	})
	// Active in other pool with different backend (should NOT be counted)
	_ = memStore.Save(model.AllocationStatus{
		ID:              "arc-alloc",
		Pool:            "other-pool",
		SelectedBackend: model.BackendARC,
		State:           model.StateReady,
		ExpiresAt:       now.Add(10 * time.Minute),
	})

	b := New(cfg).
		WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
			return stubConn{}, nil
		}).
		WithActiveCounter(memStore)

	status, err := b.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.ActiveRunners != 2 {
		t.Fatalf("expected 2 active runners across configured pools, got %d", status.ActiveRunners)
	}

	// Also verify ActiveCountFunc directly
	fn := ActiveCountFunc(cfg, memStore)
	if count := fn(); count != 2 {
		t.Fatalf("expected ActiveCountFunc to return 2, got %d", count)
	}
}

type deadlockTestingCounter struct {
	b *Backend
}

func (c *deadlockTestingCounter) CountActive(pool model.PoolName, backendName model.BackendName) int {
	// If b.mu was held while calling CountActive, attempting to acquire b.mu here would deadlock.
	c.b.mu.Lock()
	defer c.b.mu.Unlock()
	return 1
}

func TestCapacity_NoLockOrderingDeadlock(t *testing.T) {
	cfg := desktopConfig(2, "desktop.local", 22)
	b := New(cfg).WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
		return stubConn{}, nil
	})
	counter := &deadlockTestingCounter{b: b}
	b.WithActiveCounter(counter)

	done := make(chan struct{})
	go func() {
		status, err := b.Capacity(context.Background())
		if err != nil {
			t.Errorf("capacity error: %v", err)
		}
		if status.ActiveRunners != 1 {
			t.Errorf("expected 1 active runner, got %d", status.ActiveRunners)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deadlock detected: Capacity held b.mu while calling ActiveCounter")
	}
}

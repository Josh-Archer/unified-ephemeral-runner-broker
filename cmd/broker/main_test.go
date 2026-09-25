package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	desktopbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/desktop"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
)

type stubConn struct {
	net.Conn
}

func (stubConn) Close() error { return nil }

func TestWireDesktopActiveCounter(t *testing.T) {
	cfg := model.BrokerConfig{
		Pools: []model.PoolConfig{
			{
				Name: model.PoolLite,
				Backends: map[model.BackendName]model.BackendConfig{
					model.BackendDesktop: {
						Enabled:    true,
						Healthy:    true,
						MaxRunners: 2,
					},
				},
			},
		},
	}

	desktop := desktopbackend.New(cfg).WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
		return stubConn{}, nil
	})
	memStore := store.NewMemory()

	wireDesktopActiveCounter(desktop, memStore)

	capStatus, err := desktop.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity before allocation: %v", err)
	}
	if capStatus.ActiveRunners != 0 || backend.FreeSlots(capStatus) != 2 {
		t.Fatalf("expected 0 active 2 free initially, got %+v", capStatus)
	}

	err = memStore.Save(model.AllocationStatus{
		ID:              "desk-main-1",
		Pool:            model.PoolLite,
		SelectedBackend: model.BackendDesktop,
		State:           model.StateReady,
		ExpiresAt:       time.Now().Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("save allocation: %v", err)
	}

	capStatus, err = desktop.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity with active allocation: %v", err)
	}
	if capStatus.ActiveRunners != 1 || backend.FreeSlots(capStatus) != 1 {
		t.Fatalf("expected 1 active 1 free after store allocation, got %+v", capStatus)
	}
}

func TestBuildRegistryIncludesDesktopBackend(t *testing.T) {
	cfg := model.BrokerConfig{
		Pools: []model.PoolConfig{
			{
				Name: model.PoolLite,
				Backends: map[model.BackendName]model.BackendConfig{
					model.BackendDesktop: {Enabled: true, MaxRunners: 1},
				},
			},
		},
	}
	reg, desktop := buildRegistry(cfg, nil)
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
	if desktop == nil {
		t.Fatal("expected non-nil desktop backend")
	}
	got, ok := reg.Get(model.BackendDesktop)
	if !ok || got != desktop {
		t.Fatalf("registry.Get(model.BackendDesktop) = %v, want %v", got, desktop)
	}
}

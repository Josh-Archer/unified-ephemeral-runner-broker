package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/capacity"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func capsService(t *testing.T, arc model.BackendConfig) *Service {
	t.Helper()
	cfg := config.Default()
	cfg.Broker.StateStore = model.StateStoreConfig{Type: "memory"}
	cfg.Pools[0].Backends = map[model.BackendName]model.BackendConfig{model.BackendARC: arc}
	return NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendARC}), nil)
}

func TestNoCapacityCapsNamesPoolMaxRunners(t *testing.T) {
	svc := capsService(t, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, CrossPoolMaxRunners: 5, Template: "arc-full"})
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if got := svc.noCapacityCaps(model.PoolFull); got != "arc=maxRunners(1/1)" {
		t.Fatalf("caps = %q", got)
	}
}

func TestNoCapacityCapsNamesCrossPoolCap(t *testing.T) {
	svc := capsService(t, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 4, CrossPoolMaxRunners: 1, Template: "arc-full"})
	// Another pool's runner on the same backend fills the shared cap.
	if err := svc.store.Save(model.AllocationStatus{ID: "other", Pool: "other-pool", SelectedBackend: model.BackendARC, State: model.StateReady, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := svc.noCapacityCaps(model.PoolFull); got != "arc=crossPoolMaxRunners(1/1)" {
		t.Fatalf("caps = %q", got)
	}
}

func TestNoCapacityCapsReportsDisabledAndFree(t *testing.T) {
	svc := capsService(t, model.BackendConfig{Enabled: false, MaxRunners: 2, Template: "arc-full"})
	if got := svc.noCapacityCaps(model.PoolFull); got != "arc=disabled" {
		t.Fatalf("caps = %q", got)
	}
	svc = capsService(t, model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 2, Template: "arc-full"})
	if got := svc.noCapacityCaps(model.PoolFull); !strings.HasPrefix(got, "arc=free(0/2)") {
		t.Fatalf("caps = %q", got)
	}
}

func TestNotReadyNodeBackendIsNotFreeCapacity(t *testing.T) {
	const desktop model.BackendName = "kubernetes-desktop"
	cfg := config.Default()
	cfg.Broker.StateStore = model.StateStoreConfig{Type: "memory"}
	cfg.Broker.LiveCapacity = model.LiveCapacityConfig{Enabled: true, FailureMode: "pass-through"}
	cfg.Pools[0].Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 2, Template: "arc-full"},
		desktop:          {Enabled: true, Healthy: true, MaxRunners: 5},
	}
	svc := NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendARC}, testBackend{name: desktop}), nil)
	svc.CapacityManager().Set(capacity.Snapshot{
		Backend:   desktop,
		Status:    backend.CapacityStatus{MaxRunners: 5, ActiveRunners: 5, NodeUnavailable: "guarded node is not Ready"},
		UpdatedAt: time.Now(),
		Stale:     true, // stale pass-through must not reopen a known-down node
	})
	pool := cfg.Pools[0]
	if svc.backendHasFreeSchedulerCapacity(pool, desktop, pool.Backends[desktop]) {
		t.Fatal("NotReady node backend reported free scheduler capacity")
	}
	if got := svc.noCapacityCaps(pool.Name); !strings.Contains(got, "kubernetes-desktop=node-unavailable") {
		t.Fatalf("caps = %q, want node-unavailable instead of free(n/5)", got)
	}
	for i := 0; i < 2; i++ {
		status, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: pool.Name, JobTimeout: time.Minute})
		if err != nil {
			t.Fatalf("allocation %d: %v", i, err)
		}
		if status.SelectedBackend != model.BackendARC {
			t.Fatalf("allocation %d went to %s, want fallback to arc", i, status.SelectedBackend)
		}
	}
}

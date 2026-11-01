package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
)

type podInventoryBackend struct {
	testBackend
	mu   *sync.Mutex
	live *[]backend.LiveRunner
	err  error
}

func (b podInventoryBackend) Provision(ctx context.Context, request model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	runner, err := b.testBackend.Provision(ctx, request, allocation)
	runner.Metadata["pod_name"] = "uecb-runner-" + allocation.ID
	return runner, err
}

func (b podInventoryBackend) LiveRunners(context.Context) ([]backend.LiveRunner, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]backend.LiveRunner(nil), (*b.live)...), b.err
}

// Regression: allocations whose pod is gone (deleted,
// Terminating, or on a NotReady node, all absent from LiveRunners) must stop
// holding maxRunners slots.
func TestReconcileOrphanedAllocationsReleasesAllocationsWithoutPods(t *testing.T) {
	cfg := inventoryConfig(t)
	var live []backend.LiveRunner
	impl := podInventoryBackend{testBackend: testBackend{name: model.BackendARC}, mu: &sync.Mutex{}, live: &live}
	svc := NewService(cfg, backend.NewRegistry(impl), nil)

	kept, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour}); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("both slots should be held, got %v", err)
	}
	impl.mu.Lock()
	live = []backend.LiveRunner{{AllocationID: kept.ID, Pool: model.PoolFull}}
	impl.mu.Unlock()

	// Inside the grace window nothing is reaped.
	if got := svc.ReconcileOrphanedAllocations(context.Background(), time.Now()); got != 0 {
		t.Fatalf("released within grace = %d", got)
	}
	later := time.Now().Add(orphanedAllocationGrace + time.Minute)
	if got := svc.ReconcileOrphanedAllocations(context.Background(), later); got != 1 {
		t.Fatalf("released = %d, want 1", got)
	}
	if st, _ := svc.Get(stale.ID); isActiveAllocationState(st.State) {
		t.Fatalf("stale allocation still active: %s", st.State)
	}
	if st, _ := svc.Get(kept.ID); st.State != model.StateReady {
		t.Fatalf("live allocation state = %s", st.State)
	}
	if n := svc.store.CountActive(model.PoolFull, model.BackendARC); n != 1 {
		t.Fatalf("used slots = %d, want 1 (only the live pod)", n)
	}
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour}); err != nil {
		t.Fatalf("released slot should be free: %v", err)
	}
}

func TestReconcileOrphanedAllocationsIgnoresInventoryErrors(t *testing.T) {
	cfg := inventoryConfig(t)
	var live []backend.LiveRunner
	impl := podInventoryBackend{testBackend: testBackend{name: model.BackendARC}, mu: &sync.Mutex{}, live: &live, err: errors.New("api down")}
	svc := NewService(cfg, backend.NewRegistry(impl), nil)
	alloc, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.ReconcileOrphanedAllocations(context.Background(), time.Now().Add(time.Hour)); got != 0 {
		t.Fatalf("released = %d", got)
	}
	if st, _ := svc.Get(alloc.ID); st.State != model.StateReady {
		t.Fatalf("inventory error must not release: %s", st.State)
	}
}

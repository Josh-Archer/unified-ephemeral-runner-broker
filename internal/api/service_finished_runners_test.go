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

type finishedRunnerBackend struct {
	testBackend
	mu       *sync.Mutex
	finished []backend.FinishedRunner
	err      error
	cleaned  *[]string
}

func (b finishedRunnerBackend) FinishedRunners(context.Context) ([]backend.FinishedRunner, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]backend.FinishedRunner(nil), b.finished...), b.err
}

func (b finishedRunnerBackend) Cleanup(_ context.Context, status model.AllocationStatus) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	*b.cleaned = append(*b.cleaned, status.ID)
	return nil
}

func TestReconcileFinishedRunnersReleasesExitedRunners(t *testing.T) {
	cfg := inventoryConfig(t)
	var cleaned []string
	impl := &finishedRunnerBackend{testBackend: testBackend{name: model.BackendARC}, mu: &sync.Mutex{}, cleaned: &cleaned}
	svc := NewService(cfg, backend.NewRegistry(impl), nil)

	first, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour}); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("both slots should be held, got %v", err)
	}

	impl.mu.Lock()
	impl.finished = []backend.FinishedRunner{
		{AllocationID: first.ID, Pool: model.PoolFull},
		{AllocationID: second.ID, Pool: model.PoolFull, Failed: true},
		{AllocationID: "unknown-alloc", Pool: model.PoolFull, Metadata: map[string]string{"namespace": "arc-runners"}},
	}
	impl.mu.Unlock()

	if got := svc.ReconcileFinishedRunners(context.Background()); got != 2 {
		t.Fatalf("released = %d, want 2", got)
	}
	if st, _ := svc.Get(first.ID); st.State != model.StateCompleted {
		t.Fatalf("first state = %s", st.State)
	}
	if st, _ := svc.Get(second.ID); st.State != model.StateFailed {
		t.Fatalf("failed runner state = %s", st.State)
	}
	want := map[string]bool{first.ID: true, second.ID: true, "unknown-alloc": true}
	for _, id := range cleaned {
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("not cleaned up: %v (cleaned %v)", want, cleaned)
	}
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour}); err != nil {
		t.Fatalf("released slots should be free again: %v", err)
	}

	// A second pass over the same (already released) runners releases nothing.
	if got := svc.ReconcileFinishedRunners(context.Background()); got != 0 {
		t.Fatalf("second pass released %d", got)
	}
}

func TestReconcileFinishedRunnersIgnoresInventoryErrors(t *testing.T) {
	cfg := inventoryConfig(t)
	var cleaned []string
	impl := &finishedRunnerBackend{testBackend: testBackend{name: model.BackendARC}, mu: &sync.Mutex{}, cleaned: &cleaned, err: errors.New("api down")}
	svc := NewService(cfg, backend.NewRegistry(impl), nil)
	alloc, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got := svc.ReconcileFinishedRunners(context.Background()); got != 0 {
		t.Fatalf("released = %d", got)
	}
	if st, _ := svc.Get(alloc.ID); !isActiveAllocationState(st.State) {
		t.Fatalf("an inventory error must not release allocations: %s", st.State)
	}
}

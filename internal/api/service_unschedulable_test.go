package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
)

type recordingReaper struct {
	testCleanupBackend
	pods  []backend.UnschedulablePod
	calls int
}

func (b *recordingReaper) ReapUnschedulablePods(context.Context, time.Time, time.Duration) ([]backend.UnschedulablePod, error) {
	b.calls++
	return append([]backend.UnschedulablePod(nil), b.pods...), nil
}

func TestReconcileUnschedulablePodsReleasesSlot(t *testing.T) {
	cfg := inventoryConfig(t)
	cfg.Pools[0].Backends[model.BackendARC] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Template: "arc-full"}
	impl := &recordingReaper{testCleanupBackend: testCleanupBackend{testBackend: testBackend{name: model.BackendARC}}}
	svc := NewService(cfg, backend.NewRegistry(impl), nil)

	alloc, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour}); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("cap should be full, got %v", err)
	}
	impl.pods = []backend.UnschedulablePod{{AllocationID: alloc.ID, Pool: alloc.Pool}}
	if got := svc.ReconcileUnschedulablePods(context.Background(), time.Now()); got != 1 {
		t.Fatalf("reaped = %d", got)
	}
	if st, _ := svc.Get(alloc.ID); isActiveAllocationState(st.State) {
		t.Fatalf("allocation still active: %s", st.State)
	}
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour}); err != nil {
		t.Fatalf("released slot should be free: %v", err)
	}
}

func TestReconcileUnschedulablePodsRequeuesWorkflowJob(t *testing.T) {
	cfg := inventoryConfig(t)
	cfg.Pools[0].Backends[model.BackendARC] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Template: "arc-full"}
	impl := &recordingReaper{testCleanupBackend: testCleanupBackend{testBackend: testBackend{name: model.BackendARC}}}
	svc := NewService(cfg, backend.NewRegistry(impl), nil)

	alloc, err := svc.Allocate(context.Background(), model.AllocationRequest{
		Pool:             model.PoolFull,
		JobTimeout:       time.Hour,
		WorkflowJobID:    99,
		TargetRepository: "example/project",
	})
	if err != nil {
		t.Fatal(err)
	}
	impl.pods = []backend.UnschedulablePod{{
		AllocationID:  alloc.ID,
		Pool:          alloc.Pool,
		WorkflowJobID: 99,
		Repository:    "example/project",
	}}
	if got := svc.ReconcileUnschedulablePods(context.Background(), time.Now()); got != 1 {
		t.Fatalf("reaped = %d", got)
	}
	retried, ok := svc.findByWorkflowJobID(99)
	if !ok || retried.ID == alloc.ID || isTerminalAllocationState(retried.State) {
		t.Fatalf("expected a new allocation for job 99, got %+v found=%v", retried, ok)
	}
}

func TestReconcileUnschedulablePodsDisabled(t *testing.T) {
	cfg := inventoryConfig(t)
	cfg.Broker.UnschedulableTimeout = -1
	impl := &recordingReaper{testCleanupBackend: testCleanupBackend{testBackend: testBackend{name: model.BackendARC}}}
	svc := NewService(cfg, backend.NewRegistry(impl), nil)
	if got := svc.ReconcileUnschedulablePods(context.Background(), time.Now()); got != 0 || impl.calls != 0 {
		t.Fatalf("disabled reaper ran: reaped=%d calls=%d", got, impl.calls)
	}
}

func TestCapacityGroupOverflowsToUngroupedBackend(t *testing.T) {
	const small model.BackendName = "arc-small-nodes"
	const desktop model.BackendName = "kubernetes-desktop"
	cfg := config.Default()
	cfg.Broker.StateStore = model.StateStoreConfig{Type: "memory"}
	cfg.Broker.ARCFirst.SaturationWait = 0
	cfg.Pools[0].Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 1, CrossPoolMaxRunners: 1, CapacityGroup: "homelab2", CapacityGroupMax: 1, Template: "arc-full"},
		small:            {Enabled: true, Healthy: true, MaxRunners: 1, CrossPoolMaxRunners: 1, CapacityGroup: "homelab2", CapacityGroupMax: 1},
		desktop:          {Enabled: true, Healthy: true, MaxRunners: 10, CrossPoolMaxRunners: 10},
	}
	svc := NewService(cfg, backend.NewRegistry(
		testBackend{name: model.BackendARC},
		testBackend{name: small},
		testBackend{name: desktop},
	), nil)

	arc := model.BackendARC
	first, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour, Backend: &arc})
	if err != nil {
		t.Fatal(err)
	}
	if first.SelectedBackend != model.BackendARC {
		t.Fatalf("first backend = %s", first.SelectedBackend)
	}
	second, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Hour})
	if err != nil {
		t.Fatalf("expected overflow to desktop: %v", err)
	}
	if second.SelectedBackend != desktop {
		t.Fatalf("second backend = %s, want %s", second.SelectedBackend, desktop)
	}
	if got := svc.noCapacityCaps(model.PoolFull); !strings.Contains(got, "arc-small-nodes=capacityGroup(1/1)") {
		t.Fatalf("caps = %q", got)
	}
}

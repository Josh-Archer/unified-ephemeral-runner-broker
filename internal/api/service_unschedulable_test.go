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
		model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 1, CrossPoolMaxRunners: 1, CapacityGroup: "shared-node", CapacityGroupMax: 1, Template: "arc-full"},
		small:            {Enabled: true, Healthy: true, MaxRunners: 1, CrossPoolMaxRunners: 1, CapacityGroup: "shared-node", CapacityGroupMax: 1},
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

func TestUnschedulableWorkflowRetryPrefersEligibleLocalBackendAndKeepsSize(t *testing.T) {
	const desktop model.BackendName = "kubernetes-shared-desktop"
	cfg := inventoryConfig(t)
	pool := &cfg.Pools[0]
	pool.WebhookLabels = []string{"uecb-shared"}
	pool.Backends[desktop] = model.BackendConfig{Driver: "kubernetes", Enabled: true, Healthy: true, MaxRunners: 2}
	cfg.Broker.SizeClasses = map[string]model.SizeClassConfig{
		"large": {Backends: []model.BackendName{model.BackendARC, desktop}},
	}
	arc := &recordingReaper{testCleanupBackend: testCleanupBackend{testBackend: testBackend{name: model.BackendARC}}}
	ctx := context.Background()
	registry := backend.NewRegistry(arc, testBackend{name: desktop})
	svc := NewService(cfg, registry, nil)

	first, created, err := svc.AllocateForWorkflowJob(ctx, *pool, 99, "example/project", []string{"uecb-shared", "size-large"})
	if err != nil || !created || first.SelectedBackend != model.BackendARC {
		t.Fatalf("first allocation = %s created=%v err=%v", first.SelectedBackend, created, err)
	}
	arc.pods = []backend.UnschedulablePod{{AllocationID: first.ID, Pool: first.Pool, WorkflowJobID: 99, Repository: "example/project"}}
	if got := svc.ReconcileUnschedulablePods(ctx, time.Now()); got != 1 {
		t.Fatalf("reaped = %d, want 1", got)
	}
	retry, ok := svc.findByWorkflowJobID(99)
	label, sized := canonicalSizeLabel(retry.RequestedLabels)
	if !ok || retry.ID == first.ID || retry.SelectedBackend != desktop || !sized || label != "size-large" {
		t.Fatalf("retry did not preserve job size or select desktop: %+v", retry)
	}
	other, created, err := svc.AllocateForWorkflowJob(ctx, *pool, 100, "example/project", []string{"uecb-shared", "size-large"})
	if err != nil || !created || other.SelectedBackend != model.BackendARC {
		t.Fatalf("unrelated job should retain normal route: %s created=%v err=%v", other.SelectedBackend, created, err)
	}
}

// A cloud backend must not become the preferred alternative. A local
// alternative outside the requested size class must also remain ineligible.
func TestUnschedulableWorkflowRetryDoesNotPreferCloudOrChangeSizeEligibility(t *testing.T) {
	const desktop model.BackendName = "kubernetes-shared-desktop"
	cfg := inventoryConfig(t)
	pool := &cfg.Pools[0]
	pool.WebhookLabels = []string{"uecb-shared"}
	pool.Backends[desktop] = model.BackendConfig{Driver: "kubernetes", Enabled: true, Healthy: true, MaxRunners: 2}
	pool.Backends[model.BackendGCE] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 2}
	cfg.Broker.SizeClasses = map[string]model.SizeClassConfig{
		"large": {Backends: []model.BackendName{model.BackendARC, model.BackendGCE}},
	}
	arc := &recordingReaper{testCleanupBackend: testCleanupBackend{testBackend: testBackend{name: model.BackendARC}}}
	svc := NewService(cfg, backend.NewRegistry(arc, testBackend{name: desktop}, testBackend{name: model.BackendGCE}), nil)
	first, created, err := svc.AllocateForWorkflowJob(context.Background(), *pool, 99, "example/project", []string{"size-large"})
	if err != nil || !created || first.SelectedBackend != model.BackendARC {
		t.Fatalf("first allocation = %s created=%v err=%v", first.SelectedBackend, created, err)
	}
	arc.pods = []backend.UnschedulablePod{{AllocationID: first.ID, Pool: first.Pool}}
	svc.ReconcileUnschedulablePods(context.Background(), time.Now())
	retry, ok := svc.findByWorkflowJobID(99)
	if !ok || retry.ID == first.ID || retry.SelectedBackend != model.BackendARC {
		t.Fatalf("retry changed the usual route with no eligible local alternative: %+v", retry)
	}
}

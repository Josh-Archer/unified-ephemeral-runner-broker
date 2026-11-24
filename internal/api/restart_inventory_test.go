package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
)

type inventoryBackend struct {
	testBackend
	runners []backend.LiveRunner
	err     error
}

type warmCleanupInventoryBackend struct {
	inventoryBackend
	cleaned []string
}

func (b *warmCleanupInventoryBackend) Cleanup(_ context.Context, status model.AllocationStatus) error {
	b.cleaned = append(b.cleaned, status.ID)
	return nil
}

func (b inventoryBackend) LiveRunners(context.Context) ([]backend.LiveRunner, error) {
	return b.runners, b.err
}

func inventoryConfig(t *testing.T) model.BrokerConfig {
	t.Helper()
	cfg := config.Default()
	cfg.Broker.StateStore = model.StateStoreConfig{Type: "file", Path: t.TempDir() + "/allocations.json"}
	cfg.Pools[0].Backends[model.BackendARC] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 2, Template: "arc-full"}
	return cfg
}

func TestRestartCleansUpRejectedWarmRunner(t *testing.T) {
	cfg := inventoryConfig(t)
	cfg.Pools[0].Backends[model.BackendARC] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Template: "arc-full"}
	initial := NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendARC}), nil)
	now := time.Now()
	impl := &warmCleanupInventoryBackend{inventoryBackend: inventoryBackend{
		testBackend: testBackend{name: model.BackendARC},
	}}
	for _, tc := range []struct {
		id            string
		state         model.AllocationState
		reason        string
		workflowJobID int64
	}{
		{id: "newly-rejected", state: model.StateWarm},
		{id: "rejected-on-old-version", state: model.StateFailed, reason: "rehydrate skipped: no healthy backend with free capacity"},
		{id: "unrelated-failure", state: model.StateFailed, reason: "provisioning failed"},
		{id: "warm-with-work", state: model.StateFailed, reason: "rehydrate skipped: no healthy backend with free capacity", workflowJobID: 42},
		{id: "ready-at-restart", state: model.StateReady},
	} {
		if err := initial.store.Save(model.AllocationStatus{
			ID: tc.id, Pool: model.PoolFull, SelectedBackend: model.BackendARC,
			ExpiresAt: now.Add(time.Hour), State: tc.state, Error: tc.reason,
		}); err != nil {
			t.Fatal(err)
		}
		impl.runners = append(impl.runners, backend.LiveRunner{
			AllocationID: tc.id, Pool: model.PoolFull, WorkflowJobID: tc.workflowJobID,
			Metadata: map[string]string{backend.MetadataLaunchModeKey: launchModeWarm},
		})
	}

	restarted := NewService(cfg, backend.NewRegistry(impl), nil)
	if status, _ := restarted.Get("ready-at-restart"); status.State != model.StateReady {
		t.Fatalf("ready allocation changed state to %s after reducing capacity", status.State)
	}
	if len(impl.cleaned) != 2 || impl.cleaned[0] != "newly-rejected" || impl.cleaned[1] != "rejected-on-old-version" {
		t.Fatalf("cleaned %v, want only the two rejected idle warm runners", impl.cleaned)
	}

	// A pod still in inventory is retried after startup even if cleanup failed.
	impl.runners = impl.runners[:1]
	restarted.ReconcileOrphanedAllocations(context.Background(), now)
	if len(impl.cleaned) != 3 || impl.cleaned[2] != "newly-rejected" {
		t.Fatalf("cleaned %v, want retry for the still-visible rejected warm runner", impl.cleaned)
	}
}

func TestRestartAccountsForSharedProviderCapacityOnce(t *testing.T) {
	for _, knownRunner := range []bool{true, false} {
		name := "untracked pod"
		if knownRunner {
			name = "pod accounted for by sibling"
		}
		t.Run(name, func(t *testing.T) {
			cfg := inventoryConfig(t)
			names := []model.BackendName{model.BackendARC, "arc-small-nodes"}
			for _, name := range names {
				cfg.Pools[0].Backends[name] = model.BackendConfig{
					Enabled: true, Healthy: true, MaxRunners: 1, CrossPoolMaxRunners: 1,
					CapacityGroup: "shared-node", CapacityGroupMax: 1,
				}
			}
			initial := NewService(cfg, backend.NewRegistry(), nil)
			now := time.Now()
			for _, name := range names {
				if err := initial.store.Save(model.AllocationStatus{
					ID: "restart-orphan-" + string(name) + "-0", Pool: model.PoolFull,
					SelectedBackend: name, State: model.StateReserved, ExpiresAt: now.Add(time.Hour),
					Metadata: map[string]string{restartOrphanHoldMetadataKey: restartOrphanHoldMetadataVal},
				}); err != nil {
					t.Fatal(err)
				}
			}
			if knownRunner {
				if err := initial.store.Save(model.AllocationStatus{
					ID: "shared-job", Pool: model.PoolFull, SelectedBackend: names[1],
					State: model.StateReady, ExpiresAt: now.Add(time.Hour), RegisteredAt: &now,
				}); err != nil {
					t.Fatal(err)
				}
			}
			current := backend.CapacityStatus{MaxRunners: 1, ActiveRunners: 1, CountsCapacityGroup: true}
			registry := backend.NewRegistry(
				&capacityTestBackend{testBackend: testBackend{name: names[0]}, status: current},
				&capacityTestBackend{testBackend: testBackend{name: names[1]}, status: current},
			)
			restarted := NewService(cfg, registry, nil)
			if got := restarted.capacityGroupOccupancy(names); got != 1 {
				t.Fatalf("accounted for %d slots, want one for the one provider pod", got)
			}
			if knownRunner {
				if got, _ := restarted.Get("shared-job"); got.State != model.StateReady {
					t.Fatalf("real allocation changed state to %s", got.State)
				}
			}
		})
	}
}

func TestRestartRebuildsLostAllocationsFromLiveRunners(t *testing.T) {
	cfg := inventoryConfig(t)
	impl := inventoryBackend{
		testBackend: testBackend{name: model.BackendARC},
		runners: []backend.LiveRunner{{
			AllocationID:  "alloc-lost-1",
			Pool:          model.PoolFull,
			Repository:    "example/project",
			WorkflowJobID: 4242,
			Metadata:      map[string]string{"namespace": "arc-runners", "pod_name": "uecb-runner-alloc-lost-1", "github_runner_id": "77"},
		}},
	}
	svc := NewService(cfg, backend.NewRegistry(impl), nil)

	got, ok := svc.Get("alloc-lost-1")
	if !ok {
		t.Fatal("expected allocation rebuilt from live runner")
	}
	if got.State != model.StateReady || got.SelectedBackend != model.BackendARC || got.Metadata["github_runner_id"] != "77" {
		t.Fatalf("unexpected rebuilt allocation: %+v", got)
	}
	if found, ok := svc.findByWorkflowJobID(4242); !ok || found.ID != "alloc-lost-1" {
		t.Fatalf("rebuilt allocation must resolve by workflow job id, got %+v %v", found, ok)
	}
	for _, status := range svc.store.List() {
		if status.Metadata[restartOrphanHoldMetadataKey] == restartOrphanHoldMetadataVal {
			t.Fatalf("no anonymous hold expected for an identified runner: %+v", status)
		}
	}
	// One of two slots is held by the rebuilt runner.
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Minute}); err != nil {
		t.Fatalf("second slot should be free: %v", err)
	}
	if _, err := svc.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Minute}); !errors.Is(err, scheduler.ErrNoCapacity) {
		t.Fatalf("expected rebuilt runner to consume capacity, got %v", err)
	}
}

func TestRestartInventoryKeepsKnownAllocationsAndSkipsUnknownPools(t *testing.T) {
	cfg := inventoryConfig(t)
	first := NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendARC}), nil)
	known, err := first.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: time.Minute})
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	impl := inventoryBackend{
		testBackend: testBackend{name: model.BackendARC},
		runners: []backend.LiveRunner{
			{AllocationID: known.ID, Pool: model.PoolFull},
			{AllocationID: "alloc-other-pool", Pool: "no-such-pool"},
		},
	}
	restarted := NewService(cfg, backend.NewRegistry(impl), nil)
	got, ok := restarted.Get(known.ID)
	if !ok || got.Metadata[restoredFromProviderMetadataKey] != "" {
		t.Fatalf("known allocation must be kept as persisted, got %+v", got)
	}
	if _, ok := restarted.Get("alloc-other-pool"); ok {
		t.Fatal("runner from an unconfigured pool must not be rebuilt")
	}
}

func TestRestartInventoryErrorFallsBackToCapacityHolds(t *testing.T) {
	cfg := inventoryConfig(t)
	impl := inventoryBackend{testBackend: testBackend{name: model.BackendARC}, err: errors.New("api down")}
	svc := NewService(cfg, backend.NewRegistry(impl), nil)
	if n := len(svc.store.List()); n != 0 {
		t.Fatalf("expected no rebuilt allocations, got %d", n)
	}
}

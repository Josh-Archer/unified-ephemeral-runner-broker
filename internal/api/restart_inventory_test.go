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

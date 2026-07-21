package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
)

type haStubBackend struct {
	name model.BackendName
}

func (b haStubBackend) Name() model.BackendName { return b.name }

func (b haStubBackend) Provision(_ context.Context, _ model.AllocationRequest, allocation model.AllocationStatus) (backend.ProvisionedRunner, error) {
	return backend.ProvisionedRunner{
		RunnerLabel: backend.DefaultRunnerLabel(b.name, allocation.ID),
		Metadata:    map[string]string{"provisioner": "ha-test"},
	}, nil
}

func TestHASharedStoreConcurrentCapacity(t *testing.T) {
	shared := store.NewMemory()
	cfg := model.BrokerConfig{
		Broker: model.BrokerRuntimeConfig{
			DefaultPool:       model.PoolLite,
			DefaultJobTimeout: time.Minute,
			AllowUnauthenticated: true,
			StateStore:        model.StateStoreConfig{Type: "memory"},
		},
		Pools: []model.PoolConfig{{
			Name:      model.PoolLite,
			Scheduler: "round-robin",
			Backends: map[model.BackendName]model.BackendConfig{
				model.BackendARC: {
					Enabled:    true,
					Healthy:    true,
					MaxRunners: 3,
				},
			},
		}},
	}

	registry := backend.NewRegistry(haStubBackend{name: model.BackendARC})
	svcA := NewServiceWithStore(cfg, registry, nil, shared)
	svcB := NewServiceWithStore(cfg, registry, nil, shared)

	const workers = 20
	var success atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			svc := svcA
			if i%2 == 1 {
				svc = svcB
			}
			_, err := svc.Allocate(context.Background(), model.AllocationRequest{
				Pool: model.PoolLite,
			})
			if err == nil {
				success.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if success.Load() != 3 {
		t.Fatalf("expected exactly 3 successful allocations across replicas, got %d", success.Load())
	}
	if got := shared.CountActive(model.PoolLite, model.BackendARC); got != 3 {
		t.Fatalf("shared store active=%d want 3", got)
	}
}

func TestHASharedStoreGetAcrossReplicas(t *testing.T) {
	shared := store.NewMemory()
	cfg := model.BrokerConfig{
		Broker: model.BrokerRuntimeConfig{
			DefaultPool:          model.PoolLite,
			DefaultJobTimeout:    time.Minute,
			AllowUnauthenticated: true,
		},
		Pools: []model.PoolConfig{{
			Name: model.PoolLite,
			Backends: map[model.BackendName]model.BackendConfig{
				model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 2},
			},
		}},
	}
	registry := backend.NewRegistry(haStubBackend{name: model.BackendARC})
	svcA := NewServiceWithStore(cfg, registry, nil, shared)
	svcB := NewServiceWithStore(cfg, registry, nil, shared)

	allocated, err := svcA.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolLite})
	if err != nil {
		t.Fatalf("allocate via A: %v", err)
	}
	got, ok := svcB.Get(allocated.ID)
	if !ok {
		t.Fatal("replica B should see allocation created on A")
	}
	if got.ID != allocated.ID || got.State != model.StateReady {
		t.Fatalf("unexpected status from B: %#v", got)
	}

	completed, ok, err := svcB.Complete(context.Background(), allocated.ID, completionRequest{State: "completed"})
	if err != nil || !ok {
		t.Fatalf("complete via B: ok=%v err=%v", ok, err)
	}
	if completed.State != model.StateCompleted {
		t.Fatalf("expected completed, got %s", completed.State)
	}
	if _, ok := svcA.Get(allocated.ID); !ok {
		t.Fatal("replica A should still see completed allocation")
	}
}

func TestHAWarmClaimIsSingleWinner(t *testing.T) {
	shared := store.NewMemory()
	cfg := model.BrokerConfig{
		Broker: model.BrokerRuntimeConfig{
			DefaultPool:          model.PoolLite,
			DefaultJobTimeout:    time.Minute,
			AllowUnauthenticated: true,
		},
		Pools: []model.PoolConfig{{
			Name: model.PoolLite,
			Backends: map[model.BackendName]model.BackendConfig{
				model.BackendCodeBuild: {
					Enabled:    true,
					Healthy:    true,
					MaxRunners: 5,
					WarmMin:    1,
					WarmMax:    1,
					SecretRef:  "uecb-codebuild",
				},
			},
		}},
	}

	// Seed one warm allocation directly into the shared store.
	warmID := "warm-1"
	_ = shared.Save(model.AllocationStatus{
		ID:              warmID,
		Pool:            model.PoolLite,
		SelectedBackend: model.BackendCodeBuild,
		State:           model.StateWarm,
		RunnerLabel:     "warm-label",
		ExpiresAt:       time.Now().Add(10 * time.Minute),
		Metadata:        map[string]string{"launch_mode": "warm"},
	})

	// Two concurrent CAS claims — only one should win.
	var winners atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			if _, ok := shared.CompareAndMarkState(warmID, model.StateWarm, model.StateReady, time.Now(), ""); ok {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("expected exactly one warm claim winner, got %d", winners.Load())
	}

	// Keep cfg referenced for future extension (service-level warm consume).
	_ = cfg
}

func TestHALeaderElectionSingleBackgroundWorker(t *testing.T) {
	shared := store.NewMemory()
	ctx := context.Background()
	okA, err := shared.TryAcquireLeadership(ctx, store.LeaderLeaseName, "replica-a", time.Minute)
	if err != nil || !okA {
		t.Fatalf("A should lead: %v %v", okA, err)
	}
	okB, err := shared.TryAcquireLeadership(ctx, store.LeaderLeaseName, "replica-b", time.Minute)
	if err != nil {
		t.Fatalf("B election err: %v", err)
	}
	if okB {
		t.Fatal("B should not lead while A holds lease")
	}
}

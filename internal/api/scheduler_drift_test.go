package api

import (
	"context"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// driftService builds a service whose full pool has a single ARC backend, with
// or without priority-fair-share scheduling, so lifecycle paths can be checked
// for scheduler/store count divergence.
func driftService(t *testing.T, fairShare bool, maxRunners int) (*Service, model.PoolConfig, *time.Time) {
	t.Helper()
	cfg := config.Default()
	cfg.Broker.RegistrationTimeout = 5 * time.Minute
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolFull {
			continue
		}
		cfg.Pools[i].Backends = map[model.BackendName]model.BackendConfig{
			model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: maxRunners, Template: "arc-full"},
		}
		if fairShare {
			cfg.Pools[i].Scheduler = "priority-fair-share"
			cfg.Pools[i].FairShare.Enabled = true
		}
	}
	service := NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendARC}), nil)
	now := time.Unix(1000, 0)
	service.now = func() time.Time { return now }
	pool, err := service.resolvePool(model.PoolFull)
	if err != nil {
		t.Fatalf("resolve pool: %v", err)
	}
	return service, pool, &now
}

// assertNoDrift fails when the scheduler's in-memory active count differs from
// the number of active allocations in the store.
func assertNoDrift(t *testing.T, service *Service, pool model.PoolConfig, step string) {
	t.Helper()
	scheduled := service.backendActiveCount(pool, model.BackendARC)
	stored := service.store.CountActive(pool.Name, model.BackendARC)
	if scheduled != stored {
		t.Fatalf("%s: scheduler active=%d but store active=%d", step, scheduled, stored)
	}
}

func TestSchedulerDriftAcrossLifecyclePaths(t *testing.T) {
	for _, fairShare := range []bool{false, true} {
		name := "round-robin"
		if fairShare {
			name = "priority-fair-share"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			alloc := func(service *Service) model.AllocationStatus {
				t.Helper()
				a, err := service.Allocate(ctx, model.AllocationRequest{Pool: model.PoolFull, JobTimeout: 20 * time.Minute})
				if err != nil {
					t.Fatalf("allocate: %v", err)
				}
				return a
			}

			t.Run("complete", func(t *testing.T) {
				service, pool, _ := driftService(t, fairShare, 4)
				a := alloc(service)
				assertNoDrift(t, service, pool, "after allocate")
				if _, _, err := service.Complete(ctx, a.ID, completionRequest{State: "completed"}); err != nil {
					t.Fatalf("complete: %v", err)
				}
				assertNoDrift(t, service, pool, "after complete")
			})

			t.Run("failed", func(t *testing.T) {
				service, pool, _ := driftService(t, fairShare, 4)
				a := alloc(service)
				if _, _, err := service.Complete(ctx, a.ID, completionRequest{State: "failed", Error: "boom"}); err != nil {
					t.Fatalf("complete failed: %v", err)
				}
				assertNoDrift(t, service, pool, "after failed complete")
			})

			t.Run("cancel", func(t *testing.T) {
				service, pool, _ := driftService(t, fairShare, 4)
				a := alloc(service)
				if _, ok := service.Cancel(ctx, a.ID); !ok {
					t.Fatal("cancel not found")
				}
				assertNoDrift(t, service, pool, "after cancel")
			})

			t.Run("expire-unregistered", func(t *testing.T) {
				service, pool, now := driftService(t, fairShare, 4)
				alloc(service)
				alloc(service)
				assertNoDrift(t, service, pool, "after allocate")
				if swept := service.SweepExpired(now.Add(6 * time.Minute)); swept != 2 {
					t.Fatalf("swept %d, want 2", swept)
				}
				assertNoDrift(t, service, pool, "after unregistered expiry")
			})

			t.Run("expire-job-timeout", func(t *testing.T) {
				service, pool, now := driftService(t, fairShare, 4)
				a := alloc(service)
				service.now = func() time.Time { return now.Add(time.Minute) }
				if _, ok, err := service.Register(ctx, a.ID); err != nil || !ok {
					t.Fatalf("register: ok=%v err=%v", ok, err)
				}
				if swept := service.SweepExpired(now.Add(21 * time.Minute)); swept != 1 {
					t.Fatalf("swept %d, want 1", swept)
				}
				assertNoDrift(t, service, pool, "after job-timeout expiry")
			})

			t.Run("mixed-then-refill", func(t *testing.T) {
				service, pool, now := driftService(t, fairShare, 2)
				a := alloc(service)
				b := alloc(service)
				if _, _, err := service.Complete(ctx, a.ID, completionRequest{State: "completed"}); err != nil {
					t.Fatalf("complete: %v", err)
				}
				service.now = func() time.Time { return now.Add(time.Minute) }
				if _, ok, err := service.Register(ctx, b.ID); err != nil || !ok {
					t.Fatalf("register: ok=%v err=%v", ok, err)
				}
				service.SweepExpired(now.Add(21 * time.Minute))
				assertNoDrift(t, service, pool, "after mixed lifecycle")
				// The pool must be fully reusable once everything is terminal.
				alloc(service)
				alloc(service)
			})
		})
	}
}

// A penalized backend must not be dropped when the only other backends in the
// pool are disabled: that leaves nothing usable and every job gets no-capacity
// until the cooldown ends (shared pool: kubernetes-shared plus disabled cloud
// entries).
func TestRegistrationPenaltyKeepsOnlyUsableBackend(t *testing.T) {
	service, _, now := driftService(t, true, 4)
	pool := model.PoolConfig{
		Name: model.PoolFull,
		Backends: map[model.BackendName]model.BackendConfig{
			model.BackendARC:    {Enabled: true, Healthy: true, MaxRunners: 4},
			model.BackendLambda: {Enabled: false, Healthy: true, MaxRunners: 2},
			model.BackendGCE:    {Enabled: false, Healthy: true, MaxRunners: 1},
		},
	}
	for i := 0; i < registrationExpiryThreshold; i++ {
		service.recordRegistrationExpiry(pool.Name, model.BackendARC, *now)
	}
	if !service.registrationPenalized(pool.Name, model.BackendARC, *now) {
		t.Fatal("expected arc to be penalized")
	}
	got := service.filterRegistrationPenalized(pool, model.AllocationRequest{Pool: pool.Name})
	if _, ok := got.Backends[model.BackendARC]; !ok {
		t.Fatalf("penalized arc was dropped although only disabled backends remain: %v", got.Backends)
	}

	// With a genuinely usable alternative the penalized backend is still skipped.
	pool.Backends[model.BackendGCE] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}
	got = service.filterRegistrationPenalized(pool, model.AllocationRequest{Pool: pool.Name})
	if _, ok := got.Backends[model.BackendARC]; ok {
		t.Fatal("penalized arc should be skipped when an enabled alternative exists")
	}
}

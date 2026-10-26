package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// Regression for #153: a warm-pool handoff must never let a tenant exceed its
// fair-share quota, and must not leak the prewarm "default" reservation.
// Before the fix the handoff released the consumer's cold reservation and then
// re-reserved it; a concurrent request for the same tenant could take the slot
// in between, the re-reserve failed with ErrQuotaExceeded, and the warm runner
// was still handed over (quota bypass) while "default" was re-reserved and
// never released.
func TestQAWarmTransferNeverBypassesQuota(t *testing.T) {
	const iterations = 300
	const workers = 8
	for iter := 0; iter < iterations; iter++ {
		cfg := config.Default()
		for i := range cfg.Pools {
			pool := &cfg.Pools[i]
			if pool.Name != model.PoolLite {
				continue
			}
			pool.FairShare.Enabled = true
			pool.FairShare.Quotas = map[string]int{"tenant-a": 1}
			for name, b := range pool.Backends {
				b.Enabled = false
				pool.Backends[name] = b
			}
			cb := pool.Backends[model.BackendCodeBuild]
			cb.Enabled, cb.Healthy = true, true
			cb.MaxRunners = workers + 2
			cb.WarmMin, cb.WarmMax = 1, 1
			pool.Backends[model.BackendCodeBuild] = cb
		}
		service := NewService(cfg, backend.NewRegistry(
			testBackend{name: model.BackendARC},
			testBackend{name: model.BackendCodeBuild},
		), nil)
		service.ReconcileWarmPools()

		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			granted []model.AllocationStatus
		)
		start := make(chan struct{})
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				allocation, err := service.Allocate(context.Background(), model.AllocationRequest{
					Pool:       model.PoolLite,
					Tenant:     "tenant-a",
					JobTimeout: 5 * time.Minute,
				})
				if err != nil || allocation.State == model.StatePending {
					return
				}
				mu.Lock()
				granted = append(granted, allocation)
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()

		if len(granted) > 1 {
			t.Fatalf("iteration %d: tenant-a quota 1 but %d allocations granted", iter, len(granted))
		}
		for _, allocation := range granted {
			if _, ok := service.Cancel(context.Background(), allocation.ID); !ok {
				t.Fatalf("iteration %d: cancel %s failed", iter, allocation.ID)
			}
		}
		// Only the remaining (if any) warm runner may hold capacity: no leaked "default".
		warm := len(filterWarmAllocations(service.store.List(), model.PoolLite, model.BackendCodeBuild))
		if active := service.fairShare.Active(model.PoolLite, model.BackendCodeBuild); active != warm {
			t.Fatalf("iteration %d: fair-share active=%d after cancel, want %d (warm runners)", iter, active, warm)
		}
	}
}

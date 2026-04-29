package scheduler

import (
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func poolByName(name model.PoolName) model.PoolConfig {
	for _, pool := range config.Default().Pools {
		if pool.Name == name {
			return pool
		}
	}
	panic("pool not found")
}

func allocationRequest(pinned *model.BackendName, tenant, priority string) model.AllocationRequest {
	return model.AllocationRequest{
		Backend:       pinned,
		Tenant:        tenant,
		PriorityClass: priority,
	}
}

func TestRoundRobinSkipsUnhealthyBackend(t *testing.T) {
	scheduler := NewRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Backends[model.BackendCodeBuild] = model.BackendConfig{Enabled: true, Healthy: false, MaxRunners: 1}
	pool.Backends[model.BackendCloudRun] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}

	first, err := scheduler.Reserve(pool, allocationRequest(nil, "", ""))
	if err != nil {
		t.Fatalf("reserve #1 failed: %v", err)
	}
	second, err := scheduler.Reserve(pool, allocationRequest(nil, "", ""))
	if err != nil {
		t.Fatalf("reserve #2 failed: %v", err)
	}

	if first != model.BackendARC || second != model.BackendCloudRun {
		t.Fatalf("expected unhealthy backend to be skipped, got %s %s", first, second)
	}
}

func TestRoundRobinSkipsFullBackend(t *testing.T) {
	scheduler := NewRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Backends[model.BackendARC] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}
	pool.Backends[model.BackendCodeBuild] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}
	pool.Backends[model.BackendCloudRun] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}

	first, err := scheduler.Reserve(pool, allocationRequest(nil, "", ""))
	if err != nil {
		t.Fatalf("reserve #1 failed: %v", err)
	}
	second, err := scheduler.Reserve(pool, allocationRequest(nil, "", ""))
	if err != nil {
		t.Fatalf("reserve #2 failed: %v", err)
	}

	if first != model.BackendARC || second != model.BackendCodeBuild {
		t.Fatalf("expected full backend to be skipped, got %s %s", first, second)
	}
}

func TestRoundRobinPinnedBackendUsesCapacity(t *testing.T) {
	scheduler := NewRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Backends[model.BackendCodeBuild] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}

	pinned := model.BackendCodeBuild
	selected, err := scheduler.Reserve(pool, allocationRequest(&pinned, "", ""))
	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	if selected != model.BackendCodeBuild {
		t.Fatalf("expected codebuild, got %s", selected)
	}

	if _, err := scheduler.Reserve(pool, allocationRequest(&pinned, "", "")); err == nil {
		t.Fatal("expected capacity exhaustion for pinned backend")
	}
}

func TestRoundRobinReleaseReturnsCapacity(t *testing.T) {
	scheduler := NewRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Backends[model.BackendCodeBuild] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}

	pinned := model.BackendCodeBuild
	if _, err := scheduler.Reserve(pool, allocationRequest(&pinned, "", "")); err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	scheduler.Release(pool.Name, model.BackendCodeBuild, model.AllocationStatus{})
	if _, err := scheduler.Reserve(pool, allocationRequest(&pinned, "", "")); err != nil {
		t.Fatalf("expected capacity to be available after release: %v", err)
	}
}

func TestWeightedRoundRobinUsesWeights(t *testing.T) {
	scheduler := NewWeightedRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Scheduler = NameWeightedRoundRobin
	pool.Backends[model.BackendARC] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Weight: 1}
	pool.Backends[model.BackendCodeBuild] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Weight: 3}

	want := []model.BackendName{
		model.BackendARC,
		model.BackendCodeBuild,
		model.BackendCodeBuild,
		model.BackendCodeBuild,
	}

	for index, expected := range want {
		selected, err := scheduler.Reserve(pool, allocationRequest(nil, "", ""))
		if err != nil {
			t.Fatalf("reserve #%d failed: %v", index+1, err)
		}
		if selected != expected {
			t.Fatalf("reserve #%d selected %s, want %s", index+1, selected, expected)
		}
		scheduler.Release(pool.Name, selected, model.AllocationStatus{})
	}
}

func TestWeightedRoundRobinSkipsUnhealthyBackendDeterministically(t *testing.T) {
	scheduler := NewWeightedRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Scheduler = NameWeightedRoundRobin
	pool.Backends[model.BackendARC] = model.BackendConfig{Enabled: false, Healthy: true, MaxRunners: 1, Weight: 1}
	pool.Backends[model.BackendCodeBuild] = model.BackendConfig{Enabled: true, Healthy: false, MaxRunners: 1, Weight: 3}
	pool.Backends[model.BackendCloudRun] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Weight: 2}

	selected, err := scheduler.Reserve(pool, allocationRequest(nil, "", ""))
	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	if selected != model.BackendCloudRun {
		t.Fatalf("expected deterministic fallback to cloud-run, got %s", selected)
	}
}

func TestWeightedRoundRobinSkipsFullBackendDeterministically(t *testing.T) {
	scheduler := NewWeightedRoundRobin()
	pool := poolByName(model.PoolLite)
	pool.Scheduler = NameWeightedRoundRobin
	pool.Backends[model.BackendARC] = model.BackendConfig{Enabled: false, Healthy: true, MaxRunners: 1, Weight: 1}
	pool.Backends[model.BackendCodeBuild] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Weight: 3}
	pool.Backends[model.BackendCloudRun] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1, Weight: 2}

	first, err := scheduler.Reserve(pool, allocationRequest(nil, "", ""))
	if err != nil {
		t.Fatalf("reserve #1 failed: %v", err)
	}
	if first != model.BackendCodeBuild {
		t.Fatalf("expected codebuild first, got %s", first)
	}

	second, err := scheduler.Reserve(pool, allocationRequest(nil, "", ""))
	if err != nil {
		t.Fatalf("reserve #2 failed: %v", err)
	}
	if second != model.BackendCloudRun {
		t.Fatalf("expected deterministic fallback to cloud-run, got %s", second)
	}
}

func TestPriorityFairShareAllocatesByTenantShareWithoutPreemption(t *testing.T) {
	scheduler := NewPriorityFairShare()
	pool := poolByName(model.PoolLite)
	pool.FairShare.Enabled = true
	pool.Backends[model.BackendARC] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 4}
	pool.Backends[model.BackendCodeBuild] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 4}
	pool.FairShare.PriorityClasses = map[string]int{
		string(model.PriorityClassNormal): 1,
		string(model.PriorityClassHigh):   2,
	}

	pinned := model.BackendARC
	for i := 0; i < 2; i++ {
		if _, err := scheduler.Reserve(pool, allocationRequest(&pinned, "tenant-a", string(model.PriorityClassNormal))); err != nil {
			t.Fatalf("setup reserve #%d failed: %v", i+1, err)
		}
	}

	selected, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-b", string(model.PriorityClassNormal)))
	if err != nil {
		t.Fatalf("tenant-b reserve failed: %v", err)
	}
	if selected != model.BackendCodeBuild {
		t.Fatalf("expected fair-share to avoid tenant-a-heavy backend, got %s", selected)
	}
}

func TestPriorityFairShareEnforcesTenantQuotas(t *testing.T) {
	scheduler := NewPriorityFairShare()
	pool := poolByName(model.PoolLite)
	pool.FairShare.Enabled = true
	pool.FairShare.Quotas = map[string]int{
		"limited-tenant": 1,
	}
	pool.Backends[model.BackendARC] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 4}
	pool.Backends[model.BackendCodeBuild] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 4}

	// First allocation for limited-tenant should succeed
	_, err := scheduler.Reserve(pool, allocationRequest(nil, "limited-tenant", ""))
	if err != nil {
		t.Fatalf("first reserve failed: %v", err)
	}

	// Second allocation for limited-tenant should be rejected
	_, err = scheduler.Reserve(pool, allocationRequest(nil, "limited-tenant", ""))
	if err != ErrQuotaExceeded {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	}

	// Allocation for another tenant should still succeed
	_, err = scheduler.Reserve(pool, allocationRequest(nil, "other-tenant", ""))
	if err != nil {
		t.Fatalf("other-tenant reserve failed: %v", err)
	}

	// Releasing the first allocation should allow a new one
	scheduler.Release(pool.Name, model.BackendARC, model.AllocationStatus{Tenant: "limited-tenant"})
	_, err = scheduler.Reserve(pool, allocationRequest(nil, "limited-tenant", ""))
	if err != nil {
		t.Fatalf("reserve after release failed: %v", err)
	}
}

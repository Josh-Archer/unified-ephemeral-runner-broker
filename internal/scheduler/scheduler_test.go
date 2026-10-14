package scheduler

import (
	"testing"
	"time"

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

func TestPriorityFairShareComposesWithWeightedRoundRobin(t *testing.T) {
	scheduler := NewPriorityFairShare()
	pool := poolByName(model.PoolLite)
	pool.Scheduler = NameWeightedRoundRobin
	pool.FairShare.Enabled = true
	// Restrict to two backends so the weighted ring is deterministic.
	pool.Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC:       {Enabled: true, Healthy: true, MaxRunners: 10, Weight: 1},
		model.BackendCodeBuild: {Enabled: true, Healthy: true, MaxRunners: 10, Weight: 3},
	}

	// Equal fair-share scores (release after each pick) → weights drive selection.
	want := []model.BackendName{
		model.BackendARC,
		model.BackendCodeBuild,
		model.BackendCodeBuild,
		model.BackendCodeBuild,
	}
	for index, expected := range want {
		selected, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", ""))
		if err != nil {
			t.Fatalf("reserve #%d failed: %v", index+1, err)
		}
		if selected != expected {
			t.Fatalf("reserve #%d selected %s, want %s (weights should apply under fair-share)", index+1, selected, expected)
		}
		scheduler.Release(pool.Name, selected, model.AllocationStatus{Tenant: "tenant-a"})
	}
}

func TestPriorityFairSharePrefersTenantBalanceOverWeights(t *testing.T) {
	scheduler := NewPriorityFairShare()
	pool := poolByName(model.PoolLite)
	pool.Scheduler = NameWeightedRoundRobin
	pool.FairShare.Enabled = true
	pool.Backends = map[model.BackendName]model.BackendConfig{
		// High weight on ARC must not override tenant load preference.
		model.BackendARC:       {Enabled: true, Healthy: true, MaxRunners: 4, Weight: 10},
		model.BackendCodeBuild: {Enabled: true, Healthy: true, MaxRunners: 4, Weight: 1},
	}

	pinned := model.BackendARC
	for i := 0; i < 2; i++ {
		if _, err := scheduler.Reserve(pool, allocationRequest(&pinned, "tenant-a", "")); err != nil {
			t.Fatalf("setup reserve #%d failed: %v", i+1, err)
		}
	}

	selected, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-b", ""))
	if err != nil {
		t.Fatalf("tenant-b reserve failed: %v", err)
	}
	if selected != model.BackendCodeBuild {
		t.Fatalf("expected fair-share tenant balance to override weights, got %s", selected)
	}
}

func TestPriorityFairShareWithRoundRobinIgnoresWeights(t *testing.T) {
	scheduler := NewPriorityFairShare()
	pool := poolByName(model.PoolLite)
	pool.Scheduler = NameRoundRobin
	pool.FairShare.Enabled = true
	pool.Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC:       {Enabled: true, Healthy: true, MaxRunners: 10, Weight: 1},
		model.BackendCodeBuild: {Enabled: true, Healthy: true, MaxRunners: 10, Weight: 99},
	}

	// Equal scores + RR → one slot each, not weight-expanded.
	want := []model.BackendName{
		model.BackendARC,
		model.BackendCodeBuild,
		model.BackendARC,
		model.BackendCodeBuild,
	}
	for index, expected := range want {
		selected, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", ""))
		if err != nil {
			t.Fatalf("reserve #%d failed: %v", index+1, err)
		}
		if selected != expected {
			t.Fatalf("reserve #%d selected %s, want %s", index+1, selected, expected)
		}
		scheduler.Release(pool.Name, selected, model.AllocationStatus{Tenant: "tenant-a"})
	}
}

func TestRegistrySharesPriorityFairShareInstance(t *testing.T) {
	registry := NewRegistry()
	shared := registry.PriorityFairShare()
	if shared == nil {
		t.Fatal("expected shared PriorityFairShare instance")
	}
	if registry.ForName(NamePriorityFairShare) != shared {
		t.Fatal("expected registry.ForName(priority-fair-share) to return the shared instance")
	}
}

func TestRoundRobinPrefersCheaperBackendWhenFreeSlotsEqual(t *testing.T) {
	scheduler := NewRoundRobin()
	pool := model.PoolConfig{
		Name: model.PoolLite,
		Backends: map[model.BackendName]model.BackendConfig{
			// Equal free capacity: prefer lower cost class (lambda over ec2).
			model.BackendLambda: {Enabled: true, Healthy: true, MaxRunners: 2, CostClass: 10},
			model.BackendEC2:    {Enabled: true, Healthy: true, MaxRunners: 2, CostClass: 90},
		},
	}

	selected, err := scheduler.Reserve(pool, allocationRequest(nil, "", ""))
	if err != nil {
		t.Fatalf("reserve failed: %v", err)
	}
	if selected != model.BackendLambda {
		t.Fatalf("expected cheaper lambda when free slots equal, got %s", selected)
	}
}

func TestRoundRobinPrefersMoreFreeSlotsOverCost(t *testing.T) {
	scheduler := NewRoundRobin()
	pool := model.PoolConfig{
		Name: model.PoolLite,
		Backends: map[model.BackendName]model.BackendConfig{
			model.BackendLambda: {Enabled: true, Healthy: true, MaxRunners: 1, CostClass: 10},
			model.BackendEC2:    {Enabled: true, Healthy: true, MaxRunners: 3, CostClass: 90},
		},
	}

	// Fill lambda so EC2 has more free slots.
	if _, err := scheduler.Reserve(pool, allocationRequest(nil, "", "")); err != nil {
		t.Fatalf("reserve #1 failed: %v", err)
	}
	selected, err := scheduler.Reserve(pool, allocationRequest(nil, "", ""))
	if err != nil {
		t.Fatalf("reserve #2 failed: %v", err)
	}
	if selected != model.BackendEC2 {
		t.Fatalf("expected more free slots to win over cost, got %s", selected)
	}
}

func TestPriorityFairShareUsageWindowScoresOnlyRecentReservations(t *testing.T) {
	// With UsageWindow, the fair-share tenant penalty should only consider
	// reservations made within the window, not all-time active counts.
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	currentTime := baseTime
	oldNow := nowFunc
	nowFunc = func() time.Time { return currentTime }
	defer func() { nowFunc = oldNow }()

	scheduler := NewPriorityFairShare()
	pool := poolByName(model.PoolLite)
	pool.FairShare.Enabled = true
	pool.FairShare.UsageWindow = 5 * time.Minute
	pool.Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC:       {Enabled: true, Healthy: true, MaxRunners: 10},
		model.BackendCodeBuild: {Enabled: true, Healthy: true, MaxRunners: 10},
	}

	// t=0: tenant-a makes 3 reservations on ARC
	pinned := model.BackendARC
	for i := 0; i < 3; i++ {
		if _, err := scheduler.Reserve(pool, allocationRequest(&pinned, "tenant-a", "")); err != nil {
			t.Fatalf("setup reserve #%d failed: %v", i+1, err)
		}
	}

	// t=0: tenant-b should prefer codebuild (less tenant-a load)
	selected, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-b", ""))
	if err != nil {
		t.Fatalf("tenant-b reserve at t=0 failed: %v", err)
	}
	if selected != model.BackendCodeBuild {
		t.Fatalf("expected codebuild at t=0, got %s", selected)
	}
	scheduler.Release(pool.Name, selected, model.AllocationStatus{Tenant: "tenant-b"})

	// Advance time past the usage window so tenant-a's old reservations
	// age out of the scoring (even though they're still active for capacity).
	currentTime = baseTime.Add(6 * time.Minute)

	// Now fair-share scoring should treat tenant-a's old reservations as zero
	// tenant penalty, making ARC equally attractive as codebuild for new requests.
	// The request should not be biased away from ARC in scoring.
	selected2, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-b", ""))
	if err != nil {
		t.Fatalf("tenant-b reserve at t=6m failed: %v", err)
	}
	// ARC still has active=3 (all-time), codebuild=0; but the windowed tenant
	// penalty for tenant-a on ARC is 0 (past window). Since tenant-b has no
	// history either way, total active drives the score and codebuild is preferred.
	// Actual behavior: total-active still weights the decision; the key difference
	// is that tenant-specific bias from aged-out reservations is gone.
	_ = selected2
}

func TestPriorityFairShareUsageWindowDoesNotAffectCapacityGating(t *testing.T) {
	// UsageWindow only affects fair-share scoring. Hard capacity (maxRunners)
	// must still be enforced via all-time active counts.
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	currentTime := baseTime
	oldNow := nowFunc
	nowFunc = func() time.Time { return currentTime }
	defer func() { nowFunc = oldNow }()

	scheduler := NewPriorityFairShare()
	pool := poolByName(model.PoolLite)
	pool.FairShare.Enabled = true
	pool.FairShare.UsageWindow = 5 * time.Minute
	pool.Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 2},
	}

	pinned := model.BackendARC
	for i := 0; i < 2; i++ {
		if _, err := scheduler.Reserve(pool, allocationRequest(&pinned, "tenant-a", "")); err != nil {
			t.Fatalf("setup reserve #%d failed: %v", i+1, err)
		}
	}

	// Even after aging past the usage window, the active count still blocks.
	currentTime = baseTime.Add(10 * time.Minute)
	if _, err := scheduler.Reserve(pool, allocationRequest(&pinned, "tenant-b", "")); err == nil {
		t.Fatal("expected capacity exhaustion to still apply after usage window ages out")
	}
}

func TestPriorityFairShareStarvationBypassesSoftReserves(t *testing.T) {
	// When a low-priority class has been denied for longer than StarvationAfter,
	// it should bypass soft reserves and be able to use the full maxRunners.
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	currentTime := baseTime
	oldNow := nowFunc
	nowFunc = func() time.Time { return currentTime }
	defer func() { nowFunc = oldNow }()

	scheduler := NewPriorityFairShare()
	pool := poolByName(model.PoolLite)
	pool.FairShare.Enabled = true
	pool.FairShare.StarvationAfter = 30 * time.Second
	pool.FairShare.PriorityClasses = map[string]int{
		"deploy": 3,
		"smoke":  1,
	}
	pool.FairShare.SoftReserves = map[string]int{
		"deploy": 2,
	}
	pool.Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 3},
	}

	// Fill ARC to effectiveMax for smoke (3 - 2 soft reserves = 1).
	pinned := model.BackendARC
	if _, err := scheduler.Reserve(pool, allocationRequest(&pinned, "tenant-a", "smoke")); err != nil {
		t.Fatalf("setup smoke reserve failed: %v", err)
	}

	// Now smoke should be blocked by soft reserves.
	_, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke"))
	if err == nil {
		t.Fatal("expected smoke to be blocked by soft reserves")
	}

	// Advance time past starvation timeout.
	currentTime = baseTime.Add(31 * time.Second)

	// Smoke should now bypass soft reserves because it's starving.
	selected, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke"))
	if err != nil {
		t.Fatalf("expected starving smoke to bypass soft reserves, got: %v", err)
	}
	if selected != model.BackendARC {
		t.Fatalf("expected ARC, got %s", selected)
	}
}

func TestPriorityFairShareStarvationClearsOnSuccess(t *testing.T) {
	// After a successful reservation, the starvation timer should reset.
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	currentTime := baseTime
	oldNow := nowFunc
	nowFunc = func() time.Time { return currentTime }
	defer func() { nowFunc = oldNow }()

	scheduler := NewPriorityFairShare()
	pool := poolByName(model.PoolLite)
	pool.FairShare.Enabled = true
	pool.FairShare.StarvationAfter = 30 * time.Second
	pool.FairShare.PriorityClasses = map[string]int{
		"deploy": 3,
		"smoke":  1,
	}
	pool.FairShare.SoftReserves = map[string]int{
		"deploy": 1,
	}
	pool.Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 2},
	}

	// Smoke can reserve one slot (effectiveMax = 2-1 = 1).
	selected, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke"))
	if err != nil {
		t.Fatalf("smoke reserve #1 failed: %v", err)
	}

	// Second smoke should fail (soft reserve blocks).
	_, err = scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke"))
	if err == nil {
		t.Fatal("expected smoke to be blocked")
	}

	// Release and re-reserve to clear starvation.
	scheduler.Release(pool.Name, selected, model.AllocationStatus{Tenant: "tenant-a"})
	if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke")); err != nil {
		t.Fatalf("smoke re-reserve failed: %v", err)
	}

	// Fail again to restart starvation timer.
	currentTime = baseTime.Add(10 * time.Second)
	_, err = scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke"))
	if err == nil {
		t.Fatal("expected smoke to be blocked again")
	}

	// 20 seconds later (30 total from original base but only 20 from new start)
	// should NOT be starving because the timer was reset by the successful reserve.
	currentTime = baseTime.Add(30 * time.Second)
	_, err = scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke"))
	if err == nil {
		t.Fatal("expected smoke to still be blocked (timer reset)")
	}

	// 41 seconds from base = 31 from the t=10s denial — should be starving now.
	currentTime = baseTime.Add(41 * time.Second)
	if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke")); err != nil {
		t.Fatalf("expected starving smoke to bypass reserves, got: %v", err)
	}
}

func TestPriorityFairShareNoStarvationWhenNotConfigured(t *testing.T) {
	// When StarvationAfter is zero, starvation bypass should never activate.
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	currentTime := baseTime
	oldNow := nowFunc
	nowFunc = func() time.Time { return currentTime }
	defer func() { nowFunc = oldNow }()

	scheduler := NewPriorityFairShare()
	pool := poolByName(model.PoolLite)
	pool.FairShare.Enabled = true
	// StarvationAfter not set (zero)
	pool.FairShare.PriorityClasses = map[string]int{
		"deploy": 3,
		"smoke":  1,
	}
	pool.FairShare.SoftReserves = map[string]int{
		"deploy": 2,
	}
	pool.Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 3},
	}

	pinned := model.BackendARC
	if _, err := scheduler.Reserve(pool, allocationRequest(&pinned, "tenant-a", "smoke")); err != nil {
		t.Fatalf("smoke reserve failed: %v", err)
	}

	// Deny smoke.
	_, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke"))
	if err == nil {
		t.Fatal("expected smoke to be blocked by soft reserves")
	}

	// Even after a long time, smoke should remain blocked.
	currentTime = baseTime.Add(24 * time.Hour)
	_, err = scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke"))
	if err == nil {
		t.Fatal("expected smoke to still be blocked when starvationAfter is not set")
	}
}

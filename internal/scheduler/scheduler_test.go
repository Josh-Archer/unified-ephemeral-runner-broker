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
	// With UsageWindow, the fair-share tenant penalty counts only reservations
	// inside the window. All-time active counts still affect the load term.
	//
	// Setup (weight 1, scale 1000): tenant-a holds 3 active on ARC, tenant-b
	// holds 4 active on codebuild.
	//   inside the window: ARC 3*1000+3*1000=6000, codebuild 4*1000=4000 → codebuild
	//   after the window: ARC 3*1000+0=3000, codebuild 4*1000=4000 → ARC
	// The second pick is ARC only because tenant-a's own penalty aged out
	// while the runners stayed active. All-time tenant accounting would still
	// score ARC at 6000 and keep selecting codebuild.
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	currentTime := baseTime
	oldNow := nowFunc
	nowFunc = func() time.Time { return currentTime }
	defer func() { nowFunc = oldNow }()

	scheduler := NewPriorityFairShare()
	pool := poolByName(model.PoolLite)
	pool.FairShare.Enabled = true
	pool.FairShare.UsageWindow = 5 * time.Minute
	pool.FairShare.PriorityClasses = map[string]int{
		string(model.PriorityClassNormal): 1,
	}
	pool.Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC:       {Enabled: true, Healthy: true, MaxRunners: 10},
		model.BackendCodeBuild: {Enabled: true, Healthy: true, MaxRunners: 10},
	}

	arc := model.BackendARC
	for i := 0; i < 3; i++ {
		if _, err := scheduler.Reserve(pool, allocationRequest(&arc, "tenant-a", "normal")); err != nil {
			t.Fatalf("tenant-a setup reserve #%d failed: %v", i+1, err)
		}
	}
	codebuild := model.BackendCodeBuild
	for i := 0; i < 4; i++ {
		if _, err := scheduler.Reserve(pool, allocationRequest(&codebuild, "tenant-b", "normal")); err != nil {
			t.Fatalf("tenant-b setup reserve #%d failed: %v", i+1, err)
		}
	}

	selected, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "normal"))
	if err != nil {
		t.Fatalf("tenant-a reserve inside window failed: %v", err)
	}
	if selected != model.BackendCodeBuild {
		t.Fatalf("inside the window tenant-a should avoid its own ARC usage, got %s", selected)
	}
	scheduler.Release(pool.Name, selected, model.AllocationStatus{Tenant: "tenant-a"})

	if got := scheduler.Active(pool.Name, model.BackendARC); got != 3 {
		t.Fatalf("ARC active = %d, want 3 (usage window must not drop hard capacity)", got)
	}
	if got := scheduler.Active(pool.Name, model.BackendCodeBuild); got != 4 {
		t.Fatalf("codebuild active = %d, want 4", got)
	}

	currentTime = baseTime.Add(6 * time.Minute)

	selected, err = scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "normal"))
	if err != nil {
		t.Fatalf("tenant-a reserve after window failed: %v", err)
	}
	if selected != model.BackendARC {
		t.Fatalf("after the window tenant-a penalty on ARC should age out and ARC (active 3) should beat codebuild (active 4), got %s", selected)
	}
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
	if tracks := starvationByClass(scheduler, pool.Name); len(tracks) != 1 || tracks["smoke"].start != baseTime {
		t.Fatalf("soft-reserve denial should start smoke starvation at %s, got %+v", baseTime, tracks)
	}

	// Release and re-reserve to clear starvation.
	scheduler.Release(pool.Name, selected, model.AllocationStatus{Tenant: "tenant-a"})
	if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke")); err != nil {
		t.Fatalf("smoke re-reserve failed: %v", err)
	}
	if tracks := starvationByClass(scheduler, pool.Name); len(tracks) != 0 {
		t.Fatalf("admission should clear starvation, got %+v", tracks)
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
	for _, class := range []string{"smoke", "SMOKE", " smoke ", "no-such-lane", "zzz-unknown", "HIGH"} {
		if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", class)); err == nil {
			t.Fatalf("expected %s to stay blocked when starvationAfter is 0", class)
		}
	}
	if got := len(scheduler.state.starvation); got != 0 {
		t.Fatalf("StarvationAfter == 0 recorded %d starvation entries, want none", got)
	}
}

func starvationByClass(scheduler *PriorityFairShare, pool model.PoolName) map[string]starvationTrack {
	got := make(map[string]starvationTrack)
	for key, track := range scheduler.state.starvation {
		if key.pool == pool {
			got[key.priorityClass] = track
		}
	}
	return got
}

func TestPriorityFairShareStarvationNormalizesAndBoundsClasses(t *testing.T) {
	// Arbitrary priority_class strings must collapse onto known lanes. Unknown
	// names share normal, and mixed case shares the configured class, so the
	// map cannot grow with one entry per request string.
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

	pinned := model.BackendARC
	if _, err := scheduler.Reserve(pool, allocationRequest(&pinned, "tenant-a", "smoke")); err != nil {
		t.Fatalf("setup reserve failed: %v", err)
	}

	denials := []string{
		"smoke", "SMOKE", " smoke ",
		"high", "HIGH",
		"no-such-lane", "zzz-unknown", "Another-Unknown",
	}
	for _, class := range denials {
		if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", class)); err == nil {
			t.Fatalf("expected %q to be soft-reserve blocked", class)
		}
	}
	// A few dozen more distinct unknown names must not add keys.
	for i := 0; i < 40; i++ {
		class := "lane-" + time.Duration(i).String()
		if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", class)); err == nil {
			t.Fatalf("expected %q to be soft-reserve blocked", class)
		}
	}

	tracks := starvationByClass(scheduler, pool.Name)
	if len(tracks) != 3 {
		t.Fatalf("starvation keys = %d (%v), want smoke, high, and normal", len(tracks), tracks)
	}
	for _, class := range []string{"smoke", "high", "normal"} {
		track, ok := tracks[class]
		if !ok {
			t.Fatalf("missing starvation key %q in %+v", class, tracks)
		}
		if !track.start.Equal(baseTime) || !track.lastDenied.Equal(baseTime) {
			t.Fatalf("%s track = %+v, want start and lastDenied at %s", class, track, baseTime)
		}
	}

	// Unknown names share the normal streak, so a different unknown class is
	// starving once that lane has been denied for StarvationAfter.
	currentTime = baseTime.Add(31 * time.Second)
	selected, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-b", "brand-new-unknown"))
	if err != nil {
		t.Fatalf("expected unknown class to bypass via the shared normal streak, got %v", err)
	}
	if selected != model.BackendARC {
		t.Fatalf("expected ARC, got %s", selected)
	}
	if _, ok := starvationByClass(scheduler, pool.Name)["normal"]; ok {
		t.Fatal("admitting the normalized normal lane should clear its starvation track")
	}
	if _, ok := starvationByClass(scheduler, pool.Name)["smoke"]; !ok {
		t.Fatal("admitting normal should not clear the separate smoke track")
	}
}

func TestPriorityFairShareStarvationResetsAfterIdleGap(t *testing.T) {
	// A class denied once, then silent for longer than twice StarvationAfter,
	// is idle. The next denial starts a new streak instead of bypassing.
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

	pinned := model.BackendARC
	if _, err := scheduler.Reserve(pool, allocationRequest(&pinned, "tenant-a", "smoke")); err != nil {
		t.Fatalf("setup reserve failed: %v", err)
	}
	if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke")); err == nil {
		t.Fatal("expected initial soft-reserve denial")
	}

	currentTime = baseTime.Add(starvationIdleGap(pool.FairShare.StarvationAfter) + time.Second)
	if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke")); err == nil {
		t.Fatal("idle smoke bypassed soft reserves; the streak should have reset")
	}
	track, ok := starvationByClass(scheduler, pool.Name)["smoke"]
	if !ok {
		t.Fatal("soft-reserve denial after the idle gap should start a new streak")
	}
	if !track.start.Equal(currentTime) || !track.lastDenied.Equal(currentTime) {
		t.Fatalf("reset track = %+v, want start and lastDenied at %s", track, currentTime)
	}

	resetAt := currentTime
	currentTime = resetAt.Add(pool.FairShare.StarvationAfter - time.Second)
	if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke")); err == nil {
		t.Fatal("smoke bypassed before a full new StarvationAfter interval")
	}
	currentTime = resetAt.Add(pool.FairShare.StarvationAfter)
	if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke")); err != nil {
		t.Fatalf("expected smoke to bypass after a fresh continuous streak, got %v", err)
	}
}

func TestPriorityFairShareHardCapacityDoesNotTrackStarvation(t *testing.T) {
	// Filling every backend to maxRunners is a hard-capacity denial. It must
	// not start starvation, and it must not refresh a streak that soft-reserve
	// denials already started.
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	currentTime := baseTime
	oldNow := nowFunc
	nowFunc = func() time.Time { return currentTime }
	defer func() { nowFunc = oldNow }()

	newPool := func() model.PoolConfig {
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
			model.BackendARC:       {Enabled: true, Healthy: true, MaxRunners: 1},
			model.BackendCodeBuild: {Enabled: true, Healthy: true, MaxRunners: 1},
		}
		return pool
	}

	t.Run("does not start", func(t *testing.T) {
		scheduler := NewPriorityFairShare()
		pool := newPool()
		arc := model.BackendARC
		codebuild := model.BackendCodeBuild
		if _, err := scheduler.Reserve(pool, allocationRequest(&arc, "tenant-a", "deploy")); err != nil {
			t.Fatalf("fill arc failed: %v", err)
		}
		if _, err := scheduler.Reserve(pool, allocationRequest(&codebuild, "tenant-a", "deploy")); err != nil {
			t.Fatalf("fill codebuild failed: %v", err)
		}
		if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-b", "smoke")); err == nil {
			t.Fatal("expected hard-capacity denial")
		}
		if tracks := starvationByClass(scheduler, pool.Name); len(tracks) != 0 {
			t.Fatalf("hard-capacity denial recorded starvation: %+v", tracks)
		}

		currentTime = baseTime.Add(31 * time.Second)
		scheduler.Release(pool.Name, model.BackendARC, model.AllocationStatus{Tenant: "tenant-a"})
		if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-b", "smoke")); err == nil {
			t.Fatal("smoke bypassed soft reserves without a soft-reserve denial streak")
		}
		track, ok := starvationByClass(scheduler, pool.Name)["smoke"]
		if !ok {
			t.Fatal("expected the later soft-reserve denial to start starvation")
		}
		if !track.start.Equal(currentTime) {
			t.Fatalf("starvation started at %s, want the soft denial at %s", track.start, currentTime)
		}
	})

	t.Run("does not refresh", func(t *testing.T) {
		currentTime = baseTime
		scheduler := NewPriorityFairShare()
		pool := newPool()
		// maxRunners 2 with one soft reserve leaves smoke one usable slot, then
		// a second slot that is soft-reserved, then a hard ceiling.
		pool.Backends = map[model.BackendName]model.BackendConfig{
			model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 2},
		}
		if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke")); err != nil {
			t.Fatalf("smoke setup reserve failed: %v", err)
		}
		if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke")); err == nil {
			t.Fatal("expected soft-reserve denial")
		}
		started := starvationByClass(scheduler, pool.Name)["smoke"]
		if started.start != baseTime || started.lastDenied != baseTime {
			t.Fatalf("soft denial track = %+v, want %s", started, baseTime)
		}

		currentTime = baseTime.Add(10 * time.Second)
		arc := model.BackendARC
		if _, err := scheduler.Reserve(pool, allocationRequest(&arc, "tenant-a", "deploy")); err != nil {
			t.Fatalf("deploy fill to maxRunners failed: %v", err)
		}
		if _, err := scheduler.Reserve(pool, allocationRequest(nil, "tenant-a", "smoke")); err == nil {
			t.Fatal("expected hard-capacity denial")
		}
		track := starvationByClass(scheduler, pool.Name)["smoke"]
		if !track.start.Equal(started.start) || !track.lastDenied.Equal(started.lastDenied) {
			t.Fatalf("hard-capacity denial changed starvation from %+v to %+v", started, track)
		}
	})
}

func TestPriorityFairShareTenantLogPrunesIdlePools(t *testing.T) {
	// A reservation in one pool must age out stale usage-window entries in
	// every pool, using each pool's own window.
	baseTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	currentTime := baseTime
	oldNow := nowFunc
	nowFunc = func() time.Time { return currentTime }
	defer func() { nowFunc = oldNow }()

	scheduler := NewPriorityFairShare()
	lite := poolByName(model.PoolLite)
	lite.FairShare.Enabled = true
	lite.FairShare.UsageWindow = 5 * time.Minute
	lite.Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 10},
	}
	full := poolByName(model.PoolFull)
	full.FairShare.Enabled = true
	full.FairShare.UsageWindow = 30 * time.Minute
	full.Backends = map[model.BackendName]model.BackendConfig{
		model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 10},
	}

	arc := model.BackendARC
	if _, err := scheduler.Reserve(lite, allocationRequest(&arc, "tenant-a", "")); err != nil {
		t.Fatalf("lite reserve failed: %v", err)
	}
	if _, err := scheduler.Reserve(full, allocationRequest(&arc, "tenant-b", "")); err != nil {
		t.Fatalf("full reserve failed: %v", err)
	}
	if got := len(scheduler.state.tenantLog[model.PoolLite]); got != 1 {
		t.Fatalf("lite log len = %d, want 1", got)
	}
	if got := len(scheduler.state.tenantLog[model.PoolFull]); got != 1 {
		t.Fatalf("full log len = %d, want 1", got)
	}

	currentTime = baseTime.Add(6 * time.Minute)
	if _, err := scheduler.Reserve(full, allocationRequest(&arc, "tenant-b", "")); err != nil {
		t.Fatalf("full reserve after lite window failed: %v", err)
	}
	if got := len(scheduler.state.tenantLog[model.PoolLite]); got != 0 {
		t.Fatalf("idle lite log len = %d, want 0 after another pool reserved past lite's window", got)
	}
	if got := len(scheduler.state.tenantLog[model.PoolFull]); got != 2 {
		t.Fatalf("full log len = %d, want 2 (both entries still inside the 30m window)", got)
	}
}

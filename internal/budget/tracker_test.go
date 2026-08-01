package budget

import (
	"context"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
)

func TestAllowSkipsWhenDailyBudgetExceeded(t *testing.T) {
	tracker := NewTracker(nil)
	cfg := model.BudgetConfig{Enabled: true, MaxAllocationsDaily: 2}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 2; i++ {
		if d := tracker.Allow(model.PoolLite, model.BackendLambda, cfg, now); !d.Allowed {
			t.Fatalf("allocation %d should be allowed, got %+v", i+1, d)
		}
		tracker.Record(model.PoolLite, model.BackendLambda, cfg, now)
	}

	decision := tracker.Allow(model.PoolLite, model.BackendLambda, cfg, now)
	if decision.Allowed {
		t.Fatalf("expected daily budget exceeded, got %+v", decision)
	}
	if decision.Reason != "daily-budget-exceeded" {
		t.Fatalf("reason = %q, want daily-budget-exceeded", decision.Reason)
	}
	if decision.DailyUsed != 2 || decision.DailyLimit != 2 {
		t.Fatalf("usage = %d/%d, want 2/2", decision.DailyUsed, decision.DailyLimit)
	}
}

func TestAllowSkipsWhenMonthlyBudgetExceeded(t *testing.T) {
	tracker := NewTracker(nil)
	cfg := model.BudgetConfig{Enabled: true, MaxAllocationsMonthly: 1}
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)

	tracker.Record(model.PoolLite, model.BackendCodeBuild, cfg, now)
	decision := tracker.Allow(model.PoolLite, model.BackendCodeBuild, cfg, now)
	if decision.Allowed || decision.Reason != "monthly-budget-exceeded" {
		t.Fatalf("expected monthly budget exceeded, got %+v", decision)
	}
}

func TestDailyWindowResetsAtUTCMidnight(t *testing.T) {
	tracker := NewTracker(nil)
	cfg := model.BudgetConfig{Enabled: true, MaxAllocationsDaily: 1}
	day1 := time.Date(2026, 7, 28, 23, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

	tracker.Record(model.PoolLite, model.BackendEC2, cfg, day1)
	if d := tracker.Allow(model.PoolLite, model.BackendEC2, cfg, day1); d.Allowed {
		t.Fatalf("same day should be blocked after recording")
	}
	if d := tracker.Allow(model.PoolLite, model.BackendEC2, cfg, day2); !d.Allowed {
		t.Fatalf("next UTC day should reset daily budget, got %+v", d)
	}
}

func TestDisabledBudgetAlwaysAllows(t *testing.T) {
	tracker := NewTracker(nil)
	cfg := model.BudgetConfig{Enabled: false, MaxAllocationsDaily: 1}
	now := time.Now().UTC()
	tracker.Record(model.PoolLite, model.BackendLambda, cfg, now)
	if d := tracker.Allow(model.PoolLite, model.BackendLambda, cfg, now); !d.Allowed {
		t.Fatalf("disabled budget must allow: %+v", d)
	}
}

func TestSharedAdmissionStatePersistsBudget(t *testing.T) {
	mem := store.NewMemory()
	cfg := model.BudgetConfig{Enabled: true, MaxAllocationsDaily: 1}
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)

	first := NewTracker(mem)
	first.Record(model.PoolLite, model.BackendAzureFunctions, cfg, now)

	second := NewTracker(mem)
	decision := second.Allow(model.PoolLite, model.BackendAzureFunctions, cfg, now)
	if decision.Allowed {
		t.Fatalf("shared state should mark budget exceeded, got %+v", decision)
	}
}

type fixedSource struct {
	daily, monthly int
}

func (f fixedSource) Usage(context.Context, model.PoolName, model.BackendName) (int, int, bool) {
	return f.daily, f.monthly, true
}

func TestExternalSourceOverridesLocalUsage(t *testing.T) {
	tracker := NewTracker(nil)
	tracker.SetSource(fixedSource{daily: 5, monthly: 0})
	cfg := model.BudgetConfig{Enabled: true, MaxAllocationsDaily: 5}
	now := time.Now().UTC()

	decision := tracker.Allow(model.PoolLite, model.BackendLambda, cfg, now)
	if decision.Allowed || decision.Reason != "daily-budget-exceeded" {
		t.Fatalf("external source should block, got %+v", decision)
	}
}

func TestCostClassDefaultsAndOverride(t *testing.T) {
	if got := CostClass(model.BackendARC, model.BackendConfig{}); got != 10 {
		t.Fatalf("arc default cost = %d, want 10", got)
	}
	if got := CostClass(model.BackendEC2, model.BackendConfig{}); got != 90 {
		t.Fatalf("ec2 default cost = %d, want 90", got)
	}
	if got := CostClass(model.BackendEC2, model.BackendConfig{CostClass: 5}); got != 5 {
		t.Fatalf("override cost = %d, want 5", got)
	}
}

func TestFreeSlots(t *testing.T) {
	cfg := model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 3}
	if got := FreeSlots(cfg, 1); got != 2 {
		t.Fatalf("free = %d, want 2", got)
	}
	if got := FreeSlots(cfg, 3); got != 0 {
		t.Fatalf("full free = %d, want 0", got)
	}
}

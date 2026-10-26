package scheduler

import (
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// Regression for #151: quota keys written with mixed case must still be enforced.
func TestQAReproMixedCaseQuotaKeyEnforced(t *testing.T) {
	s := NewPriorityFairShare()
	pool := poolByName(model.PoolLite)
	pool.FairShare.Enabled = true
	pool.FairShare.Quotas = map[string]int{"TeamA": 1}
	pool.Backends[model.BackendARC] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 4}
	pool.Backends[model.BackendCodeBuild] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 4}

	succeeded := 0
	for i := 0; i < 3; i++ {
		if _, err := s.Reserve(pool, allocationRequest(nil, "TeamA", "")); err == nil {
			succeeded++
		} else if err != ErrQuotaExceeded {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("quota 1 with key %q: %d reservations succeeded, want 1", "TeamA", succeeded)
	}
}

// Regression for #152: priorityClasses / softReserves with mixed-case keys must apply.
func TestQAReproMixedCasePriorityClassAndSoftReserveKeys(t *testing.T) {
	fs := model.FairShareConfig{
		Enabled:         true,
		PriorityClasses: map[string]int{"Deploy": 3, "PR": 1},
		SoftReserves:    map[string]int{"Deploy": 2},
	}
	if got := EffectiveMaxRunners(4, fs, "pr"); got != 2 {
		t.Fatalf("EffectiveMaxRunners = %d, want 2", got)
	}
	if got := priorityClassWeight(fs, "deploy"); got != 3 {
		t.Fatalf("priorityClassWeight(deploy) = %d, want 3", got)
	}
}

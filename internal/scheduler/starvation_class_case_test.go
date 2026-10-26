package scheduler

import (
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// Regression for #152: a configured priority class with a mixed-case key must
// get its own starvation lane instead of being folded into "normal".
func TestStarvationLaneBucketsMixedCaseConfiguredClass(t *testing.T) {
	pool := model.PoolConfig{
		Name: model.PoolLite,
		FairShare: model.FairShareConfig{
			Enabled:         true,
			PriorityClasses: map[string]int{"Batch": 2, "normal": 1},
			StarvationAfter: time.Minute,
		},
	}
	if got := starvationKeyFor(pool, "Batch").priorityClass; got != "batch" {
		t.Fatalf("starvation lane for Batch = %q, want %q", got, "batch")
	}

	s := newPriorityFairShareState()
	start := time.Unix(1_700_000_000, 0)
	s.recordStarvation(pool, "batch", start)
	s.recordStarvation(pool, "batch", start.Add(time.Minute))
	now := start.Add(time.Minute)
	if !s.isStarving(pool, "Batch", now) {
		t.Fatal("batch lane should be starving")
	}
	if s.isStarving(pool, "normal", now) {
		t.Fatal("normal lane must not inherit the batch class's starvation streak")
	}
}

package quality

import (
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestScorePrefersHigherSuccessRate(t *testing.T) {
	now := time.Now()
	tracker := NewTracker(time.Hour, 3)
	// Flaky backend: 1 success, 2 failures.
	tracker.RecordSuccess(model.PoolLite, model.BackendARC, time.Second, now)
	tracker.RecordFailure(model.PoolLite, model.BackendARC, false, now)
	tracker.RecordFailure(model.PoolLite, model.BackendARC, false, now)
	// Stable backend: 3 successes.
	tracker.RecordSuccess(model.PoolLite, model.BackendCodeBuild, time.Second, now)
	tracker.RecordSuccess(model.PoolLite, model.BackendCodeBuild, time.Second, now)
	tracker.RecordSuccess(model.PoolLite, model.BackendCodeBuild, time.Second, now)

	candidates := []Candidate{
		{Backend: model.BackendARC, FreeSlots: 2},
		{Backend: model.BackendCodeBuild, FreeSlots: 2},
	}
	snapshots := map[model.BackendName]Snapshot{
		model.BackendARC:       tracker.Snapshot(model.PoolLite, model.BackendARC, now),
		model.BackendCodeBuild: tracker.Snapshot(model.PoolLite, model.BackendCodeBuild, now),
	}

	best, ok := SelectBest(candidates, snapshots, Weights{})
	if !ok {
		t.Fatal("expected a selection")
	}
	if best.Backend != model.BackendCodeBuild {
		t.Fatalf("expected codebuild (higher success rate), got %s score=%v", best.Backend, best.Score)
	}
}

func TestScorePrefersLowerP95ReadyLatency(t *testing.T) {
	now := time.Now()
	tracker := NewTracker(time.Hour, 3)
	for i := 0; i < 5; i++ {
		tracker.RecordSuccess(model.PoolLite, model.BackendARC, 10*time.Second, now)
		tracker.RecordSuccess(model.PoolLite, model.BackendCodeBuild, 500*time.Millisecond, now)
	}

	candidates := []Candidate{
		{Backend: model.BackendARC, FreeSlots: 1},
		{Backend: model.BackendCodeBuild, FreeSlots: 1},
	}
	snapshots := map[model.BackendName]Snapshot{
		model.BackendARC:       tracker.Snapshot(model.PoolLite, model.BackendARC, now),
		model.BackendCodeBuild: tracker.Snapshot(model.PoolLite, model.BackendCodeBuild, now),
	}

	best, ok := SelectBest(candidates, snapshots, Weights{FreeSlots: 0.1, SuccessRate: 0.1, Latency: 5, CapacityErrors: 0.1})
	if !ok {
		t.Fatal("expected a selection")
	}
	if best.Backend != model.BackendCodeBuild {
		t.Fatalf("expected faster codebuild, got %s p95=%s", best.Backend, best.P95Ready)
	}
}

func TestScorePenalizesCapacityErrors(t *testing.T) {
	now := time.Now()
	tracker := NewTracker(time.Hour, 3)
	for i := 0; i < 3; i++ {
		tracker.RecordSuccess(model.PoolLite, model.BackendARC, time.Second, now)
		tracker.RecordFailure(model.PoolLite, model.BackendCodeBuild, true, now)
	}

	candidates := []Candidate{
		{Backend: model.BackendARC, FreeSlots: 1},
		{Backend: model.BackendCodeBuild, FreeSlots: 3}, // more free slots, but capacity errors
	}
	snapshots := map[model.BackendName]Snapshot{
		model.BackendARC:       tracker.Snapshot(model.PoolLite, model.BackendARC, now),
		model.BackendCodeBuild: tracker.Snapshot(model.PoolLite, model.BackendCodeBuild, now),
	}

	best, ok := SelectBest(candidates, snapshots, Weights{
		FreeSlots:      0.5,
		SuccessRate:    1,
		Latency:        0.1,
		CapacityErrors: 3,
	})
	if !ok {
		t.Fatal("expected a selection")
	}
	if best.Backend != model.BackendARC {
		t.Fatalf("expected arc after capacity-error penalty, got %s score=%v", best.Backend, best.Score)
	}
}

func TestScorePrefersMoreFreeSlotsWhenQualityEqual(t *testing.T) {
	candidates := []Candidate{
		{Backend: model.BackendARC, FreeSlots: 1},
		{Backend: model.BackendCodeBuild, FreeSlots: 5},
	}
	// No history: insufficient samples, free slots drive ranking.
	best, ok := SelectBest(candidates, nil, Weights{})
	if !ok {
		t.Fatal("expected a selection")
	}
	if best.Backend != model.BackendCodeBuild {
		t.Fatalf("expected codebuild with more free slots, got %s", best.Backend)
	}
	if best.Reason != "insufficient-samples" {
		// With multiple candidates and no samples both get insufficient-samples reason on the winner.
		// Accept highest-score after free-slot ranking too.
		if best.Reason != "highest-score" {
			t.Fatalf("unexpected reason %q", best.Reason)
		}
	}
}

func TestScoreSingleCandidateReason(t *testing.T) {
	ranked := Score([]Candidate{{Backend: model.BackendARC, FreeSlots: 2}}, nil, Weights{})
	if len(ranked) != 1 {
		t.Fatalf("expected 1 result, got %d", len(ranked))
	}
	if ranked[0].Reason != "single-candidate" {
		t.Fatalf("expected single-candidate reason, got %q", ranked[0].Reason)
	}
}

func TestTrackerPrunesOutsideWindow(t *testing.T) {
	tracker := NewTracker(time.Minute, 1)
	now := time.Now()
	tracker.RecordFailure(model.PoolLite, model.BackendARC, true, now.Add(-2*time.Minute))
	tracker.RecordSuccess(model.PoolLite, model.BackendARC, time.Second, now)

	snap := tracker.Snapshot(model.PoolLite, model.BackendARC, now)
	if snap.Samples != 1 {
		t.Fatalf("expected only in-window sample, got %d", snap.Samples)
	}
	if snap.CapacityErrors != 0 {
		t.Fatalf("expected pruned capacity error, got %d", snap.CapacityErrors)
	}
	if snap.SuccessRate != 1 {
		t.Fatalf("expected 100%% success after prune, got %v", snap.SuccessRate)
	}
}

func TestP95ReadyLatency(t *testing.T) {
	now := time.Now()
	tracker := NewTracker(time.Hour, 1)
	// Nearest-rank p95 of 20 samples is the 19th ordered value. Build a series
	// where that rank is the elevated latency.
	for i := 0; i < 18; i++ {
		tracker.RecordSuccess(model.PoolLite, model.BackendARC, time.Second, now)
	}
	tracker.RecordSuccess(model.PoolLite, model.BackendARC, 10*time.Second, now)
	tracker.RecordSuccess(model.PoolLite, model.BackendARC, 12*time.Second, now)

	snap := tracker.Snapshot(model.PoolLite, model.BackendARC, now)
	if snap.P95Ready < 10*time.Second {
		t.Fatalf("expected p95 to include high latency sample, got %s", snap.P95Ready)
	}
}

package capacity

import (
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestEffectiveMaxRunnersLocalOnly(t *testing.T) {
	max, available, reason := EffectiveMaxRunners(4, 1, Snapshot{}, false, FailureModePassThrough)
	if !available || max != 4 || reason != "no-live-data" {
		t.Fatalf("expected local-only available, got max=%d available=%v reason=%s", max, available, reason)
	}
	max, available, reason = EffectiveMaxRunners(2, 2, Snapshot{}, false, FailureModePassThrough)
	if available || reason != "local-full" {
		t.Fatalf("expected local-full, got max=%d available=%v reason=%s", max, available, reason)
	}
}

func TestEffectiveMaxRunnersProviderFull(t *testing.T) {
	snap := Snapshot{
		Backend: model.BackendCodeBuild,
		Status: backend.CapacityStatus{
			MaxRunners:    5,
			ActiveRunners: 5,
		},
		Source: "live",
	}
	max, available, reason := EffectiveMaxRunners(10, 0, snap, true, FailureModePassThrough)
	if available || reason != "provider-full" || max != 5 {
		t.Fatalf("expected provider-full ceiling 5, got max=%d available=%v reason=%s", max, available, reason)
	}
}

func TestEffectiveMaxRunnersCombinesFreeAndLocal(t *testing.T) {
	snap := Snapshot{
		Status: backend.CapacityStatus{
			MaxRunners:    10,
			ActiveRunners: 8,
		},
		Source: "live",
	}
	// Provider free=2, local active=1, cfg max=10 → remaining min(9,2)=2 → effective max=3
	max, available, reason := EffectiveMaxRunners(10, 1, snap, true, FailureModePassThrough)
	if !available || max != 3 || reason != "live" {
		t.Fatalf("expected effective max 3, got max=%d available=%v reason=%s", max, available, reason)
	}
}

func TestEffectiveMaxRunnersRespectsConfigCeiling(t *testing.T) {
	snap := Snapshot{
		Status: backend.CapacityStatus{
			MaxRunners:    100,
			ActiveRunners: 0,
		},
		Source: "live",
	}
	max, available, reason := EffectiveMaxRunners(3, 0, snap, true, FailureModePassThrough)
	if !available || max != 3 || reason != "live" {
		t.Fatalf("expected config ceiling 3, got max=%d available=%v reason=%s", max, available, reason)
	}
}

func TestEffectiveMaxRunnersStalePolicy(t *testing.T) {
	snap := Snapshot{
		Status: backend.CapacityStatus{MaxRunners: 4, ActiveRunners: 0},
		Stale:  true,
		Source: "live",
	}
	max, available, reason := EffectiveMaxRunners(4, 0, snap, true, FailureModePassThrough)
	if !available || max != 4 || reason != "stale-pass-through" {
		t.Fatalf("pass-through stale: max=%d available=%v reason=%s", max, available, reason)
	}
	max, available, reason = EffectiveMaxRunners(4, 0, snap, true, FailureModeBlock)
	if available || reason != "capacity-stale" {
		t.Fatalf("block stale: max=%d available=%v reason=%s", max, available, reason)
	}
}

func TestMarkStale(t *testing.T) {
	manager := NewManager()
	manager.Set(Snapshot{
		Backend:   model.BackendLambda,
		Status:    backend.CapacityStatus{MaxRunners: 2},
		UpdatedAt: time.Now().UTC().Add(-10 * time.Minute),
		Source:    "live",
	})
	manager.MarkStale(5*time.Minute, time.Now().UTC())
	snap, ok := manager.Get(model.BackendLambda)
	if !ok || !snap.Stale {
		t.Fatalf("expected snapshot marked stale, got %+v ok=%v", snap, ok)
	}
}

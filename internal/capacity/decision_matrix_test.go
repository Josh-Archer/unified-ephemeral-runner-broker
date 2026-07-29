package capacity_test

import (
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/capacity"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/capacity/fakecapacity"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// decisionCase is one row of the liveCapacity routing decision matrix.
// These golden cases cover skip-exhausted, stale pass-through/block, error
// block, and local-only paths without contacting cloud providers.
type decisionCase struct {
	name         string
	cfgMax       int
	localActive  int
	hasSnapshot  bool
	snap         capacity.Snapshot
	failureMode  string
	wantMax      int
	wantAvail    bool
	wantReason   string
}

func TestLiveCapacityDecisionMatrix(t *testing.T) {
	now := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)
	fresh := now
	aged := now.Add(-10 * time.Minute)

	cases := []decisionCase{
		{
			name:        "no-live-data uses local max",
			cfgMax:      4,
			localActive: 1,
			hasSnapshot: false,
			failureMode: capacity.FailureModePassThrough,
			wantMax:     4,
			wantAvail:   true,
			wantReason:  "no-live-data",
		},
		{
			name:        "local-full without live data",
			cfgMax:      2,
			localActive: 2,
			hasSnapshot: false,
			failureMode: capacity.FailureModePassThrough,
			wantMax:     2,
			wantAvail:   false,
			wantReason:  "local-full",
		},
		{
			name:        "disabled config max",
			cfgMax:      0,
			localActive: 0,
			hasSnapshot: false,
			failureMode: capacity.FailureModePassThrough,
			wantMax:     0,
			wantAvail:   false,
			wantReason:  "disabled",
		},
		{
			name:        "skip exhausted provider-full",
			cfgMax:      10,
			localActive: 0,
			hasSnapshot: true,
			snap:        fakecapacity.LiveSnapshot(model.BackendCodeBuild, fakecapacity.Full(5), fresh),
			failureMode: capacity.FailureModePassThrough,
			wantMax:     5,
			wantAvail:   false,
			wantReason:  "provider-full",
		},
		{
			name:        "skip exhausted via warm slots",
			cfgMax:      8,
			localActive: 0,
			hasSnapshot: true,
			snap: fakecapacity.LiveSnapshot(model.BackendLambda, fakecapacity.Detailed(
				4, 1, 1, 2,
			), fresh),
			failureMode: capacity.FailureModePassThrough,
			wantMax:     4,
			wantAvail:   false,
			wantReason:  "provider-full",
		},
		{
			name:        "live free slots cap remaining admits",
			cfgMax:      10,
			localActive: 1,
			hasSnapshot: true,
			// free=2 → effective max = localActive + min(remaining, free) = 1+2 = 3
			snap:        fakecapacity.LiveSnapshot(model.BackendCodeBuild, fakecapacity.Free(10, 2), fresh),
			failureMode: capacity.FailureModePassThrough,
			wantMax:     3,
			wantAvail:   true,
			wantReason:  "live",
		},
		{
			name:        "config ceiling below provider max",
			cfgMax:      3,
			localActive: 0,
			hasSnapshot: true,
			snap:        fakecapacity.LiveSnapshot(model.BackendARC, fakecapacity.Free(100, 100), fresh),
			failureMode: capacity.FailureModePassThrough,
			wantMax:     3,
			wantAvail:   true,
			wantReason:  "live",
		},
		{
			name:        "provider ceiling below config max",
			cfgMax:      10,
			localActive: 2,
			hasSnapshot: true,
			snap:        fakecapacity.LiveSnapshot(model.BackendCodeBuild, fakecapacity.Free(2, 2), fresh),
			failureMode: capacity.FailureModePassThrough,
			wantMax:     2,
			wantAvail:   false,
			wantReason:  "provider-ceiling",
		},
		{
			name:        "stale pass-through ignores live counters",
			cfgMax:      4,
			localActive: 0,
			hasSnapshot: true,
			snap:        fakecapacity.StaleSnapshot(model.BackendCodeBuild, fakecapacity.Full(1), aged),
			failureMode: capacity.FailureModePassThrough,
			wantMax:     4,
			wantAvail:   true,
			wantReason:  "stale-pass-through",
		},
		{
			name:        "stale block skips backend",
			cfgMax:      4,
			localActive: 0,
			hasSnapshot: true,
			snap:        fakecapacity.StaleSnapshot(model.BackendCodeBuild, fakecapacity.Free(4, 4), aged),
			failureMode: capacity.FailureModeBlock,
			wantMax:     4,
			wantAvail:   false,
			wantReason:  "capacity-stale",
		},
		{
			name:        "error pass-through uses local max",
			cfgMax:      4,
			localActive: 0,
			hasSnapshot: true,
			snap:        fakecapacity.ErrorSnapshot(model.BackendLambda, "probe timeout", nil, fresh),
			failureMode: capacity.FailureModePassThrough,
			wantMax:     4,
			wantAvail:   true,
			wantReason:  "stale-pass-through",
		},
		{
			name:        "error block skips backend",
			cfgMax:      4,
			localActive: 0,
			hasSnapshot: true,
			snap:        fakecapacity.ErrorSnapshot(model.BackendLambda, "probe timeout", nil, fresh),
			failureMode: capacity.FailureModeBlock,
			wantMax:     4,
			wantAvail:   false,
			wantReason:  "capacity-error",
		},
		{
			name:        "invalid max pass-through",
			cfgMax:      4,
			localActive: 0,
			hasSnapshot: true,
			snap:        fakecapacity.LiveSnapshot(model.BackendEC2, fakecapacity.InvalidMax(), fresh),
			failureMode: capacity.FailureModePassThrough,
			wantMax:     4,
			wantAvail:   true,
			wantReason:  "stale-pass-through",
		},
		{
			name:        "invalid max block",
			cfgMax:      4,
			localActive: 0,
			hasSnapshot: true,
			snap:        fakecapacity.LiveSnapshot(model.BackendEC2, fakecapacity.InvalidMax(), fresh),
			failureMode: capacity.FailureModeBlock,
			wantMax:     4,
			wantAvail:   false,
			wantReason:  "capacity-invalid",
		},
		{
			name:        "error snapshot preserves last-good under pass-through",
			cfgMax:      6,
			localActive: 0,
			hasSnapshot: true,
			// MaxRunners set from last good reading but Source=error still staleOrError.
			snap: func() capacity.Snapshot {
				prev := fakecapacity.Free(6, 3)
				return fakecapacity.ErrorSnapshot(model.BackendCodeBuild, "upstream 503", &prev, fresh)
			}(),
			failureMode: capacity.FailureModePassThrough,
			wantMax:     6,
			wantAvail:   true,
			wantReason:  "stale-pass-through",
		},
		{
			name:        "block-stale alias",
			cfgMax:      4,
			localActive: 0,
			hasSnapshot: true,
			snap:        fakecapacity.StaleSnapshot(model.BackendCodeBuild, fakecapacity.Free(4, 1), aged),
			failureMode: "block-stale",
			wantMax:     4,
			wantAvail:   false,
			wantReason:  "capacity-stale",
		},
		{
			name:        "fail-closed alias",
			cfgMax:      4,
			localActive: 0,
			hasSnapshot: true,
			snap:        fakecapacity.ErrorSnapshot(model.BackendCodeBuild, "boom", nil, fresh),
			failureMode: "fail-closed",
			wantMax:     4,
			wantAvail:   false,
			wantReason:  "capacity-error",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gotMax, gotAvail, gotReason := capacity.EffectiveMaxRunners(
				tc.cfgMax,
				tc.localActive,
				tc.snap,
				tc.hasSnapshot,
				tc.failureMode,
			)
			if gotMax != tc.wantMax || gotAvail != tc.wantAvail || gotReason != tc.wantReason {
				t.Fatalf(
					"EffectiveMaxRunners() = max=%d available=%v reason=%q; want max=%d available=%v reason=%q (snap=%+v)",
					gotMax, gotAvail, gotReason, tc.wantMax, tc.wantAvail, tc.wantReason, tc.snap,
				)
			}
		})
	}
}

func TestDecisionMatrixSeedManagerFixtures(t *testing.T) {
	// Smoke: fixtures seed a manager the same way allocation tests do, without
	// cloud credentials.
	manager := capacity.NewManager()
	now := time.Now().UTC()
	fakecapacity.SeedManager(manager,
		fakecapacity.LiveSnapshot(model.BackendCodeBuild, fakecapacity.Full(2), now),
		fakecapacity.LiveSnapshot(model.BackendARC, fakecapacity.Free(4, 2), now),
	)

	full, ok := manager.Get(model.BackendCodeBuild)
	if !ok || backend.FreeSlots(full.Status) != 0 {
		t.Fatalf("expected codebuild full snapshot, got %+v ok=%v", full, ok)
	}
	free, ok := manager.Get(model.BackendARC)
	if !ok || backend.FreeSlots(free.Status) != 2 {
		t.Fatalf("expected arc free=2, got %+v ok=%v", free, ok)
	}
}

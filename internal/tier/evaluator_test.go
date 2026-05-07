package tier

import (
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestEvaluateUsageThresholds(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		used float64
		want string
	}{
		{name: "healthy", used: 70, want: StateHealthy},
		{name: "approaching", used: 82, want: StateApproaching},
		{name: "exceeded", used: 96, want: StateExceeded},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := Evaluate(EvaluationInput{
				Pool:    model.PoolLite,
				Backend: model.BackendCodeBuild,
				Rule: Rule{
					SoftLimitRatio: 0.8,
					HardLimitRatio: 0.95,
					Action:         ActionDisable,
				},
				Snapshots: []SourceSnapshot{{
					Source:    "aws",
					Limit:     100,
					Used:      tc.used,
					UpdatedAt: now,
				}},
				Now: now,
			})
			if decision.State != tc.want {
				t.Fatalf("expected %s, got %+v", tc.want, decision)
			}
		})
	}
}

func TestEvaluateCreditThresholdIsMostRestrictive(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	decision := Evaluate(EvaluationInput{
		Pool:    model.PoolLite,
		Backend: model.BackendCloudRun,
		Rule: Rule{
			SoftLimitRatio:     0.8,
			HardLimitRatio:     0.95,
			MinRemainingCredit: 5,
			Action:             ActionDisable,
		},
		Snapshots: []SourceSnapshot{{
			Source:          "gcp",
			Limit:           100,
			Used:            10,
			RemainingCredit: 3,
			UpdatedAt:       now,
		}},
		Now: now,
	})
	if decision.State != StateExceeded {
		t.Fatalf("expected exceeded, got %+v", decision)
	}
}

func TestEvaluateBurnRateProjectionApproachesLimit(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	decision := Evaluate(EvaluationInput{
		Pool:    model.PoolLite,
		Backend: model.BackendLambda,
		Rule: Rule{
			SoftLimitRatio:   0.8,
			HardLimitRatio:   0.95,
			ProjectionWindow: time.Hour,
			Action:           ActionDeprioritize,
		},
		Snapshots: []SourceSnapshot{{
			Source:    "aws",
			Limit:     100,
			Used:      70,
			BurnRate:  0.01,
			UpdatedAt: now,
		}},
		Now: now,
	})
	if decision.State != StateApproaching {
		t.Fatalf("expected projected approaching state, got %+v", decision)
	}
}

func TestEvaluateUnknownOnSourceError(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	decision := Evaluate(EvaluationInput{
		Pool:    model.PoolLite,
		Backend: model.BackendGCE,
		Rule:    Rule{HardLimitRatio: 0.9},
		Snapshots: []SourceSnapshot{{
			Source:    "gcp",
			Err:       "api unavailable",
			UpdatedAt: now,
		}},
		Now: now,
	})
	if decision.State != StateUnknown {
		t.Fatalf("expected unknown, got %+v", decision)
	}
}

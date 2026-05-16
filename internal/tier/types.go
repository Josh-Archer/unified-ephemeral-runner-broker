package tier

import (
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const (
	StateHealthy     = "healthy"
	StateApproaching = "approaching"
	StateExceeded    = "exceeded"
	StateUnknown     = "unknown"

	ActionObserveOnly  = "observe-only"
	ActionDeprioritize = "deprioritize"
	ActionDisable      = "disable"

	FailureModePassThrough = "pass-through-round-robin"
	FailureModeBlock       = "block"
	FailureModeFallback    = "fallback-backends"

	CombineMostRestrictive = "most-restrictive"
)

type Decision struct {
	Pool      model.PoolName
	Backend   model.BackendName
	State     string
	Action    string
	Reason    string
	UpdatedAt time.Time
	Stale     bool
}

type SourceSnapshot struct {
	Source          string
	Limit           float64
	Used            float64
	BurnRate        float64
	RemainingCredit float64
	WindowEnd       time.Time
	UpdatedAt       time.Time
	Err             string
}

type Rule struct {
	Name               string
	Action             string
	SoftLimitRatio     float64
	HardLimitRatio     float64
	MinRemainingCredit float64
	ProjectionWindow   time.Duration
	Combine            string
}

type EvaluationInput struct {
	Pool      model.PoolName
	Backend   model.BackendName
	Rule      Rule
	Snapshots []SourceSnapshot
	Now       time.Time
}

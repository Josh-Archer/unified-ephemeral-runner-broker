package tier

import (
	"fmt"
	"math"
	"strings"
	"time"
)

func Evaluate(input EvaluationInput) Decision {
	now := input.Now
	if now.IsZero() {
		now = time.Now()
	}
	rule := normalizeRule(input.Rule)
	decision := Decision{
		Pool:      input.Pool,
		Backend:   input.Backend,
		State:     StateHealthy,
		Action:    rule.Action,
		UpdatedAt: now,
	}
	if len(input.Snapshots) == 0 {
		decision.State = StateUnknown
		decision.Reason = "no tier data"
		return decision
	}

	state := StateHealthy
	reasons := make([]string, 0)
	usable := 0
	for _, snapshot := range input.Snapshots {
		if strings.TrimSpace(snapshot.Err) != "" {
			decision.State = StateUnknown
			decision.Reason = fmt.Sprintf("%s: %s", snapshot.Source, snapshot.Err)
			return decision
		}
		if snapshot.UpdatedAt.IsZero() {
			decision.State = StateUnknown
			decision.Reason = fmt.Sprintf("%s: missing update timestamp", snapshot.Source)
			return decision
		}
		usable++
		sourceState, reason := evaluateSnapshot(rule, snapshot, now)
		if moreRestrictive(sourceState, state) {
			state = sourceState
		}
		if reason != "" {
			reasons = append(reasons, reason)
		}
	}
	if usable == 0 {
		decision.State = StateUnknown
		decision.Reason = "no usable tier data"
		return decision
	}
	decision.State = state
	decision.Reason = strings.Join(reasons, "; ")
	return decision
}

func evaluateSnapshot(rule Rule, snapshot SourceSnapshot, now time.Time) (string, string) {
	state := StateHealthy
	reasons := make([]string, 0)
	if snapshot.Limit > 0 {
		ratio := snapshot.Used / snapshot.Limit
		if rule.HardLimitRatio > 0 && ratio >= rule.HardLimitRatio {
			state = StateExceeded
		} else if rule.SoftLimitRatio > 0 && ratio >= rule.SoftLimitRatio {
			state = StateApproaching
		}
		reasons = append(reasons, fmt.Sprintf("%s usage %.4g/%.4g", snapshot.Source, snapshot.Used, snapshot.Limit))
		if projected, ok := projectedUsage(rule, snapshot, now); ok {
			projectedRatio := projected / snapshot.Limit
			if rule.HardLimitRatio > 0 && projectedRatio >= rule.HardLimitRatio && state != StateExceeded {
				state = StateApproaching
				reasons = append(reasons, fmt.Sprintf("%s projected usage %.4g/%.4g", snapshot.Source, projected, snapshot.Limit))
			} else if rule.SoftLimitRatio > 0 && projectedRatio >= rule.SoftLimitRatio && state == StateHealthy {
				state = StateApproaching
				reasons = append(reasons, fmt.Sprintf("%s projected usage %.4g/%.4g", snapshot.Source, projected, snapshot.Limit))
			}
		}
	}
	if rule.MinRemainingCredit > 0 {
		if snapshot.RemainingCredit < rule.MinRemainingCredit {
			state = StateExceeded
			reasons = append(reasons, fmt.Sprintf("%s remaining credit %.4g below %.4g", snapshot.Source, snapshot.RemainingCredit, rule.MinRemainingCredit))
		}
	}
	if snapshot.Limit <= 0 && rule.MinRemainingCredit <= 0 {
		return StateUnknown, fmt.Sprintf("%s: no limit or credit threshold", snapshot.Source)
	}
	if math.IsNaN(snapshot.Used) || math.IsNaN(snapshot.Limit) || math.IsNaN(snapshot.BurnRate) || math.IsNaN(snapshot.RemainingCredit) {
		return StateUnknown, fmt.Sprintf("%s: invalid numeric value", snapshot.Source)
	}
	return state, strings.Join(reasons, ", ")
}

func projectedUsage(rule Rule, snapshot SourceSnapshot, now time.Time) (float64, bool) {
	if snapshot.BurnRate <= 0 {
		return 0, false
	}
	window := rule.ProjectionWindow
	if window <= 0 && !snapshot.WindowEnd.IsZero() && snapshot.WindowEnd.After(now) {
		window = snapshot.WindowEnd.Sub(now)
	}
	if window <= 0 {
		return 0, false
	}
	return snapshot.Used + snapshot.BurnRate*window.Seconds(), true
}

func normalizeRule(rule Rule) Rule {
	if rule.Action == "" {
		rule.Action = ActionDisable
	}
	if rule.Combine == "" {
		rule.Combine = CombineMostRestrictive
	}
	return rule
}

func moreRestrictive(candidate, current string) bool {
	return restrictiveness(candidate) > restrictiveness(current)
}

func restrictiveness(state string) int {
	switch state {
	case StateExceeded:
		return 3
	case StateApproaching:
		return 2
	case StateUnknown:
		return 1
	default:
		return 0
	}
}

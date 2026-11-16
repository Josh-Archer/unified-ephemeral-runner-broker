package api

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/capacity"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

// BackendCapacitySummary captures the real-time capacity and admission state of a backend.
type BackendCapacitySummary struct {
	AvailableSlots    int `json:"available_slots"`
	ActiveAllocations int `json:"active_allocations"`
	MaxConcurrency    int `json:"max_concurrency"`
	// Status is one of "available", "exhausted", "rate-limited",
	// "circuit-open", "quarantined", "tier-blocked", "budget-exceeded",
	// "disabled", "unhealthy", or a live-capacity reason such as
	// "provider-full" or "node-unavailable".
	Status string `json:"status"`
	// Reason says which limit or rule produced Status, for example
	// "maxRunners(1/1)", "crossPoolMaxRunners(10/10)", a tier rule message, or
	// the failure class that opened the circuit. Empty when available.
	Reason string `json:"reason,omitempty"`
	// CircuitState is "closed", "open" or "half-open" when the backend has a
	// circuit breaker; omitted otherwise.
	CircuitState string `json:"circuit_state,omitempty"`
	// RetryAt is when the broker will next consider the backend (circuit probe,
	// rate-limit window reset, or end of a registration quarantine).
	RetryAt *time.Time `json:"retry_at,omitempty"`
}

// AllocationError represents a rich error payload with capacity telemetry and suggested remediation.
type AllocationError struct {
	Err              error                             `json:"-"`
	ErrorCode        string                            `json:"error"`
	Message          string                            `json:"message"`
	Pool             model.PoolName                    `json:"pool,omitempty"`
	RequestedBackend string                            `json:"requested_backend,omitempty"`
	CapacitySnapshot map[string]BackendCapacitySummary `json:"capacity_snapshot,omitempty"`
	SuggestedAction  string                            `json:"suggested_action,omitempty"`
}

func (e *AllocationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.ErrorCode
}

func (e *AllocationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// BuildCapacitySnapshot computes the available and active capacity across backends in a pool.
func (s *Service) BuildCapacitySnapshot(pool model.PoolConfig, request model.AllocationRequest) map[string]BackendCapacitySummary {
	if s == nil || len(pool.Backends) == 0 {
		return nil
	}
	snapshot := make(map[string]BackendCapacitySummary, len(pool.Backends))

	sched := s.scheds.ForPool(pool)
	now := s.now()
	registrationEligible := s.filterRegistrationPenalized(pool, request)

	for name, backendCfg := range pool.Backends {
		nameStr := string(name)
		active := 0
		if sched != nil {
			active = sched.Active(pool.Name, name)
		}
		maxRunners := backendCfg.MaxRunners
		avail := maxRunners - active
		if avail < 0 {
			avail = 0
		}

		status := "available"
		reason := ""
		circuitState := ""
		var retryAt *time.Time
		if s.admission != nil && backendCfg.CircuitBreaker.Enabled {
			circuitState = s.admission.circuitDetail(pool.Name, name).State
		}
		if !backendCfg.Enabled {
			status = "disabled"
			reason = "enabled: false in broker config"
			avail = 0
		} else if !backendCfg.Healthy {
			status = "unhealthy"
			reason = "marked unhealthy by the broker"
			avail = 0
		} else if s.admission != nil {
			decision := s.admission.allow(pool.Name, name, backendCfg, now, false, false)
			if !decision.Allowed {
				switch decision.Reason {
				case "rate-limited":
					status = "rate-limited"
					used, permits, resetAt := s.admission.rateLimitDetail(pool.Name, name, backendCfg.RateLimit)
					reason = fmt.Sprintf("rateLimit(%d/%d per %s)", used, permits, backendCfg.RateLimit.Interval)
					if !resetAt.IsZero() {
						retryAt = &resetAt
					}
				case "circuit-open":
					status = "circuit-open"
					detail := s.admission.circuitDetail(pool.Name, name)
					reason = "switched off after backend failures"
					if detail.LastTransitionReason != "" {
						reason += " (" + detail.LastTransitionReason + ")"
					}
					if !detail.NextProbeAt.IsZero() {
						next := detail.NextProbeAt
						retryAt = &next
					}
				default:
					status = decision.Reason
				}
				avail = 0
			}
		}

		if _, eligible := registrationEligible.Backends[name]; status == "available" && !eligible {
			if until, penalized := s.registrationPenaltyUntil(pool.Name, name, now); penalized {
				status = "quarantined"
				reason = "runners never registered; skipped while another backend can take the job"
				retryAt = &until
				avail = 0
			}
		}

		if status == "available" && s.store != nil {
			if backendCfg.CrossPoolMaxRunners > 0 {
				shared := s.store.CountBackendActive(name)
				if shared >= backendCfg.CrossPoolMaxRunners {
					status = "exhausted"
					reason = fmt.Sprintf("crossPoolMaxRunners(%d/%d)", shared, backendCfg.CrossPoolMaxRunners)
					avail = 0
				} else if room := backendCfg.CrossPoolMaxRunners - shared; room < avail {
					avail = room
				}
			}
		}
		if status == "available" && s.store != nil {
			if group := model.ResolveCapacityGroup(s.cfg, name); group.Cap > 0 {
				used := s.capacityGroupOccupancy(group.Members)
				if used >= group.Cap {
					status = "exhausted"
					reason = fmt.Sprintf("capacityGroup(%d/%d)", used, group.Cap)
					avail = 0
				} else if room := group.Cap - used; room < avail {
					avail = room
				}
			}
		}

		if status == "available" {
			if s.tierMgr != nil {
				if decision, ok := s.tierMgr.Decision(pool.Name, name); ok {
					if decision.State == tier.StateExceeded || (decision.State == tier.StateApproaching && decision.Action == tier.ActionDisable) {
						status = "tier-blocked"
						reason = decision.Reason
						avail = 0
					}
				}
			}
		}

		if status == "available" {
			if s.budgets != nil && backendCfg.Budget.Enabled {
				decision := s.budgets.Allow(pool.Name, name, backendCfg.Budget, now)
				if !decision.Allowed {
					status = "budget-exceeded"
					reason = decision.Reason
					avail = 0
				}
			}
		}

		if status == "available" && s.capacityMgr != nil {
			if snap, ok := s.capacityMgr.Get(name); ok {
				failureMode := s.cfg.Broker.LiveCapacity.FailureMode
				effMax, okAvail, liveReason := capacity.EffectiveMaxRunners(maxRunners, active, snap, true, failureMode)
				if !okAvail {
					status = liveReason
					if status == "" || status == "local-full" {
						status = "exhausted"
					} else {
						reason = "live capacity: " + liveReason
					}
					avail = 0
				} else {
					liveAvail := effMax - active
					if liveAvail < avail {
						avail = liveAvail
					}
					if !snap.Stale && snap.Err == "" && snap.Source != "error" {
						free := backend.FreeSlots(snap.Status)
						if free < avail {
							avail = free
						}
					}
				}
			}
		}

		if status == "available" && avail <= 0 {
			status = "exhausted"
		}
		if status == "exhausted" && reason == "" {
			reason = fmt.Sprintf("maxRunners(%d/%d)", active, maxRunners)
		}

		snapshot[nameStr] = BackendCapacitySummary{
			AvailableSlots:    avail,
			ActiveAllocations: active,
			MaxConcurrency:    maxRunners,
			Status:            status,
			Reason:            reason,
			CircuitState:      circuitState,
			RetryAt:           retryAt,
		}
	}

	return snapshot
}

// FormatSuggestedAction generates a helpful remediation tip based on available capacity.
func FormatSuggestedAction(requestedBackend *model.BackendName, snapshot map[string]BackendCapacitySummary) string {
	var availableBackends []string
	for name, summary := range snapshot {
		if summary.Status == "available" && summary.AvailableSlots > 0 {
			availableBackends = append(availableBackends, name)
		}
	}
	sort.Strings(availableBackends)

	if len(availableBackends) > 0 {
		availList := strings.Join(availableBackends, ", ")
		if requestedBackend != nil && *requestedBackend != "" {
			return fmt.Sprintf("Pinned backend %q is unavailable. Retry with backend=auto or fallback to an available backend: [%s].", *requestedBackend, availList)
		}
		return fmt.Sprintf("Retry with backend=auto or select an available backend: [%s].", availList)
	}

	return "All pool backends are currently exhausted or blocked. Wait for in-flight jobs to complete or check cluster capacity."
}

func (s *Service) wrapAllocationError(rawPool model.PoolConfig, request model.AllocationRequest, err error) error {
	if err == nil {
		return nil
	}
	var existing *AllocationError
	if errors.As(err, &existing) {
		return err
	}

	pool := rawPool
	if len(pool.Backends) == 0 && request.Pool != "" {
		if resolved, resolveErr := s.resolvePool(request.Pool); resolveErr == nil {
			pool = resolved
		}
	}

	snapshot := s.BuildCapacitySnapshot(pool, request)
	suggestedAction := FormatSuggestedAction(request.Backend, snapshot)

	errorCode := "allocation_failed"
	switch {
	case errors.Is(err, model.ErrUnknownSizeClass):
		errorCode = "unknown_size_class"
		suggestedAction = "Use a size class defined in broker.sizeClasses, or omit the size-* label to keep the default pod shape."
	case errors.Is(err, ErrBackendLiveCapacity), errors.Is(err, scheduler.ErrNoCapacity):
		errorCode = "capacity_exhausted"
	case errors.Is(err, ErrBackendRateLimited):
		errorCode = "rate_limited"
	case errors.Is(err, ErrBackendCircuitOpen):
		errorCode = "circuit_open"
	case errors.Is(err, ErrBackendTierBlocked):
		errorCode = "tier_blocked"
	case errors.Is(err, ErrBackendBudgetExceeded):
		errorCode = "budget_exceeded"
	case errors.Is(err, ErrNoMatchingBackendCapabilities):
		errorCode = "no_matching_capabilities"
	case errors.Is(err, ErrUnknownPool):
		errorCode = "unknown_pool"
	}

	requestedBackend := ""
	if request.Backend != nil {
		requestedBackend = string(*request.Backend)
	}

	return &AllocationError{
		Err:              err,
		ErrorCode:        errorCode,
		Message:          err.Error(),
		Pool:             request.Pool,
		RequestedBackend: requestedBackend,
		CapacitySnapshot: snapshot,
		SuggestedAction:  suggestedAction,
	}
}

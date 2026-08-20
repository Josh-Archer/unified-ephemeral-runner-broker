package api

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/capacity"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

// BackendCapacitySummary captures the real-time capacity and admission state of a backend.
type BackendCapacitySummary struct {
	AvailableSlots    int    `json:"available_slots"`
	ActiveAllocations int    `json:"active_allocations"`
	MaxConcurrency    int    `json:"max_concurrency"`
	Status            string `json:"status"` // "available", "exhausted", "rate-limited", "circuit-open", "tier-blocked", "budget-exceeded", "disabled", "unhealthy"
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
		if !backendCfg.Enabled {
			status = "disabled"
			avail = 0
		} else if !backendCfg.Healthy {
			status = "unhealthy"
			avail = 0
		} else if s.admission != nil {
			decision := s.admission.allow(pool.Name, name, backendCfg, now, false, false)
			if !decision.Allowed {
				if decision.Reason == "rate-limited" {
					status = "rate-limited"
				} else if decision.Reason == "circuit-open" {
					status = "circuit-open"
				} else {
					status = decision.Reason
				}
				avail = 0
			}
		}

		if status == "available" {
			if s.tierMgr != nil {
				if decision, ok := s.tierMgr.Decision(pool.Name, name); ok {
					if decision.State == tier.StateExceeded || (decision.State == tier.StateApproaching && decision.Action == tier.ActionDisable) {
						status = "tier-blocked"
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
					avail = 0
				}
			}
		}

		if status == "available" && s.capacityMgr != nil {
			if snap, ok := s.capacityMgr.Get(name); ok {
				failureMode := s.cfg.Broker.LiveCapacity.FailureMode
				effMax, okAvail, reason := capacity.EffectiveMaxRunners(maxRunners, active, snap, true, failureMode)
				if !okAvail {
					status = reason
					if status == "" || status == "local-full" {
						status = "exhausted"
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

		snapshot[nameStr] = BackendCapacitySummary{
			AvailableSlots:    avail,
			ActiveAllocations: active,
			MaxConcurrency:    maxRunners,
			Status:            status,
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

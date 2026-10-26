package api

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Compile-time checks: both implementations must satisfy Observer.
var (
	_ Observer = noopObserver{}
	_ Observer = (*PrometheusObserver)(nil)
)

func TestNoopObserver(t *testing.T) {
	var observer Observer = noopObserver{}
	observer.ObserveAllocationStart(model.PoolFull)
	observer.ObserveAllocationResult(model.PoolFull, model.BackendARC, "success", time.Second)
	observer.ObserveLaunchLatency(model.PoolFull, model.BackendARC, "cold", time.Second)
	observer.ObserveRegistrationLatency(model.PoolFull, model.BackendARC, time.Second)
	observer.ObserveFinalization(model.PoolFull, model.BackendARC, "completed", time.Second)
	observer.ObserveCancellation(model.PoolFull, model.BackendARC, "success", time.Second)
	observer.ObserveActiveAllocations(nil)
	observer.ObserveActiveAllocations([]model.AllocationStatus{{Pool: model.PoolFull, State: model.StateReady}})
	observer.ObserveCapacity(model.BrokerConfig{}, nil)
	observer.ObserveBackendCircuitState(nil)
	observer.ObserveBackendCircuitTransition(model.PoolFull, model.BackendARC, circuitStateClosed, circuitStateOpen, "timeout")
	observer.ObserveBackendAdmissionRejected(model.PoolFull, model.BackendARC, "circuit-open")
	observer.ObserveBackendProbe(model.PoolFull, model.BackendARC, "success")
	observer.ObserveTierState(nil)
	observer.ObserveTierFallback(model.PoolFull, "deprioritize", "approaching")
	observer.ObserveTierBlocked(model.PoolFull, model.BackendARC, "exceeded")
	observer.ObserveLiveCapacityState(nil)
	observer.ObserveLiveCapacityDecision(model.PoolFull, model.BackendARC, "provider-full", 0)
	observer.ObserveBudgetBlocked(model.PoolFull, model.BackendARC, "daily")
	observer.ObserveBudgetUsage(model.PoolFull, model.BackendARC, 1, 2, 3, 4)
	observer.ObserveQualitySelection(model.PoolFull, model.BackendARC, "higher-score", 0.5)
	observer.ObserveQualityScore(model.PoolFull, model.BackendARC, 0.5, 1, time.Second, 0, 1)
	observer.ObserveAllocationReaped(model.PoolFull, model.BackendARC, "expired")
	observer.ObserveAllocationExpired(model.PoolFull, model.BackendARC, "unregistered")
	observer.ObserveOrphanCleanup(model.PoolFull, model.BackendARC, orphanActionExpired)
	observer.ObserveLabelGarbage(model.PoolFull, model.BackendARC, labelGarbageReclaimed)
	observer.ObserveStaleRunnerLabels(nil)
	observer.ObserveProcessLocalStateLoss(store.TypeMemory)
	observer.ObserveRestartOrphans(model.BackendARC, orphanReasonCapacityGap, 1)
}

func TestPrometheusObserverRegister(t *testing.T) {
	reg := prometheus.NewRegistry()
	obs := NewPrometheusObserver(reg)
	if obs == nil {
		t.Fatal("NewPrometheusObserver returned nil")
	}
	if err := obs.Register(reg); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := obs.Register(reg)
	if !isAlreadyRegistered(err) {
		t.Fatalf("second Register = %v, want AlreadyRegisteredError", err)
	}

	other := NewPrometheusObserver(reg)
	err = other.Register(reg)
	if !isAlreadyRegistered(err) {
		t.Fatalf("second observer Register = %v, want AlreadyRegisteredError", err)
	}
}

func TestPrometheusObserverRegisterNilUsesDefaultRegisterer(t *testing.T) {
	reg := prometheus.NewRegistry()
	previousRegisterer := prometheus.DefaultRegisterer
	previousGatherer := prometheus.DefaultGatherer
	prometheus.DefaultRegisterer = reg
	prometheus.DefaultGatherer = reg
	t.Cleanup(func() {
		prometheus.DefaultRegisterer = previousRegisterer
		prometheus.DefaultGatherer = previousGatherer
	})

	obs := NewPrometheusObserver(nil)
	if err := obs.Register(nil); err != nil {
		t.Fatalf("Register(nil): %v", err)
	}
	if err := obs.Register(nil); !isAlreadyRegistered(err) {
		t.Fatalf("second Register(nil) = %v, want AlreadyRegisteredError", err)
	}
}

func TestPrometheusObserverAllocationMetrics(t *testing.T) {
	obs, reg := newRegisteredObserver(t)
	latency := 500 * time.Millisecond

	obs.ObserveAllocationStart(model.PoolFull)
	obs.ObserveAllocationStart(model.PoolFull)
	obs.ObserveAllocationStart(model.PoolLite)
	requireFloat(t, obs.queueDepth.WithLabelValues("full", "reserved"), 2)
	requireFloat(t, obs.queueDepth.WithLabelValues("lite", "reserved"), 1)

	obs.ObserveAllocationResult(model.PoolFull, model.BackendARC, "success", latency)
	obs.ObserveAllocationResult(model.PoolFull, model.BackendARC, "success", latency)
	obs.ObserveAllocationResult(model.PoolFull, model.BackendARC, "error", latency)
	requireFloat(t, obs.allocations.WithLabelValues("full", "arc", "success"), 2)
	requireFloat(t, obs.allocations.WithLabelValues("full", "arc", "error"), 1)
	compareRegistered(t, reg, obs.allocations, "uecb_allocations_total", expectFamily(
		"uecb_allocations_total",
		"Allocation attempts by pool, backend, and result.",
		"counter",
		metricSeries{`backend="arc",pool="full",result="error"`, 1},
		metricSeries{`backend="arc",pool="full",result="success"`, 2},
	))
	compareRegistered(t, reg, obs.allocationLatency, "uecb_allocation_latency_seconds", expectHistogram(
		"uecb_allocation_latency_seconds",
		"End-to-end allocation latency from broker admission through backend provisioning.",
		histPoint{`backend="arc",pool="full",result="success"`, []float64{0.5, 0.5}},
		histPoint{`backend="arc",pool="full",result="error"`, []float64{0.5}},
	))

	obs.ObserveLaunchLatency(model.PoolFull, model.BackendARC, "cold", latency)
	obs.ObserveLaunchLatency(model.PoolLite, model.BackendLambda, "warm", latency)
	compareRegistered(t, reg, obs.launchLatency, "uecb_launch_latency_seconds", expectHistogram(
		"uecb_launch_latency_seconds",
		"Backend launch latency for a selected ephemeral runner.",
		histPoint{`backend="arc",launch_mode="cold",pool="full"`, []float64{0.5}},
		histPoint{`backend="lambda",launch_mode="warm",pool="lite"`, []float64{0.5}},
	))

	obs.ObserveRegistrationLatency(model.PoolFull, model.BackendARC, latency)
	compareRegistered(t, reg, obs.registrationLatency, "uecb_registration_latency_seconds", expectHistogram(
		"uecb_registration_latency_seconds",
		"Observed latency until a provisioned runner registration response is available to the broker.",
		histPoint{`backend="arc",pool="full"`, []float64{0.5}},
	))

	obs.ObserveFinalization(model.PoolFull, model.BackendARC, "completed", latency)
	obs.ObserveFinalization(model.PoolFull, model.BackendARC, "", latency)
	requireFloat(t, obs.finalizations.WithLabelValues("full", "arc", "completed"), 1)
	requireFloat(t, obs.finalizations.WithLabelValues("full", "arc", "unknown"), 1)
	compareRegistered(t, reg, obs.finalizations, "uecb_finalizations_total", expectFamily(
		"uecb_finalizations_total",
		"Allocation finalize callbacks by pool, backend, and result (completed, failed, error).",
		"counter",
		metricSeries{`backend="arc",pool="full",result="completed"`, 1},
		metricSeries{`backend="arc",pool="full",result="unknown"`, 1},
	))
	compareRegistered(t, reg, obs.finalizationLatency, "uecb_finalization_latency_seconds", expectHistogram(
		"uecb_finalization_latency_seconds",
		"Latency of allocation finalize (complete/fail) callbacks.",
		histPoint{`backend="arc",pool="full",result="completed"`, []float64{0.5}},
		histPoint{`backend="arc",pool="full",result="unknown"`, []float64{0.5}},
	))

	obs.ObserveCancellation(model.PoolFull, model.BackendARC, "success", latency)
	obs.ObserveCancellation(model.PoolLite, model.BackendLambda, "", latency)
	requireFloat(t, obs.cancellations.WithLabelValues("full", "arc", "success"), 1)
	requireFloat(t, obs.cancellations.WithLabelValues("lite", "lambda", "unknown"), 1)
	compareRegistered(t, reg, obs.cancellations, "uecb_cancellations_total", expectFamily(
		"uecb_cancellations_total",
		"Allocation cancel requests by pool, backend, and result.",
		"counter",
		metricSeries{`backend="arc",pool="full",result="success"`, 1},
		metricSeries{`backend="lambda",pool="lite",result="unknown"`, 1},
	))
	compareRegistered(t, reg, obs.cancellationLatency, "uecb_cancellation_latency_seconds", expectHistogram(
		"uecb_cancellation_latency_seconds",
		"Latency of allocation cancel requests.",
		histPoint{`backend="arc",pool="full",result="success"`, []float64{0.5}},
		histPoint{`backend="lambda",pool="lite",result="unknown"`, []float64{0.5}},
	))

	obs.ObserveAllocationExpired(model.PoolFull, model.BackendARC, "unregistered")
	obs.ObserveAllocationExpired(model.PoolFull, model.BackendARC, "job_timeout")
	obs.ObserveAllocationExpired(model.PoolFull, model.BackendARC, "job_timeout")
	obs.ObserveAllocationExpired(model.PoolLite, model.BackendLambda, "")
	requireFloat(t, obs.allocationExpired.WithLabelValues("arc", "full", "job_timeout"), 2)
	requireFloat(t, obs.allocationExpired.WithLabelValues("lambda", "lite", "unknown"), 1)
	compareRegistered(t, reg, obs.allocationExpired, "uecb_allocation_expired_total", expectFamily(
		"uecb_allocation_expired_total",
		"Total allocations that expired, by backend, pool, and reason (unregistered, unclaimed, job_timeout, quarantine_ttl_elapsed).",
		"counter",
		metricSeries{`backend="arc",pool="full",reason="job_timeout"`, 2},
		metricSeries{`backend="arc",pool="full",reason="unregistered"`, 1},
		metricSeries{`backend="lambda",pool="lite",reason="unknown"`, 1},
	))

	obs.ObserveActiveAllocations([]model.AllocationStatus{
		{Pool: model.PoolFull, State: model.StateReady},
		{Pool: model.PoolFull, State: model.StateReady},
		{Pool: model.PoolLite, State: model.StatePending},
	})
	compareRegistered(t, reg, obs.queueDepth, "uecb_queue_depth", expectFamily(
		"uecb_queue_depth",
		"Current allocation count by state.",
		"gauge",
		metricSeries{`pool="full",state="ready"`, 2},
		metricSeries{`pool="lite",state="pending"`, 1},
	))
	requireFloat(t, obs.queueDepth.WithLabelValues("full", "ready"), 2)

	obs.ObserveActiveAllocations([]model.AllocationStatus{
		{Pool: model.PoolFull, State: model.StateReserved},
	})
	compareRegistered(t, reg, obs.queueDepth, "uecb_queue_depth", expectFamily(
		"uecb_queue_depth",
		"Current allocation count by state.",
		"gauge",
		metricSeries{`pool="full",state="reserved"`, 1},
	))
}

func TestPrometheusObserverCapacityResetsMissingSeries(t *testing.T) {
	obs, reg := newRegisteredObserver(t)
	obs.ObserveCapacity(model.BrokerConfig{Pools: []model.PoolConfig{
		{
			Name: model.PoolFull,
			Backends: map[model.BackendName]model.BackendConfig{
				model.BackendARC:    {Enabled: true, MaxRunners: 4},
				model.BackendLambda: {Enabled: true, MaxRunners: 2},
				model.BackendGCE:    {Enabled: false, MaxRunners: 4},
				model.BackendEC2:    {Enabled: true, MaxRunners: 0},
			},
		},
		{
			Name: model.PoolLite,
			Backends: map[model.BackendName]model.BackendConfig{
				model.BackendARC: {Enabled: true, MaxRunners: 4},
			},
		},
	}}, []model.AllocationStatus{
		{Pool: model.PoolFull, SelectedBackend: model.BackendARC, State: model.StateReady},
		{Pool: model.PoolFull, SelectedBackend: model.BackendARC, State: model.StateReserved},
		{Pool: model.PoolFull, SelectedBackend: model.BackendARC, State: model.StatePending},
		{Pool: model.PoolFull, SelectedBackend: model.BackendARC, State: model.StateWarm},
		{Pool: model.PoolFull, SelectedBackend: model.BackendLambda, State: model.StateReady},
		{Pool: model.PoolFull, SelectedBackend: model.BackendGCE, State: model.StateReady},
		{Pool: model.PoolLite, SelectedBackend: model.BackendARC, State: model.StateReady},
		{Pool: model.PoolLite, SelectedBackend: model.BackendARC, State: model.StateCompleted},
	})
	requireFloat(t, obs.capacityUtilization.WithLabelValues("full", "arc"), 0.5)
	requireFloat(t, obs.capacityUtilization.WithLabelValues("full", "lambda"), 0.5)
	requireFloat(t, obs.capacityUtilization.WithLabelValues("lite", "arc"), 0.25)
	compareRegistered(t, reg, obs.capacityUtilization, "uecb_capacity_utilization_ratio", expectFamily(
		"uecb_capacity_utilization_ratio",
		"Active allocations divided by configured backend capacity.",
		"gauge",
		metricSeries{`backend="arc",pool="full"`, 0.5},
		metricSeries{`backend="lambda",pool="full"`, 0.5},
		metricSeries{`backend="arc",pool="lite"`, 0.25},
	))

	obs.ObserveCapacity(model.BrokerConfig{Pools: []model.PoolConfig{
		{
			Name: model.PoolLite,
			Backends: map[model.BackendName]model.BackendConfig{
				model.BackendLambda: {Enabled: true, MaxRunners: 4},
			},
		},
	}}, []model.AllocationStatus{
		{Pool: model.PoolLite, SelectedBackend: model.BackendLambda, State: model.StateReserved},
	})
	requireFloat(t, obs.capacityUtilization.WithLabelValues("lite", "lambda"), 0.25)
	compareRegistered(t, reg, obs.capacityUtilization, "uecb_capacity_utilization_ratio", expectFamily(
		"uecb_capacity_utilization_ratio",
		"Active allocations divided by configured backend capacity.",
		"gauge",
		metricSeries{`backend="lambda",pool="lite"`, 0.25},
	))
}

func TestPrometheusObserverCircuitMetrics(t *testing.T) {
	obs, reg := newRegisteredObserver(t)
	obs.ObserveBackendCircuitState([]backendCircuitSnapshot{
		{Pool: model.PoolFull, Backend: model.BackendARC, State: circuitStateOpen},
		{Pool: model.PoolLite, Backend: model.BackendLambda, State: circuitStateHalfOpen},
	})
	requireFloat(t, obs.circuitState.WithLabelValues("full", "arc", circuitStateOpen), 1)
	requireFloat(t, obs.circuitState.WithLabelValues("full", "arc", circuitStateClosed), 0)
	requireFloat(t, obs.circuitState.WithLabelValues("lite", "lambda", circuitStateHalfOpen), 1)
	compareRegistered(t, reg, obs.circuitState, "uecb_backend_circuit_state", expectFamily(
		"uecb_backend_circuit_state",
		"Runtime backend circuit state. A value of 1 marks the active state for a pool/backend.",
		"gauge",
		append(
			circuitSeries(model.PoolFull, model.BackendARC, circuitStateOpen),
			circuitSeries(model.PoolLite, model.BackendLambda, circuitStateHalfOpen)...,
		)...,
	))

	obs.ObserveBackendCircuitState([]backendCircuitSnapshot{
		{Pool: model.PoolFull, Backend: model.BackendARC, State: circuitStateClosed},
	})
	requireFloat(t, obs.circuitState.WithLabelValues("full", "arc", circuitStateClosed), 1)
	compareRegistered(t, reg, obs.circuitState, "uecb_backend_circuit_state", expectFamily(
		"uecb_backend_circuit_state",
		"Runtime backend circuit state. A value of 1 marks the active state for a pool/backend.",
		"gauge",
		circuitSeries(model.PoolFull, model.BackendARC, circuitStateClosed)...,
	))

	obs.ObserveBackendCircuitTransition(model.PoolFull, model.BackendARC, circuitStateClosed, circuitStateOpen, "timeout")
	obs.ObserveBackendCircuitTransition(model.PoolFull, model.BackendARC, circuitStateClosed, circuitStateOpen, "timeout")
	requireFloat(t, obs.circuitTransitions.WithLabelValues("full", "arc", circuitStateClosed, circuitStateOpen, "timeout"), 2)
	compareRegistered(t, reg, obs.circuitTransitions, "uecb_backend_circuit_transitions_total", expectFamily(
		"uecb_backend_circuit_transitions_total",
		"Runtime backend circuit transitions by pool, backend, state, and reason.",
		"counter",
		metricSeries{`backend="arc",from="closed",pool="full",reason="timeout",to="open"`, 2},
	))

	obs.ObserveBackendAdmissionRejected(model.PoolFull, model.BackendARC, "rate-limited")
	requireFloat(t, obs.admissionRejections.WithLabelValues("full", "arc", "rate-limited"), 1)
	compareRegistered(t, reg, obs.admissionRejections, "uecb_backend_admission_rejections_total", expectFamily(
		"uecb_backend_admission_rejections_total",
		"Backend admission rejections before scheduler reservation.",
		"counter",
		metricSeries{`backend="arc",pool="full",reason="rate-limited"`, 1},
	))

	obs.ObserveBackendProbe(model.PoolFull, model.BackendARC, "failure")
	obs.ObserveBackendProbe(model.PoolFull, model.BackendARC, "success")
	requireFloat(t, obs.probeResults.WithLabelValues("full", "arc", "success"), 1)
	compareRegistered(t, reg, obs.probeResults, "uecb_backend_probe_results_total", expectFamily(
		"uecb_backend_probe_results_total",
		"Background backend recovery probe results.",
		"counter",
		metricSeries{`backend="arc",pool="full",result="failure"`, 1},
		metricSeries{`backend="arc",pool="full",result="success"`, 1},
	))
}

func TestPrometheusObserverTierMetrics(t *testing.T) {
	obs, reg := newRegisteredObserver(t)
	obs.ObserveTierState([]tierDecisionSnapshot{
		{Pool: model.PoolFull, Backend: model.BackendARC, State: "exceeded", Stale: true},
		{Pool: model.PoolLite, Backend: model.BackendLambda, State: "approaching", Stale: false},
	})
	requireFloat(t, obs.tierState.WithLabelValues("full", "arc", "exceeded", "true"), 1)
	requireFloat(t, obs.tierState.WithLabelValues("full", "arc", "healthy", "true"), 0)
	requireFloat(t, obs.tierState.WithLabelValues("lite", "lambda", "approaching", "false"), 1)
	compareRegistered(t, reg, obs.tierState, "uecb_tier_state", expectFamily(
		"uecb_tier_state",
		"Cached tier routing state. A value of 1 marks the active state for a pool/backend.",
		"gauge",
		append(
			tierSeries(model.PoolFull, model.BackendARC, "exceeded", "true"),
			tierSeries(model.PoolLite, model.BackendLambda, "approaching", "false")...,
		)...,
	))

	obs.ObserveTierState([]tierDecisionSnapshot{
		{Pool: model.PoolFull, Backend: model.BackendGCE, State: "healthy", Stale: false},
	})
	requireFloat(t, obs.tierState.WithLabelValues("full", "gce", "healthy", "false"), 1)
	compareRegistered(t, reg, obs.tierState, "uecb_tier_state", expectFamily(
		"uecb_tier_state",
		"Cached tier routing state. A value of 1 marks the active state for a pool/backend.",
		"gauge",
		tierSeries(model.PoolFull, model.BackendGCE, "healthy", "false")...,
	))

	obs.ObserveTierFallback(model.PoolFull, "deprioritize", "approaching")
	obs.ObserveTierFallback(model.PoolLite, "blocked", "all-backends-exceeded")
	requireFloat(t, obs.tierFallbacks.WithLabelValues("full", "deprioritize", "approaching"), 1)
	compareRegistered(t, reg, obs.tierFallbacks, "uecb_tier_fallback_total", expectFamily(
		"uecb_tier_fallback_total",
		"Tier routing fallback decisions by pool, mode, and reason.",
		"counter",
		metricSeries{`mode="blocked",pool="lite",reason="all-backends-exceeded"`, 1},
		metricSeries{`mode="deprioritize",pool="full",reason="approaching"`, 1},
	))

	obs.ObserveTierBlocked(model.PoolFull, model.BackendLambda, "exceeded")
	obs.ObserveTierBlocked(model.PoolFull, model.BackendLambda, "exceeded")
	requireFloat(t, obs.tierBlocked.WithLabelValues("full", "lambda", "exceeded"), 2)
	compareRegistered(t, reg, obs.tierBlocked, "uecb_tier_blocked_allocations_total", expectFamily(
		"uecb_tier_blocked_allocations_total",
		"Allocation attempts blocked by tier routing.",
		"counter",
		metricSeries{`backend="lambda",pool="full",reason="exceeded"`, 2},
	))
}

func TestPrometheusObserverLiveCapacityMetrics(t *testing.T) {
	obs, reg := newRegisteredObserver(t)
	obs.ObserveLiveCapacityState([]liveCapacitySnapshot{
		{Backend: model.BackendARC, Free: 3, Max: 10, Stale: false, Source: "provider"},
		{Backend: model.BackendLambda, Free: 0, Max: 4, Stale: true, Source: "provider", Err: true},
		{Backend: model.BackendGCE, Free: 1, Max: 2, Stale: false, Source: ""},
	})
	requireFloat(t, obs.liveCapacityFree.WithLabelValues("arc", "provider"), 3)
	requireFloat(t, obs.liveCapacityFree.WithLabelValues("lambda", "error"), 0)
	requireFloat(t, obs.liveCapacityFree.WithLabelValues("gce", "unknown"), 1)
	requireFloat(t, obs.liveCapacityStale.WithLabelValues("arc"), 0)
	requireFloat(t, obs.liveCapacityStale.WithLabelValues("lambda"), 1)
	compareRegistered(t, reg, obs.liveCapacityFree, "uecb_live_capacity_free_slots", expectFamily(
		"uecb_live_capacity_free_slots",
		"Cached provider-reported free runner slots per backend.",
		"gauge",
		metricSeries{`backend="arc",source="provider"`, 3},
		metricSeries{`backend="gce",source="unknown"`, 1},
		metricSeries{`backend="lambda",source="error"`, 0},
	))
	compareRegistered(t, reg, obs.liveCapacityStale, "uecb_live_capacity_stale", expectFamily(
		"uecb_live_capacity_stale",
		"Whether the cached live capacity reading for a backend is stale (1) or fresh (0).",
		"gauge",
		metricSeries{`backend="arc"`, 0},
		metricSeries{`backend="gce"`, 0},
		metricSeries{`backend="lambda"`, 1},
	))

	obs.ObserveLiveCapacityState([]liveCapacitySnapshot{
		{Backend: model.BackendARC, Free: 4, Max: 10, Stale: false, Source: "provider"},
	})
	requireFloat(t, obs.liveCapacityFree.WithLabelValues("arc", "provider"), 4)
	requireFloat(t, obs.liveCapacityStale.WithLabelValues("arc"), 0)
	compareRegistered(t, reg, obs.liveCapacityFree, "uecb_live_capacity_free_slots", expectFamily(
		"uecb_live_capacity_free_slots",
		"Cached provider-reported free runner slots per backend.",
		"gauge",
		metricSeries{`backend="arc",source="provider"`, 4},
	))
	compareRegistered(t, reg, obs.liveCapacityStale, "uecb_live_capacity_stale", expectFamily(
		"uecb_live_capacity_stale",
		"Whether the cached live capacity reading for a backend is stale (1) or fresh (0).",
		"gauge",
		metricSeries{`backend="arc"`, 0},
	))

	obs.ObserveLiveCapacityDecision(model.PoolFull, model.BackendARC, "", 7)
	obs.ObserveLiveCapacityDecision(model.PoolFull, model.BackendARC, "provider-full", 3)
	obs.ObserveLiveCapacityDecision(model.PoolFull, model.BackendARC, "provider-full", 3)
	requireFloat(t, obs.liveCapacityDecisions.WithLabelValues("full", "arc", "unknown"), 1)
	requireFloat(t, obs.liveCapacityDecisions.WithLabelValues("full", "arc", "provider-full"), 2)
	compareRegistered(t, reg, obs.liveCapacityDecisions, "uecb_live_capacity_decisions_total", expectFamily(
		"uecb_live_capacity_decisions_total",
		"Live capacity routing decisions by pool, backend, and reason.",
		"counter",
		metricSeries{`backend="arc",pool="full",reason="provider-full"`, 2},
		metricSeries{`backend="arc",pool="full",reason="unknown"`, 1},
	))
}

func TestPrometheusObserverBudgetAndQuality(t *testing.T) {
	obs, reg := newRegisteredObserver(t)
	obs.ObserveBudgetBlocked(model.PoolFull, model.BackendARC, "")
	obs.ObserveBudgetBlocked(model.PoolFull, model.BackendARC, "monthly-budget")
	obs.ObserveBudgetBlocked(model.PoolFull, model.BackendARC, "monthly-budget")
	requireFloat(t, obs.budgetBlocked.WithLabelValues("full", "arc", "unknown"), 1)
	requireFloat(t, obs.budgetBlocked.WithLabelValues("full", "arc", "monthly-budget"), 2)
	compareRegistered(t, reg, obs.budgetBlocked, "uecb_backend_budget_blocked_total", expectFamily(
		"uecb_backend_budget_blocked_total",
		"Allocation attempts skipped because a backend daily/monthly budget was exceeded.",
		"counter",
		metricSeries{`backend="arc",pool="full",reason="monthly-budget"`, 2},
		metricSeries{`backend="arc",pool="full",reason="unknown"`, 1},
	))

	obs.ObserveBudgetUsage(model.PoolFull, model.BackendARC, 2, 10, 4, 100)
	requireFloat(t, obs.budgetUsage.WithLabelValues("full", "arc", "daily", "used"), 2)
	requireFloat(t, obs.budgetUsage.WithLabelValues("full", "arc", "daily", "limit"), 10)
	requireFloat(t, obs.budgetUsage.WithLabelValues("full", "arc", "monthly", "used"), 4)
	requireFloat(t, obs.budgetUsage.WithLabelValues("full", "arc", "monthly", "limit"), 100)
	compareRegistered(t, reg, obs.budgetUsage, "uecb_backend_budget_usage", expectFamily(
		"uecb_backend_budget_usage",
		"Current backend budget usage against configured limits (window is daily or monthly).",
		"gauge",
		metricSeries{`backend="arc",kind="limit",pool="full",window="daily"`, 10},
		metricSeries{`backend="arc",kind="used",pool="full",window="daily"`, 2},
		metricSeries{`backend="arc",kind="limit",pool="full",window="monthly"`, 100},
		metricSeries{`backend="arc",kind="used",pool="full",window="monthly"`, 4},
	))

	obs.ObserveQualitySelection(model.PoolFull, model.BackendARC, "", 0.25)
	requireFloat(t, obs.qualitySelections.WithLabelValues("full", "arc", "unknown"), 1)
	requireFloat(t, obs.qualityScore.WithLabelValues("full", "arc"), 0.25)
	obs.ObserveQualityScore(model.PoolFull, model.BackendARC, 0.75, 1, 2*time.Second, 3, 4)
	requireFloat(t, obs.qualityScore.WithLabelValues("full", "arc"), 0.75)
	requireFloat(t, obs.qualitySuccessRate.WithLabelValues("full", "arc"), 1)
	requireFloat(t, obs.qualityP95Ready.WithLabelValues("full", "arc"), 2)
	requireFloat(t, obs.qualityCapacityErrors.WithLabelValues("full", "arc"), 3)
	requireFloat(t, obs.qualityFreeSlots.WithLabelValues("full", "arc"), 4)
	obs.ObserveQualitySelection(model.PoolFull, model.BackendLambda, "higher-score", 0.5)
	requireFloat(t, obs.qualitySelections.WithLabelValues("full", "lambda", "higher-score"), 1)
	requireFloat(t, obs.qualityScore.WithLabelValues("full", "lambda"), 0.5)

	compareRegistered(t, reg, obs.qualitySelections, "uecb_quality_selection_total", expectFamily(
		"uecb_quality_selection_total",
		"Quality-aware backend selection decisions by pool, backend, and reason.",
		"counter",
		metricSeries{`backend="arc",pool="full",reason="unknown"`, 1},
		metricSeries{`backend="lambda",pool="full",reason="higher-score"`, 1},
	))
	compareRegistered(t, reg, obs.qualityScore, "uecb_quality_score", expectFamily(
		"uecb_quality_score",
		"Latest quality-aware composite score observed for a pool/backend candidate.",
		"gauge",
		metricSeries{`backend="arc",pool="full"`, 0.75},
		metricSeries{`backend="lambda",pool="full"`, 0.5},
	))
	compareRegistered(t, reg, obs.qualitySuccessRate, "uecb_quality_success_rate", expectFamily(
		"uecb_quality_success_rate",
		"Rolling success rate used by quality-aware selection (0-1).",
		"gauge",
		metricSeries{`backend="arc",pool="full"`, 1},
	))
	compareRegistered(t, reg, obs.qualityP95Ready, "uecb_quality_p95_ready_seconds", expectFamily(
		"uecb_quality_p95_ready_seconds",
		"Rolling p95 ready/launch latency used by quality-aware selection.",
		"gauge",
		metricSeries{`backend="arc",pool="full"`, 2},
	))
	compareRegistered(t, reg, obs.qualityCapacityErrors, "uecb_quality_capacity_errors", expectFamily(
		"uecb_quality_capacity_errors",
		"Recent capacity-error count in the quality-aware rolling window.",
		"gauge",
		metricSeries{`backend="arc",pool="full"`, 3},
	))
	compareRegistered(t, reg, obs.qualityFreeSlots, "uecb_quality_free_slots", expectFamily(
		"uecb_quality_free_slots",
		"Free slots considered by quality-aware selection for a pool/backend candidate.",
		"gauge",
		metricSeries{`backend="arc",pool="full"`, 4},
	))
}

func TestPrometheusObserverReapOrphanAndStaleLabels(t *testing.T) {
	obs, reg := newRegisteredObserver(t)
	obs.ObserveAllocationReaped(model.PoolFull, model.BackendARC, "")
	obs.ObserveAllocationReaped(model.PoolFull, model.BackendARC, "quarantined")
	requireFloat(t, obs.allocationsReaped.WithLabelValues("full", "arc", "expired"), 1)
	requireFloat(t, obs.allocationsReaped.WithLabelValues("full", "arc", "quarantined"), 1)
	compareRegistered(t, reg, obs.allocationsReaped, "uecb_allocations_reaped_total", expectFamily(
		"uecb_allocations_reaped_total",
		"Stuck allocations reaped by the broker-side reaper after job_timeout (+ grace), by pool, backend, and terminal result.",
		"counter",
		metricSeries{`backend="arc",pool="full",result="expired"`, 1},
		metricSeries{`backend="arc",pool="full",result="quarantined"`, 1},
	))

	obs.ObserveOrphanCleanup(model.PoolFull, model.BackendARC, "")
	obs.ObserveOrphanCleanup(model.PoolFull, model.BackendARC, orphanActionExpired)
	obs.ObserveOrphanCleanup(model.PoolFull, model.BackendARC, orphanActionQuarantineExpired)
	requireFloat(t, obs.orphanCleanupActions.WithLabelValues("full", "arc", "unknown"), 1)
	requireFloat(t, obs.orphanCleanupActions.WithLabelValues("full", "arc", orphanActionExpired), 1)
	compareRegistered(t, reg, obs.orphanCleanupActions, "uecb_orphan_cleanup_actions_total", expectFamily(
		"uecb_orphan_cleanup_actions_total",
		"Orphan cleanup sweep actions for stale allocations (missing finalize / hard job kill).",
		"counter",
		metricSeries{`action="expired",backend="arc",pool="full"`, 1},
		metricSeries{`action="quarantine_expired",backend="arc",pool="full"`, 1},
		metricSeries{`action="unknown",backend="arc",pool="full"`, 1},
	))

	obs.ObserveLabelGarbage(model.PoolFull, model.BackendARC, "")
	obs.ObserveLabelGarbage(model.PoolFull, model.BackendARC, labelGarbageCleanupFailed)
	obs.ObserveLabelGarbage(model.PoolFull, model.BackendARC, labelGarbageNoCleanupHook)
	requireFloat(t, obs.labelGarbage.WithLabelValues("full", "arc", "unknown"), 1)
	requireFloat(t, obs.labelGarbage.WithLabelValues("full", "arc", labelGarbageNoCleanupHook), 1)
	compareRegistered(t, reg, obs.labelGarbage, "uecb_label_garbage_total", expectFamily(
		"uecb_label_garbage_total",
		"Runner label garbage-collection outcomes during orphan cleanup.",
		"counter",
		metricSeries{`backend="arc",pool="full",result="cleanup_failed"`, 1},
		metricSeries{`backend="arc",pool="full",result="no_cleanup_hook"`, 1},
		metricSeries{`backend="arc",pool="full",result="unknown"`, 1},
	))

	obs.ObserveStaleRunnerLabels([]staleRunnerLabelSnapshot{
		{Pool: model.PoolFull, Backend: model.BackendARC, Phase: stalePhaseOverdue, Count: 3},
		{Pool: model.PoolFull, Backend: model.BackendARC, Phase: stalePhaseQuarantined, Count: 1},
		{Pool: model.PoolLite, Backend: model.BackendLambda, Phase: "", Count: 2},
	})
	requireFloat(t, obs.staleRunnerLabels.WithLabelValues("full", "arc", stalePhaseOverdue), 3)
	requireFloat(t, obs.staleRunnerLabels.WithLabelValues("full", "arc", stalePhaseQuarantined), 1)
	requireFloat(t, obs.staleRunnerLabels.WithLabelValues("lite", "lambda", "unknown"), 2)
	compareRegistered(t, reg, obs.staleRunnerLabels, "uecb_stale_runner_labels", expectFamily(
		"uecb_stale_runner_labels",
		"Active or quarantined allocations holding runner labels past their job-timeout TTL.",
		"gauge",
		metricSeries{`backend="arc",phase="overdue",pool="full"`, 3},
		metricSeries{`backend="arc",phase="quarantined",pool="full"`, 1},
		metricSeries{`backend="lambda",phase="unknown",pool="lite"`, 2},
	))

	obs.ObserveStaleRunnerLabels([]staleRunnerLabelSnapshot{
		{Pool: model.PoolFull, Backend: model.BackendARC, Phase: stalePhaseOverdue, Count: 5},
	})
	requireFloat(t, obs.staleRunnerLabels.WithLabelValues("full", "arc", stalePhaseOverdue), 5)
	compareRegistered(t, reg, obs.staleRunnerLabels, "uecb_stale_runner_labels", expectFamily(
		"uecb_stale_runner_labels",
		"Active or quarantined allocations holding runner labels past their job-timeout TTL.",
		"gauge",
		metricSeries{`backend="arc",phase="overdue",pool="full"`, 5},
	))
}

func TestPrometheusObserverRestartMetrics(t *testing.T) {
	obs, reg := newRegisteredObserver(t)
	obs.ObserveProcessLocalStateLoss("")
	obs.ObserveProcessLocalStateLoss(store.TypeMemory)
	obs.ObserveProcessLocalStateLoss(store.TypeFile)
	requireFloat(t, obs.processLocalStateLoss.WithLabelValues(store.TypeMemory), 2)
	requireFloat(t, obs.processLocalStateLoss.WithLabelValues(store.TypeFile), 1)
	compareRegistered(t, reg, obs.processLocalStateLoss, "uecb_process_local_state_loss_total", expectFamily(
		"uecb_process_local_state_loss_total",
		"Broker startups that use a process-local state store with no durable allocation history (memory).",
		"counter",
		metricSeries{`store="file"`, 1},
		metricSeries{`store="memory"`, 2},
	))

	obs.ObserveRestartOrphans(model.BackendARC, orphanReasonCapacityGap, 0)
	obs.ObserveRestartOrphans("", "", 2)
	requireFloat(t, obs.restartOrphansTotal.WithLabelValues("unknown", "unknown"), 2)
	requireFloat(t, obs.orphanedRunners.WithLabelValues("unknown", "unknown"), 2)
	obs.ObserveRestartOrphans("", "", -1)
	requireFloat(t, obs.restartOrphansTotal.WithLabelValues("unknown", "unknown"), 2)
	requireFloat(t, obs.orphanedRunners.WithLabelValues("unknown", "unknown"), 2)
	obs.ObserveRestartOrphans(model.BackendLambda, orphanReasonMidAllocate, 1)
	obs.ObserveRestartOrphans(model.BackendLambda, orphanReasonMidAllocate, 3)
	requireFloat(t, obs.restartOrphansTotal.WithLabelValues("lambda", orphanReasonMidAllocate), 4)
	requireFloat(t, obs.orphanedRunners.WithLabelValues("lambda", orphanReasonMidAllocate), 4)
	compareRegistered(t, reg, obs.restartOrphansTotal, "uecb_restart_orphans_total", expectFamily(
		"uecb_restart_orphans_total",
		"Orphaned allocations or provider runners detected during startup restart reconciliation.",
		"counter",
		metricSeries{`backend="lambda",reason="mid_allocate"`, 4},
		metricSeries{`backend="unknown",reason="unknown"`, 2},
	))
	compareRegistered(t, reg, obs.orphanedRunners, "uecb_orphaned_runners", expectFamily(
		"uecb_orphaned_runners",
		"Estimated orphaned runners observed at the last startup reconciliation (capacity gap or mid-allocate).",
		"gauge",
		metricSeries{`backend="lambda",reason="mid_allocate"`, 4},
		metricSeries{`backend="unknown",reason="unknown"`, 2},
	))
}

func newRegisteredObserver(t *testing.T) (*PrometheusObserver, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	obs := NewPrometheusObserver(reg)
	if obs == nil {
		t.Fatal("NewPrometheusObserver returned nil")
	}
	if err := obs.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return obs, reg
}

func isAlreadyRegistered(err error) bool {
	var already prometheus.AlreadyRegisteredError
	return errors.As(err, &already)
}

func requireFloat(t *testing.T, c prometheus.Collector, want float64) {
	t.Helper()
	got := testutil.ToFloat64(c)
	if got != want {
		t.Fatalf("metric value = %v, want %v", got, want)
	}
}

func compareRegistered(t *testing.T, reg prometheus.Gatherer, c prometheus.Collector, name, expected string) {
	t.Helper()
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatalf("metric %s:\n%v", name, err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() == name {
			if len(family.GetMetric()) == 0 {
				t.Fatalf("metric %s is registered but has no series", name)
			}
			return
		}
	}
	t.Fatalf("registry missing metric %s", name)
}

type metricSeries struct {
	labels string
	value  float64
}

func expectFamily(name, help, kind string, series ...metricSeries) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	for _, s := range series {
		fmt.Fprintf(&b, "%s{%s} %s\n", name, s.labels, strconv.FormatFloat(s.value, 'f', -1, 64))
	}
	return b.String()
}

type histPoint struct {
	labels string
	values []float64
}

func expectHistogram(name, help string, points ...histPoint) string {
	bounds := []struct {
		label string
		le    float64
	}{
		{"0.1", 0.1},
		{"0.25", 0.25},
		{"0.5", 0.5},
		{"1", 1},
		{"2.5", 2.5},
		{"5", 5},
		{"10", 10},
		{"30", 30},
		{"60", 60},
		{"90", 90},
		{"+Inf", math.Inf(1)},
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	for _, point := range points {
		for _, bound := range bounds {
			n := 0
			for _, value := range point.values {
				if value <= bound.le {
					n++
				}
			}
			fmt.Fprintf(&b, "%s_bucket{%s,le=%q} %d\n", name, point.labels, bound.label, n)
		}
		sum := 0.0
		for _, value := range point.values {
			sum += value
		}
		fmt.Fprintf(&b, "%s_sum{%s} %s\n", name, point.labels, strconv.FormatFloat(sum, 'f', -1, 64))
		fmt.Fprintf(&b, "%s_count{%s} %d\n", name, point.labels, len(point.values))
	}
	return b.String()
}

func circuitSeries(pool model.PoolName, backend model.BackendName, active string) []metricSeries {
	series := make([]metricSeries, 0, 3)
	for _, state := range []string{circuitStateClosed, circuitStateOpen, circuitStateHalfOpen} {
		value := 0.0
		if state == active {
			value = 1
		}
		series = append(series, metricSeries{
			labels: fmt.Sprintf(`backend=%q,pool=%q,state=%q`, backend, pool, state),
			value:  value,
		})
	}
	return series
}

func tierSeries(pool model.PoolName, backend model.BackendName, active, stale string) []metricSeries {
	series := make([]metricSeries, 0, 4)
	for _, state := range []string{"healthy", "approaching", "exceeded", "unknown"} {
		value := 0.0
		if state == active {
			value = 1
		}
		series = append(series, metricSeries{
			labels: fmt.Sprintf(`backend=%q,pool=%q,stale=%q,state=%q`, backend, pool, stale, state),
			value:  value,
		})
	}
	return series
}

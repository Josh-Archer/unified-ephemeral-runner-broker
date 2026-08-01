package api

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	correlationIDHeader = "X-Correlation-ID"
	correlationIDKey    = "correlation_id"
)

type contextKey string

const (
	correlationContextKey contextKey = correlationIDKey
	principalContextKey   contextKey = "oidc_principal"
)

type Observer interface {
	ObserveAllocationStart(model.PoolName)
	ObserveAllocationResult(model.PoolName, model.BackendName, string, time.Duration)
	ObserveLaunchLatency(model.PoolName, model.BackendName, string, time.Duration)
	ObserveRegistrationLatency(model.PoolName, model.BackendName, time.Duration)
	ObserveActiveAllocations([]model.AllocationStatus)
	ObserveCapacity(model.BrokerConfig, []model.AllocationStatus)
	ObserveBackendCircuitState([]backendCircuitSnapshot)
	ObserveBackendCircuitTransition(model.PoolName, model.BackendName, string, string, string)
	ObserveBackendAdmissionRejected(model.PoolName, model.BackendName, string)
	ObserveBackendProbe(model.PoolName, model.BackendName, string)
	ObserveTierState([]tierDecisionSnapshot)
	ObserveTierFallback(model.PoolName, string, string)
	ObserveTierBlocked(model.PoolName, model.BackendName, string)
	ObserveLiveCapacityState([]liveCapacitySnapshot)
	ObserveLiveCapacityDecision(model.PoolName, model.BackendName, string, int)
	// ObserveProcessLocalStateLoss records that a process-local (non-HA) store
	// started without durable allocation history (typical for memory).
	ObserveProcessLocalStateLoss(storeType string)
	// ObserveRestartOrphans records orphans detected during startup reconciliation.
	// reason is mid_allocate | capacity_gap | unrehydratable.
	ObserveRestartOrphans(backend model.BackendName, reason string, count int)
}

type noopObserver struct{}

func (noopObserver) ObserveAllocationStart(model.PoolName) {}
func (noopObserver) ObserveAllocationResult(model.PoolName, model.BackendName, string, time.Duration) {
}
func (noopObserver) ObserveLaunchLatency(model.PoolName, model.BackendName, string, time.Duration) {}
func (noopObserver) ObserveRegistrationLatency(model.PoolName, model.BackendName, time.Duration) {
}
func (noopObserver) ObserveActiveAllocations([]model.AllocationStatus)            {}
func (noopObserver) ObserveCapacity(model.BrokerConfig, []model.AllocationStatus) {}
func (noopObserver) ObserveBackendCircuitState([]backendCircuitSnapshot)          {}
func (noopObserver) ObserveBackendCircuitTransition(model.PoolName, model.BackendName, string, string, string) {
}
func (noopObserver) ObserveBackendAdmissionRejected(model.PoolName, model.BackendName, string) {}
func (noopObserver) ObserveBackendProbe(model.PoolName, model.BackendName, string)             {}
func (noopObserver) ObserveTierState([]tierDecisionSnapshot)                                   {}
func (noopObserver) ObserveTierFallback(model.PoolName, string, string)                        {}
func (noopObserver) ObserveTierBlocked(model.PoolName, model.BackendName, string)              {}
func (noopObserver) ObserveLiveCapacityState([]liveCapacitySnapshot)                           {}
func (noopObserver) ObserveLiveCapacityDecision(model.PoolName, model.BackendName, string, int) {
}
func (noopObserver) ObserveProcessLocalStateLoss(string)                            {}
func (noopObserver) ObserveRestartOrphans(model.BackendName, string, int)           {}

type liveCapacitySnapshot struct {
	Backend model.BackendName
	Free    int
	Max     int
	Stale   bool
	Source  string
	Err     bool
}

// staleRunnerLabelSnapshot counts overdue ready/reserved allocations and
// quarantined allocations still holding a runner label after finalize missed.
type staleRunnerLabelSnapshot struct {
	Pool    model.PoolName
	Backend model.BackendName
	// Phase is overdue (past ExpiresAt, pending sweep) or quarantined.
	Phase string
	Count int
}

const (
	orphanActionExpired           = "expired"
	orphanActionQuarantined       = "quarantined"
	orphanActionQuarantineExpired = "quarantine_expired"
	labelGarbageReclaimed         = "reclaimed"
	labelGarbageCleanupFailed     = "cleanup_failed"
	labelGarbageNoCleanupHook     = "no_cleanup_hook"
	stalePhaseOverdue             = "overdue"
	stalePhaseQuarantined         = "quarantined"
)

type PrometheusObserver struct {
	allocationLatency     *prometheus.HistogramVec
	launchLatency         *prometheus.HistogramVec
	registrationLatency   *prometheus.HistogramVec
	allocations           *prometheus.CounterVec
	queueDepth            *prometheus.GaugeVec
	capacityUtilization   *prometheus.GaugeVec
	circuitState          *prometheus.GaugeVec
	circuitTransitions    *prometheus.CounterVec
	admissionRejections   *prometheus.CounterVec
	probeResults          *prometheus.CounterVec
	tierState             *prometheus.GaugeVec
	tierFallbacks         *prometheus.CounterVec
	tierBlocked           *prometheus.CounterVec
	liveCapacityFree      *prometheus.GaugeVec
	liveCapacityStale     *prometheus.GaugeVec
	liveCapacityDecisions *prometheus.CounterVec
	processLocalStateLoss *prometheus.CounterVec
	restartOrphansTotal   *prometheus.CounterVec
	orphanedRunners       *prometheus.GaugeVec
}

func NewPrometheusObserver(registerer prometheus.Registerer) *PrometheusObserver {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}

	return &PrometheusObserver{
		allocationLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "uecb_allocation_latency_seconds",
			Help:    "End-to-end allocation latency from broker admission through backend provisioning.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 90},
		}, []string{"pool", "backend", "result"}),
		launchLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "uecb_launch_latency_seconds",
			Help:    "Backend launch latency for a selected ephemeral runner.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 90},
		}, []string{"pool", "backend", "launch_mode"}),
		registrationLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "uecb_registration_latency_seconds",
			Help:    "Observed latency until a provisioned runner registration response is available to the broker.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 90},
		}, []string{"pool", "backend"}),
		allocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_allocations_total",
			Help: "Allocation attempts by pool, backend, and result.",
		}, []string{"pool", "backend", "result"}),
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "uecb_queue_depth",
			Help: "Current allocation count by state.",
		}, []string{"pool", "state"}),
		capacityUtilization: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "uecb_capacity_utilization_ratio",
			Help: "Active allocations divided by configured backend capacity.",
		}, []string{"pool", "backend"}),
		circuitState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "uecb_backend_circuit_state",
			Help: "Runtime backend circuit state. A value of 1 marks the active state for a pool/backend.",
		}, []string{"pool", "backend", "state"}),
		circuitTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_backend_circuit_transitions_total",
			Help: "Runtime backend circuit transitions by pool, backend, state, and reason.",
		}, []string{"pool", "backend", "from", "to", "reason"}),
		admissionRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_backend_admission_rejections_total",
			Help: "Backend admission rejections before scheduler reservation.",
		}, []string{"pool", "backend", "reason"}),
		probeResults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_backend_probe_results_total",
			Help: "Background backend recovery probe results.",
		}, []string{"pool", "backend", "result"}),
		tierState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "uecb_tier_state",
			Help: "Cached tier routing state. A value of 1 marks the active state for a pool/backend.",
		}, []string{"pool", "backend", "state", "stale"}),
		tierFallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_tier_fallback_total",
			Help: "Tier routing fallback decisions by pool, mode, and reason.",
		}, []string{"pool", "mode", "reason"}),
		tierBlocked: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_tier_blocked_allocations_total",
			Help: "Allocation attempts blocked by tier routing.",
		}, []string{"pool", "backend", "reason"}),
		liveCapacityFree: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "uecb_live_capacity_free_slots",
			Help: "Cached provider-reported free runner slots per backend.",
		}, []string{"backend", "source"}),
		liveCapacityStale: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "uecb_live_capacity_stale",
			Help: "Whether the cached live capacity reading for a backend is stale (1) or fresh (0).",
		}, []string{"backend"}),
		liveCapacityDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_live_capacity_decisions_total",
			Help: "Live capacity routing decisions by pool, backend, and reason.",
		}, []string{"pool", "backend", "reason"}),
		processLocalStateLoss: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_process_local_state_loss_total",
			Help: "Broker startups that use a process-local state store with no durable allocation history (memory).",
		}, []string{"store"}),
		restartOrphansTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_restart_orphans_total",
			Help: "Orphaned allocations or provider runners detected during startup restart reconciliation.",
		}, []string{"backend", "reason"}),
		orphanedRunners: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "uecb_orphaned_runners",
			Help: "Estimated orphaned runners observed at the last startup reconciliation (capacity gap or mid-allocate).",
		}, []string{"backend", "reason"}),
	}
}

func (o *PrometheusObserver) Register(registerer prometheus.Registerer) error {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	for _, collector := range []prometheus.Collector{
		o.allocationLatency,
		o.launchLatency,
		o.registrationLatency,
		o.allocations,
		o.queueDepth,
		o.capacityUtilization,
		o.circuitState,
		o.circuitTransitions,
		o.admissionRejections,
		o.probeResults,
		o.tierState,
		o.tierFallbacks,
		o.tierBlocked,
		o.liveCapacityFree,
		o.liveCapacityStale,
		o.liveCapacityDecisions,
		o.processLocalStateLoss,
		o.restartOrphansTotal,
		o.orphanedRunners,
	} {
		if err := registerer.Register(collector); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				return err
			}
		}
	}
	return nil
}

func (o *PrometheusObserver) ObserveOrphanCleanup(pool model.PoolName, backend model.BackendName, action string) {
	if action == "" {
		action = "unknown"
	}
	o.orphanCleanupActions.WithLabelValues(string(pool), string(backend), action).Inc()
}

func (o *PrometheusObserver) ObserveLabelGarbage(pool model.PoolName, backend model.BackendName, result string) {
	if result == "" {
		result = "unknown"
	}
	o.labelGarbage.WithLabelValues(string(pool), string(backend), result).Inc()
}

func (o *PrometheusObserver) ObserveStaleRunnerLabels(snapshots []staleRunnerLabelSnapshot) {
	o.staleRunnerLabels.Reset()
	for _, snapshot := range snapshots {
		phase := snapshot.Phase
		if phase == "" {
			phase = "unknown"
		}
		o.staleRunnerLabels.WithLabelValues(string(snapshot.Pool), string(snapshot.Backend), phase).Set(float64(snapshot.Count))
	}
}

func (o *PrometheusObserver) ObserveLiveCapacityState(snapshots []liveCapacitySnapshot) {
	o.liveCapacityFree.Reset()
	o.liveCapacityStale.Reset()
	for _, snapshot := range snapshots {
		source := snapshot.Source
		if source == "" {
			source = "unknown"
		}
		if snapshot.Err {
			source = "error"
		}
		o.liveCapacityFree.WithLabelValues(string(snapshot.Backend), source).Set(float64(snapshot.Free))
		stale := 0.0
		if snapshot.Stale {
			stale = 1
		}
		o.liveCapacityStale.WithLabelValues(string(snapshot.Backend)).Set(stale)
	}
}

func (o *PrometheusObserver) ObserveLiveCapacityDecision(pool model.PoolName, backend model.BackendName, reason string, _ int) {
	if reason == "" {
		reason = "unknown"
	}
	o.liveCapacityDecisions.WithLabelValues(string(pool), string(backend), reason).Inc()
}

func (o *PrometheusObserver) ObserveProcessLocalStateLoss(storeType string) {
	if storeType == "" {
		storeType = store.TypeMemory
	}
	o.processLocalStateLoss.WithLabelValues(storeType).Inc()
}

func (o *PrometheusObserver) ObserveRestartOrphans(backend model.BackendName, reason string, count int) {
	if count <= 0 {
		return
	}
	if reason == "" {
		reason = "unknown"
	}
	backendLabel := string(backend)
	if backendLabel == "" {
		backendLabel = "unknown"
	}
	o.restartOrphansTotal.WithLabelValues(backendLabel, reason).Add(float64(count))
	// Gauge accumulates across mid-allocate / unrehydratable detections during
	// a single startup pass; capacity_gap is reported once per backend.
	o.orphanedRunners.WithLabelValues(backendLabel, reason).Add(float64(count))
}

type tierDecisionSnapshot struct {
	Pool    model.PoolName
	Backend model.BackendName
	State   string
	Stale   bool
}

func (o *PrometheusObserver) ObserveTierState(snapshots []tierDecisionSnapshot) {
	o.tierState.Reset()
	for _, snapshot := range snapshots {
		stale := "false"
		if snapshot.Stale {
			stale = "true"
		}
		for _, state := range []string{"healthy", "approaching", "exceeded", "unknown"} {
			value := 0.0
			if snapshot.State == state {
				value = 1
			}
			o.tierState.WithLabelValues(string(snapshot.Pool), string(snapshot.Backend), state, stale).Set(value)
		}
	}
}

func (o *PrometheusObserver) ObserveTierFallback(pool model.PoolName, mode, reason string) {
	o.tierFallbacks.WithLabelValues(string(pool), mode, reason).Inc()
}

func (o *PrometheusObserver) ObserveTierBlocked(pool model.PoolName, backend model.BackendName, reason string) {
	o.tierBlocked.WithLabelValues(string(pool), string(backend), reason).Inc()
}

func (o *PrometheusObserver) ObserveBackendCircuitState(snapshots []backendCircuitSnapshot) {
	o.circuitState.Reset()
	for _, snapshot := range snapshots {
		for _, state := range []string{circuitStateClosed, circuitStateOpen, circuitStateHalfOpen} {
			value := 0.0
			if snapshot.State == state {
				value = 1
			}
			o.circuitState.WithLabelValues(string(snapshot.Pool), string(snapshot.Backend), state).Set(value)
		}
	}
}

func (o *PrometheusObserver) ObserveBackendCircuitTransition(pool model.PoolName, backend model.BackendName, from, to, reason string) {
	o.circuitTransitions.WithLabelValues(string(pool), string(backend), from, to, reason).Inc()
}

func (o *PrometheusObserver) ObserveBackendAdmissionRejected(pool model.PoolName, backend model.BackendName, reason string) {
	o.admissionRejections.WithLabelValues(string(pool), string(backend), reason).Inc()
}

func (o *PrometheusObserver) ObserveBackendProbe(pool model.PoolName, backend model.BackendName, result string) {
	o.probeResults.WithLabelValues(string(pool), string(backend), result).Inc()
}

func (o *PrometheusObserver) ObserveAllocationStart(pool model.PoolName) {
	o.queueDepth.WithLabelValues(string(pool), string(model.StateReserved)).Inc()
}

func (o *PrometheusObserver) ObserveAllocationResult(pool model.PoolName, backend model.BackendName, result string, latency time.Duration) {
	o.allocations.WithLabelValues(string(pool), string(backend), result).Inc()
	o.allocationLatency.WithLabelValues(string(pool), string(backend), result).Observe(latency.Seconds())
}

func (o *PrometheusObserver) ObserveLaunchLatency(pool model.PoolName, backend model.BackendName, launchMode string, latency time.Duration) {
	o.launchLatency.WithLabelValues(string(pool), string(backend), launchMode).Observe(latency.Seconds())
}

func (o *PrometheusObserver) ObserveRegistrationLatency(pool model.PoolName, backend model.BackendName, latency time.Duration) {
	o.registrationLatency.WithLabelValues(string(pool), string(backend)).Observe(latency.Seconds())
}

func (o *PrometheusObserver) ObserveActiveAllocations(statuses []model.AllocationStatus) {
	o.queueDepth.Reset()
	for _, status := range statuses {
		o.queueDepth.WithLabelValues(string(status.Pool), string(status.State)).Inc()
	}
}

func (o *PrometheusObserver) ObserveCapacity(cfg model.BrokerConfig, statuses []model.AllocationStatus) {
	o.capacityUtilization.Reset()
	active := map[model.PoolName]map[model.BackendName]int{}
	for _, status := range statuses {
		if status.State != model.StateReady && status.State != model.StateReserved {
			continue
		}
		if active[status.Pool] == nil {
			active[status.Pool] = map[model.BackendName]int{}
		}
		active[status.Pool][status.SelectedBackend]++
	}
	for _, pool := range cfg.Pools {
		for name, backend := range pool.Backends {
			if !backend.Enabled || backend.MaxRunners <= 0 {
				continue
			}
			used := active[pool.Name][name]
			o.capacityUtilization.WithLabelValues(string(pool.Name), string(name)).Set(float64(used) / float64(backend.MaxRunners))
		}
	}
}

func withCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationContextKey, id)
}

func correlationIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(correlationContextKey).(string); ok {
		return id
	}
	return ""
}

func withPrincipal(ctx context.Context, claims OIDCClaims) context.Context {
	if claims.Sub == "" {
		return ctx
	}
	return context.WithValue(ctx, principalContextKey, claims)
}

func principalFromContext(ctx context.Context) (OIDCClaims, bool) {
	claims, ok := ctx.Value(principalContextKey).(OIDCClaims)
	if !ok || claims.Sub == "" {
		return OIDCClaims{}, false
	}
	return claims, true
}

func applyPrincipal(status *model.AllocationStatus, claims OIDCClaims) {
	if status == nil || claims.Sub == "" {
		return
	}
	status.Subject = claims.Sub
	status.Repository = claims.EffectiveRepository()
	status.Owner = claims.EffectiveOwner()
}

func correlationIDFromRequest(r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get(correlationIDHeader))
	if id == "" {
		return newID()
	}
	return id
}

func logAllocationEvent(ctx context.Context, event string, fields map[string]string) {
	log.Printf("event=%s %s=%s%s", event, correlationIDKey, correlationIDFromContext(ctx), formatLogFields(fields))
}

func formatLogFields(fields map[string]string) string {
	if len(fields) == 0 {
		return ""
	}
	var builder strings.Builder
	for key, value := range fields {
		builder.WriteByte(' ')
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(value)
	}
	return builder.String()
}

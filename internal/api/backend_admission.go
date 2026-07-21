package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
)

var ErrBackendCircuitOpen = errors.New("backend circuit is open")
var ErrBackendRateLimited = errors.New("backend is rate limited")

const (
	circuitStateClosed   = "closed"
	circuitStateOpen     = "open"
	circuitStateHalfOpen = "half-open"

	defaultFailureThreshold         = 3
	defaultEvaluationWindow         = 5 * time.Minute
	defaultOpenDuration             = 2 * time.Minute
	defaultProbeInterval            = 30 * time.Second
	defaultProbeTimeout             = 10 * time.Second
	defaultRecoverySuccessThreshold = 1
	defaultHalfOpenMaxRequests      = 1
)

type backendProbe interface {
	Probe(context.Context, model.PoolConfig, model.BackendConfig) error
}

type backendAdmission struct {
	mu       sync.Mutex
	circuits map[backendAdmissionKey]*backendCircuit
	limits   map[backendAdmissionKey]*backendRateLimit
	shared   store.AdmissionStateStore
}

type backendAdmissionKey struct {
	pool    model.PoolName
	backend model.BackendName
}

type backendCircuit struct {
	state                string
	failures             []time.Time
	openedAt             time.Time
	nextProbeAt          time.Time
	halfOpenInFlight     int
	recoverySuccesses    int
	lastTransitionReason string
}

type backendRateLimit struct {
	windowStart time.Time
	used        int
}

type backendAdmissionDecision struct {
	Allowed bool
	Reason  string
	State   string
}

func newBackendAdmission(shared store.AdmissionStateStore) *backendAdmission {
	admission := &backendAdmission{
		circuits: map[backendAdmissionKey]*backendCircuit{},
		limits:   map[backendAdmissionKey]*backendRateLimit{},
		shared:   shared,
	}
	admission.reloadShared()
	return admission
}

func (a *backendAdmission) reloadShared() {
	if a == nil || a.shared == nil {
		return
	}
	doc, err := a.shared.LoadAdmissionState(context.Background())
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applySharedDocumentLocked(doc)
}

func (a *backendAdmission) applySharedDocumentLocked(doc store.AdmissionStateDocument) {
	for key, circuitState := range doc.Circuits {
		pool, backendName, ok := splitAdmissionKey(key)
		if !ok {
			continue
		}
		admissionKey := backendAdmissionKey{pool: pool, backend: backendName}
		circuit := a.circuit(admissionKey)
		circuit.state = circuitState.State
		circuit.failures = append([]time.Time(nil), circuitState.Failures...)
		circuit.openedAt = circuitState.OpenedAt
		circuit.nextProbeAt = circuitState.NextProbeAt
		circuit.halfOpenInFlight = circuitState.HalfOpenInFlight
		circuit.recoverySuccesses = circuitState.RecoverySuccesses
		circuit.lastTransitionReason = circuitState.LastTransitionReason
	}
	for key, limitState := range doc.Limits {
		pool, backendName, ok := splitAdmissionKey(key)
		if !ok {
			continue
		}
		admissionKey := backendAdmissionKey{pool: pool, backend: backendName}
		a.limits[admissionKey] = &backendRateLimit{
			windowStart: limitState.WindowStart,
			used:        limitState.Used,
		}
	}
}

func (a *backendAdmission) persistSharedLocked() {
	if a == nil || a.shared == nil {
		return
	}
	doc := store.AdmissionStateDocument{
		Circuits: map[string]store.AdmissionCircuitState{},
		Limits:   map[string]store.AdmissionRateLimit{},
	}
	for key, circuit := range a.circuits {
		doc.Circuits[store.AdmissionKey(key.pool, key.backend)] = store.AdmissionCircuitState{
			State:                circuit.state,
			Failures:             append([]time.Time(nil), circuit.failures...),
			OpenedAt:             circuit.openedAt,
			NextProbeAt:          circuit.nextProbeAt,
			HalfOpenInFlight:     circuit.halfOpenInFlight,
			RecoverySuccesses:    circuit.recoverySuccesses,
			LastTransitionReason: circuit.lastTransitionReason,
		}
	}
	for key, limit := range a.limits {
		doc.Limits[store.AdmissionKey(key.pool, key.backend)] = store.AdmissionRateLimit{
			WindowStart: limit.windowStart,
			Used:        limit.used,
		}
	}
	_ = a.shared.SaveAdmissionState(context.Background(), doc)
}

func splitAdmissionKey(key string) (model.PoolName, model.BackendName, bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return model.PoolName(key[:i]), model.BackendName(key[i+1:]), true
		}
	}
	return "", "", false
}

func (a *backendAdmission) filter(pool model.PoolConfig, request model.AllocationRequest, now time.Time) (model.PoolConfig, error) {
	a.reloadShared()
	filtered := pool
	filtered.Backends = make(map[model.BackendName]model.BackendConfig, len(pool.Backends))
	rateLimited := 0
	for name, cfg := range pool.Backends {
		decision := a.allow(pool.Name, name, cfg, now, false, false)
		if decision.Allowed {
			filtered.Backends[name] = cfg
			continue
		}
		if decision.Reason == "rate-limited" {
			rateLimited++
		}
	}

	if request.Backend != nil {
		cfg, ok := pool.Backends[*request.Backend]
		if !ok {
			return model.PoolConfig{}, scheduler.ErrUnknownBackend
		}
		decision := a.allow(pool.Name, *request.Backend, cfg, now, false, false)
		if !decision.Allowed {
			switch decision.Reason {
			case "circuit-open":
				return model.PoolConfig{}, fmt.Errorf("pinned backend %q is circuit-open: %w", *request.Backend, ErrBackendCircuitOpen)
			case "rate-limited":
				return model.PoolConfig{}, fmt.Errorf("pinned backend %q is rate-limited: %w", *request.Backend, ErrBackendRateLimited)
			default:
				return model.PoolConfig{}, fmt.Errorf("pinned backend %q is not admissible: %s", *request.Backend, decision.Reason)
			}
		}
		return filtered, nil
	}

	if len(filtered.Backends) == 0 {
		if rateLimited == len(pool.Backends) {
			return model.PoolConfig{}, fmt.Errorf("all eligible backends for pool %q are rate-limited: %w", pool.Name, ErrBackendRateLimited)
		}
		return model.PoolConfig{}, fmt.Errorf("%w for pool %q after backend admission filtering", scheduler.ErrNoCapacity, pool.Name)
	}
	return filtered, nil
}

func (a *backendAdmission) allow(pool model.PoolName, name model.BackendName, cfg model.BackendConfig, now time.Time, warm bool, consume bool) backendAdmissionDecision {
	if consume {
		// Refresh shared admission state before consuming permits / half-open slots.
		a.reloadShared()
	}
	if cfg.CircuitBreaker.Enabled {
		decision := a.circuitDecision(pool, name, cfg.CircuitBreaker, now, consume)
		if !decision.Allowed {
			return decision
		}
	}
	if !warm && cfg.RateLimit.Enabled {
		return a.rateLimitDecision(pool, name, cfg.RateLimit, now, consume)
	}
	return backendAdmissionDecision{Allowed: true, State: circuitStateClosed}
}

func (a *backendAdmission) circuitDecision(pool model.PoolName, name model.BackendName, cfg model.CircuitBreakerConfig, now time.Time, consume bool) backendAdmissionDecision {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := backendAdmissionKey{pool: pool, backend: name}
	circuit := a.circuit(key)
	if circuit.state == "" {
		circuit.state = circuitStateClosed
	}

	switch circuit.state {
	case circuitStateOpen:
		if now.Before(circuit.nextProbeAt) {
			return backendAdmissionDecision{Allowed: false, Reason: "circuit-open", State: circuitStateOpen}
		}
		if !consume {
			return backendAdmissionDecision{Allowed: true, State: circuitStateOpen}
		}
		circuit.state = circuitStateHalfOpen
		circuit.halfOpenInFlight = 1
		circuit.recoverySuccesses = 0
		if consume {
			a.persistSharedLocked()
		}
		return backendAdmissionDecision{Allowed: true, State: circuitStateHalfOpen}
	case circuitStateHalfOpen:
		if !consume {
			return backendAdmissionDecision{Allowed: true, State: circuitStateHalfOpen}
		}
		if circuit.halfOpenInFlight >= normalizeHalfOpenMaxRequests(cfg) {
			return backendAdmissionDecision{Allowed: false, Reason: "circuit-open", State: circuitStateHalfOpen}
		}
		circuit.halfOpenInFlight++
		a.persistSharedLocked()
		return backendAdmissionDecision{Allowed: true, State: circuitStateHalfOpen}
	}

	return backendAdmissionDecision{Allowed: true, State: circuit.state}
}

func (a *backendAdmission) rateLimitDecision(pool model.PoolName, name model.BackendName, cfg model.RateLimitConfig, now time.Time, consume bool) backendAdmissionDecision {
	a.mu.Lock()
	defer a.mu.Unlock()

	interval := cfg.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	permits := cfg.Permits
	if permits <= 0 {
		permits = 1
	}
	if cfg.Burst > permits {
		permits = cfg.Burst
	}

	key := backendAdmissionKey{pool: pool, backend: name}
	limit := a.limits[key]
	if limit == nil {
		limit = &backendRateLimit{windowStart: now}
		a.limits[key] = limit
	}
	if limit.windowStart.IsZero() || !now.Before(limit.windowStart.Add(interval)) {
		limit.windowStart = now
		limit.used = 0
	}
	if limit.used >= permits {
		return backendAdmissionDecision{Allowed: false, Reason: "rate-limited", State: circuitStateClosed}
	}
	if consume {
		limit.used++
		a.persistSharedLocked()
	}
	return backendAdmissionDecision{Allowed: true, State: circuitStateClosed}
}

func (a *backendAdmission) recordSuccess(pool model.PoolName, name model.BackendName, cfg model.BackendConfig, now time.Time) (from, to, reason string, changed bool) {
	if !cfg.CircuitBreaker.Enabled {
		return "", "", "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	key := backendAdmissionKey{pool: pool, backend: name}
	circuit := a.circuit(key)
	previous := normalizeCircuitState(circuit.state)
	circuit.failures = nil
	circuit.halfOpenInFlight = 0
	if previous == circuitStateHalfOpen {
		circuit.recoverySuccesses++
		if circuit.recoverySuccesses < normalizeRecoverySuccessThreshold(cfg.CircuitBreaker) {
			a.persistSharedLocked()
			return "", "", "", false
		}
	}
	circuit.state = circuitStateClosed
	circuit.recoverySuccesses = 0
	circuit.lastTransitionReason = "success"
	changed = previous != circuitStateClosed
	a.persistSharedLocked()
	return previous, circuitStateClosed, "success", changed
}

func (a *backendAdmission) recordProbeSuccess(pool model.PoolName, name model.BackendName, cfg model.BackendConfig, now time.Time) (from, to, reason string, changed bool) {
	if !cfg.CircuitBreaker.Enabled {
		return "", "", "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	key := backendAdmissionKey{pool: pool, backend: name}
	circuit := a.circuit(key)
	previous := normalizeCircuitState(circuit.state)
	if previous == circuitStateClosed {
		return "", "", "", false
	}
	circuit.recoverySuccesses++
	if circuit.recoverySuccesses < normalizeRecoverySuccessThreshold(cfg.CircuitBreaker) {
		circuit.nextProbeAt = now.Add(normalizeProbeInterval(cfg.CircuitBreaker))
		a.persistSharedLocked()
		return "", "", "", false
	}
	circuit.state = circuitStateClosed
	circuit.failures = nil
	circuit.halfOpenInFlight = 0
	circuit.recoverySuccesses = 0
	circuit.lastTransitionReason = "probe-success"
	a.persistSharedLocked()
	return previous, circuitStateClosed, "probe-success", true
}

func (a *backendAdmission) recordFailure(pool model.PoolName, name model.BackendName, cfg model.BackendConfig, reason string, now time.Time) (from, to string, changed bool) {
	reason = backend.NormalizeFailureReason(reason)
	if !cfg.CircuitBreaker.Enabled || !tripReasonEnabled(cfg.CircuitBreaker, reason) {
		return "", "", false
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	key := backendAdmissionKey{pool: pool, backend: name}
	circuit := a.circuit(key)
	previous := normalizeCircuitState(circuit.state)
	window := normalizeEvaluationWindow(cfg.CircuitBreaker)
	cutoff := now.Add(-window)
	failures := circuit.failures[:0]
	for _, failure := range circuit.failures {
		if !failure.Before(cutoff) {
			failures = append(failures, failure)
		}
	}
	failures = append(failures, now)
	circuit.failures = failures
	circuit.halfOpenInFlight = 0
	circuit.recoverySuccesses = 0

	if previous == circuitStateHalfOpen || len(failures) >= normalizeFailureThreshold(cfg.CircuitBreaker) {
		circuit.state = circuitStateOpen
		circuit.openedAt = now
		circuit.nextProbeAt = now.Add(normalizeOpenDuration(cfg.CircuitBreaker))
		circuit.lastTransitionReason = reason
		changed := previous != circuitStateOpen
		a.persistSharedLocked()
		return previous, circuitStateOpen, changed
	}
	circuit.state = previous
	a.persistSharedLocked()
	return "", "", false
}

func (a *backendAdmission) probeDue(pool model.PoolName, name model.BackendName, cfg model.BackendConfig, now time.Time) bool {
	if !cfg.CircuitBreaker.Enabled {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	circuit := a.circuit(backendAdmissionKey{pool: pool, backend: name})
	return normalizeCircuitState(circuit.state) == circuitStateOpen && !now.Before(circuit.nextProbeAt)
}

func (a *backendAdmission) deferProbe(pool model.PoolName, name model.BackendName, cfg model.CircuitBreakerConfig, now time.Time, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	circuit := a.circuit(backendAdmissionKey{pool: pool, backend: name})
	circuit.state = circuitStateOpen
	circuit.nextProbeAt = now.Add(normalizeProbeInterval(cfg))
	circuit.recoverySuccesses = 0
	circuit.halfOpenInFlight = 0
	circuit.lastTransitionReason = reason
	a.persistSharedLocked()
}

func (a *backendAdmission) stateSnapshot(cfg model.BrokerConfig) []backendCircuitSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	snapshots := make([]backendCircuitSnapshot, 0)
	for _, pool := range cfg.Pools {
		for name, backendCfg := range pool.Backends {
			if !backendCfg.CircuitBreaker.Enabled {
				continue
			}
			circuit := a.circuit(backendAdmissionKey{pool: pool.Name, backend: name})
			snapshots = append(snapshots, backendCircuitSnapshot{
				Pool:    pool.Name,
				Backend: name,
				State:   normalizeCircuitState(circuit.state),
			})
		}
	}
	return snapshots
}

func (a *backendAdmission) circuit(key backendAdmissionKey) *backendCircuit {
	circuit := a.circuits[key]
	if circuit == nil {
		circuit = &backendCircuit{state: circuitStateClosed}
		a.circuits[key] = circuit
	}
	return circuit
}

type backendCircuitSnapshot struct {
	Pool    model.PoolName
	Backend model.BackendName
	State   string
}

func normalizeCircuitState(state string) string {
	switch state {
	case circuitStateOpen, circuitStateHalfOpen:
		return state
	default:
		return circuitStateClosed
	}
}

func normalizeFailureThreshold(cfg model.CircuitBreakerConfig) int {
	if cfg.FailureThreshold > 0 {
		return cfg.FailureThreshold
	}
	return defaultFailureThreshold
}

func normalizeEvaluationWindow(cfg model.CircuitBreakerConfig) time.Duration {
	if cfg.EvaluationWindow > 0 {
		return cfg.EvaluationWindow
	}
	return defaultEvaluationWindow
}

func normalizeOpenDuration(cfg model.CircuitBreakerConfig) time.Duration {
	if cfg.OpenDuration > 0 {
		return cfg.OpenDuration
	}
	return defaultOpenDuration
}

func normalizeProbeInterval(cfg model.CircuitBreakerConfig) time.Duration {
	if cfg.ProbeInterval > 0 {
		return cfg.ProbeInterval
	}
	return defaultProbeInterval
}

func normalizeProbeTimeout(cfg model.CircuitBreakerConfig) time.Duration {
	if cfg.ProbeTimeout > 0 {
		return cfg.ProbeTimeout
	}
	return defaultProbeTimeout
}

func normalizeRecoverySuccessThreshold(cfg model.CircuitBreakerConfig) int {
	if cfg.RecoverySuccessThreshold > 0 {
		return cfg.RecoverySuccessThreshold
	}
	return defaultRecoverySuccessThreshold
}

func normalizeHalfOpenMaxRequests(cfg model.CircuitBreakerConfig) int {
	if cfg.HalfOpenMaxRequests > 0 {
		return cfg.HalfOpenMaxRequests
	}
	return defaultHalfOpenMaxRequests
}

func tripReasonEnabled(cfg model.CircuitBreakerConfig, reason string) bool {
	reason = backend.NormalizeFailureReason(reason)
	if reason == "" {
		return false
	}
	if len(cfg.TripReasons) == 0 {
		cfg.TripReasons = []string{
			backend.FailureReasonTimeout,
			backend.FailureReasonTransport,
			backend.FailureReasonThrottled,
			backend.FailureReasonServerError,
			backend.FailureReasonWaitTimeout,
		}
	}
	for _, allowed := range cfg.TripReasons {
		if backend.NormalizeFailureReason(allowed) == reason {
			return true
		}
	}
	return false
}

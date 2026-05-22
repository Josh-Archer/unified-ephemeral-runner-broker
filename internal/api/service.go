package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

var ErrUnknownPool = errors.New("pool is not configured")
var ErrNoMatchingBackendCapabilities = errors.New("no backend matches the requested capabilities")
var ErrAllocationNotFound = errors.New("allocation not found")
var ErrAllocationAlreadyCompleted = errors.New("allocation already in terminal state")
var ErrInvalidCompletionState = errors.New("invalid completion state")
var ErrBackendTierBlocked = errors.New("backend tier policy blocked allocation")

const (
	defaultWarmTTL = 15 * time.Minute
	launchModeCold = "cold"
	launchModeWarm = "warm"
)

type Service struct {
	cfg       model.BrokerConfig
	registry  *backend.Registry
	scheds    *scheduler.Registry
	fairShare *scheduler.PriorityFairShare
	store     store.Store
	observer  Observer
	admission *backendAdmission
	tierMgr   *tier.Manager
	warmMu    sync.Mutex
	initErr   error
	health    func(context.Context) error
	now       func() time.Time
}

func NewService(cfg model.BrokerConfig, registry *backend.Registry, health func(context.Context) error) *Service {
	if health == nil {
		health = func(context.Context) error { return nil }
	}
	schedulerRegistry := scheduler.NewRegistry()
	stateStore, storeErr := store.NewFromConfig(cfg.Broker.StateStore)
	if stateStore == nil {
		stateStore = store.NewMemory()
	}
	service := &Service{
		cfg:       cfg,
		registry:  registry,
		scheds:    schedulerRegistry,
		fairShare: scheduler.NewPriorityFairShare(),
		store:     stateStore,
		observer:  noopObserver{},
		admission: newBackendAdmission(),
		tierMgr:   tier.NewManager(),
		health:    health,
		now:       time.Now,
	}
	service.initErr = firstErr(storeErr, validateSchedulers(cfg.Pools, schedulerRegistry), service.rehydrateSchedulerState())
	return service
}

func (s *Service) SetTierManager(manager *tier.Manager) {
	s.tierMgr = manager
}

func (s *Service) SetObserver(observer Observer) {
	if observer == nil {
		s.observer = noopObserver{}
		return
	}
	s.observer = observer
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) rehydrateSchedulerState() error {
	if s.store == nil {
		return nil
	}
	now := s.now()
	for _, status := range s.store.List() {
		if !isSchedulerAccountedState(status.State) {
			continue
		}
		pool, err := s.resolvePool(status.Pool)
		if err != nil {
			s.expireUnrehydratableAllocation(status, now, fmt.Sprintf("rehydrate skipped: %v", err))
			continue
		}
		selected := status.SelectedBackend
		if selected == "" {
			s.expireUnrehydratableAllocation(status, now, "rehydrate skipped: selected backend is empty")
			continue
		}
		if !status.ExpiresAt.IsZero() && !status.ExpiresAt.After(now) {
			s.expireUnrehydratableAllocation(status, now, "rehydrate skipped: allocation expired")
			continue
		}
		request := model.AllocationRequest{
			Pool:          status.Pool,
			Backend:       &selected,
			Tenant:        status.Tenant,
			PriorityClass: status.PriorityClass,
		}
		if _, err := s.reserve(pool, request); err != nil {
			s.expireUnrehydratableAllocation(status, now, fmt.Sprintf("rehydrate skipped: %v", err))
			continue
		}
	}
	return nil
}

func (s *Service) expireUnrehydratableAllocation(status model.AllocationStatus, now time.Time, reason string) {
	if status.ID == "" || s.store == nil {
		return
	}
	nextState := model.StateExpired
	if status.State == model.StateWarm {
		nextState = model.StateFailed
	}
	_, _ = s.store.MarkState(status.ID, nextState, now, reason)
	logAllocationEvent(context.Background(), "allocation_rehydrate_skipped", map[string]string{
		"allocation_id": allocationIDLabel(status.ID),
		"pool":          string(status.Pool),
		"backend":       string(status.SelectedBackend),
		"state":         string(status.State),
		"reason":        reason,
	})
}

func (s *Service) Allocate(ctx context.Context, request model.AllocationRequest) (model.AllocationStatus, error) {
	return s.allocateNow(ctx, request, model.AllocationStatus{})
}

func (s *Service) allocateNow(ctx context.Context, request model.AllocationRequest, existing model.AllocationStatus) (model.AllocationStatus, error) {
	started := s.now()
	resultPool := request.Pool
	var resultBackend model.BackendName
	launchMode := launchModeCold
	result := "failure"
	defer func() {
		s.observer.ObserveAllocationResult(resultPool, resultBackend, result, s.now().Sub(started))
		s.observeState()
	}()

	if s.initErr != nil {
		return model.AllocationStatus{}, s.initErr
	}
	if err := s.health(ctx); err != nil {
		return model.AllocationStatus{}, err
	}

	pool, err := s.resolvePool(request.Pool)
	if err != nil {
		return model.AllocationStatus{}, err
	}
	resultPool = pool.Name

	explicitTimeout := request.JobTimeout > 0
	timeout := request.JobTimeout
	if timeout <= 0 {
		timeout = s.cfg.Broker.DefaultJobTimeout
	}
	request.JobTimeout = timeout

	pool, err = filterEligibleBackends(pool, request)
	if err != nil {
		if queued, ok := s.queueAllocation(ctx, request, resultPool, "", err, existing); ok {
			result = "queued"
			return queued, nil
		}
		return model.AllocationStatus{}, err
	}
	if explicitTimeout {
		pool, err = filterBackendsByTimeout(pool, request)
		if err != nil {
			if queued, ok := s.queueAllocation(ctx, request, resultPool, "", err, existing); ok {
				result = "queued"
				return queued, nil
			}
			return model.AllocationStatus{}, err
		}
	}
	pinnedBackend := request.Backend
	var fallbackFromRateLimit bool
	pool, request, fallbackFromRateLimit, err = s.filterBackendsByAdmission(pool, request)
	if err != nil {
		if queued, ok := s.queueAllocation(ctx, request, resultPool, "", err, existing); ok {
			result = "queued"
			return queued, nil
		}
		return model.AllocationStatus{}, err
	}
	pool, err = s.filterBackendsByTierState(pool, request)
	if err != nil {
		if queued, ok := s.queueAllocation(ctx, request, resultPool, "", err, existing); ok {
			result = "queued"
			return queued, nil
		}
		return model.AllocationStatus{}, err
	}

	reservePool := pool
	reserveRequest := request
	var selected model.BackendName
	var rateLimitedFallback model.BackendName
	for {
		selected, err = s.reserve(reservePool, reserveRequest)
		if err != nil {
			if errors.Is(err, scheduler.ErrNoCapacity) {
				if rateLimitedFallback != "" {
					return model.AllocationStatus{}, fmt.Errorf("selected backend %q is rate-limited and no fallback backend is available: %w", rateLimitedFallback, ErrBackendRateLimited)
				}
				if fallbackFromRateLimit && pinnedBackend != nil {
					return model.AllocationStatus{}, fmt.Errorf("pinned backend %q is rate-limited and no fallback backend is available: %w", *pinnedBackend, ErrBackendRateLimited)
				}
			}
			if queued, ok := s.queueAllocation(ctx, reserveRequest, reservePool.Name, "", err, existing); ok {
				result = "queued"
				return queued, nil
			}
			return model.AllocationStatus{}, err
		}
		if s.admission == nil {
			pool = reservePool
			request = reserveRequest
			break
		}
		decision := s.admission.allow(reservePool.Name, selected, reservePool.Backends[selected], s.now(), false, true)
		if !decision.Allowed {
			s.release(context.Background(), reservePool, selected, model.AllocationStatus{Pool: reservePool.Name, SelectedBackend: selected})
			s.observer.ObserveBackendAdmissionRejected(reservePool.Name, selected, decision.Reason)
			switch decision.Reason {
			case "circuit-open":
				err := fmt.Errorf("selected backend %q is not admissible: %w", selected, ErrBackendCircuitOpen)
				if queued, ok := s.queueAllocation(ctx, reserveRequest, reservePool.Name, selected, err, existing); ok {
					result = "queued"
					return queued, nil
				}
				return model.AllocationStatus{}, err
			case "rate-limited":
				rateLimitedFallback = selected
				reservePool = withoutBackend(reservePool, selected)
				reserveRequest.Backend = nil
				if len(reservePool.Backends) > 0 {
					continue
				}
				err := fmt.Errorf("selected backend %q is rate-limited and no fallback backend is available: %w", selected, ErrBackendRateLimited)
				return model.AllocationStatus{}, err
			default:
				err := fmt.Errorf("selected backend %q is not admissible: %s", selected, decision.Reason)
				if queued, ok := s.queueAllocation(ctx, reserveRequest, reservePool.Name, selected, err, existing); ok {
					result = "queued"
					return queued, nil
				}
				return model.AllocationStatus{}, err
			}
		}
		pool = reservePool
		request = reserveRequest
		break
	}
	resultBackend = selected
	s.observer.ObserveAllocationStart(pool.Name)
	logAllocationEvent(ctx, "allocation_admitted", map[string]string{
		"pool":    string(pool.Name),
		"backend": string(selected),
	})

	allocation := s.prepareAllocation(ctx, request, existing, pool.Name, selected, timeout)

	s.store.Save(allocation)

	backendImpl, ok := s.registry.Get(selected)
	if !ok {
		s.release(context.Background(), pool, selected, allocation)
		s.store.MarkState(allocation.ID, model.StateFailed, s.now(), "backend not registered")
		return model.AllocationStatus{}, fmt.Errorf("backend implementation missing: %s", selected)
	}

	if warmAllocation, ok := s.consumeWarmAllocation(ctx, pool, selected, allocation); ok {
		s.schedulerForPool(pool).Release(pool.Name, selected, allocation)
		launchMode = launchModeWarm
		warmAllocation.State = model.StateReady
		warmAllocation.CorrelationID = allocation.CorrelationID
		warmAllocation.Tenant = request.Tenant
		warmAllocation.PriorityClass = request.PriorityClass
		warmAllocation.RequestedLabels = append([]string(nil), request.Labels...)
		warmAllocation.ExpiresAt = allocation.ExpiresAt
		warmAllocation.Metadata = withLaunchModeMetadata(
			backend.WithCapabilitiesMetadata(pool.Backends[selected], warmAllocation.Metadata),
			launchMode,
		)
		s.store.Save(warmAllocation)
		launchLatency := time.Duration(0)
		s.observer.ObserveLaunchLatency(pool.Name, selected, launchMode, launchLatency)
		s.observer.ObserveRegistrationLatency(pool.Name, selected, launchLatency)
		result = "success"
		allocation = warmAllocation
		logAllocationEvent(ctx, "allocation_ready", map[string]string{
			"allocation_id": allocation.ID,
			"pool":          string(pool.Name),
			"backend":       string(selected),
			"launch_mode":   launchMode,
		})
		return allocation, nil
	}

	launchStarted := s.now()
	provisioned, err := backendImpl.Provision(ctx, request, allocation)
	launchLatency := s.now().Sub(launchStarted)
	s.observer.ObserveLaunchLatency(pool.Name, selected, launchMode, launchLatency)
	s.observer.ObserveRegistrationLatency(pool.Name, selected, launchLatency)
	if err != nil {
		s.release(context.Background(), pool, selected, allocation)
		s.recordBackendFailure(pool, selected, err, s.now())
		if queued, ok := s.queueAllocation(ctx, request, pool.Name, selected, err, allocation); ok {
			result = "queued"
			return queued, nil
		}
		s.store.MarkState(allocation.ID, model.StateFailed, s.now(), err.Error())
		logAllocationEvent(ctx, "allocation_failed", map[string]string{
			"allocation_id": allocation.ID,
			"pool":          string(pool.Name),
			"backend":       string(selected),
			"error":         err.Error(),
		})
		return model.AllocationStatus{}, err
	}

	allocation.RunnerLabel = provisioned.RunnerLabel
	allocation.Metadata = withLaunchModeMetadata(
		backend.WithCapabilitiesMetadata(pool.Backends[selected], provisioned.Metadata),
		launchMode,
	)
	allocation.State = model.StateReady
	s.store.Save(allocation)
	s.recordBackendSuccess(pool, selected, s.now())
	result = "success"
	logAllocationEvent(ctx, "allocation_ready", map[string]string{
		"allocation_id": allocation.ID,
		"pool":          string(pool.Name),
		"backend":       string(selected),
		"launch_mode":   launchMode,
	})

	return allocation, nil
}

func (s *Service) prepareAllocation(ctx context.Context, request model.AllocationRequest, existing model.AllocationStatus, pool model.PoolName, selected model.BackendName, timeout time.Duration) model.AllocationStatus {
	allocation := existing
	if allocation.ID == "" {
		allocation.ID = newID()
		allocation.CorrelationID = correlationIDFromContext(ctx)
		allocation.Attempts = 0
	}
	allocation.Pool = pool
	allocation.SelectedBackend = selected
	allocation.Tenant = request.Tenant
	allocation.PriorityClass = request.PriorityClass
	allocation.RequestedLabels = append([]string(nil), request.Labels...)
	allocation.Metadata = map[string]string{backend.MetadataLaunchModeKey: launchModeCold}
	allocation.ExpiresAt = s.now().Add(timeout)
	allocation.RetryAfter = time.Time{}
	allocation.State = model.StateReserved
	allocation.Error = ""
	requestCopy := request
	allocation.Request = &requestCopy
	return allocation
}

func (s *Service) queueAllocation(ctx context.Context, request model.AllocationRequest, pool model.PoolName, selected model.BackendName, cause error, existing model.AllocationStatus) (model.AllocationStatus, bool) {
	if !s.queueEnabled() || !queueableError(cause) {
		return model.AllocationStatus{}, false
	}
	if existing.ID != "" && existing.Attempts >= normalizeQueueMaxAttempts(s.cfg.Broker.Queue) {
		return model.AllocationStatus{}, false
	}

	now := s.now()
	timeout := request.JobTimeout
	if timeout <= 0 {
		timeout = s.cfg.Broker.DefaultJobTimeout
	}
	if pool == "" {
		pool = request.Pool
	}
	status := existing
	if status.ID == "" {
		status.ID = newID()
		status.CorrelationID = correlationIDFromContext(ctx)
		status.ExpiresAt = now.Add(timeout)
	}
	status.Pool = pool
	status.SelectedBackend = selected
	status.Tenant = request.Tenant
	status.PriorityClass = request.PriorityClass
	status.RequestedLabels = append([]string(nil), request.Labels...)
	status.RetryAfter = now.Add(normalizeQueueRetryAfter(s.cfg.Broker.Queue))
	status.State = model.StatePending
	status.Error = cause.Error()
	requestCopy := request
	requestCopy.JobTimeout = timeout
	status.Request = &requestCopy
	if status.Metadata == nil {
		status.Metadata = map[string]string{}
	}
	status.Metadata["queue_reason"] = queueReason(cause)
	_ = s.store.Save(status)
	logAllocationEvent(ctx, "allocation_pending", map[string]string{
		"allocation_id": allocationIDLabel(status.ID),
		"pool":          string(status.Pool),
		"backend":       string(status.SelectedBackend),
		"reason":        status.Metadata["queue_reason"],
	})
	return status, true
}

func (s *Service) ReconcileQueue(ctx context.Context, now time.Time) int {
	if !s.queueEnabled() {
		return 0
	}
	updated := 0
	for _, status := range s.store.List() {
		if status.State != model.StatePending {
			continue
		}
		if !status.RetryAfter.IsZero() && status.RetryAfter.After(now) {
			continue
		}
		if status.Request == nil {
			s.store.MarkState(status.ID, model.StateFailed, now, "pending allocation is missing original request")
			updated++
			continue
		}
		if status.ExpiresAt.Before(now) {
			s.store.MarkState(status.ID, model.StateExpired, now, "pending allocation expired")
			updated++
			continue
		}
		if status.Attempts >= normalizeQueueMaxAttempts(s.cfg.Broker.Queue) {
			s.store.MarkState(status.ID, model.StateFailed, now, "pending allocation retry attempts exhausted")
			updated++
			continue
		}
		status.Attempts++
		status.RetryAfter = now.Add(normalizeQueueRetryAfter(s.cfg.Broker.Queue))
		_ = s.store.Save(status)
		if _, err := s.allocateNow(ctx, *status.Request, status); err != nil {
			if queued, ok := s.queueAllocation(ctx, *status.Request, status.Pool, status.SelectedBackend, err, status); ok {
				_ = s.store.Save(queued)
			} else {
				s.store.MarkState(status.ID, model.StateFailed, now, err.Error())
			}
		}
		updated++
	}
	s.observeState()
	return updated
}

func (s *Service) ReconcileWarmPools() {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()

	now := s.now()
	statuses := s.store.List()
	for _, pool := range s.cfg.Pools {
		s.reconcileWarmForPool(pool, statuses, now)
	}
}

func (s *Service) Health(ctx context.Context) error {
	if s.initErr != nil {
		return s.initErr
	}
	return s.health(ctx)
}

func (s *Service) Get(id string) (model.AllocationStatus, bool) {
	return s.store.Get(id)
}

func (s *Service) Cancel(id string) (model.AllocationStatus, bool) {
	status, ok := s.store.MarkState(id, model.StateCanceled, s.now(), "")
	if !ok {
		return model.AllocationStatus{}, false
	}
	if pool, err := s.resolvePool(status.Pool); err == nil {
		s.release(context.Background(), pool, status.SelectedBackend, status)
	}
	s.observeState()
	return status, true
}

type completionRequest struct {
	State        string `json:"state"`
	Error        string `json:"error"`
	Reason       string `json:"reason"`
	FailureClass string `json:"failure_class"`
}

func (s *Service) Complete(ctx context.Context, id string, request completionRequest) (model.AllocationStatus, bool, error) {
	status, ok := s.store.Get(id)
	if !ok {
		return model.AllocationStatus{}, false, nil
	}

	targetState, message, err := parseCompletionState(request)
	if err != nil {
		return model.AllocationStatus{}, false, err
	}
	if strings.TrimSpace(message) == "" {
		message = defaultCompletionMessage(targetState)
	}
	if isTerminalAllocationState(status.State) {
		if status.State == targetState {
			return status, true, nil
		}
		return status, true, fmt.Errorf("%w: current=%s requested=%s", ErrAllocationAlreadyCompleted, status.State, targetState)
	}
	if status.State == targetState {
		return status, true, nil
	}

	updated, ok := s.store.MarkState(id, targetState, s.now(), message)
	if !ok {
		return model.AllocationStatus{}, false, nil
	}
	if isActiveAllocationState(status.State) {
		if pool, err := s.resolvePool(status.Pool); err == nil {
			s.release(ctx, pool, status.SelectedBackend, status)
			if targetState == model.StateFailed {
				s.recordBackendFailureClass(pool, status.SelectedBackend, request.FailureClass, s.now())
			}
		}
	}
	logAllocationEvent(ctx, completionEventName(targetState), map[string]string{
		"allocation_id": allocationIDLabel(status.ID),
		"pool":          string(status.Pool),
		"backend":       string(status.SelectedBackend),
		"state":         string(targetState),
		"error":         message,
	})
	s.observeState()
	return updated, true, nil
}

func (s *Service) SweepExpired(now time.Time) int {
	updated := 0
	for _, status := range s.store.List() {
		if isActiveAllocationState(status.State) {
			if status.ExpiresAt.After(now) {
				continue
			}

			nextState := model.StateExpired
			nextMessage := "allocation expired"
			nextExpiresAt := now
			if s.cfg.Broker.OrphanCleanup.Enabled {
				nextState = model.StateQuarantined
				nextMessage = "allocation quarantined"
				nextExpiresAt = now
				if s.cfg.Broker.OrphanCleanup.QuarantineTTL > 0 {
					nextExpiresAt = now.Add(s.cfg.Broker.OrphanCleanup.QuarantineTTL)
				}
			}
			if _, ok := s.store.MarkState(status.ID, nextState, nextExpiresAt, nextMessage); ok {
				if pool, err := s.resolvePool(status.Pool); err == nil {
					s.release(context.Background(), pool, status.SelectedBackend, status)
					s.recordBackendFailureClass(pool, status.SelectedBackend, backend.FailureReasonWaitTimeout, now)
				}
				logAllocationEvent(context.Background(), "allocation_"+string(nextState), map[string]string{
					"allocation_id": allocationIDLabel(status.ID),
					"pool":          string(status.Pool),
					"backend":       string(status.SelectedBackend),
				})
				updated++
			}
			continue
		}

		if status.State != model.StateQuarantined {
			continue
		}

		if status.ExpiresAt.After(now) {
			continue
		}
		if _, ok := s.store.MarkState(status.ID, model.StateExpired, now, "allocation quarantine expired"); ok {
			if pool, err := s.resolvePool(status.Pool); err == nil {
				s.release(context.Background(), pool, status.SelectedBackend, status)
			}
			logAllocationEvent(context.Background(), "allocation_expired", map[string]string{
				"allocation_id": allocationIDLabel(status.ID),
				"pool":          string(status.Pool),
				"backend":       string(status.SelectedBackend),
			})
			updated++
		}
	}
	s.observeState()
	return updated
}

func (s *Service) ReconcileBackendHealth() {
	if s.admission == nil {
		return
	}
	now := s.now()
	for _, pool := range s.cfg.Pools {
		for backendName, cfg := range pool.Backends {
			if !cfg.Enabled || !cfg.Healthy || !s.admission.probeDue(pool.Name, backendName, cfg, now) {
				continue
			}
			backendImpl, ok := s.registry.Get(backendName)
			if !ok {
				continue
			}
			probe, ok := backendImpl.(backendProbe)
			if !ok {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), normalizeProbeTimeout(cfg.CircuitBreaker))
			err := probe.Probe(ctx, pool, cfg)
			cancel()
			if err != nil {
				s.admission.deferProbe(pool.Name, backendName, cfg.CircuitBreaker, now, backend.FailureReasonProbeFailed)
				s.observer.ObserveBackendProbe(pool.Name, backendName, "failure")
				logAllocationEvent(context.Background(), "backend_probe_failed", map[string]string{
					"pool":    string(pool.Name),
					"backend": string(backendName),
					"error":   err.Error(),
				})
				continue
			}
			s.observer.ObserveBackendProbe(pool.Name, backendName, "success")
			s.recordBackendProbeSuccess(pool, backendName, now)
		}
	}
	s.observeCircuitState()
}

func (s *Service) observeState() {
	statuses := s.store.List()
	s.observer.ObserveActiveAllocations(statuses)
	s.observer.ObserveCapacity(s.cfg, statuses)
	s.observeCircuitState()
	s.observeTierState()
}

func (s *Service) observeCircuitState() {
	if s.admission == nil {
		return
	}
	s.observer.ObserveBackendCircuitState(s.admission.stateSnapshot(s.cfg))
}

func (s *Service) observeTierState() {
	if s.tierMgr == nil {
		return
	}
	decisions := s.tierMgr.Snapshot()
	snapshots := make([]tierDecisionSnapshot, 0, len(decisions))
	for _, decision := range decisions {
		snapshots = append(snapshots, tierDecisionSnapshot{
			Pool:    decision.Pool,
			Backend: decision.Backend,
			State:   decision.State,
			Stale:   decision.Stale,
		})
	}
	s.observer.ObserveTierState(snapshots)
}

func (s *Service) consumeWarmAllocation(ctx context.Context, pool model.PoolConfig, backendName model.BackendName, request model.AllocationStatus) (model.AllocationStatus, bool) {
	cfg, ok := pool.Backends[backendName]
	if !ok || !cfg.Enabled || !cfg.Healthy || !isWarmProvisionableBackend(backendName) {
		return model.AllocationStatus{}, false
	}

	s.warmMu.Lock()
	defer s.warmMu.Unlock()

	ttl := resolveWarmTTL(cfg)
	now := s.now()
	warm := filterWarmAllocations(s.store.List(), pool.Name, backendName)
	if len(warm) == 0 {
		return model.AllocationStatus{}, false
	}

	sortWarmByExpiration(warm)
	for _, candidate := range warm {
		if isWarmExpired(candidate, now, ttl) {
			s.recycleWarmAllocation(ctx, pool, candidate, "warm allocation expired")
			continue
		}
		return candidate, true
	}

	return model.AllocationStatus{}, false
}

func (s *Service) recycleWarmAllocation(ctx context.Context, pool model.PoolConfig, status model.AllocationStatus, reason string) {
	updated, ok := s.store.MarkState(status.ID, model.StateFailed, s.now(), reason)
	if !ok {
		return
	}
	s.release(ctx, pool, status.SelectedBackend, updated)
}

func (s *Service) reconcileWarmForPool(pool model.PoolConfig, statuses []model.AllocationStatus, now time.Time) {
	for backendName, cfg := range pool.Backends {
		if !isWarmProvisionableBackend(backendName) {
			continue
		}
		s.reconcileWarmForBackend(pool, backendName, cfg, statuses, now)
	}
}

func (s *Service) reconcileWarmForBackend(pool model.PoolConfig, backendName model.BackendName, cfg model.BackendConfig, statuses []model.AllocationStatus, now time.Time) {
	warmMin, warmMax := normalizeWarmBounds(cfg)
	ttl := resolveWarmTTL(cfg)

	warm := filterWarmAllocations(statuses, pool.Name, backendName)

	if !cfg.Enabled || !cfg.Healthy || cfg.MaxRunners <= 0 {
		for _, warmStatus := range warm {
			s.recycleWarmAllocation(context.Background(), pool, warmStatus, "warm backend disabled or unhealthy")
		}
		return
	}

	if warmMax <= 0 {
		for _, warmStatus := range warm {
			s.recycleWarmAllocation(context.Background(), pool, warmStatus, "warm disabled")
		}
		return
	}
	if s.backendTierBlockedForWarm(pool.Name, backendName) {
		for _, warmStatus := range warm {
			s.recycleWarmAllocation(context.Background(), pool, warmStatus, "warm backend blocked by tier policy")
		}
		return
	}

	sortWarmByExpiration(warm)
	for _, status := range warm {
		if isWarmExpired(status, now, ttl) {
			s.recycleWarmAllocation(context.Background(), pool, status, "warm allocation expired")
		}
	}

	warm = filterFreshWarm(s.store.List(), pool.Name, backendName, now, ttl)
	if len(warm) > warmMax {
		excess := len(warm) - warmMax
		for _, status := range warm[len(warm)-excess:] {
			s.recycleWarmAllocation(context.Background(), pool, status, "warm pool at max capacity")
		}
		warm = warm[:warmMax]
	}

	target := normalizeWarmTarget(warmMin, warmMax, cfg.MaxRunners)
	for len(warm) < target {
		if err := s.createWarmAllocation(pool, backendName, now); err != nil {
			return
		}
		warm = append(warm, model.AllocationStatus{})
	}
}

func (s *Service) createWarmAllocation(pool model.PoolConfig, backendName model.BackendName, now time.Time) error {
	cfg, ok := pool.Backends[backendName]
	if !ok {
		return fmt.Errorf("backend %q is not configured for pool %q", backendName, pool.Name)
	}
	if err := s.validateWarmBackend(pool, backendName); err != nil {
		return err
	}
	if s.admission != nil {
		decision := s.admission.allow(pool.Name, backendName, cfg, now, false, true)
		if !decision.Allowed {
			return fmt.Errorf("warm backend %q is not admissible: %s", backendName, decision.Reason)
		}
	}
	if s.backendTierBlockedForWarm(pool.Name, backendName) {
		return fmt.Errorf("warm backend %q is blocked by tier policy", backendName)
	}

	ttl := resolveWarmTTL(cfg)
	request := model.AllocationRequest{
		Pool:    pool.Name,
		Backend: &backendName,
	}
	if cfg.MaxJobDuration > 0 {
		request.JobTimeout = cfg.MaxJobDuration
	} else {
		request.JobTimeout = s.cfg.Broker.DefaultJobTimeout
	}

	selection, err := s.reserveForBackend(pool, backendName)
	if err != nil {
		return err
	}
	if selection != backendName {
		return fmt.Errorf("expected warm reservation for %s, got %s", backendName, selection)
	}

	allocation := model.AllocationStatus{
		ID:              newID(),
		Pool:            pool.Name,
		SelectedBackend: backendName,
		CorrelationID:   "",
		Metadata:        map[string]string{backend.MetadataLaunchModeKey: launchModeWarm},
		Tenant:          "",
		PriorityClass:   "",
		ExpiresAt:       now.Add(ttl),
		State:           model.StateReserved,
	}

	backendImpl, ok := s.registry.Get(backendName)
	if !ok {
		s.release(context.Background(), pool, backendName, allocation)
		return fmt.Errorf("backend implementation missing: %s", backendName)
	}

	provisioned, err := backendImpl.Provision(context.Background(), request, allocation)
	if err != nil {
		s.release(context.Background(), pool, backendName, allocation)
		s.recordBackendFailure(pool, backendName, err, now)
		return err
	}
	s.recordBackendSuccess(pool, backendName, now)

	allocation.RunnerLabel = provisioned.RunnerLabel
	allocation.State = model.StateWarm
	allocation.Metadata = withLaunchModeMetadata(
		backend.WithCapabilitiesMetadata(cfg, provisioned.Metadata),
		launchModeWarm,
	)
	allocation.ExpiresAt = now.Add(ttl)
	s.store.Save(allocation)
	return nil
}

func (s *Service) reserveForBackend(pool model.PoolConfig, backendName model.BackendName) (model.BackendName, error) {
	request := model.AllocationRequest{
		Pool:    pool.Name,
		Backend: &backendName,
	}
	return s.reserve(pool, request)
}

func (s *Service) filterBackendsByAdmission(pool model.PoolConfig, request model.AllocationRequest) (model.PoolConfig, model.AllocationRequest, bool, error) {
	if s.admission == nil {
		return pool, request, false, nil
	}
	filtered, err := s.admission.filter(pool, request, s.now())
	if err != nil {
		if request.Backend != nil {
			reason := "circuit-open"
			if errors.Is(err, ErrBackendRateLimited) {
				reason = "rate-limited"
			}
			s.observer.ObserveBackendAdmissionRejected(pool.Name, *request.Backend, reason)
			if errors.Is(err, ErrBackendRateLimited) {
				fallbackRequest := request
				fallbackRequest.Backend = nil
				fallback, fallbackErr := s.admission.filter(pool, fallbackRequest, s.now())
				if fallbackErr == nil {
					s.observeAdmissionRejections(pool, fallback)
					return fallback, fallbackRequest, true, nil
				}
				return model.PoolConfig{}, request, false, fmt.Errorf("pinned backend %q is rate-limited and no fallback backend is available: %w", *request.Backend, fallbackErr)
			}
		}
		return model.PoolConfig{}, request, false, err
	}
	s.observeAdmissionRejections(pool, filtered)
	return filtered, request, false, nil
}

func (s *Service) observeAdmissionRejections(pool model.PoolConfig, filtered model.PoolConfig) {
	for name := range pool.Backends {
		if _, ok := filtered.Backends[name]; !ok {
			decision := s.admission.allow(pool.Name, name, pool.Backends[name], s.now(), false, false)
			if !decision.Allowed {
				s.observer.ObserveBackendAdmissionRejected(pool.Name, name, decision.Reason)
			}
		}
	}
}

func withoutBackend(pool model.PoolConfig, name model.BackendName) model.PoolConfig {
	filtered := pool
	filtered.Backends = make(map[model.BackendName]model.BackendConfig, len(pool.Backends))
	for candidate, cfg := range pool.Backends {
		if candidate == name {
			continue
		}
		filtered.Backends[candidate] = cfg
	}
	return filtered
}

func (s *Service) filterBackendsByTierState(pool model.PoolConfig, request model.AllocationRequest) (model.PoolConfig, error) {
	if !s.cfg.Broker.TierRouting.Enabled || s.tierMgr == nil {
		return pool, nil
	}
	eligible, deprioritized, blocked := s.tierEligibleBackends(pool)
	if request.Backend != nil {
		if _, ok := pool.Backends[*request.Backend]; !ok {
			return model.PoolConfig{}, scheduler.ErrUnknownBackend
		}
		if s.pinnedBackendAllowedByTier(pool.Name, *request.Backend) {
			return pool, nil
		}
		if _, ok := eligible.Backends[*request.Backend]; ok {
			return eligible, nil
		}
		if _, ok := deprioritized.Backends[*request.Backend]; ok {
			return deprioritized, nil
		}
		decision := blocked[*request.Backend]
		s.observer.ObserveTierBlocked(pool.Name, *request.Backend, tierBlockReason(decision))
		return model.PoolConfig{}, fmt.Errorf("pinned backend %q is blocked by tier policy: %w", *request.Backend, ErrBackendTierBlocked)
	}
	if len(eligible.Backends) > 0 {
		return eligible, nil
	}
	if len(deprioritized.Backends) > 0 {
		s.observer.ObserveTierFallback(pool.Name, "deprioritize", "approaching")
		return deprioritized, nil
	}
	if len(blocked) == 0 {
		return pool, nil
	}

	mode := normalizeTierFailureMode(s.cfg.Broker.TierRouting.FailureMode)
	switch mode {
	case tier.FailureModeBlock:
		s.observer.ObserveTierFallback(pool.Name, mode, "all-backends-blocked")
		return model.PoolConfig{}, fmt.Errorf("%w for pool %q", ErrBackendTierBlocked, pool.Name)
	case tier.FailureModeFallback:
		fallback := filterTierFallbackBackends(pool, s.cfg.Broker.TierRouting.FallbackBackends)
		if len(fallback.Backends) > 0 {
			s.observer.ObserveTierFallback(pool.Name, mode, "fallback-backends")
			return fallback, nil
		}
		s.observer.ObserveTierFallback(pool.Name, mode, "fallback-empty")
		return model.PoolConfig{}, fmt.Errorf("%w for pool %q: no fallback backends available", ErrBackendTierBlocked, pool.Name)
	default:
		s.observer.ObserveTierFallback(pool.Name, tier.FailureModePassThrough, "pass-through")
		return pool, nil
	}
}

func (s *Service) tierEligibleBackends(pool model.PoolConfig) (model.PoolConfig, model.PoolConfig, map[model.BackendName]tier.Decision) {
	filtered := pool
	filtered.Backends = make(map[model.BackendName]model.BackendConfig, len(pool.Backends))
	deprioritized := pool
	deprioritized.Backends = make(map[model.BackendName]model.BackendConfig, len(pool.Backends))
	blocked := map[model.BackendName]tier.Decision{}
	for name, cfg := range pool.Backends {
		decision, ok := s.tierMgr.Decision(pool.Name, name)
		if !ok {
			decision = tier.Decision{
				Pool:    pool.Name,
				Backend: name,
				State:   tier.StateUnknown,
				Action:  tier.ActionDisable,
				Reason:  "missing tier data",
			}
		}
		if s.tierDecisionAllowsBackendNormally(decision) {
			if !s.backendHasFreeSchedulerCapacity(pool, name, cfg) {
				continue
			}
			filtered.Backends[name] = cfg
			continue
		}
		if tierDecisionIsDeprioritized(decision) {
			if !s.backendHasFreeSchedulerCapacity(pool, name, cfg) {
				continue
			}
			deprioritized.Backends[name] = cfg
			continue
		}
		blocked[name] = decision
		s.observer.ObserveTierBlocked(pool.Name, name, tierBlockReason(decision))
	}
	return filtered, deprioritized, blocked
}

func (s *Service) pinnedBackendAllowedByTier(pool model.PoolName, backendName model.BackendName) bool {
	decision, ok := s.tierMgr.Decision(pool, backendName)
	if !ok {
		decision = tier.Decision{
			Pool:    pool,
			Backend: backendName,
			State:   tier.StateUnknown,
			Action:  tier.ActionDisable,
			Reason:  "missing tier data",
		}
	}
	if decision.Action == tier.ActionObserveOnly {
		return true
	}
	if decision.Stale || decision.State == tier.StateUnknown {
		return normalizeTierFailureMode(s.cfg.Broker.TierRouting.FailureMode) == tier.FailureModePassThrough
	}
	if decision.State == tier.StateHealthy {
		return true
	}
	if decision.State == tier.StateApproaching {
		return decision.Action != tier.ActionDisable
	}
	return false
}

func (s *Service) tierDecisionAllowsBackendNormally(decision tier.Decision) bool {
	if decision.Action == tier.ActionObserveOnly {
		return true
	}
	if decision.Stale || decision.State == tier.StateUnknown {
		return normalizeTierFailureMode(s.cfg.Broker.TierRouting.FailureMode) == tier.FailureModePassThrough
	}
	if decision.State == tier.StateHealthy {
		return true
	}
	return false
}

func tierDecisionIsDeprioritized(decision tier.Decision) bool {
	return !decision.Stale && decision.State == tier.StateApproaching && decision.Action == tier.ActionDeprioritize
}

func (s *Service) backendTierBlockedForWarm(pool model.PoolName, backendName model.BackendName) bool {
	if !s.cfg.Broker.TierRouting.Enabled || s.tierMgr == nil {
		return false
	}
	decision, ok := s.tierMgr.Decision(pool, backendName)
	if !ok {
		return normalizeTierFailureMode(s.cfg.Broker.TierRouting.FailureMode) == tier.FailureModeBlock
	}
	if decision.Action == tier.ActionObserveOnly {
		return false
	}
	if decision.Stale || decision.State == tier.StateUnknown {
		return normalizeTierFailureMode(s.cfg.Broker.TierRouting.FailureMode) == tier.FailureModeBlock
	}
	return decision.State == tier.StateExceeded || (decision.State == tier.StateApproaching && decision.Action == tier.ActionDisable)
}

func (s *Service) backendHasFreeSchedulerCapacity(pool model.PoolConfig, backendName model.BackendName, cfg model.BackendConfig) bool {
	if !cfg.Enabled || !cfg.Healthy || cfg.MaxRunners <= 0 {
		return false
	}
	active := 0
	if pool.FairShare.Enabled {
		if s.fairShare != nil {
			active = s.fairShare.Active(pool.Name, backendName)
		}
	} else {
		active = s.schedulerForPool(pool).Active(pool.Name, backendName)
	}
	return active < cfg.MaxRunners
}

func filterTierFallbackBackends(pool model.PoolConfig, fallbackBackends []model.BackendName) model.PoolConfig {
	filtered := pool
	filtered.Backends = make(map[model.BackendName]model.BackendConfig, len(pool.Backends))
	allowed := map[model.BackendName]struct{}{}
	for _, backendName := range fallbackBackends {
		allowed[backendName] = struct{}{}
	}
	for name, cfg := range pool.Backends {
		if _, ok := allowed[name]; ok {
			filtered.Backends[name] = cfg
		}
	}
	return filtered
}

func normalizeTierFailureMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case tier.FailureModeBlock:
		return tier.FailureModeBlock
	case tier.FailureModeFallback:
		return tier.FailureModeFallback
	default:
		return tier.FailureModePassThrough
	}
}

func tierBlockReason(decision tier.Decision) string {
	if decision.State == "" {
		return "unknown"
	}
	if decision.Stale {
		return "stale-" + decision.State
	}
	return decision.State
}

func (s *Service) recordBackendFailure(pool model.PoolConfig, backendName model.BackendName, err error, now time.Time) {
	reason, ok := backend.FailureReason(err)
	if !ok {
		return
	}
	s.recordBackendFailureClass(pool, backendName, reason, now)
}

func (s *Service) recordBackendFailureClass(pool model.PoolConfig, backendName model.BackendName, reason string, now time.Time) {
	reason = backend.NormalizeFailureReason(reason)
	if reason == "" || s.admission == nil {
		return
	}
	cfg, ok := pool.Backends[backendName]
	if !ok {
		return
	}
	from, to, changed := s.admission.recordFailure(pool.Name, backendName, cfg, reason, now)
	if !changed {
		return
	}
	s.observer.ObserveBackendCircuitTransition(pool.Name, backendName, from, to, reason)
	logAllocationEvent(context.Background(), "backend_circuit_opened", map[string]string{
		"pool":    string(pool.Name),
		"backend": string(backendName),
		"from":    from,
		"to":      to,
		"reason":  reason,
	})
}

func (s *Service) recordBackendSuccess(pool model.PoolConfig, backendName model.BackendName, now time.Time) {
	if s.admission == nil {
		return
	}
	cfg, ok := pool.Backends[backendName]
	if !ok {
		return
	}
	from, to, reason, changed := s.admission.recordSuccess(pool.Name, backendName, cfg, now)
	if !changed {
		return
	}
	s.observer.ObserveBackendCircuitTransition(pool.Name, backendName, from, to, reason)
	logAllocationEvent(context.Background(), "backend_circuit_closed", map[string]string{
		"pool":    string(pool.Name),
		"backend": string(backendName),
		"from":    from,
		"to":      to,
		"reason":  reason,
	})
}

func (s *Service) recordBackendProbeSuccess(pool model.PoolConfig, backendName model.BackendName, now time.Time) {
	if s.admission == nil {
		return
	}
	cfg, ok := pool.Backends[backendName]
	if !ok {
		return
	}
	from, to, reason, changed := s.admission.recordProbeSuccess(pool.Name, backendName, cfg, now)
	if !changed {
		return
	}
	s.observer.ObserveBackendCircuitTransition(pool.Name, backendName, from, to, reason)
	logAllocationEvent(context.Background(), "backend_circuit_closed", map[string]string{
		"pool":    string(pool.Name),
		"backend": string(backendName),
		"from":    from,
		"to":      to,
		"reason":  reason,
	})
}

func (s *Service) validateWarmBackend(pool model.PoolConfig, backendName model.BackendName) error {
	cfg, ok := pool.Backends[backendName]
	if !ok {
		return fmt.Errorf("backend %q is not configured for pool %q", backendName, pool.Name)
	}
	if !cfg.Enabled {
		return fmt.Errorf("backend %q is not enabled", backendName)
	}
	if !cfg.Healthy {
		return fmt.Errorf("backend %q is unhealthy", backendName)
	}
	if !isWarmProvisionableBackend(backendName) {
		return fmt.Errorf("backend %q does not support warm provisioning", backendName)
	}
	return nil
}

func resolveWarmTTL(cfg model.BackendConfig) time.Duration {
	if cfg.WarmTTL > 0 {
		return cfg.WarmTTL
	}
	return defaultWarmTTL
}

func normalizeWarmBounds(cfg model.BackendConfig) (int, int) {
	min := cfg.WarmMin
	max := cfg.WarmMax
	if min < 0 {
		min = 0
	}
	if max < 0 {
		max = 0
	}
	if max < min {
		max = min
	}
	return min, max
}

func normalizeWarmTarget(min, max, maxRunners int) int {
	if maxRunners <= 0 {
		return 0
	}
	if max > maxRunners {
		max = maxRunners
	}
	if min < 0 {
		min = 0
	}
	if min > max {
		min = max
	}
	return min
}

func isWarmExpired(status model.AllocationStatus, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	return !status.ExpiresAt.IsZero() && !now.Before(status.ExpiresAt)
}

func isWarmProvisionableBackend(backendName model.BackendName) bool {
	switch backendName {
	case model.BackendARC, model.BackendAzureVM:
		return false
	default:
		return true
	}
}

func filterWarmAllocations(statuses []model.AllocationStatus, poolName model.PoolName, backendName model.BackendName) []model.AllocationStatus {
	result := make([]model.AllocationStatus, 0)
	for _, status := range statuses {
		if status.Pool != poolName {
			continue
		}
		if status.SelectedBackend != backendName {
			continue
		}
		if status.State != model.StateWarm {
			continue
		}
		result = append(result, status)
	}
	return result
}

func filterFreshWarm(statuses []model.AllocationStatus, poolName model.PoolName, backendName model.BackendName, now time.Time, ttl time.Duration) []model.AllocationStatus {
	result := make([]model.AllocationStatus, 0)
	for _, status := range statuses {
		if status.Pool != poolName {
			continue
		}
		if status.SelectedBackend != backendName {
			continue
		}
		if status.State != model.StateWarm {
			continue
		}
		if !isWarmExpired(status, now, ttl) {
			result = append(result, status)
		}
	}
	return result
}

func sortWarmByExpiration(warm []model.AllocationStatus) {
	sort.Slice(warm, func(i, j int) bool {
		if !warm[i].ExpiresAt.Equal(warm[j].ExpiresAt) {
			return warm[i].ExpiresAt.Before(warm[j].ExpiresAt)
		}
		return warm[i].ID < warm[j].ID
	})
}

func withLaunchModeMetadata(metadata map[string]string, mode string) map[string]string {
	if mode == "" {
		return metadata
	}
	cloned := copyStringMap(metadata)
	cloned[backend.MetadataLaunchModeKey] = mode
	return cloned
}

func copyStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return map[string]string{}
	}
	copied := make(map[string]string, len(source))
	for key, value := range source {
		copied[key] = value
	}
	return copied
}

func (s *Service) resolvePool(name model.PoolName) (model.PoolConfig, error) {
	if name == "" {
		name = s.cfg.Broker.DefaultPool
	}
	for _, pool := range s.cfg.Pools {
		if pool.Name == name {
			return pool, nil
		}
	}
	return model.PoolConfig{}, ErrUnknownPool
}

func (s *Service) schedulerForPool(pool model.PoolConfig) scheduler.Scheduler {
	if s.scheds == nil {
		return scheduler.NewRoundRobin()
	}
	return s.scheds.ForPool(pool)
}

func (s *Service) reserve(pool model.PoolConfig, request model.AllocationRequest) (model.BackendName, error) {
	if pool.FairShare.Enabled {
		if s.fairShare == nil {
			return scheduler.NewPriorityFairShare().Reserve(pool, request)
		}
		return s.fairShare.Reserve(pool, request)
	}
	return s.schedulerForPool(pool).Reserve(pool, request)
}

func (s *Service) release(ctx context.Context, pool model.PoolConfig, backend model.BackendName, allocation model.AllocationStatus) {
	if pool.FairShare.Enabled {
		if s.fairShare != nil {
			s.fairShare.Release(pool.Name, backend, allocation)
		}
		s.cleanupAllocation(ctx, allocation)
		return
	}
	s.schedulerForPool(pool).Release(pool.Name, backend, allocation)
	s.cleanupAllocation(ctx, allocation)
}

func (s *Service) cleanupAllocation(ctx context.Context, allocation model.AllocationStatus) {
	backendImpl, ok := s.registry.Get(allocation.SelectedBackend)
	if !ok {
		return
	}
	cleanupBackend, ok := backendImpl.(backend.CleanupBackend)
	if !ok {
		return
	}
	if err := cleanupBackend.Cleanup(ctx, allocation); err != nil {
		logAllocationEvent(ctx, "allocation_cleanup_failed", map[string]string{
			"allocation_id": allocationIDLabel(allocation.ID),
			"pool":          string(allocation.Pool),
			"backend":       string(allocation.SelectedBackend),
			"error":         err.Error(),
		})
		log.Printf("allocation cleanup failed for %s: %v", allocation.ID, err)
	}
}

func allocationIDLabel(id string) string {
	return strings.TrimSpace(id)
}

func isActiveAllocationState(state model.AllocationState) bool {
	return state == model.StateReady || state == model.StateReserved
}

func isSchedulerAccountedState(state model.AllocationState) bool {
	return state == model.StateReady || state == model.StateReserved || state == model.StateWarm
}

func isTerminalAllocationState(state model.AllocationState) bool {
	switch state {
	case model.StateCompleted, model.StateFailed, model.StateCanceled, model.StateExpired, model.StateQuarantined:
		return true
	default:
		return false
	}
}

func parseCompletionState(request completionRequest) (model.AllocationState, string, error) {
	state := strings.TrimSpace(strings.ToLower(request.State))
	if state == "" {
		return model.StateCompleted, "", nil
	}

	switch state {
	case "complete", "completed", "success", "succeeded":
		return model.StateCompleted, request.Reason, nil
	case "failed", "failure", "error":
		return model.StateFailed, request.Error, nil
	case "canceled", "cancelled", "cancel":
		return model.StateCanceled, request.Reason, nil
	case "expired":
		return model.StateExpired, request.Reason, nil
	case "quarantined", "quarantine":
		return model.StateQuarantined, request.Reason, nil
	default:
		return "", "", fmt.Errorf("%w: %q", ErrInvalidCompletionState, request.State)
	}
}

func defaultCompletionMessage(state model.AllocationState) string {
	switch state {
	case model.StateCompleted:
		return "allocation completed"
	case model.StateFailed:
		return "allocation failed"
	case model.StateCanceled:
		return "allocation canceled"
	case model.StateExpired:
		return "allocation expired"
	case model.StateQuarantined:
		return "allocation quarantined"
	default:
		return ""
	}
}

func (s *Service) queueEnabled() bool {
	return s.cfg.Broker.Queue.Enabled
}

func normalizeQueueRetryAfter(cfg model.AdmissionQueueConfig) time.Duration {
	if cfg.RetryAfter > 0 {
		return cfg.RetryAfter
	}
	return 30 * time.Second
}

func normalizeQueueMaxAttempts(cfg model.AdmissionQueueConfig) int {
	if cfg.MaxAttempts > 0 {
		return cfg.MaxAttempts
	}
	return 3
}

func queueableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrBackendRateLimited) {
		return false
	}
	if errors.Is(err, scheduler.ErrNoCapacity) || errors.Is(err, ErrBackendCircuitOpen) {
		return true
	}
	reason, ok := backend.FailureReason(err)
	if !ok {
		return false
	}
	switch reason {
	case backend.FailureReasonTimeout, backend.FailureReasonTransport, backend.FailureReasonThrottled, backend.FailureReasonServerError:
		return true
	default:
		return false
	}
}

func queueReason(err error) string {
	if errors.Is(err, scheduler.ErrNoCapacity) {
		return "no-capacity"
	}
	if errors.Is(err, ErrBackendRateLimited) {
		return "rate-limited"
	}
	if errors.Is(err, ErrBackendCircuitOpen) {
		return "circuit-open"
	}
	if reason, ok := backend.FailureReason(err); ok {
		return reason
	}
	return "retryable"
}

func completionEventName(state model.AllocationState) string {
	switch state {
	case model.StateCompleted:
		return "allocation_completed"
	case model.StateFailed:
		return "allocation_failed"
	case model.StateCanceled:
		return "allocation_canceled"
	case model.StateExpired:
		return "allocation_expired"
	case model.StateQuarantined:
		return "allocation_quarantined"
	default:
		return "allocation_updated"
	}
}

func validateSchedulers(pools []model.PoolConfig, registry *scheduler.Registry) error {
	for _, pool := range pools {
		if err := registry.ValidateName(pool.Scheduler); err != nil {
			return fmt.Errorf("pool %q: %w", pool.Name, err)
		}
	}
	return nil
}

func filterEligibleBackends(pool model.PoolConfig, request model.AllocationRequest) (model.PoolConfig, error) {
	required := backend.NormalizeCapabilities(request.RequiredCapabilities)
	excluded := backend.NormalizeCapabilities(request.ExcludedCapabilities)
	if len(required) == 0 && len(excluded) == 0 {
		return pool, nil
	}

	filtered := pool
	filtered.Backends = make(map[model.BackendName]model.BackendConfig, len(pool.Backends))

	for name, cfg := range pool.Backends {
		cfg.Capabilities = backend.NormalizeCapabilities(cfg.Capabilities)
		if backendMatchesCapabilities(cfg, required, excluded) {
			filtered.Backends[name] = cfg
		}
	}

	if request.Backend != nil {
		if _, ok := pool.Backends[*request.Backend]; !ok {
			return model.PoolConfig{}, scheduler.ErrUnknownBackend
		}
		if _, ok := filtered.Backends[*request.Backend]; !ok {
			return model.PoolConfig{}, fmt.Errorf("pinned backend %q does not match the requested capabilities: %w", *request.Backend, ErrNoMatchingBackendCapabilities)
		}
		return filtered, nil
	}

	if len(filtered.Backends) == 0 {
		return model.PoolConfig{}, fmt.Errorf("%w for pool %q", ErrNoMatchingBackendCapabilities, pool.Name)
	}

	return filtered, nil
}

func filterBackendsByTimeout(pool model.PoolConfig, request model.AllocationRequest) (model.PoolConfig, error) {
	timeout := request.JobTimeout
	if timeout <= 0 {
		return pool, nil
	}

	filtered := pool
	filtered.Backends = make(map[model.BackendName]model.BackendConfig, len(pool.Backends))
	for name, cfg := range pool.Backends {
		if cfg.MaxJobDuration > 0 && timeout > cfg.MaxJobDuration {
			continue
		}
		filtered.Backends[name] = cfg
	}

	if request.Backend != nil {
		cfg, ok := pool.Backends[*request.Backend]
		if !ok {
			return model.PoolConfig{}, scheduler.ErrUnknownBackend
		}
		if cfg.MaxJobDuration > 0 && timeout > cfg.MaxJobDuration {
			return model.PoolConfig{}, fmt.Errorf("requested timeout %s exceeds backend limit %s", timeout, cfg.MaxJobDuration)
		}
		return filtered, nil
	}

	if len(filtered.Backends) == 0 {
		return model.PoolConfig{}, fmt.Errorf("%w for pool %q with timeout %s", scheduler.ErrNoCapacity, pool.Name, timeout)
	}

	return filtered, nil
}

func backendMatchesCapabilities(cfg model.BackendConfig, required, excluded []string) bool {
	capabilities := backend.CapabilitySet(cfg.Capabilities)

	for _, capability := range required {
		if _, ok := capabilities[capability]; !ok {
			return false
		}
	}

	for _, capability := range excluded {
		if _, ok := capabilities[capability]; ok {
			return false
		}
	}

	return true
}

func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

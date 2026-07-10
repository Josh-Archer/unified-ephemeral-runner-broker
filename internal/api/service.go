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
)

var ErrUnknownPool = errors.New("pool is not configured")
var ErrNoMatchingBackendCapabilities = errors.New("no backend matches the requested capabilities")
var ErrAllocationNotFound = errors.New("allocation not found")
var ErrAllocationAlreadyCompleted = errors.New("allocation already in terminal state")
var ErrInvalidCompletionState = errors.New("invalid completion state")

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
	store     *store.Memory
	observer  Observer
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
	return &Service{
		cfg:       cfg,
		registry:  registry,
		scheds:    schedulerRegistry,
		fairShare: scheduler.NewPriorityFairShare(),
		store:     store.NewMemory(),
		observer:  noopObserver{},
		initErr:   validateSchedulers(cfg.Pools, schedulerRegistry),
		health:    health,
		now:       time.Now,
	}
}

func (s *Service) SetObserver(observer Observer) {
	if observer == nil {
		s.observer = noopObserver{}
		return
	}
	s.observer = observer
}

func (s *Service) Allocate(ctx context.Context, request model.AllocationRequest) (model.AllocationStatus, error) {
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
	request.Backend = s.resolveRequestedBackend(pool, request.Backend)

	explicitTimeout := request.JobTimeout > 0
	timeout := request.JobTimeout
	if timeout <= 0 {
		timeout = s.cfg.Broker.DefaultJobTimeout
	}
	request.JobTimeout = timeout

	pool, err = filterEligibleBackends(pool, request)
	if err != nil {
		return model.AllocationStatus{}, err
	}
	if explicitTimeout {
		pool, err = filterBackendsByTimeout(pool, request)
		if err != nil {
			return model.AllocationStatus{}, err
		}
	}

	selected, err := s.reserve(pool, request)
	if err != nil {
		return model.AllocationStatus{}, err
	}
	resultBackend = selected
	s.observer.ObserveAllocationStart(pool.Name)
	logAllocationEvent(ctx, "allocation_admitted", map[string]string{
		"pool":    string(pool.Name),
		"backend": string(selected),
	})

	allocation := model.AllocationStatus{
		ID:              newID(),
		CorrelationID:   correlationIDFromContext(ctx),
		Pool:            pool.Name,
		SelectedBackend: selected,
		Tenant:          request.Tenant,
		PriorityClass:   request.PriorityClass,
		RequestedLabels: append([]string(nil), request.Labels...),
		Metadata:        map[string]string{backend.MetadataLaunchModeKey: launchModeCold},
		ExpiresAt:       s.now().Add(timeout),
		State:           model.StateReserved,
	}

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
	result = "success"
	logAllocationEvent(ctx, "allocation_ready", map[string]string{
		"allocation_id": allocation.ID,
		"pool":          string(pool.Name),
		"backend":       string(selected),
		"launch_mode":   launchMode,
	})

	return allocation, nil
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
	status, ok := s.store.Get(id)
	if !ok {
		return model.AllocationStatus{}, false
	}
	if isTerminalAllocationState(status.State) {
		return status, true
	}
	updated, ok := s.store.MarkState(id, model.StateCanceled, s.now(), "")
	if !ok {
		return model.AllocationStatus{}, false
	}
	if pool, err := s.resolvePool(updated.Pool); err == nil {
		s.release(context.Background(), pool, updated.SelectedBackend, updated)
	}
	s.observeState()
	return updated, true
}

type completionRequest struct {
	State  string `json:"state"`
	Error  string `json:"error"`
	Reason string `json:"reason"`
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

func (s *Service) observeState() {
	statuses := s.store.List()
	s.observer.ObserveActiveAllocations(statuses)
	s.observer.ObserveCapacity(s.cfg, statuses)
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
		return err
	}

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
	case model.BackendARC:
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

func (s *Service) resolveRequestedBackend(pool model.PoolConfig, requested *model.BackendName) *model.BackendName {
	if requested == nil {
		return nil
	}
	if *requested != model.BackendLambda {
		return requested
	}

	lambdaCfg, hasLambda := pool.Backends[model.BackendLambda]
	codebuildCfg, hasCodebuild := pool.Backends[model.BackendCodeBuild]
	if (!hasLambda || !lambdaCfg.Enabled) && hasCodebuild && codebuildCfg.Enabled {
		backend := model.BackendCodeBuild
		return &backend
	}
	return requested
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

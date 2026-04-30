package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
)

var ErrUnknownPool = errors.New("pool is not configured")
var ErrNoMatchingBackendCapabilities = errors.New("no backend matches the requested capabilities")

type Service struct {
	cfg       model.BrokerConfig
	registry  *backend.Registry
	scheds    *scheduler.Registry
	fairShare *scheduler.PriorityFairShare
	store     *store.Memory
	observer  Observer
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
		ExpiresAt:       s.now().Add(timeout),
		State:           model.StateReserved,
	}

	s.store.Save(allocation)

	backendImpl, ok := s.registry.Get(selected)
	if !ok {
		s.release(pool, selected, allocation)
		s.store.MarkState(allocation.ID, model.StateFailed, s.now(), "backend not registered")
		return model.AllocationStatus{}, fmt.Errorf("backend implementation missing: %s", selected)
	}

	launchStarted := s.now()
	provisioned, err := backendImpl.Provision(ctx, request, allocation)
	launchLatency := s.now().Sub(launchStarted)
	s.observer.ObserveLaunchLatency(pool.Name, selected, launchLatency)
	s.observer.ObserveRegistrationLatency(pool.Name, selected, launchLatency)
	if err != nil {
		s.release(pool, selected, allocation)
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
	allocation.Metadata = backend.WithCapabilitiesMetadata(pool.Backends[selected], provisioned.Metadata)
	allocation.State = model.StateReady
	s.store.Save(allocation)
	result = "success"
	logAllocationEvent(ctx, "allocation_ready", map[string]string{
		"allocation_id": allocation.ID,
		"pool":          string(pool.Name),
		"backend":       string(selected),
	})

	return allocation, nil
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
		s.release(pool, status.SelectedBackend, status)
	}
	s.observeState()
	return status, true
}

func (s *Service) SweepExpired(now time.Time) int {
	expired := 0
	for _, status := range s.store.List() {
		if status.State != model.StateReady && status.State != model.StateReserved {
			continue
		}
		if status.ExpiresAt.After(now) {
			continue
		}
		if _, ok := s.store.MarkState(status.ID, model.StateExpired, now, "allocation expired"); ok {
			if pool, err := s.resolvePool(status.Pool); err == nil {
				s.release(pool, status.SelectedBackend, status)
			}
			expired++
		}
	}
	s.observeState()
	return expired
}

func (s *Service) observeState() {
	statuses := s.store.List()
	s.observer.ObserveActiveAllocations(statuses)
	s.observer.ObserveCapacity(s.cfg, statuses)
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

func (s *Service) release(pool model.PoolConfig, backend model.BackendName, allocation model.AllocationStatus) {
	if pool.FairShare.Enabled {
		if s.fairShare != nil {
			s.fairShare.Release(pool.Name, backend, allocation)
		}
		return
	}
	s.schedulerForPool(pool).Release(pool.Name, backend, allocation)
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

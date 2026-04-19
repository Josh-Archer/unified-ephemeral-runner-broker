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

type Service struct {
	cfg      model.BrokerConfig
	registry *backend.Registry
	sched    *scheduler.RoundRobin
	store    *store.Memory
	health   func(context.Context) error
	now      func() time.Time
}

func NewService(cfg model.BrokerConfig, registry *backend.Registry, health func(context.Context) error) *Service {
	if health == nil {
		health = func(context.Context) error { return nil }
	}
	return &Service{
		cfg:      cfg,
		registry: registry,
		sched:    scheduler.NewRoundRobin(),
		store:    store.NewMemory(),
		health:   health,
		now:      time.Now,
	}
}

func (s *Service) Allocate(ctx context.Context, request model.AllocationRequest) (model.AllocationStatus, error) {
	if err := s.health(ctx); err != nil {
		return model.AllocationStatus{}, err
	}

	pool, err := s.resolvePool(request.Pool)
	if err != nil {
		return model.AllocationStatus{}, err
	}

	timeout := request.JobTimeout
	if timeout <= 0 {
		timeout = s.cfg.Broker.DefaultJobTimeout
	}

	if request.Backend != nil {
		backendCfg, ok := pool.Backends[*request.Backend]
		if !ok {
			return model.AllocationStatus{}, scheduler.ErrUnknownBackend
		}
		if backendCfg.MaxJobDuration > 0 && timeout > backendCfg.MaxJobDuration {
			return model.AllocationStatus{}, fmt.Errorf("requested timeout %s exceeds backend limit %s", timeout, backendCfg.MaxJobDuration)
		}
	}

	selected, err := s.sched.Reserve(pool, request.Backend)
	if err != nil {
		return model.AllocationStatus{}, err
	}

	allocation := model.AllocationStatus{
		ID:              newID(),
		Pool:            pool.Name,
		SelectedBackend: selected,
		RequestedLabels: append([]string(nil), request.Labels...),
		ExpiresAt:       s.now().Add(timeout),
		State:           model.StateReserved,
	}

	s.store.Save(allocation)

	backendImpl, ok := s.registry.Get(selected)
	if !ok {
		s.sched.Release(pool.Name, selected)
		s.store.MarkState(allocation.ID, model.StateFailed, s.now(), "backend not registered")
		return model.AllocationStatus{}, fmt.Errorf("backend implementation missing: %s", selected)
	}

	provisioned, err := backendImpl.Provision(ctx, request, allocation)
	if err != nil {
		s.sched.Release(pool.Name, selected)
		s.store.MarkState(allocation.ID, model.StateFailed, s.now(), err.Error())
		return model.AllocationStatus{}, err
	}

	allocation.RunnerLabel = provisioned.RunnerLabel
	allocation.Metadata = provisioned.Metadata
	allocation.State = model.StateReady
	s.store.Save(allocation)

	return allocation, nil
}

func (s *Service) Health(ctx context.Context) error {
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
	s.sched.Release(status.Pool, status.SelectedBackend)
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
			s.sched.Release(status.Pool, status.SelectedBackend)
			expired++
		}
	}
	return expired
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

func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

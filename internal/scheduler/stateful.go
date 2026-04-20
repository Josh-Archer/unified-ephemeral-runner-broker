package scheduler

import (
	"sync"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type orderedScheduler struct {
	mu      sync.Mutex
	cursors map[model.PoolName]int
	active  map[model.PoolName]map[model.BackendName]int
	order   func(model.PoolConfig) []model.BackendName
}

func newOrderedScheduler(order func(model.PoolConfig) []model.BackendName) *orderedScheduler {
	return &orderedScheduler{
		cursors: map[model.PoolName]int{},
		active:  map[model.PoolName]map[model.BackendName]int{},
		order:   order,
	}
}

func (s *orderedScheduler) Reserve(pool model.PoolConfig, pinned *model.BackendName) (model.BackendName, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.active[pool.Name]; !ok {
		s.active[pool.Name] = map[model.BackendName]int{}
	}

	if pinned != nil {
		cfg, ok := pool.Backends[*pinned]
		if !ok {
			return "", ErrUnknownBackend
		}
		if !backendAvailable(cfg, s.active[pool.Name][*pinned]) {
			return "", ErrNoCapacity
		}
		s.active[pool.Name][*pinned]++
		return *pinned, nil
	}

	backends := s.order(pool)
	if len(backends) == 0 {
		return "", ErrNoCapacity
	}

	start := s.cursors[pool.Name] % len(backends)
	for offset := range len(backends) {
		candidate := backends[(start+offset)%len(backends)]
		cfg := pool.Backends[candidate]
		if !backendAvailable(cfg, s.active[pool.Name][candidate]) {
			continue
		}
		s.active[pool.Name][candidate]++
		s.cursors[pool.Name] = (start + offset + 1) % len(backends)
		return candidate, nil
	}

	return "", ErrNoCapacity
}

func (s *orderedScheduler) Release(pool model.PoolName, backend model.BackendName) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.active[pool]; !ok {
		return
	}
	if s.active[pool][backend] > 0 {
		s.active[pool][backend]--
	}
}

func (s *orderedScheduler) Active(pool model.PoolName, backend model.BackendName) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[pool][backend]
}

func backendAvailable(cfg model.BackendConfig, active int) bool {
	if !cfg.Enabled || !cfg.Healthy {
		return false
	}
	return active < cfg.MaxRunners
}

func backendWeight(cfg model.BackendConfig) int {
	if cfg.Weight > 0 {
		return cfg.Weight
	}
	return 1
}

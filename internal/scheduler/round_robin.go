package scheduler

import (
	"errors"
	"sync"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

var (
	ErrUnknownBackend = errors.New("backend is not configured for the pool")
	ErrNoCapacity     = errors.New("no healthy backend with free capacity")
)

type RoundRobin struct {
	mu      sync.Mutex
	cursors map[model.PoolName]int
	active  map[model.PoolName]map[model.BackendName]int
}

func NewRoundRobin() *RoundRobin {
	return &RoundRobin{
		cursors: map[model.PoolName]int{},
		active:  map[model.PoolName]map[model.BackendName]int{},
	}
}

func (r *RoundRobin) Reserve(pool model.PoolConfig, pinned *model.BackendName) (model.BackendName, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.active[pool.Name]; !ok {
		r.active[pool.Name] = map[model.BackendName]int{}
	}

	if pinned != nil {
		cfg, ok := pool.Backends[*pinned]
		if !ok {
			return "", ErrUnknownBackend
		}
		if !cfg.Enabled || !cfg.Healthy || r.active[pool.Name][*pinned] >= cfg.MaxRunners {
			return "", ErrNoCapacity
		}
		r.active[pool.Name][*pinned]++
		return *pinned, nil
	}

	backends := orderedBackends(pool)
	if len(backends) == 0 {
		return "", ErrNoCapacity
	}

	start := r.cursors[pool.Name] % len(backends)
	for offset := range len(backends) {
		candidate := backends[(start+offset)%len(backends)]
		cfg := pool.Backends[candidate]
		if !cfg.Enabled || !cfg.Healthy {
			continue
		}
		if r.active[pool.Name][candidate] >= cfg.MaxRunners {
			continue
		}
		r.active[pool.Name][candidate]++
		r.cursors[pool.Name] = (start + offset + 1) % len(backends)
		return candidate, nil
	}

	return "", ErrNoCapacity
}

func (r *RoundRobin) Release(pool model.PoolName, backend model.BackendName) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.active[pool]; !ok {
		return
	}
	if r.active[pool][backend] > 0 {
		r.active[pool][backend]--
	}
}

func (r *RoundRobin) Active(pool model.PoolName, backend model.BackendName) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[pool][backend]
}

func orderedBackends(pool model.PoolConfig) []model.BackendName {
	preferred := []model.BackendName{
		model.BackendARC,
		model.BackendLambda,
		model.BackendCloudRun,
		model.BackendAzureFunctions,
	}

	result := make([]model.BackendName, 0, len(pool.Backends))
	for _, candidate := range preferred {
		if _, ok := pool.Backends[candidate]; ok {
			result = append(result, candidate)
		}
	}
	return result
}

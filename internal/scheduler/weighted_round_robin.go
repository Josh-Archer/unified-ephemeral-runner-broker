package scheduler

import "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"

type WeightedRoundRobin struct {
	state *orderedScheduler
}

func NewWeightedRoundRobin() *WeightedRoundRobin {
	return &WeightedRoundRobin{
		state: newOrderedScheduler(weightedBackends),
	}
}

func (w *WeightedRoundRobin) Reserve(pool model.PoolConfig, request model.AllocationRequest) (model.BackendName, error) {
	return w.state.Reserve(pool, request)
}

func (w *WeightedRoundRobin) Release(pool model.PoolName, backend model.BackendName, allocation model.AllocationStatus) {
	w.state.Release(pool, backend, allocation)
}

func (w *WeightedRoundRobin) Active(pool model.PoolName, backend model.BackendName) int {
	return w.state.Active(pool, backend)
}

func weightedBackends(pool model.PoolConfig) []model.BackendName {
	ordered := orderedBackends(pool)
	result := make([]model.BackendName, 0, len(ordered))
	for _, backend := range ordered {
		weight := backendWeight(pool.Backends[backend])
		for range weight {
			result = append(result, backend)
		}
	}
	return result
}

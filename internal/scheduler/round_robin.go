package scheduler

import (
	"errors"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

var (
	ErrUnknownBackend = errors.New("backend is not configured for the pool")
	ErrNoCapacity     = errors.New("no healthy backend with free capacity")
)

type RoundRobin struct {
	state *orderedScheduler
}

func NewRoundRobin() *RoundRobin {
	return &RoundRobin{
		state: newOrderedScheduler(orderedBackends),
	}
}

func (r *RoundRobin) Reserve(pool model.PoolConfig, request model.AllocationRequest) (model.BackendName, error) {
	return r.state.Reserve(pool, request)
}

func (r *RoundRobin) Release(pool model.PoolName, backend model.BackendName, allocation model.AllocationStatus) {
	r.state.Release(pool, backend, allocation)
}

func (r *RoundRobin) Active(pool model.PoolName, backend model.BackendName) int {
	return r.state.Active(pool, backend)
}

func orderedBackends(pool model.PoolConfig) []model.BackendName {
	preferred := []model.BackendName{
		model.BackendARC,
		model.BackendCodeBuild,
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

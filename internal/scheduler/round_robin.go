package scheduler

import (
	"errors"
	"sort"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/budget"
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

// preferredBackendOrder is the stable default order before cost-class sorting.
// Cost class is applied as a primary key so cheaper backends are preferred when
// free capacity is otherwise equal (round-robin still rotates within the order).
var preferredBackendOrder = []model.BackendName{
	model.BackendARC,
	model.BackendCodeBuild,
	model.BackendLambda,
	model.BackendCloudRun,
	model.BackendAzureFunctions,
	model.BackendAzureVM,
	model.BackendEC2,
	model.BackendGCE,
	model.BackendDesktop,
}

func orderedBackends(pool model.PoolConfig) []model.BackendName {
	type ranked struct {
		name  model.BackendName
		cost  int
		order int
	}
	rankIndex := map[model.BackendName]int{}
	for i, name := range preferredBackendOrder {
		rankIndex[name] = i
	}

	candidates := make([]ranked, 0, len(pool.Backends))
	for name, cfg := range pool.Backends {
		order, ok := rankIndex[name]
		if !ok {
			order = len(preferredBackendOrder)
		}
		candidates = append(candidates, ranked{
			name:  name,
			cost:  budget.CostClass(name, cfg),
			order: order,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].cost != candidates[j].cost {
			return candidates[i].cost < candidates[j].cost
		}
		return candidates[i].order < candidates[j].order
	})

	result := make([]model.BackendName, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, candidate.name)
	}
	return result
}

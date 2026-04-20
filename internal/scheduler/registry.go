package scheduler

import (
	"fmt"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const (
	NameRoundRobin         = "round-robin"
	NameWeightedRoundRobin = "weighted-round-robin"
)

type Scheduler interface {
	Reserve(pool model.PoolConfig, pinned *model.BackendName) (model.BackendName, error)
	Release(pool model.PoolName, backend model.BackendName)
	Active(pool model.PoolName, backend model.BackendName) int
}

type Registry struct {
	defaultScheduler Scheduler
	schedulers       map[string]Scheduler
}

func NewRegistry() *Registry {
	roundRobin := NewRoundRobin()
	weightedRoundRobin := NewWeightedRoundRobin()

	return &Registry{
		defaultScheduler: roundRobin,
		schedulers: map[string]Scheduler{
			NameRoundRobin:         roundRobin,
			NameWeightedRoundRobin: weightedRoundRobin,
		},
	}
}

func (r *Registry) ForPool(pool model.PoolConfig) Scheduler {
	if r == nil {
		return nil
	}
	return r.ForName(pool.Scheduler)
}

func (r *Registry) ForName(name string) Scheduler {
	if r == nil || r.defaultScheduler == nil {
		return nil
	}

	if scheduler, ok := r.schedulers[normalizeSchedulerName(name)]; ok {
		return scheduler
	}

	return r.defaultScheduler
}

func normalizeSchedulerName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func (r *Registry) ValidateName(name string) error {
	if r == nil {
		return fmt.Errorf("scheduler registry is not configured")
	}

	normalized := normalizeSchedulerName(name)
	if normalized == "" {
		return nil
	}
	if _, ok := r.schedulers[normalized]; ok {
		return nil
	}

	return fmt.Errorf("unknown scheduler %q", name)
}

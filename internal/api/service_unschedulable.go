package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
)

// defaultUnschedulableTimeout is how long a broker-owned runner pod may stay
// Pending/Unschedulable before it is deleted and its allocation released.
const defaultUnschedulableTimeout = 2 * time.Minute

func (s *Service) unschedulableTimeout() time.Duration {
	if s.cfg.Broker.UnschedulableTimeout < 0 {
		return 0
	}
	if s.cfg.Broker.UnschedulableTimeout == 0 {
		return defaultUnschedulableTimeout
	}
	return s.cfg.Broker.UnschedulableTimeout
}

// ReconcileUnschedulablePods deletes broker-owned pods that have stayed
// Unschedulable past the configured timeout, releases their allocations so the
// slots no longer count against caps, and retries the original workflow job
// so it can overflow to another backend. It is safe to run after a restart:
// delete 404s are ignored and already-terminal allocations are not failed
// twice. It returns the number of pods reaped.
func (s *Service) ReconcileUnschedulablePods(ctx context.Context, now time.Time) int {
	if s.registry == nil {
		return 0
	}
	timeout := s.unschedulableTimeout()
	if timeout <= 0 {
		return 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = s.now()
	}
	reaped := 0
	seen := map[model.BackendName]struct{}{}
	for _, pool := range s.cfg.Pools {
		for backendName, cfg := range pool.Backends {
			if !cfg.Enabled {
				continue
			}
			if _, ok := seen[backendName]; ok {
				continue
			}
			seen[backendName] = struct{}{}
			impl, ok := s.registry.Get(backendName)
			if !ok {
				continue
			}
			reaper, ok := impl.(backend.UnschedulableReaper)
			if !ok {
				continue
			}
			probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			pods, err := reaper.ReapUnschedulablePods(probeCtx, now, timeout)
			cancel()
			if err != nil {
				logAllocationEvent(ctx, "unschedulable_pod_reap_failed", map[string]string{
					"backend": string(backendName),
					"error":   err.Error(),
				})
				continue
			}
			for _, pod := range pods {
				s.releaseUnschedulablePod(ctx, backendName, pod)
				reaped++
			}
		}
	}
	return reaped
}

func (s *Service) releaseUnschedulablePod(ctx context.Context, backendName model.BackendName, pod backend.UnschedulablePod) {
	poolName := pod.Pool
	repo := pod.Repository
	jobID := pod.WorkflowJobID
	if pod.AllocationID != "" {
		if status, ok := s.store.Get(pod.AllocationID); ok {
			if poolName == "" {
				poolName = status.Pool
			}
			if repo == "" {
				repo = status.TargetRepository()
			}
			if jobID == 0 && status.Request != nil {
				jobID = status.Request.WorkflowJobID
			}
			if status.SelectedBackend == backendName && isActiveAllocationState(status.State) {
				request := completionRequest{State: "failed", Error: "runner pod unschedulable"}
				if _, _, err := s.Complete(ctx, status.ID, request); err != nil && !errors.Is(err, ErrAllocationAlreadyCompleted) {
					logAllocationEvent(ctx, "unschedulable_pod_release_failed", map[string]string{
						"allocation_id": allocationIDLabel(status.ID),
						"backend":       string(backendName),
						"error":         err.Error(),
					})
				} else {
					logAllocationEvent(ctx, "unschedulable_pod_released", map[string]string{
						"allocation_id": allocationIDLabel(status.ID),
						"pool":          string(status.Pool),
						"backend":       string(backendName),
					})
				}
			}
		}
	}
	if jobID == 0 || poolName == "" {
		return
	}
	pool, err := s.resolvePool(poolName)
	if err != nil {
		return
	}
	if _, _, err := s.AllocateForWorkflowJob(withRetryFailedWorkflowJob(ctx), pool, jobID, repo, nil); err != nil {
		logAllocationEvent(ctx, "unschedulable_pod_requeue_failed", map[string]string{
			"backend": string(backendName),
			"error":   err.Error(),
		})
	}
}

func (s *Service) capacityGroupOccupancy(members []model.BackendName) int {
	if s.store == nil {
		return 0
	}
	total := 0
	for _, member := range members {
		total += s.store.CountBackendActive(member)
	}
	return total
}

// filterBackendsByCapacityGroup clamps or skips backends that share a
// scheduling cap with peers that already occupy the group.
func (s *Service) filterBackendsByCapacityGroup(pool model.PoolConfig, request model.AllocationRequest) (model.PoolConfig, error) {
	filtered := pool
	filtered.Backends = make(map[model.BackendName]model.BackendConfig, len(pool.Backends))
	skipped := 0
	for name, cfg := range pool.Backends {
		if !cfg.Enabled || !cfg.Healthy || cfg.MaxRunners <= 0 {
			filtered.Backends[name] = cfg
			continue
		}
		adjusted, ok := s.applyCapacityGroup(name, cfg)
		if !ok {
			skipped++
			adjusted.Healthy = false
			filtered.Backends[name] = adjusted
			logAllocationEvent(context.Background(), "capacity_group_skip", map[string]string{
				"pool":    string(pool.Name),
				"backend": string(name),
			})
			continue
		}
		filtered.Backends[name] = adjusted
	}
	if request.Backend != nil {
		cfg, ok := pool.Backends[*request.Backend]
		if !ok {
			return model.PoolConfig{}, scheduler.ErrUnknownBackend
		}
		if !cfg.Enabled || !cfg.Healthy || cfg.MaxRunners <= 0 {
			return model.PoolConfig{}, fmt.Errorf("pinned backend %q is not available: %w", *request.Backend, scheduler.ErrNoCapacity)
		}
		adjusted, allowed := s.applyCapacityGroup(*request.Backend, cfg)
		if !allowed {
			return model.PoolConfig{}, fmt.Errorf("pinned backend %q capacity group is full: %w", *request.Backend, scheduler.ErrNoCapacity)
		}
		pinned := pool
		pinned.Backends = map[model.BackendName]model.BackendConfig{*request.Backend: adjusted}
		return pinned, nil
	}
	if !poolHasSchedulableBackend(filtered) {
		if skipped > 0 {
			return model.PoolConfig{}, fmt.Errorf("%w for pool %q after capacity group filtering", scheduler.ErrNoCapacity, pool.Name)
		}
		// Earlier filters already left the pool unschedulable (rate-limit,
		// admission, live capacity). Do not rewrite that as a group-cap miss.
		return filtered, nil
	}
	return filtered, nil
}

func (s *Service) applyCapacityGroup(name model.BackendName, cfg model.BackendConfig) (model.BackendConfig, bool) {
	group := model.ResolveCapacityGroup(s.cfg, name)
	if group.Cap <= 0 {
		return cfg, true
	}
	occupancy := s.capacityGroupOccupancy(group.Members)
	thisActive := 0
	if s.store != nil {
		thisActive = s.store.CountBackendActive(name)
	}
	other := occupancy - thisActive
	if other < 0 {
		other = 0
	}
	room := group.Cap - other
	if room <= 0 {
		return cfg, false
	}
	if room < cfg.MaxRunners {
		cfg.MaxRunners = room
	}
	return cfg, true
}

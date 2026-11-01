package api

import (
	"context"
	"errors"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// ReconcileFinishedRunners releases allocations whose runner has already
// exited. A workflow_job completion normally finalizes an allocation, but none
// arrives when a runner exits before or without running a job (for example a
// registration failure), and an allocation would otherwise hold its slot until
// its job timeout. Finished runners whose allocation is unknown or already
// terminal only get their pod and registration cleaned up. It returns the
// number of allocations released.
func (s *Service) ReconcileFinishedRunners(ctx context.Context) int {
	if s.registry == nil {
		return 0
	}
	released := 0
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
			lister, ok := impl.(backend.FinishedRunnerBackend)
			if !ok {
				continue
			}
			probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			runners, err := lister.FinishedRunners(probeCtx)
			cancel()
			if err != nil {
				logAllocationEvent(ctx, "finished_runner_inventory_failed", map[string]string{
					"backend": string(backendName),
					"error":   err.Error(),
				})
				continue
			}
			for _, runner := range runners {
				if s.releaseFinishedRunner(ctx, backendName, runner) {
					released++
				}
			}
		}
	}
	return released
}

func (s *Service) releaseFinishedRunner(ctx context.Context, backendName model.BackendName, runner backend.FinishedRunner) bool {
	status, exists := s.store.Get(runner.AllocationID)
	if exists && status.SelectedBackend == backendName && isActiveAllocationState(status.State) {
		request := completionRequest{State: "completed", Reason: "runner pod exited"}
		if runner.Failed {
			request = completionRequest{State: "failed", Error: "runner pod exited with failure"}
		}
		// Complete releases the slot and runs the backend Cleanup (pod and
		// runner registration).
		if _, _, err := s.Complete(ctx, status.ID, request); err != nil && !errors.Is(err, ErrAllocationAlreadyCompleted) {
			logAllocationEvent(ctx, "finished_runner_release_failed", map[string]string{
				"allocation_id": allocationIDLabel(status.ID),
				"backend":       string(backendName),
				"error":         err.Error(),
			})
			return false
		}
		logAllocationEvent(ctx, "finished_runner_released", map[string]string{
			"allocation_id": allocationIDLabel(status.ID),
			"pool":          string(status.Pool),
			"backend":       string(backendName),
		})
		return true
	}
	// Unknown or already terminal: only the leftover pod remains to clean up.
	s.cleanupAllocation(ctx, model.AllocationStatus{
		ID:              runner.AllocationID,
		Pool:            runner.Pool,
		SelectedBackend: backendName,
		Repository:      runner.Repository,
		State:           model.StateCompleted,
		Metadata:        runner.Metadata,
	})
	return false
}

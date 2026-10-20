package api

import (
	"context"
	"errors"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// MatchWebhookPool returns the pool opted into webhook mode whose webhookLabels
// cover every runs-on label of a job. GitHub routes a job to a runner whose
// label set is a superset of the job's labels, so the job labels must be a
// subset of the pool labels. A job naming only "self-hosted" is never matched
// because it would be ambiguous across pools. Matching is case-insensitive and
// the first pool in config order wins.
func (s *Service) MatchWebhookPool(jobLabels []string) (model.PoolConfig, bool) {
	wanted := normalizeLabelSet(jobLabels)
	specific := false
	for label := range wanted {
		if label != "self-hosted" {
			specific = true
		}
	}
	if !specific {
		return model.PoolConfig{}, false
	}
	for _, pool := range s.cfg.Pools {
		if len(pool.WebhookLabels) == 0 {
			continue
		}
		have := normalizeLabelSet(pool.WebhookLabels)
		covered := true
		for label := range wanted {
			if _, ok := have[label]; !ok {
				covered = false
				break
			}
		}
		if covered {
			return pool, true
		}
	}
	return model.PoolConfig{}, false
}

func normalizeLabelSet(labels []string) map[string]struct{} {
	set := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label != "" {
			set[label] = struct{}{}
		}
	}
	return set
}

// findByWorkflowJobID returns the allocation created for a GitHub workflow job.
func (s *Service) findByWorkflowJobID(jobID int64) (model.AllocationStatus, bool) {
	if jobID == 0 {
		return model.AllocationStatus{}, false
	}
	var found model.AllocationStatus
	ok := false
	for _, status := range s.store.List() {
		if status.Request == nil || status.Request.WorkflowJobID != jobID {
			continue
		}
		// Prefer a non-terminal allocation if a job somehow has several.
		if !ok || (isTerminalAllocationState(found.State) && !isTerminalAllocationState(status.State)) {
			found, ok = status, true
		}
	}
	return found, ok
}

// AllocateForWorkflowJob allocates capacity for a queued workflow job through
// the normal scheduler/budget/admission path. The runner registers with the
// pool's static webhook labels and the job id is recorded on the allocation.
// It is idempotent per job id: a second call returns the existing allocation
// with created=false.
func (s *Service) AllocateForWorkflowJob(ctx context.Context, pool model.PoolConfig, jobID int64) (status model.AllocationStatus, created bool, err error) {
	s.workflowJobMu.Lock()
	defer s.workflowJobMu.Unlock()
	if existing, ok := s.findByWorkflowJobID(jobID); ok {
		return existing, false, nil
	}
	status, err = s.Allocate(ctx, model.AllocationRequest{
		Pool:          pool.Name,
		Labels:        append([]string(nil), pool.WebhookLabels...),
		WorkflowJobID: jobID,
	})
	if err != nil {
		return model.AllocationStatus{}, false, err
	}
	return status, true, nil
}

// FinalizeWorkflowJob finalizes the allocation recorded for jobID. Canceled
// jobs (and allocations still waiting in the admission queue) are canceled;
// all other conclusions complete the allocation. found is false when no
// allocation exists for the job; already-terminal allocations are a no-op.
func (s *Service) FinalizeWorkflowJob(ctx context.Context, jobID int64, conclusion string) (status model.AllocationStatus, found bool, err error) {
	status, found = s.findByWorkflowJobID(jobID)
	if !found {
		return model.AllocationStatus{}, false, nil
	}
	if isTerminalAllocationState(status.State) {
		return status, true, nil
	}
	canceled := strings.EqualFold(strings.TrimSpace(conclusion), "cancelled") || strings.EqualFold(strings.TrimSpace(conclusion), "canceled")
	if canceled || status.State == model.StatePending {
		updated, ok := s.Cancel(ctx, status.ID)
		if !ok {
			return status, true, ErrAllocationNotFound
		}
		return updated, true, nil
	}
	updated, ok, err := s.Complete(ctx, status.ID, completionRequest{State: "completed", Reason: "workflow_job completed"})
	if err != nil && !errors.Is(err, ErrAllocationAlreadyCompleted) {
		return status, true, err
	}
	if !ok {
		return status, true, ErrAllocationNotFound
	}
	return updated, true, nil
}

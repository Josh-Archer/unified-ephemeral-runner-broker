package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
)

// Webhook deliveries have no caller retry loop: GitHub does not retry HTTP500.
// Retain temporary capacity waits without changing API clients' fail-fast path.
func workflowCapacityError(request model.AllocationRequest, err error) bool {
	return request.WorkflowJobID > 0 && (errors.Is(err, scheduler.ErrNoCapacity) ||
		errors.Is(err, ErrBackendLiveCapacity) || backend.IsCapacityExhausted(err))
}

func waitingForWorkflowCapacity(status model.AllocationStatus) bool {
	if status.Request == nil || status.Request.WorkflowJobID <= 0 {
		return false
	}
	reason := status.Metadata["queue_reason"]
	return reason == "no-capacity" || reason == "live-capacity"
}

// MatchWebhookPoolForRepository is MatchWebhookPool restricted to pools whose
// webhookRepositories allow repository. A pool that matches the labels but not
// the repository is skipped, so a later pool with the same labels can serve it.
func (s *Service) MatchWebhookPoolForRepository(jobLabels []string, repository string) (model.PoolConfig, bool) {
	return s.matchWebhookPool(jobLabels, func(pool model.PoolConfig) bool {
		return pool.ServesWebhookRepository(repository)
	})
}

// MatchWebhookPool returns the pool opted into webhook mode whose webhookLabels
// cover every runs-on label of a job. GitHub routes a job to a runner whose
// label set is a superset of the job's labels, so the job labels must be a
// subset of the pool labels. A job naming only "self-hosted" is never matched
// because it would be ambiguous across pools. Matching is case-insensitive and
// the first pool in config order wins.
func (s *Service) MatchWebhookPool(jobLabels []string) (model.PoolConfig, bool) {
	return s.matchWebhookPool(jobLabels, nil)
}

func (s *Service) matchWebhookPool(jobLabels []string, allow func(model.PoolConfig) bool) (model.PoolConfig, bool) {
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
		if len(pool.WebhookLabels) == 0 || (allow != nil && !allow(pool)) {
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

// AcceptsWebhookRepository confines every webhook action to the runner scope.
// A valid signature authenticates the sender, not the repository named by it.
func (s *Service) AcceptsWebhookRepository(repository string) bool {
	repository = strings.TrimSpace(repository)
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return false
	}
	switch strings.TrimSpace(s.cfg.GitHub.Scope.Type) {
	case model.GitHubScopeOwner:
		_, err := s.cfg.GitHub.Scope.ForRepository(repository)
		return err == nil
	case model.GitHubScopeRepository:
		return strings.EqualFold(repository, strings.TrimSpace(s.cfg.GitHub.Scope.Owner)+"/"+strings.TrimSpace(s.cfg.GitHub.Scope.Repository))
	case model.GitHubScopeOrganization:
		return strings.EqualFold(owner, strings.TrimSpace(s.cfg.GitHub.Scope.Organization))
	default:
		return false
	}
}

// AllocateForWorkflowJob allocates capacity for a queued workflow job through
// the normal scheduler/budget/admission path. The runner registers with the
// pool's static webhook labels and the job id is recorded on the allocation,
// along with the repository that queued it. It is idempotent per job id: a
// second call returns the existing allocation with created=false.
func (s *Service) AllocateForWorkflowJob(ctx context.Context, pool model.PoolConfig, jobID int64, repository string) (status model.AllocationStatus, created bool, err error) {
	s.workflowJobMu.Lock()
	defer s.workflowJobMu.Unlock()
	if existing, ok := s.findByWorkflowJobID(jobID); ok {
		return existing, false, nil
	}
	tenant := ""
	if s.cfg.GitHub.Scope.IsPerRepository() {
		// One pool serves every repository, so fair-share quotas apply per repository.
		tenant = strings.ToLower(strings.TrimSpace(repository))
	}
	request := model.AllocationRequest{
		Pool:             pool.Name,
		JobTimeout:       pool.WebhookJobTimeout,
		Tenant:           tenant,
		Labels:           append([]string(nil), pool.WebhookLabels...),
		WorkflowJobID:    jobID,
		TargetRepository: strings.TrimSpace(repository),
	}
	status, err = s.Allocate(ctx, request)
	if err != nil {
		// Provider rejection can leave the selection loop before its queue path.
		if workflowCapacityError(request, err) {
			if pending, ok := s.queueAllocation(ctx, request, pool.Name, "", err, model.AllocationStatus{}); ok {
				return pending, true, nil
			}
		}
		return model.AllocationStatus{}, false, err
	}
	return status, true, nil
}

// WorkflowJobRunner identifies the runner GitHub actually assigned. Queued
// demand is not ownership: runners sharing static labels may exchange jobs.
type WorkflowJobRunner struct {
	Repository string
	Name       string
	ID         int64
	StartedAt  time.Time
}

const (
	executionJobKey       = "workflow_job_execution_id"
	deferredConclusionKey = "workflow_job_deferred_conclusion"
)

// allocationServesRepository also checks the original repository for owner
// scope, where a single broker serves several independently registered repos.
func (s *Service) allocationServesRepository(status model.AllocationStatus, repository string) bool {
	if repository == "" {
		// Compatibility for direct service callers; the HTTP handler always
		// requires and validates repository before reaching these methods.
		return true
	}
	if !s.AcceptsWebhookRepository(repository) {
		return false
	}
	if target := status.TargetRepository(); target != "" {
		return strings.EqualFold(target, repository)
	}
	return !s.cfg.GitHub.Scope.IsPerRepository()
}

func executionJob(status model.AllocationStatus) int64 {
	id, _ := strconv.ParseInt(status.Metadata[executionJobKey], 10, 64)
	return id
}

func copyWorkflowMetadata(status *model.AllocationStatus) {
	metadata := make(map[string]string, len(status.Metadata)+1)
	for key, value := range status.Metadata {
		metadata[key] = value
	}
	status.Metadata = metadata
}

// executionAllocation resolves a known runner or an already persisted binding.
// Shared runs-on labels alone are never evidence of runner identity.
func (s *Service) executionAllocation(jobID int64, runner WorkflowJobRunner) (model.AllocationStatus, bool) {
	hasIdentity := runner.Name != "" || runner.ID > 0
	var chosen model.AllocationStatus
	var chosenLease time.Time
	found := false
	for _, status := range s.store.List() {
		if !s.allocationServesRepository(status, runner.Repository) {
			continue
		}
		if !hasIdentity {
			if executionJob(status) == jobID {
				return status, true
			}
			continue
		}
		if runner.Name != "" && !strings.EqualFold(strings.TrimSpace(status.RunnerLabel), strings.TrimSpace(runner.Name)) {
			continue
		}
		id := status.Metadata["github_runner_id"]
		if runner.ID > 0 && id != "" && id != strconv.FormatInt(runner.ID, 10) {
			continue
		}
		if runner.Name == "" && id == "" {
			continue
		}
		bound := executionJob(status)
		if bound != 0 && bound != jobID {
			continue
		}
		var lease time.Time
		if raw := status.Metadata["runner_lease_started_at"]; raw != "" {
			var err error
			lease, err = time.Parse(time.RFC3339, raw)
			// Persistent workers reuse names. Require signed execution time and id;
			// delayed events from an older lease cannot bind to a newer lease.
			if err != nil || runner.ID <= 0 || id == "" || runner.StartedAt.IsZero() || runner.StartedAt.Before(lease) {
				continue
			}
		}
		if bound == jobID {
			return status, true
		}
		if !found || lease.After(chosenLease) || (lease.Equal(chosenLease) && isTerminalAllocationState(chosen.State) && !isTerminalAllocationState(status.State)) {
			chosen, chosenLease, found = status, lease, true
		}
	}
	return chosen, found
}

func (s *Service) bindWorkflowExecution(status model.AllocationStatus, jobID int64) (model.AllocationStatus, error) {
	if executionJob(status) == jobID {
		return status, nil
	}
	copyWorkflowMetadata(&status)
	status.Metadata[executionJobKey] = strconv.FormatInt(jobID, 10)
	saved, err := s.store.SaveIfState(status, status.State)
	if err != nil {
		return status, err
	}
	if !saved {
		return status, ErrAllocationAlreadyCompleted
	}
	return status, nil
}

// recordDemandConclusion records completion independently of runner ownership.
// An allocation launched for this demand may currently execute another job.
func (s *Service) recordDemandConclusion(jobID int64, repository, conclusion string) error {
	origin, found := s.findByWorkflowJobID(jobID)
	if !found || !s.allocationServesRepository(origin, repository) {
		return nil
	}
	copyWorkflowMetadata(&origin)
	origin.Metadata[deferredConclusionKey] = conclusion
	_, err := s.store.SaveIfState(origin, origin.State)
	return err
}

// cancelPendingWorkflowDemand cannot race a reservation into provider cleanup:
// Pending has no scheduler reservation or resource; Reserved may already have
// registered a shared-label runner and must await a proven execution identity.
func (s *Service) cancelPendingWorkflowDemand(status model.AllocationStatus) (model.AllocationStatus, bool) {
	updated, ok := s.store.CompareAndMarkState(status.ID, model.StatePending, model.StateCanceled, s.now(), "workflow demand no longer pending")
	if ok {
		s.notifyLifecycleTerminal(updated)
		s.observeState()
	}
	return updated, ok
}

// suppressSurplusDemand only removes a pending origin when the actual runner
// has no displaced live demand. Otherwise that pending allocation remains the
// supply for the original job whose runner GitHub assigned elsewhere.
func (s *Service) suppressSurplusDemand(jobID int64, repository string, actual model.AllocationStatus) {
	if actual.Request == nil || actual.Request.WorkflowJobID == jobID {
		return
	}
	if _, completed := actual.Metadata[deferredConclusionKey]; !completed {
		return
	}
	origin, found := s.findByWorkflowJobID(jobID)
	if found && origin.ID != actual.ID && s.allocationServesRepository(origin, repository) && origin.State == model.StatePending {
		s.cancelPendingWorkflowDemand(origin)
	}
}

// legacyARCWorkflowAllocation identifies the label-only adapter, which owns no
// runner resource. A logical arc target migrated to Kubernetes owns JIT pods.
func (s *Service) legacyARCWorkflowAllocation(status model.AllocationStatus) bool {
	if status.SelectedBackend != model.BackendARC {
		return false
	}
	for _, pool := range s.cfg.Pools {
		if pool.Name == status.Pool {
			return pool.Backends[model.BackendARC].Driver != "kubernetes"
		}
	}
	return false
}

// FinalizeWorkflowJob uses actual runner ownership whenever supplied. A job
// canceled before assignment cannot safely delete the runner launched for its
// queued event: that runner may already be executing a different queued job.
func (s *Service) FinalizeWorkflowJob(ctx context.Context, jobID int64, conclusion string, runners ...WorkflowJobRunner) (status model.AllocationStatus, found bool, err error) {
	s.workflowJobMu.Lock()
	defer s.workflowJobMu.Unlock()
	runner := WorkflowJobRunner{}
	if len(runners) > 0 {
		runner = runners[0]
	}
	if err := s.recordDemandConclusion(jobID, runner.Repository, conclusion); err != nil {
		return status, false, err
	}
	status, found = s.executionAllocation(jobID, runner)
	if found {
		if isTerminalAllocationState(status.State) {
			return status, true, nil
		}
		status, err = s.bindWorkflowExecution(status, jobID)
		if err != nil {
			return status, true, err
		}
		s.suppressSurplusDemand(jobID, runner.Repository, status)
		return s.finishWorkflowExecution(ctx, status, conclusion)
	}
	// A supplied but unknown runner belongs to ARC, another controller, or
	// another repository. It must never release an unrelated JIT runner.
	if runner.Name != "" || runner.ID > 0 {
		return model.AllocationStatus{}, false, nil
	}
	status, found = s.findByWorkflowJobID(jobID)
	if !found || !s.allocationServesRepository(status, runner.Repository) {
		return model.AllocationStatus{}, false, nil
	}
	if isTerminalAllocationState(status.State) {
		return status, true, nil
	}
	// ARC owns no runner resource in this broker; retain its legacy accounting.
	if s.legacyARCWorkflowAllocation(status) {
		return s.finishWorkflowExecution(ctx, status, conclusion)
	}
	if status.State == model.StatePending {
		if updated, ok := s.cancelPendingWorkflowDemand(status); ok {
			return updated, true, nil
		}
		// A queue worker may have reserved it since we read the record. Do
		// not fall through to Cancel, which would delete a newly born runner.
		current, ok := s.store.Get(status.ID)
		return current, ok, nil
	}
	// Persist an out-of-order cancellation/completion until an event identifies
	// the actual runner. Keep any already launched runner available for demand.

	return status, true, nil
}

func (s *Service) finishWorkflowExecution(ctx context.Context, status model.AllocationStatus, conclusion string) (model.AllocationStatus, bool, error) {
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

// ClaimWorkflowJob binds the execution to the actual unique runner name/id.
// Job labels and the originating demand's job id are insufficient for JIT
// runners because GitHub independently assigns jobs sharing those labels.
func (s *Service) ClaimWorkflowJob(ctx context.Context, jobID int64, runnerName string, labels []string, runners ...WorkflowJobRunner) (status model.AllocationStatus, found bool, err error) {
	s.workflowJobMu.Lock()
	defer s.workflowJobMu.Unlock()
	runner := WorkflowJobRunner{Name: runnerName}
	if len(runners) > 0 {
		runner = runners[0]
		runner.Name = runnerName
	}
	status, found = s.executionAllocation(jobID, runner)
	if !found {
		// Persistent workers can finish a job and accept another before the
		// prior completion delivery removes routing labels. The signed event
		// proves this demand is already served; remove only Pending demand,
		// never a reservation or another allocation's runner resource.
		if runner.ID > 0 && runner.Name != "" && s.managedPersistentRunner(runner.Name, runner.Repository) {
			origin, ok := s.findByWorkflowJobID(jobID)
			if ok && origin.State == model.StatePending && s.allocationServesRepository(origin, runner.Repository) {
				updated, canceled := s.cancelPendingWorkflowDemand(origin)
				if canceled {
					return updated, true, nil
				}
			}
		}
		// ARC has no provider resource to delete and retains its static-label
		// accounting. Never apply this fallback to broker-created runner pods.
		origin, ok := s.findByWorkflowJobID(jobID)
		if !ok || !s.legacyARCWorkflowAllocation(origin) || !s.allocationServesRepository(origin, runner.Repository) {
			return model.AllocationStatus{}, false, nil
		}
		status, found = origin, true
	}
	if isTerminalAllocationState(status.State) {
		return status, true, nil
	}
	status, err = s.bindWorkflowExecution(status, jobID)
	if err != nil {
		return status, true, err
	}
	now := s.now()
	if status.ClaimedAt == nil || status.ClaimedAt.IsZero() {
		status.ClaimedAt = &now
	}
	if status.RegisteredAt == nil || status.RegisteredAt.IsZero() {
		status.RegisteredAt = &now
	}
	saved, err := s.store.SaveIfState(status, status.State)
	if err != nil {
		return status, true, err
	}
	if !saved {
		return status, true, ErrAllocationAlreadyCompleted
	}
	claimed := status
	s.suppressSurplusDemand(jobID, runner.Repository, claimed)
	// If completion arrived without runner identity before in_progress, it is
	// now safe to release precisely the runner that executed that canceled job.
	if origin, exists := s.findByWorkflowJobID(jobID); exists && s.allocationServesRepository(origin, runner.Repository) {
		if conclusion, deferred := origin.Metadata[deferredConclusionKey]; deferred {
			return s.finishWorkflowExecution(ctx, claimed, conclusion)
		}
	}
	return claimed, true, nil
}

func (s *Service) managedPersistentRunner(name, repository string) bool {
	for _, pool := range s.cfg.Pools {
		target := pool.Backends[model.BackendAzureVM].AzureVM
		if target == nil || !strings.EqualFold(target.Repository, repository) {
			continue
		}
		for _, allowed := range target.RunnerNames {
			if strings.EqualFold(allowed, name) {
				return true
			}
		}
	}
	return false
}

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func ownershipService(t *testing.T) (*Service, *testCleanupBackend) {
	t.Helper()
	cfg := config.Default()
	cfg.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeRepository, Owner: "example", Repository: "project"}
	cfg.Pools = []model.PoolConfig{{Name: model.PoolFull, Labels: []string{"linux"}, WebhookLabels: []string{"self-hosted", "shared-runner"}, Backends: map[model.BackendName]model.BackendConfig{
		model.BackendKubernetes: {Enabled: true, Healthy: true, MaxRunners: 5},
	}}}
	count := 0
	cleanup := &testCleanupBackend{testBackend: testBackend{name: model.BackendKubernetes}, cleanupCount: &count}
	return NewService(cfg, backend.NewRegistry(cleanup), nil), cleanup
}

func ownershipAllocate(t *testing.T, service *Service, jobID int64) model.AllocationStatus {
	t.Helper()
	status, created, err := service.AllocateForWorkflowJob(context.Background(), service.cfg.Pools[0], jobID, "example/project")
	if err != nil || !created {
		t.Fatalf("allocate job %d: status=%+v created=%v err=%v", jobID, status, created, err)
	}
	return status
}

func ownedRunner(status model.AllocationStatus) WorkflowJobRunner {
	return WorkflowJobRunner{Repository: "example/project", Name: status.RunnerLabel}
}

func TestWorkflowJobSwappedRunnersFinalizeActualExecution(t *testing.T) {
	service, cleanup := ownershipService(t)
	first, second := ownershipAllocate(t, service, 101), ownershipAllocate(t, service, 102)
	ctx := context.Background()
	for _, execution := range []struct {
		jobID  int64
		runner model.AllocationStatus
	}{{101, second}, {102, first}} {
		got, found, err := service.ClaimWorkflowJob(ctx, execution.jobID, execution.runner.RunnerLabel, []string{"shared-runner"}, ownedRunner(execution.runner))
		if err != nil || !found || got.ID != execution.runner.ID {
			t.Fatalf("claim job %d: status=%+v found=%v err=%v", execution.jobID, got, found, err)
		}
	}
	got, found, err := service.FinalizeWorkflowJob(ctx, 101, "success", ownedRunner(second))
	if err != nil || !found || got.ID != second.ID || got.State != model.StateCompleted {
		t.Fatalf("complete first job: %+v found=%v err=%v", got, found, err)
	}
	stillRunning, _ := service.Get(first.ID)
	if stillRunning.State != model.StateReady || executionJob(stillRunning) != 102 || *cleanup.cleanupCount != 1 {
		t.Fatalf("other execution disturbed: %+v cleanups=%d", stillRunning, *cleanup.cleanupCount)
	}
	got, found, err = service.FinalizeWorkflowJob(ctx, 102, "success", ownedRunner(first))
	if err != nil || !found || got.ID != first.ID || got.State != model.StateCompleted || *cleanup.cleanupCount != 2 {
		t.Fatalf("complete second job: %+v found=%v err=%v cleanups=%d", got, found, err, *cleanup.cleanupCount)
	}
}

func TestWorkflowJobCancellationDoesNotDeleteDifferentExecution(t *testing.T) {
	service, cleanup := ownershipService(t)
	first, second := ownershipAllocate(t, service, 201), ownershipAllocate(t, service, 202)
	ctx := context.Background()
	if _, _, err := service.ClaimWorkflowJob(ctx, 201, second.RunnerLabel, nil, ownedRunner(second)); err != nil {
		t.Fatal(err)
	}
	// The demand for 202 launched the runner that GitHub assigned to 201.
	if _, found, err := service.FinalizeWorkflowJob(ctx, 202, "cancelled", WorkflowJobRunner{Repository: "example/project"}); err != nil || !found {
		t.Fatalf("cancel demand: found=%v err=%v", found, err)
	}
	running, _ := service.Get(second.ID)
	if running.State != model.StateReady || executionJob(running) != 201 || *cleanup.cleanupCount != 0 {
		t.Fatalf("cancel deleted another execution: %+v cleanups=%d", running, *cleanup.cleanupCount)
	}
	if _, _, err := service.FinalizeWorkflowJob(ctx, 201, "success", ownedRunner(second)); err != nil {
		t.Fatal(err)
	}
	// A delayed start event identifies the actual runner for the canceled job.
	got, found, err := service.ClaimWorkflowJob(ctx, 202, first.RunnerLabel, nil, ownedRunner(first))
	if err != nil || !found || got.ID != first.ID || got.State != model.StateCanceled || *cleanup.cleanupCount != 2 {
		t.Fatalf("delayed canceled execution: %+v found=%v err=%v cleanups=%d", got, found, err, *cleanup.cleanupCount)
	}
}

func TestWorkflowJobCompletionBeforeStartUsesRunnerIdentity(t *testing.T) {
	service, cleanup := ownershipService(t)
	first, second := ownershipAllocate(t, service, 301), ownershipAllocate(t, service, 302)
	ctx := context.Background()
	got, found, err := service.FinalizeWorkflowJob(ctx, 301, "success", ownedRunner(second))
	if err != nil || !found || got.ID != second.ID || got.State != model.StateCompleted || *cleanup.cleanupCount != 1 {
		t.Fatalf("complete before start: %+v found=%v err=%v", got, found, err)
	}
	got, found, err = service.ClaimWorkflowJob(ctx, 301, second.RunnerLabel, nil, ownedRunner(second))
	if err != nil || !found || got.State != model.StateCompleted || *cleanup.cleanupCount != 1 {
		t.Fatalf("late start resurrected runner: %+v found=%v err=%v", got, found, err)
	}
	untouched, _ := service.Get(first.ID)
	if untouched.State != model.StateReady || untouched.ClaimedAt != nil {
		t.Fatalf("origin demand was confused with ownership: %+v", untouched)
	}
}

func TestWorkflowJobCannotClaimOrDeleteBySharedLabelsOrForeignIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		runner WorkflowJobRunner
	}{
		{"no runner", WorkflowJobRunner{Repository: "example/project"}},
		{"unknown runner", WorkflowJobRunner{Repository: "example/project", Name: "foreign-runner"}},
		{"foreign repository", WorkflowJobRunner{Repository: "example/other"}},
		{"conflicting runner id", WorkflowJobRunner{Repository: "example/project", ID: 999}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, cleanup := ownershipService(t)
			status := ownershipAllocate(t, service, 401)
			status.Metadata = map[string]string{"github_runner_id": "123"}
			if err := service.store.Save(status); err != nil {
				t.Fatal(err)
			}
			runner := tc.runner
			if tc.name == "foreign repository" || tc.name == "conflicting runner id" {
				runner.Name = status.RunnerLabel
			}
			if _, found, err := service.ClaimWorkflowJob(context.Background(), 401, runner.Name, []string{"shared-runner"}, runner); err != nil || found {
				t.Fatalf("unproven claim: found=%v err=%v", found, err)
			}
			_, _, err := service.FinalizeWorkflowJob(context.Background(), 401, "success", runner)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := service.Get(status.ID)
			if after.State != model.StateReady || after.ClaimedAt != nil || *cleanup.cleanupCount != 0 {
				t.Fatalf("unproven event altered runner: %+v cleanups=%d", after, *cleanup.cleanupCount)
			}
		})
	}
}

func TestWebhookRepositoryScopeMatchesEveryAction(t *testing.T) {
	service, _ := ownershipService(t)
	for repo, want := range map[string]bool{"example/project": true, "EXAMPLE/PROJECT": true, "example/other": false, "other/project": false, "": false, "example/project/extra": false} {
		if got := service.AcceptsWebhookRepository(repo); got != want {
			t.Errorf("AcceptsWebhookRepository(%q)=%v want %v", repo, got, want)
		}
	}
}

func TestWorkflowJobSignedHandlerUsesActualRunnerAndRepository(t *testing.T) {
	service, cleanup := ownershipService(t)
	first, second := ownershipAllocate(t, service, 501), ownershipAllocate(t, service, 502)
	h := newWebhookHarness(t, true)
	h.service = service
	h.server.service = service
	h.server.githubWebhook.service = service
	event := func(action, repo, runner, conclusion string) string {
		body := repoJobBody(action, 501, repo, "self-hosted", "shared-runner")
		body = strings.Replace(body, `"id":501,`, fmt.Sprintf(`"id":501,"runner_name":%q,`, runner), 1)
		if conclusion != "" {
			body = strings.Replace(body, `"conclusion":null`, fmt.Sprintf(`"conclusion":%q`, conclusion), 1)
		}
		return body
	}
	rec := h.send("workflow_job", "foreign-execution", event("in_progress", "example/other", second.RunnerLabel, ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "foreign_repository") {
		t.Fatalf("foreign claim: %d %s", rec.Code, rec.Body)
	}
	rec = h.send("workflow_job", "actual-execution", event("in_progress", "example/project", second.RunnerLabel, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("actual claim: %d %s", rec.Code, rec.Body)
	}
	claimed, _ := service.Get(second.ID)
	if executionJob(claimed) != 501 {
		t.Fatalf("wrong execution ownership: %+v", claimed)
	}
	rec = h.send("workflow_job", "actual-complete", event("completed", "example/project", second.RunnerLabel, "success"))
	if rec.Code != http.StatusOK || *cleanup.cleanupCount != 1 {
		t.Fatalf("actual complete: %d %s cleanups=%d", rec.Code, rec.Body, *cleanup.cleanupCount)
	}
	untouched, _ := service.Get(first.ID)
	if untouched.State != model.StateReady {
		t.Fatalf("cleaned up origin demand instead of actual runner: %+v", untouched)
	}
}

func TestWorkflowJobExecutionBindingSurvivesServiceRestart(t *testing.T) {
	service, cleanup := ownershipService(t)
	first, second := ownershipAllocate(t, service, 601), ownershipAllocate(t, service, 602)
	if _, _, err := service.ClaimWorkflowJob(context.Background(), 601, second.RunnerLabel, nil, ownedRunner(second)); err != nil {
		t.Fatal(err)
	}
	restarted := NewServiceWithStore(service.cfg, service.registry, nil, service.store)
	got, found, err := restarted.FinalizeWorkflowJob(context.Background(), 601, "success", WorkflowJobRunner{Repository: "example/project"})
	if err != nil || !found || got.ID != second.ID || *cleanup.cleanupCount != 1 {
		t.Fatalf("persisted binding: %+v found=%v err=%v", got, found, err)
	}
	untouched, _ := restarted.Get(first.ID)
	if untouched.State != model.StateReady {
		t.Fatalf("restart lost execution ownership: %+v", untouched)
	}
}

func TestWorkflowJobAndAPIShareNamedBackendCrossPoolLimit(t *testing.T) {
	service, cleanup := ownershipService(t)
	target := model.BackendName("kubernetes-idle")
	cleanup.testBackend.name = target
	backendConfig := model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 5, CrossPoolMaxRunners: 5}
	service.cfg.Pools = []model.PoolConfig{
		{Name: model.PoolLite, Backends: map[model.BackendName]model.BackendConfig{target: backendConfig}},
		{Name: "queue", WebhookLabels: []string{"self-hosted", "shared-runner"}, Backends: map[model.BackendName]model.BackendConfig{target: backendConfig}},
	}
	service.cfg.Broker.Queue.Enabled = false
	service = NewService(service.cfg, backend.NewRegistry(cleanup), nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := service.Allocate(ctx, model.AllocationRequest{Pool: model.PoolLite}); err != nil {
			t.Fatalf("API allocation %d: %v", i, err)
		}
	}
	var completed model.AllocationStatus
	for i := 0; i < 2; i++ {
		var err error
		completed, _, err = service.AllocateForWorkflowJob(ctx, service.cfg.Pools[1], int64(700+i), "example/project")
		if err != nil {
			t.Fatalf("webhook allocation %d: %v", i, err)
		}
	}
	if _, err := service.Allocate(ctx, model.AllocationRequest{Pool: model.PoolLite}); err == nil {
		t.Fatal("API exceeded shared five-runner limit")
	}
	if _, _, err := service.AllocateForWorkflowJob(ctx, service.cfg.Pools[1], 703, "example/project"); err == nil {
		t.Fatal("webhook exceeded shared five-runner limit")
	}
	if _, _, err := service.FinalizeWorkflowJob(ctx, 701, "success", ownedRunner(completed)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Allocate(ctx, model.AllocationRequest{Pool: model.PoolLite}); err != nil {
		t.Fatalf("released slot unavailable across pools: %v", err)
	}
}

func TestWorkflowJobPoolTimeoutDoesNotChangeAPIDefault(t *testing.T) {
	service, _ := ownershipService(t)
	service.cfg.Pools[0].WebhookJobTimeout = 6 * time.Hour
	webhook := ownershipAllocate(t, service, 801)
	if got := webhook.ExpiresAt.Sub(webhook.CreatedAt); got < 6*time.Hour || got > 6*time.Hour+time.Second {
		t.Fatalf("webhook timeout=%v want6h", got)
	}
	apiAllocation, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull})
	if err != nil {
		t.Fatal(err)
	}
	if got := apiAllocation.ExpiresAt.Sub(apiAllocation.CreatedAt); got < service.cfg.Broker.DefaultJobTimeout || got > service.cfg.Broker.DefaultJobTimeout+time.Second {
		t.Fatalf("API default changed=%v", got)
	}
	service.cfg.Pools[0].WebhookJobTimeout = 0
	defaultWebhook := ownershipAllocate(t, service, 802)
	if got := defaultWebhook.ExpiresAt.Sub(defaultWebhook.CreatedAt); got < service.cfg.Broker.DefaultJobTimeout || got > service.cfg.Broker.DefaultJobTimeout+time.Second {
		t.Fatalf("webhook default changed=%v", got)
	}
}

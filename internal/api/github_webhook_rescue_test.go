package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type rescueLister struct {
	fakeWorkflowJobLister
	gets int
}

func (r *rescueLister) GetWorkflowJob(_ context.Context, repository string, jobID int64) (github.WorkflowJob, error) {
	r.gets++
	return github.WorkflowJob{
		ID:     jobID,
		RunID:  12,
		Status: "queued",
		Labels: []string{"self-hosted", "uecb-full"},
	}, nil
}

func TestReconcileRescuesTerminalStillQueuedJobsFirst(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolFull {
			pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
		}
	})
	server := newTestServer(t, service)
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	failed := model.AllocationStatus{
		ID:        "old",
		Pool:      model.PoolFull,
		State:     model.StateFailed,
		CreatedAt: now.Add(-time.Hour),
		ExpiresAt: now.Add(-30 * time.Minute),
		Request: &model.AllocationRequest{
			Pool:             model.PoolFull,
			WorkflowJobID:    42,
			TargetRepository: "my-org/home",
			Labels:           []string{"self-hosted", "uecb-full"},
		},
	}
	if err := service.store.Save(failed); err != nil {
		t.Fatal(err)
	}

	lister := &rescueLister{
		fakeWorkflowJobLister: fakeWorkflowJobLister{
			repos: []string{"my-org/home"},
			runs:  map[string][]int64{"my-org/home": {}},
			jobs:  map[string][]github.WorkflowJob{},
		},
	}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: lister})

	got, err := server.ReconcileQueuedGitHubJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got < 1 {
		t.Fatalf("allocated=%d, want at least the rescued job", got)
	}
	if lister.gets < 1 {
		t.Fatal("rescue pass should GetWorkflowJob")
	}
	live, ok := service.findByWorkflowJobID(42)
	if !ok || isTerminalAllocationState(live.State) {
		t.Fatalf("rescued allocation = %+v ok=%v", live, ok)
	}
}

func TestProcessRunPrefersTerminalJobs(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolFull {
			pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
		}
	})
	now := time.Now().UTC()
	if err := service.store.Save(model.AllocationStatus{
		ID: "term", Pool: model.PoolFull, State: model.StateExpired, CreatedAt: now.Add(-time.Hour),
		Request: &model.AllocationRequest{WorkflowJobID: 2, TargetRepository: "o/r", Labels: []string{"self-hosted", "uecb-full"}},
	}); err != nil {
		t.Fatal(err)
	}
	h := &githubWebhookHandler{service: service}
	r := &reconcileRun{h: h}
	if r.terminalPriority(2) != 1 {
		t.Fatal("terminal job should rank higher")
	}
	if r.terminalPriority(3) != 0 {
		t.Fatal("unknown job should rank lower")
	}
}

func TestGitHubWebhookAcknowledgesWithAccepted(t *testing.T) {
	h := newWebhookHarness(t, true)
	rec := h.send("workflow_job", "async-ack-1", jobBody("queued", 9001, "", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d %s, want 202 Accepted", rec.Code, rec.Body)
	}
	if _, ok := h.service.findByWorkflowJobID(9001); !ok {
		t.Fatal("worker should allocate after ack")
	}
}

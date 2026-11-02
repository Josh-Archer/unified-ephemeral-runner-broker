package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func reconcileRetryHarness(t *testing.T) (*Service, *Server, *time.Time) {
	t.Helper()
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolFull {
			pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
		}
	})
	clock := time.Now()
	service.now = func() time.Time { return clock }
	server := newTestServer(t, service)
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: fakeWorkflowJobLister{
		repos: []string{"my-org/home"},
		runs:  map[string][]int64{"my-org/home": {12}},
		jobs: map[string][]github.WorkflowJob{"12": {
			{ID: 99, RunID: 12, Status: "queued", Labels: []string{"self-hosted", "uecb-full"}},
		}},
	}})
	return service, server, &clock
}

func terminate(t *testing.T, service *Service, state model.AllocationState) string {
	t.Helper()
	current, ok := service.findByWorkflowJobID(99)
	if !ok {
		t.Fatal("allocation missing")
	}
	current.State = state
	if err := service.Store().Save(current); err != nil {
		t.Fatal(err)
	}
	return current.ID
}

func TestReconcileReallocatesQueuedJobAfterAnyTerminalState(t *testing.T) {
	for _, state := range []model.AllocationState{model.StateExpired, model.StateCanceled, model.StateCompleted, model.StateQuarantined, model.StateFailed} {
		t.Run(string(state), func(t *testing.T) {
			service, server, _ := reconcileRetryHarness(t)
			if got, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil || got != 1 {
				t.Fatalf("first reconcile allocated=%d err=%v", got, err)
			}
			old := terminate(t, service, state)
			if got, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil || got != 1 {
				t.Fatalf("reconcile after %s allocated=%d err=%v, want re-allocation", state, got, err)
			}
			if fresh, ok := service.findByWorkflowJobID(99); !ok || fresh.ID == old || isTerminalAllocationState(fresh.State) {
				t.Fatalf("fresh allocation = %+v ok=%v", fresh, ok)
			}
		})
	}
}

func TestReconcileReallocationBacksOffAndIsBounded(t *testing.T) {
	service, server, clock := reconcileRetryHarness(t)
	if got, _ := server.ReconcileQueuedGitHubJobs(context.Background()); got != 1 {
		t.Fatalf("first reconcile allocated=%d", got)
	}
	terminate(t, service, model.StateExpired)
	if got, _ := server.ReconcileQueuedGitHubJobs(context.Background()); got != 1 {
		t.Fatalf("first retry allocated=%d, want immediate retry", got)
	}
	terminate(t, service, model.StateExpired)
	if got, _ := server.ReconcileQueuedGitHubJobs(context.Background()); got != 0 {
		t.Fatalf("retry inside backoff allocated=%d, want 0", got)
	}
	*clock = clock.Add(workflowJobRetryMax)
	terminate(t, service, model.StateExpired) // still expired from above
	current, _ := service.findByWorkflowJobID(99)
	for n := service.countWorkflowJobAllocations(99); n < maxWorkflowJobAllocations; n++ {
		filler := current
		filler.ID = fmt.Sprintf("filler-%d", n)
		if err := service.Store().Save(filler); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := server.ReconcileQueuedGitHubJobs(context.Background()); got != 0 {
		t.Fatalf("reconcile past retry cap allocated=%d, want 0", got)
	}
	if service.mayReallocateWorkflowJob(99, *clock) {
		t.Fatal("job at its allocation cap must not be re-allocated")
	}
	if n := service.countWorkflowJobAllocations(99); n != maxWorkflowJobAllocations {
		t.Fatalf("allocations for job = %d, want cap %d", n, maxWorkflowJobAllocations)
	}
}

func TestWorkflowJobRetryBackoff(t *testing.T) {
	if workflowJobRetryBackoff(1) != 30*time.Second || workflowJobRetryBackoff(2) != time.Minute || workflowJobRetryBackoff(10) != workflowJobRetryMax {
		t.Fatal("unexpected backoff schedule")
	}
}

func TestRegistrationExpiriesSkipBackendWhileOthersRemain(t *testing.T) {
	service := newServiceWithConfig(nil)
	now := time.Now()
	service.now = func() time.Time { return now }
	pool := model.PoolConfig{Name: "shared", Backends: map[model.BackendName]model.BackendConfig{
		model.BackendGCE: {}, model.BackendAzureVM: {}, "kubernetes-shared": {},
	}}
	service.recordRegistrationExpiry(pool.Name, model.BackendGCE, now)
	if got := service.filterRegistrationPenalized(pool, model.AllocationRequest{}); len(got.Backends) != 3 {
		t.Fatalf("one expiry should not skip: %v", got.Backends)
	}
	service.recordRegistrationExpiry(pool.Name, model.BackendGCE, now)
	service.recordRegistrationExpiry(pool.Name, model.BackendAzureVM, now)
	service.recordRegistrationExpiry(pool.Name, model.BackendAzureVM, now)
	got := service.filterRegistrationPenalized(pool, model.AllocationRequest{})
	if _, ok := got.Backends["kubernetes-shared"]; !ok || len(got.Backends) != 1 {
		t.Fatalf("penalized backends not skipped: %v", got.Backends)
	}
	pinned := model.BackendGCE
	if got := service.filterRegistrationPenalized(pool, model.AllocationRequest{Backend: &pinned}); len(got.Backends) != 3 {
		t.Fatal("pinned request must not be filtered")
	}
	only := model.PoolConfig{Name: "shared", Backends: map[model.BackendName]model.BackendConfig{model.BackendGCE: {}, model.BackendAzureVM: {}}}
	if got := service.filterRegistrationPenalized(only, model.AllocationRequest{}); len(got.Backends) != 2 {
		t.Fatal("must not filter every backend away")
	}
	service.recordRegistrationSuccess(pool.Name, model.BackendGCE)
	if got := service.filterRegistrationPenalized(pool, model.AllocationRequest{}); len(got.Backends) != 2 {
		t.Fatalf("registration success should clear penalty: %v", got.Backends)
	}
	now = now.Add(registrationExpiryCooldown + time.Second)
	if got := service.filterRegistrationPenalized(pool, model.AllocationRequest{}); len(got.Backends) != 3 {
		t.Fatalf("cooldown should elapse: %v", got.Backends)
	}
}

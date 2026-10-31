package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/capacity"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

func workflowQueueConfig() model.BrokerConfig {
	cfg := config.Default()
	cfg.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeRepository, Owner: "example", Repository: "project"}
	cfg.Broker.Queue = model.AdmissionQueueConfig{Enabled: true, RetryAfter: time.Second, MaxAttempts: 1}
	cfg.Pools = []model.PoolConfig{{Name: model.PoolFull, WebhookJobTimeout: time.Hour, WebhookLabels: []string{"self-hosted", "shared-runner"}, Backends: map[model.BackendName]model.BackendConfig{
		model.BackendKubernetes: {Enabled: true, Healthy: true, MaxRunners: 1, CrossPoolMaxRunners: 1},
	}}}
	return cfg
}

func TestWorkflowCapacityQueueSurvivesRestartAndDrains(t *testing.T) {
	cfg := workflowQueueConfig()
	cfg.Broker.StateStore = model.StateStoreConfig{Type: "file", Path: t.TempDir() + "/allocations.json"}
	makeService := func() *Service {
		return NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendKubernetes}), nil)
	}
	now := time.Now().UTC()
	service := makeService()
	service.now = func() time.Time { return now }
	first := ownershipAllocate(t, service, 1001)
	pending := ownershipAllocate(t, service, 1002)
	if pending.State != model.StatePending {
		t.Fatalf("full capacity should persist pending demand: %+v", pending)
	}
	duplicate, created, err := service.AllocateForWorkflowJob(context.Background(), cfg.Pools[0], 1002, "example/project")
	if err != nil || created || duplicate.ID != pending.ID {
		t.Fatalf("duplicate: %+v created=%v err=%v", duplicate, created, err)
	}
	service = makeService()
	service.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		now = now.Add(2 * time.Second)
		service.ReconcileQueue(context.Background(), now)
		got, _ := service.Get(pending.ID)
		if got.State != model.StatePending || got.Attempts != 0 || !got.ExpiresAt.Equal(pending.ExpiresAt) {
			t.Fatalf("capacity wait consumed retry budget/deadline: %+v", got)
		}
	}
	if _, _, err := service.FinalizeWorkflowJob(context.Background(), 1001, "success", ownedRunner(first)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	service.ReconcileQueue(context.Background(), now)
	got, _ := service.Get(pending.ID)
	if got.State != model.StateReady || got.ID != pending.ID || !got.ExpiresAt.Equal(pending.ExpiresAt) {
		t.Fatalf("pending demand did not become ready: %+v", got)
	}
	if n := service.store.CountBackendActive(model.BackendKubernetes); n != 1 {
		t.Fatalf("shared capacity exceeded: %d", n)
	}
}

func TestWorkflowLiveCapacityQueuesButAPIStillFailsFast(t *testing.T) {
	cfg := workflowQueueConfig()
	enableLiveCapacity(&cfg)
	service := NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendKubernetes}), nil)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	manager := capacity.NewManager()
	manager.Set(capacity.Snapshot{Backend: model.BackendKubernetes, Status: backend.CapacityStatus{MaxRunners: 1, ActiveRunners: 1}, UpdatedAt: now, Source: "live"})
	service.SetCapacityManager(manager)
	if _, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull}); !errors.Is(err, ErrBackendLiveCapacity) {
		t.Fatalf("API must fail fast: %v", err)
	}
	server := newTestServer(t, service)
	server.SetGitHubWebhook(GitHubWebhookOptions{Secret: func(context.Context) (string, error) { return testWebhookSecret, nil }})
	harness := webhookHarness{t: t, service: service, handler: server.Handler()}
	body := `{"action":"queued","workflow_job":{"id":2001,"labels":["shared-runner"]},"repository":{"full_name":"example/project"}}`
	rec := harness.send("workflow_job", "capacity-event", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook must acknowledge durable demand: %d %s", rec.Code, rec.Body)
	}
	pending, ok := service.findByWorkflowJobID(2001)
	if !ok || pending.State != model.StatePending {
		t.Fatalf("missing pending demand: %+v", pending)
	}
	now = now.Add(2 * time.Second)
	manager.Set(capacity.Snapshot{Backend: model.BackendKubernetes, Status: backend.CapacityStatus{MaxRunners: 1}, UpdatedAt: now, Source: "live"})
	service.ReconcileQueue(context.Background(), now)
	got, _ := service.Get(pending.ID)
	if got.State != model.StateReady {
		t.Fatalf("capacity did not recover: %+v", got)
	}
}

func TestWorkflowProviderCapacityRejectionQueues(t *testing.T) {
	cfg := workflowQueueConfig()
	capacityErr := backend.NewAllocationError(errors.New("node unavailable"), backend.ErrBackendCapacityExhausted, true)
	impl := &sequenceBackend{testBackend: testBackend{name: model.BackendKubernetes}, errs: []error{capacityErr, capacityErr, capacityErr}}
	service := NewService(cfg, backend.NewRegistry(impl), nil)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	pending := ownershipAllocate(t, service, 3001)
	if pending.State != model.StatePending {
		t.Fatalf("provider rejection lost demand: %+v", pending)
	}
	for i := 0; i < 2; i++ {
		now = now.Add(10 * time.Minute)
		service.ReconcileQueue(context.Background(), now)
		got, _ := service.Get(pending.ID)
		if got.State != model.StatePending || got.Attempts != 0 || !got.ExpiresAt.Equal(pending.ExpiresAt) {
			t.Fatalf("provider capacity wait reset deadline or retry count: %+v", got)
		}
	}
	now = now.Add(2 * time.Second)
	service.ReconcileQueue(context.Background(), now)
	got, _ := service.Get(pending.ID)
	if got.State != model.StateReady {
		t.Fatalf("provider recovery: %+v", got)
	}
}

func TestWorkflowPendingCancellationCannotBeResurrected(t *testing.T) {
	cfg := workflowQueueConfig()
	service := NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendKubernetes}), nil)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	first := ownershipAllocate(t, service, 4001)
	pending := ownershipAllocate(t, service, 4002)
	if _, _, err := service.FinalizeWorkflowJob(context.Background(), 4002, "cancelled", WorkflowJobRunner{Repository: "example/project"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.FinalizeWorkflowJob(context.Background(), 4001, "success", ownedRunner(first)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	// Reproduce a queue snapshot taken before cancellation acquired the lock.
	service.reconcileQueuedAllocation(context.Background(), pending, now)
	got, _ := service.Get(pending.ID)
	if got.State != model.StateCanceled {
		t.Fatalf("cancelled demand resurrected: %+v", got)
	}
}

func TestWorkflowCapacityWaitStillExpires(t *testing.T) {
	cfg := workflowQueueConfig()
	service := NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendKubernetes}), nil)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	ownershipAllocate(t, service, 5001)
	pending := ownershipAllocate(t, service, 5002)
	now = pending.ExpiresAt.Add(time.Second)
	service.ReconcileQueue(context.Background(), now)
	got, _ := service.Get(pending.ID)
	if got.State != model.StateExpired {
		t.Fatalf("capacity wait ignored deadline: %+v", got)
	}
}

func TestWorkflowCapacityClassificationPreservesBudgetAndRateLimits(t *testing.T) {
	for _, err := range []error{ErrBackendRateLimited, ErrBackendBudgetExceeded, errors.New("invalid request")} {
		if workflowCapacityError(model.AllocationRequest{WorkflowJobID: 1}, err) {
			t.Fatalf("noncapacity error queued as capacity: %v", err)
		}
	}
	if workflowCapacityError(model.AllocationRequest{}, scheduler.ErrNoCapacity) {
		t.Fatal("API capacity policy changed")
	}
}

func TestWorkflowTierBlockedPrimaryAndFullFallbackKeepsDemand(t *testing.T) {
	for _, state := range []string{tier.StateHealthy, tier.StateApproaching, tier.StateUnknown} {
		t.Run(string(state), func(t *testing.T) {
			cfg := workflowQueueConfig()
			cfg.Broker.TierRouting.Enabled = true
			cfg.Broker.TierRouting.FailureMode = tier.FailureModePassThrough
			cfg.Pools[0].Backends[model.BackendARC] = model.BackendConfig{Enabled: true, Healthy: true, MaxRunners: 1}
			service := NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendKubernetes}, testBackend{name: model.BackendARC}), nil)
			now := time.Now().UTC()
			service.now = func() time.Time { return now }
			manager := tier.NewManager()
			manager.SetDecision(tier.Decision{Pool: model.PoolFull, Backend: model.BackendARC, State: tier.StateExceeded, Action: tier.ActionDisable, UpdatedAt: now})
			if state != tier.StateUnknown {
				manager.SetDecision(tier.Decision{Pool: model.PoolFull, Backend: model.BackendKubernetes, State: state, Action: tier.ActionDeprioritize, UpdatedAt: now})
			}
			service.SetTierManager(manager)
			first := ownershipAllocate(t, service, 6001)
			if first.SelectedBackend != model.BackendKubernetes {
				t.Fatalf("pressure-blocked primary was used: %+v", first)
			}
			if _, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull}); !errors.Is(err, scheduler.ErrNoCapacity) {
				t.Fatalf("API should report full fallback, not a tier rejection: %v", err)
			}
			pending := ownershipAllocate(t, service, 6002)
			for i := 0; i < 3; i++ {
				now = now.Add(2 * time.Second)
				service.ReconcileQueue(context.Background(), now)
				got, _ := service.Get(pending.ID)
				if got.State != model.StatePending || got.Attempts != 0 {
					t.Fatalf("full tier-eligible fallback lost demand: %+v", got)
				}
			}
			if _, _, err := service.FinalizeWorkflowJob(context.Background(), 6001, "success", ownedRunner(first)); err != nil {
				t.Fatal(err)
			}
			now = now.Add(2 * time.Second)
			service.ReconcileQueue(context.Background(), now)
			got, _ := service.Get(pending.ID)
			if got.State != model.StateReady || got.SelectedBackend != model.BackendKubernetes || !got.ExpiresAt.Equal(pending.ExpiresAt) {
				t.Fatalf("fallback did not drain safely: %+v", got)
			}
		})
	}
}

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scaleset"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

func TestScaleSetModeConstantsMatchListener(t *testing.T) {
	if model.ScaleSetModeObserve != scaleset.ModeObserve || model.ScaleSetModeAllocate != scaleset.ModeAllocate {
		t.Fatalf("mode constants drifted: model %s/%s scaleset %s/%s", model.ScaleSetModeObserve, model.ScaleSetModeAllocate, scaleset.ModeObserve, scaleset.ModeAllocate)
	}
}

func TestScaleSetListenerAllocatesFromMessagesNotQueuedRuns(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolFull {
			return
		}
		pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
		pool.ScaleSet = model.PoolScaleSetConfig{Name: "uecb-shared", ID: 42}
	})
	service.cfg.Broker.ScaleSetListener.Enabled = true
	service.cfg.Broker.ScaleSetListener.Mode = model.ScaleSetModeAllocate

	calls := 0
	server := newTestServer(t, service)
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: scanningLister{
		fakeWorkflowJobLister: fakeWorkflowJobLister{
			repos: []string{"my-org/home"},
			runs:  map[string][]int64{"my-org/home": {12}},
			jobs: map[string][]github.WorkflowJob{"12": {{
				ID: 99, RunID: 12, Status: "queued", Labels: []string{"self-hosted", "uecb-full"},
			}}},
		},
		calls: &calls,
	}})
	if got, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil || got != 0 || calls != 0 {
		t.Fatalf("reconcile allocated=%d scanCalls=%d err=%v, want the queued-run scan skipped", got, calls, err)
	}

	client := &scaleQueue{
		session: scaleset.Session{
			SessionID:               "sess-1",
			MessageQueueURL:         "https://example.test/queue",
			MessageQueueAccessToken: "token",
			Statistics:              &scaleset.Statistic{TotalAssignedJobs: 0},
		},
		msgs: []*scaleset.Message{
			{MessageID: 7, Statistics: &scaleset.Statistic{TotalAssignedJobs: 2}},
			{MessageID: 8, Statistics: &scaleset.Statistic{TotalAssignedJobs: 2}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- service.RunScaleSetListenerWithClients(ctx, map[string]scaleset.Client{"uecb-shared": client}, nil)
	}()
	waitScale := func(allocs, acks int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			deleted, _ := client.snapshot()
			if len(service.Store().List()) >= allocs && len(deleted) >= acks {
				return
			}
			select {
			case err := <-done:
				t.Fatalf("listener stopped early: %v allocations=%d deleted=%v", err, len(service.Store().List()), deleted)
			case <-time.After(10 * time.Millisecond):
			}
		}
		deleted, maxes := client.snapshot()
		t.Fatalf("timed out allocations=%d deleted=%v maxes=%v", len(service.Store().List()), deleted, maxes)
	}
	waitScale(2, 2)
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not stop")
	}
	if calls != 0 {
		t.Fatalf("queued-run scan calls=%d", calls)
	}
	allocs := service.Store().List()
	if len(allocs) != 2 {
		t.Fatalf("allocations=%d, want 2 from TotalAssignedJobs", len(allocs))
	}
	for _, status := range allocs {
		if status.Metadata["demand_source"] != "scaleset" || status.Metadata["scale_set"] != "uecb-shared" {
			t.Fatalf("metadata=%v", status.Metadata)
		}
		if status.State != model.StateReady || status.Pool != model.PoolFull || status.SelectedBackend != model.BackendARC {
			t.Fatalf("status=%+v", status)
		}
		joined := strings.Join(status.RequestedLabels, ",")
		if !strings.Contains(joined, "uecb-shared") {
			t.Fatalf("labels=%v", status.RequestedLabels)
		}
	}
	_, maxes := client.snapshot()
	if len(maxes) == 0 || maxes[0] != 4 {
		t.Fatalf("reported max runners=%v, want the pool's enabled maxRunners sum 4", maxes)
	}
}

func TestScaleSetListenerCountsExistingRunners(t *testing.T) {
	service := scaleSetService(t, model.ScaleSetModeAllocate)
	if err := service.Store().Save(model.AllocationStatus{
		ID: "already", Pool: model.PoolFull, SelectedBackend: model.BackendARC,
		State: model.StateReady, ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	client := &scaleQueue{
		session: scaleset.Session{SessionID: "sess", MessageQueueURL: "https://example.test/q", MessageQueueAccessToken: "t", Statistics: &scaleset.Statistic{}},
		msgs:    []*scaleset.Message{{MessageID: 1, Statistics: &scaleset.Statistic{TotalAssignedJobs: 2}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- service.RunScaleSetListenerWithClients(ctx, map[string]scaleset.Client{"uecb-shared": client}, nil)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(service.Store().List()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if got := len(service.Store().List()); got != 2 {
		t.Fatalf("allocations=%d, want the existing runner counted toward TotalAssignedJobs", got)
	}
}

func TestScaleSetAllocateUsesTierFallbackAndQuota(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name != model.PoolFull {
			return
		}
		pool.ScaleSet = model.PoolScaleSetConfig{Name: "uecb-shared", ID: 7}
		pool.FairShare.Enabled = true
		pool.FairShare.Quotas = map[string]int{"default": 1}
		codebuild := pool.Backends[model.BackendCodeBuild]
		codebuild.Enabled = true
		codebuild.Healthy = true
		codebuild.MaxRunners = 2
		pool.Backends[model.BackendCodeBuild] = codebuild
	})
	service.cfg.Broker.ScaleSetListener.Enabled = true
	service.cfg.Broker.ScaleSetListener.Mode = model.ScaleSetModeAllocate
	service.cfg.Broker.TierRouting.Enabled = true
	service.cfg.Broker.TierRouting.FailureMode = tier.FailureModePassThrough
	manager := tier.NewManager()
	now := time.Now().UTC()
	manager.SetDecision(tier.Decision{
		Pool: model.PoolFull, Backend: model.BackendARC,
		State: tier.StateExceeded, Action: tier.ActionDisable, UpdatedAt: now,
	})
	service.SetTierManager(manager)

	client := &scaleQueue{
		session: scaleset.Session{SessionID: "sess", MessageQueueURL: "https://example.test/q", MessageQueueAccessToken: "t", Statistics: &scaleset.Statistic{}},
		msgs:    []*scaleset.Message{{MessageID: 3, Statistics: &scaleset.Statistic{TotalAssignedJobs: 3}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- service.RunScaleSetListenerWithClients(ctx, map[string]scaleset.Client{"uecb-shared": client}, nil)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		deleted, _ := client.snapshot()
		if len(deleted) >= 1 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("listener stopped before ack: %v", err)
		case <-time.After(10 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for ack")
			}
		}
	}
	cancel()
	<-done
	allocs := service.Store().List()
	if len(allocs) != 1 {
		t.Fatalf("allocations=%d, want the tenant quota of 1 even though 3 jobs are assigned", len(allocs))
	}
	if allocs[0].SelectedBackend != model.BackendCodeBuild {
		t.Fatalf("backend=%s, want codebuild after ARC was disabled by tier", allocs[0].SelectedBackend)
	}
	if allocs[0].State != model.StateReady {
		t.Fatalf("state=%s", allocs[0].State)
	}
}

func TestScaleSetObserveLogsDemandWithoutAllocatingOrSkippingScan(t *testing.T) {
	service := scaleSetService(t, model.ScaleSetModeObserve)
	service.cfg.Pools[0].WebhookLabels = []string{"self-hosted", "uecb-full"}
	calls := 0
	server := newTestServer(t, service)
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: scanningLister{
		fakeWorkflowJobLister: fakeWorkflowJobLister{repos: []string{"my-org/home"}},
		calls:                 &calls,
	}})
	if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil || calls == 0 {
		t.Fatalf("observe mode scan calls=%d err=%v, want the queued-run reconcile to keep running", calls, err)
	}
	before := counterValue(t, "uecb_scaleset_decisions_total", map[string]string{"pool": "full", "mode": "observe", "outcome": "disagree"})
	client := &scaleQueue{
		session: scaleset.Session{SessionID: "sess", MessageQueueURL: "https://example.test/q", MessageQueueAccessToken: "t", Statistics: &scaleset.Statistic{}},
		msgs:    []*scaleset.Message{{MessageID: 4, Statistics: &scaleset.Statistic{TotalAssignedJobs: 4}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- service.RunScaleSetListenerWithClients(ctx, map[string]scaleset.Client{"uecb-shared": client}, nil)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for counterValue(t, "uecb_scaleset_decisions_total", map[string]string{"pool": "full", "mode": "observe", "outcome": "disagree"}) < before+1 {
		select {
		case err := <-done:
			t.Fatalf("listener stopped: %v", err)
		case <-time.After(10 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for disagreement")
			}
		}
	}
	cancel()
	<-done
	if got := len(service.Store().List()); got != 0 {
		t.Fatalf("observe mode allocated %d runners", got)
	}
}

func TestScaleSetAllocateSkipsWebhookAndLeavesOtherPoolsOnTheScan(t *testing.T) {
	h := newWebhookHarness(t, true)
	h.service.cfg.Broker.ScaleSetListener.Enabled = true
	h.service.cfg.Broker.ScaleSetListener.Mode = model.ScaleSetModeAllocate
	h.service.cfg.Pools[0].ScaleSet = model.PoolScaleSetConfig{Name: "uecb-shared", ID: 9}
	before := counterValue(t, "uecb_github_webhook_events_ignored_total", map[string]string{"reason": "scale_set_listener"})
	body := jobBody("queued", 50, "", "self-hosted", "uecb-full")
	rec := h.send("workflow_job", "scale-set-skip", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if h.activeCount() != 0 {
		t.Fatalf("webhook allocated %d runners for a scale-set pool", h.activeCount())
	}
	if got := counterValue(t, "uecb_github_webhook_events_ignored_total", map[string]string{"reason": "scale_set_listener"}); got != before+1 {
		t.Fatalf("ignored counter=%v, want %v", got, before+1)
	}

	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		switch pool.Name {
		case model.PoolFull:
			pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
			pool.ScaleSet = model.PoolScaleSetConfig{Name: "uecb-shared", ID: 9}
		case model.PoolLite:
			pool.WebhookLabels = []string{"self-hosted", "uecb-lite"}
		}
	})
	service.cfg.Broker.ScaleSetListener.Enabled = true
	service.cfg.Broker.ScaleSetListener.Mode = model.ScaleSetModeAllocate
	server := newTestServer(t, service)
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: fakeWorkflowJobLister{
		repos: []string{"my-org/home"},
		runs:  map[string][]int64{"my-org/home": {12}},
		jobs: map[string][]github.WorkflowJob{"12": {
			{ID: 1, RunID: 12, Status: "queued", Labels: []string{"self-hosted", "uecb-full"}},
			{ID: 2, RunID: 12, Status: "queued", Labels: []string{"self-hosted", "uecb-lite"}},
		}},
	}})
	got, err := server.ReconcileQueuedGitHubJobs(context.Background())
	if err != nil || got != 1 {
		t.Fatalf("allocated=%d err=%v, want only the non-scale-set pool", got, err)
	}
	if _, ok := service.findByWorkflowJobID(1); ok {
		t.Fatal("queued-run scan allocated the scale-set pool")
	}
	if status, ok := service.findByWorkflowJobID(2); !ok || status.Pool != model.PoolLite {
		t.Fatalf("lite allocation=%+v ok=%v", status, ok)
	}
}

func scaleSetService(t *testing.T, mode string) *Service {
	t.Helper()
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolFull {
			pool.ScaleSet = model.PoolScaleSetConfig{Name: "uecb-shared", ID: 42}
		}
	})
	service.cfg.Broker.ScaleSetListener.Enabled = true
	service.cfg.Broker.ScaleSetListener.Mode = mode
	return service
}

type scanningLister struct {
	fakeWorkflowJobLister
	calls *int
}

func (l scanningLister) ListRepositories(ctx context.Context, scope model.GitHubScope) ([]string, error) {
	*l.calls++
	return l.fakeWorkflowJobLister.ListRepositories(ctx, scope)
}

func (l scanningLister) ListQueuedWorkflowRuns(ctx context.Context, repository string, since time.Time) ([]int64, error) {
	*l.calls++
	return l.fakeWorkflowJobLister.ListQueuedWorkflowRuns(ctx, repository, since)
}

type scaleQueue struct {
	mu      sync.Mutex
	session scaleset.Session
	msgs    []*scaleset.Message
	pos     int
	maxes   []int
	deleted []int
}

func (q *scaleQueue) Session() scaleset.Session { return q.session }

func (q *scaleQueue) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.Message, error) {
	q.mu.Lock()
	q.maxes = append(q.maxes, maxCapacity)
	if q.pos < len(q.msgs) {
		msg := q.msgs[q.pos]
		q.pos++
		q.mu.Unlock()
		_ = lastMessageID
		return msg, nil
	}
	q.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (q *scaleQueue) DeleteMessage(_ context.Context, id int) error {
	q.mu.Lock()
	q.deleted = append(q.deleted, id)
	q.mu.Unlock()
	return nil
}

func (q *scaleQueue) AcquireJobs(context.Context, []int64) ([]int64, error) { return nil, nil }

func (q *scaleQueue) snapshot() (deleted, maxes []int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]int(nil), q.deleted...), append([]int(nil), q.maxes...)
}

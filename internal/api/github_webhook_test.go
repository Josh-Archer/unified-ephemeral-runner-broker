package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

const testWebhookSecret = "s3cret"

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func jobBody(action string, id int64, conclusion string, labels ...string) string {
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = fmt.Sprintf("%q", l)
	}
	concl := "null"
	if conclusion != "" {
		concl = fmt.Sprintf("%q", conclusion)
	}
	return fmt.Sprintf(`{"action":%q,"workflow_job":{"id":%d,"conclusion":%s,"labels":[%s]},"repository":{"full_name":"my-org/home"}}`, action, id, concl, strings.Join(quoted, ","))
}

type webhookHarness struct {
	t       *testing.T
	service *Service
	handler http.Handler
	leader  bool
	inbox   store.WebhookInbox
	server  *Server
}

type fakeWorkflowJobLister struct {
	repos []string
	runs  map[string][]int64
	jobs  map[string][]github.WorkflowJob
	err   error
	since *time.Time
}

func (f fakeWorkflowJobLister) ListRepositories(context.Context, model.GitHubScope) ([]string, error) {
	return f.repos, f.err
}
func (f fakeWorkflowJobLister) ListQueuedWorkflowRuns(_ context.Context, repository string, since time.Time) ([]int64, error) {
	if f.since != nil {
		*f.since = since
	}
	return f.runs[repository], f.err
}
func (f fakeWorkflowJobLister) ListWorkflowJobs(_ context.Context, _ string, runID int64) ([]github.WorkflowJob, error) {
	return f.jobs[fmt.Sprint(runID)], f.err
}

func TestReconcileQueuedGitHubJobsAllocatesIdempotentlyAndRespectsLabels(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolFull {
			pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
		}
	})
	server := newTestServer(t, service)
	server.SetGitHubWebhook(GitHubWebhookOptions{
		WorkflowJobs: fakeWorkflowJobLister{
			repos: []string{"my-org/home"},
			runs:  map[string][]int64{"my-org/home": {12}},
			jobs: map[string][]github.WorkflowJob{"12": {
				{ID: 99, RunID: 12, Status: "queued", Labels: []string{"self-hosted", "uecb-full"}},
				{ID: 100, RunID: 12, Status: "queued", Labels: []string{"self-hosted", "unserved"}},
				{ID: 101, RunID: 12, Status: "in_progress", Labels: []string{"self-hosted", "uecb-full"}},
			}},
		},
	})
	for attempt, want := range []int{1, 0} {
		got, err := server.ReconcileQueuedGitHubJobs(context.Background())
		if err != nil || got != want {
			t.Fatalf("attempt %d allocated=%d err=%v, want %d", attempt, got, err, want)
		}
	}
	if got := len(service.Store().List()); got != 1 {
		t.Fatalf("allocations=%d, want one idempotent allocation", got)
	}
	if allocation, ok := service.findByWorkflowJobID(99); !ok || allocation.Request.TargetRepository != "my-org/home" {
		t.Fatalf("queued job allocation missing or wrong repository: %+v found=%v", allocation, ok)
	}
}

type rateLimitedLister struct {
	fakeWorkflowJobLister
	remaining int
	reset     time.Time
	calls     *int
}

func (l rateLimitedLister) RateLimit() (int, time.Time, bool) { return l.remaining, l.reset, true }
func (l rateLimitedLister) ListRepositories(ctx context.Context, scope model.GitHubScope) ([]string, error) {
	*l.calls++
	return l.fakeWorkflowJobLister.ListRepositories(ctx, scope)
}

func TestReconcileQueuedGitHubJobsBacksOffBelowRateLimitFloor(t *testing.T) {
	service := newServiceWithConfig(nil)
	server := newTestServer(t, service)
	calls := 0
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: rateLimitedLister{
		fakeWorkflowJobLister: fakeWorkflowJobLister{repos: []string{"my-org/home"}},
		remaining:             999,
		reset:                 time.Now().Add(time.Hour),
		calls:                 &calls,
	}})
	if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); !errors.Is(err, ErrReconcileRateLimited) {
		t.Fatalf("err=%v, want rate-limit backoff", err)
	}
	if calls != 0 {
		t.Fatalf("reconcile called GitHub %d times while rate limited", calls)
	}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: rateLimitedLister{
		fakeWorkflowJobLister: fakeWorkflowJobLister{repos: []string{"my-org/home"}},
		remaining:             1000,
		calls:                 &calls,
	}})
	if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d, want a scan at the floor", err, calls)
	}
}

func TestReconcileQueuedGitHubJobsStopsWhenContextCancelled(t *testing.T) {
	server := newTestServer(t, newServiceWithConfig(nil))
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: fakeWorkflowJobLister{repos: []string{"my-org/home"}}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := server.ReconcileQueuedGitHubJobs(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}

func TestReconcileQueuedGitHubJobsRetriesFailedAllocation(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolFull {
			pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
		}
	})
	server := newTestServer(t, service)
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: fakeWorkflowJobLister{
		repos: []string{"my-org/home"},
		runs:  map[string][]int64{"my-org/home": {12}},
		jobs: map[string][]github.WorkflowJob{"12": {
			{ID: 99, RunID: 12, Status: "queued", Labels: []string{"self-hosted", "uecb-full"}},
		}},
	}})
	if got, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil || got != 1 {
		t.Fatalf("first reconcile allocated=%d err=%v", got, err)
	}
	failed, ok := service.findByWorkflowJobID(99)
	if !ok {
		t.Fatal("allocation missing")
	}
	failed.State = model.StateFailed
	if err := service.Store().Save(failed); err != nil {
		t.Fatalf("save failed allocation: %v", err)
	}
	if got, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil || got != 1 {
		t.Fatalf("retry reconcile allocated=%d err=%v, want the still-queued failed job retried", got, err)
	}
	if retried, ok := service.findByWorkflowJobID(99); !ok || retried.ID == failed.ID || retried.State == model.StateFailed {
		t.Fatalf("retried allocation = %+v ok=%v", retried, ok)
	}
	// A webhook redelivery (no reconcile context) must not replace a failed allocation.
	if _, created, err := service.AllocateForWorkflowJob(context.Background(), service.cfg.Pools[0], 99, "my-org/home"); err != nil || created {
		t.Fatalf("webhook path created=%v err=%v, want existing allocation returned", created, err)
	}
}

func TestReconcileQueuedGitHubJobsReturnsDiscoveryErrorsForRetry(t *testing.T) {
	server := newTestServer(t, newServiceWithConfig(nil))
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: fakeWorkflowJobLister{err: errors.New("GitHub API timeout")}})
	if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("reconciliation error=%v, want retryable discovery error", err)
	}
}

func newWebhookHarness(t *testing.T, enable bool) *webhookHarness {
	t.Helper()
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolFull && enable {
			pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
		}
	})
	server := newTestServer(t, service)
	h := &webhookHarness{t: t, service: service, leader: true, server: server, inbox: store.AsWebhookInbox(service.Store())}
	server.SetGitHubWebhook(GitHubWebhookOptions{
		Secret:   func(context.Context) (string, error) { return testWebhookSecret, nil },
		IsLeader: func(context.Context) (bool, error) { return h.leader, nil },
		Inbox:    h.inbox,
	})
	h.handler = server.Handler()
	return h
}

func (h *webhookHarness) post(event, delivery, body, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func (h *webhookHarness) send(event, delivery, body string) *httptest.ResponseRecorder {
	return h.post(event, delivery, body, sign(testWebhookSecret, body))
}

func (h *webhookHarness) activeCount() int {
	n := 0
	for _, st := range h.service.Store().List() {
		if isActiveAllocationState(st.State) {
			n++
		}
	}
	return n
}

func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, m := range family.GetMetric() {
			if labelsMatch(m, labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	for k, v := range want {
		found := false
		for _, l := range m.GetLabel() {
			if l.GetName() == k && l.GetValue() == v {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestGitHubWebhookSignatureValidation(t *testing.T) {
	h := newWebhookHarness(t, true)
	body := jobBody("queued", 1, "", "self-hosted", "uecb-full")

	if rec := h.post("workflow_job", "d1", body, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing signature: got %d", rec.Code)
	}
	if rec := h.post("workflow_job", "d1", body, sign("wrong", body)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: got %d", rec.Code)
	}
	if rec := h.post("workflow_job", "d1", body, "sha256=zz"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("malformed signature: got %d", rec.Code)
	}
	if h.activeCount() != 0 {
		t.Fatalf("unsigned requests must not allocate")
	}
	if rec := h.send("workflow_job", "d1", body); rec.Code != http.StatusAccepted {
		t.Fatalf("good signature: got %d body=%s", rec.Code, rec.Body)
	}
}

func TestGitHubWebhookIgnoresNonWorkflowJobEvents(t *testing.T) {
	h := newWebhookHarness(t, true)
	for _, event := range []string{"ping", "push", ""} {
		rec := h.send(event, "d-"+event, `{"zen":"hi"}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "unsupported_event") {
			t.Fatalf("event %q: got %d %s", event, rec.Code, rec.Body)
		}
	}
	rec := h.send("workflow_job", "d-waiting", jobBody("waiting", 9, "", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "unsupported_action") {
		t.Fatalf("waiting: got %d %s", rec.Code, rec.Body)
	}
	if h.activeCount() != 0 {
		t.Fatalf("ignored events must not allocate")
	}
	if got := counterValue(t, "uecb_github_webhook_events_ignored_total", map[string]string{"reason": "unsupported_event"}); got != 3 {
		t.Fatalf("ignored metric = %v, want 3", got)
	}
	if got := counterValue(t, "uecb_github_webhook_events_received_total", map[string]string{"event": "ping"}); got != 1 {
		t.Fatalf("received metric = %v, want 1", got)
	}
}

func TestGitHubWebhookLabelMatching(t *testing.T) {
	h := newWebhookHarness(t, true)
	cases := []struct {
		labels []string
		match  bool
	}{
		{[]string{"self-hosted", "uecb-full"}, true},
		{[]string{"UECB-Full"}, true},
		{[]string{"uecb-full"}, true},
		{[]string{"self-hosted"}, false},
		{[]string{"self-hosted", "uecb-full", "linux"}, false},
		{[]string{"ubuntu-latest"}, false},
		{nil, false},
	}
	for i, tc := range cases {
		_, ok := h.service.MatchWebhookPool(tc.labels)
		if ok != tc.match {
			t.Errorf("case %d labels=%v: match=%v want %v", i, tc.labels, ok, tc.match)
		}
	}
	rec := h.send("workflow_job", "d-nomatch", jobBody("queued", 5, "", "ubuntu-latest"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "no_matching_pool") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if h.activeCount() != 0 {
		t.Fatal("non-matching job must not allocate")
	}
}

func TestGitHubWebhookQueuedAllocatesAndCompletedFinalizes(t *testing.T) {
	h := newWebhookHarness(t, true)
	rec := h.send("workflow_job", "q1", jobBody("queued", 42, "", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("queued: got %d %s", rec.Code, rec.Body)
	}
	allocation, ok := h.service.findByWorkflowJobID(42)
	if !ok || allocation.State != model.StateReady {
		t.Fatalf("allocation = %+v ok=%v", allocation, ok)
	}
	if allocation.Pool != model.PoolFull {
		t.Fatalf("pool = %s", allocation.Pool)
	}
	if strings.Join(allocation.RequestedLabels, ",") != "self-hosted,uecb-full" {
		t.Fatalf("requested labels = %v", allocation.RequestedLabels)
	}

	rec = h.send("workflow_job", "c1", jobBody("completed", 42, "success", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusOK {
		t.Fatalf("completed: got %d %s", rec.Code, rec.Body)
	}
	final, _ := h.service.Get(allocation.ID)
	if final.State != model.StateCompleted {
		t.Fatalf("state = %s", final.State)
	}
	if h.activeCount() != 0 {
		t.Fatal("capacity not released")
	}
}

func TestGitHubWebhookInProgressClaimsAllocation(t *testing.T) {
	h := newWebhookHarness(t, true)
	rec := h.send("workflow_job", "q1", jobBody("queued", 42, "", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("queued: got %d %s", rec.Code, rec.Body)
	}
	allocation, ok := h.service.findByWorkflowJobID(42)
	if !ok {
		t.Fatal("allocation not found")
	}
	if allocation.ClaimedAt != nil {
		t.Fatal("expected ClaimedAt to be nil initially")
	}

	rec = h.send("workflow_job", "ip1", jobBody("in_progress", 42, "", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusOK {
		t.Fatalf("in_progress: got %d %s", rec.Code, rec.Body)
	}

	updated, _ := h.service.Get(allocation.ID)
	if updated.ClaimedAt == nil {
		t.Fatal("expected ClaimedAt to be set after in_progress")
	}
}

func TestGitHubWebhookCancelledFinalizesAsCanceled(t *testing.T) {
	h := newWebhookHarness(t, true)
	h.send("workflow_job", "q1", jobBody("queued", 7, "", "self-hosted", "uecb-full"))
	rec := h.send("workflow_job", "c1", jobBody("completed", 7, "cancelled", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	allocation, _ := h.service.findByWorkflowJobID(7)
	if allocation.State != model.StateCanceled {
		t.Fatalf("state = %s", allocation.State)
	}
	if h.activeCount() != 0 {
		t.Fatal("capacity not released")
	}
}

func TestGitHubWebhookCompletedUnknownJobIgnored(t *testing.T) {
	h := newWebhookHarness(t, true)
	rec := h.send("workflow_job", "c1", jobBody("completed", 999, "success", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "unknown_job") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestGitHubWebhookDuplicateDeliveryIdempotent(t *testing.T) {
	h := newWebhookHarness(t, true)
	body := jobBody("queued", 11, "", "self-hosted", "uecb-full")
	if rec := h.send("workflow_job", "same-guid", body); rec.Code != http.StatusAccepted {
		t.Fatalf("first: %d %s", rec.Code, rec.Body)
	}
	rec := h.send("workflow_job", "same-guid", body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate_delivery") {
		t.Fatalf("redelivery: %d %s", rec.Code, rec.Body)
	}
	// A different delivery for the same job (e.g. manual redelivery) must also
	// not allocate again.
	rec = h.send("workflow_job", "other-guid", body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate_job") {
		t.Fatalf("same job new guid: %d %s", rec.Code, rec.Body)
	}
	if n := h.activeCount(); n != 1 {
		t.Fatalf("active allocations = %d, want 1", n)
	}
}

func TestGitHubWebhookNonLeaderNoInboxRejects(t *testing.T) {
	h := newWebhookHarness(t, true)
	h.leader = false
	h.server.githubWebhook.opts.Inbox = nil
	rec := h.send("workflow_job", "nl0", jobBody("queued", 20, "", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not_leader") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestGitHubWebhookNonLeaderQueuesForLeader(t *testing.T) {
	h := newWebhookHarness(t, true)
	h.leader = false
	queued := jobBody("queued", 21, "", "self-hosted", "uecb-full")
	rec := h.send("workflow_job", "nl1", queued)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"queued"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	// Redelivery of the same GUID must not duplicate the queued entry.
	h.send("workflow_job", "nl1", queued)
	if h.activeCount() != 0 {
		t.Fatal("non-leader must not allocate")
	}

	// The leader's drain loop picks the delivery up and allocates exactly once.
	h.leader = true
	n, err := h.server.DrainGitHubWebhookInbox(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("drain = %d, %v; want 1, nil", n, err)
	}
	if got := h.activeCount(); got != 1 {
		t.Fatalf("active allocations = %d, want 1", got)
	}
	if n, _ := h.server.DrainGitHubWebhookInbox(context.Background()); n != 0 {
		t.Fatalf("inbox not emptied, drained %d more", n)
	}
}

func TestGitHubWebhookLeaderDrainsInboxBeforeProcessing(t *testing.T) {
	h := newWebhookHarness(t, true)
	h.leader = false
	h.send("workflow_job", "o1", jobBody("queued", 22, "", "self-hosted", "uecb-full"))
	h.send("workflow_job", "o2", jobBody("completed", 22, "success", "self-hosted", "uecb-full"))
	// Leadership returns and an unrelated delivery arrives: the backlog is
	// applied in order (allocate then finalize) before it.
	h.leader = true
	if rec := h.send("workflow_job", "o3", jobBody("queued", 23, "", "self-hosted", "uecb-full")); rec.Code != http.StatusAccepted {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if got := h.activeCount(); got != 1 {
		t.Fatalf("active allocations = %d, want 1 (job 22 finalized, job 23 live)", got)
	}
}

func TestGitHubWebhookInboxFailureRetried(t *testing.T) {
	defer func(old time.Duration) { inboxLease = old }(inboxLease)
	inboxLease = 0
	h := newWebhookHarness(t, true)
	h.leader = false
	h.send("workflow_job", "r1", jobBody("queued", 24, "", "self-hosted", "uecb-full"))
	h.leader = true
	h.service.initErr = errors.New("boom")
	if n, _ := h.server.DrainGitHubWebhookInbox(context.Background()); n != 0 {
		t.Fatalf("failed delivery must not be acked, drained %d", n)
	}
	// Still queued: once the lease lapses and the fault clears it is applied.
	h.service.initErr = nil
	if n, err := h.server.DrainGitHubWebhookInbox(context.Background()); err != nil || n != 1 {
		t.Fatalf("retry drain = %d, %v", n, err)
	}
	if h.activeCount() != 1 {
		t.Fatal("retried delivery did not allocate")
	}
}

func TestGitHubWebhookFailedAllocationAllowsRedelivery(t *testing.T) {
	h := newWebhookHarness(t, true)
	// A failing allocation releases the delivery claim so GitHub can redeliver.
	h.service.initErr = errors.New("boom")
	body := jobBody("queued", 31, "", "self-hosted", "uecb-full")
	if rec := h.send("workflow_job", "f1", body); rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	h.service.initErr = nil
	if rec := h.send("workflow_job", "f1", body); rec.Code != http.StatusAccepted {
		t.Fatalf("redelivery after failure: %d %s", rec.Code, rec.Body)
	}
}

func TestGitHubWebhookPoolsWithoutLabelsUnaffected(t *testing.T) {
	h := newWebhookHarness(t, false)
	rec := h.send("workflow_job", "q1", jobBody("queued", 50, "", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "no_matching_pool") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if h.activeCount() != 0 {
		t.Fatal("pool without webhookLabels must not allocate from webhooks")
	}
	// Explicit API still works.
	status, err := h.service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull})
	if err != nil || status.State != model.StateReady {
		t.Fatalf("explicit allocate: %+v %v", status, err)
	}
	if status.Request != nil && status.Request.WorkflowJobID != 0 {
		t.Fatal("explicit allocation must not carry a workflow job id")
	}
}

func TestGitHubWebhookDisabledReturnsNotFound(t *testing.T) {
	server := newTestServer(t, newServiceWithConfig(nil))
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestGitHubWebhookRejectsNonPost(t *testing.T) {
	h := newWebhookHarness(t, true)
	req := httptest.NewRequest(http.MethodGet, "/webhooks/github", nil)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestGitHubWebhookSecretFunc(t *testing.T) {
	read := func(_ context.Context, name string) (map[string]string, error) {
		if name != "ref" {
			return nil, errors.New("missing")
		}
		return map[string]string{"webhook_secret": " abc ", "custom": "xyz"}, nil
	}
	ctx := context.Background()
	if v, err := GitHubWebhookSecretFunc(model.GitHubWebhookConfig{Secret: "inline"}, read)(ctx); err != nil || v != "inline" {
		t.Fatalf("inline: %q %v", v, err)
	}
	if v, err := GitHubWebhookSecretFunc(model.GitHubWebhookConfig{SecretRef: "ref"}, read)(ctx); err != nil || v != "abc" {
		t.Fatalf("ref default key: %q %v", v, err)
	}
	if v, err := GitHubWebhookSecretFunc(model.GitHubWebhookConfig{SecretRef: "ref", SecretKey: "custom"}, read)(ctx); err != nil || v != "xyz" {
		t.Fatalf("ref custom key: %q %v", v, err)
	}
	if _, err := GitHubWebhookSecretFunc(model.GitHubWebhookConfig{SecretRef: "ref", SecretKey: "nope"}, read)(ctx); err == nil {
		t.Fatal("empty key must error")
	}
	if _, err := GitHubWebhookSecretFunc(model.GitHubWebhookConfig{}, read)(ctx); err == nil {
		t.Fatal("unconfigured must error")
	}
}

func repoJobBody(action string, id int64, repository string, labels ...string) string {
	body := jobBody(action, id, "", labels...)
	return strings.Replace(body, `"full_name":"my-org/home"`, fmt.Sprintf(`"full_name":%q`, repository), 1)
}

func TestGitHubWebhookOwnerScopeRecordsRepositoryAndRejectsOthers(t *testing.T) {
	h := newWebhookHarness(t, true)
	h.service.cfg.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "example-owner"}

	rec := h.send("workflow_job", "o1", repoJobBody("queued", 501, "Example-Owner/questkit", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("queued: got %d %s", rec.Code, rec.Body)
	}
	allocation, ok := h.service.findByWorkflowJobID(501)
	if !ok {
		t.Fatal("expected an allocation for the owner's repository")
	}
	if got := allocation.TargetRepository(); got != "Example-Owner/questkit" {
		t.Fatalf("target repository = %q", got)
	}
	if allocation.Tenant != "example-owner/questkit" {
		t.Fatalf("tenant = %q, want the repository for per-repository fair share", allocation.Tenant)
	}

	before := counterValue(t, "uecb_github_webhook_events_ignored_total", map[string]string{"reason": "foreign_repository"})
	for i, repo := range []string{"someone-else/repo", ""} {
		rec = h.send("workflow_job", fmt.Sprintf("o-foreign-%d", i), repoJobBody("queued", int64(600+i), repo, "self-hosted", "uecb-full"))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "foreign_repository") {
			t.Fatalf("repository %q: got %d %s", repo, rec.Code, rec.Body)
		}
		if _, ok := h.service.findByWorkflowJobID(int64(600 + i)); ok {
			t.Fatalf("repository %q must not allocate", repo)
		}
	}
	if after := counterValue(t, "uecb_github_webhook_events_ignored_total", map[string]string{"reason": "foreign_repository"}); after-before != 2 {
		t.Fatalf("foreign_repository ignored delta = %v", after-before)
	}
}

func TestWarmRunnersOnlyServeTheDefaultRepositoryUnderOwnerScope(t *testing.T) {
	service := newServiceWithConfig(func(*model.PoolConfig) {})
	request := func(repo string) model.AllocationStatus { return model.AllocationStatus{Repository: repo} }

	if !service.warmRunnerServes(request("anyone/anything")) {
		t.Fatal("fixed scopes keep sharing warm runners")
	}

	service.cfg.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "example-owner", Repository: "home"}
	for repo, want := range map[string]bool{
		"example-owner/home":     true,
		"Example-Owner/HOME":     true,
		"":                       true,
		"example-owner/questkit": false,
		"someone-else/home":      false,
	} {
		if got := service.warmRunnerServes(request(repo)); got != want {
			t.Errorf("warmRunnerServes(%q) = %v, want %v", repo, got, want)
		}
	}

	service.cfg.GitHub.Scope.Repository = ""
	if service.warmRunnerServes(request("example-owner/home")) {
		t.Fatal("without a default repository there are no warm runners to share")
	}
}

func TestGitHubWebhookRejectsForkPullRequestJobs(t *testing.T) {
	h := newWebhookHarness(t, true)
	h.service.cfg.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "example-owner"}
	heads := map[int64]string{1: "example-owner/questkit", 2: "attacker/questkit"}
	lookups := 0
	h.server.githubWebhook.opts.RejectForks = true
	h.server.githubWebhook.opts.WorkflowRun = func(_ context.Context, repo string, runID int64) (github.WorkflowRun, error) {
		lookups++
		if repo != "example-owner/questkit" {
			t.Errorf("looked up %q", repo)
		}
		if head, ok := heads[runID]; ok {
			return github.WorkflowRun{HeadRepository: head, Actor: "someone", TriggeringActor: "someone"}, nil
		}
		return github.WorkflowRun{}, fmt.Errorf("run %d not found", runID)
	}
	body := func(jobID, runID int64) string {
		b := repoJobBody("queued", jobID, "example-owner/questkit", "self-hosted", "uecb-full")
		return strings.Replace(b, fmt.Sprintf(`"id":%d,`, jobID), fmt.Sprintf(`"id":%d,"run_id":%d,`, jobID, runID), 1)
	}

	if rec := h.send("workflow_job", "f1", body(701, 1)); rec.Code != http.StatusAccepted {
		t.Fatalf("same-repository job: got %d %s", rec.Code, rec.Body)
	}
	rec := h.send("workflow_job", "f2", body(702, 2))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fork_pull_request") {
		t.Fatalf("fork job: got %d %s", rec.Code, rec.Body)
	}
	if _, ok := h.service.findByWorkflowJobID(702); ok {
		t.Fatal("a fork pull request job must not get a runner")
	}
	if rec := h.send("workflow_job", "f3", body(703, 3)); rec.Code < 500 {
		t.Fatalf("a failed lookup must fail the delivery, got %d %s", rec.Code, rec.Body)
	}
	if _, ok := h.service.findByWorkflowJobID(703); ok {
		t.Fatal("a failed lookup must not allocate")
	}
	before := lookups
	if rec := h.send("workflow_job", "f1-dup", body(701, 1)); rec.Code != http.StatusOK {
		t.Fatalf("duplicate: got %d", rec.Code)
	}
	if lookups != before {
		t.Fatal("an already allocated job needs no second lookup")
	}
}

func TestGitHubWebhookPoolRepositoryAllowlist(t *testing.T) {
	h := newWebhookHarness(t, true)
	h.service.cfg.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "example-owner", Repository: "home"}
	for i := range h.service.cfg.Pools {
		if h.service.cfg.Pools[i].Name == model.PoolFull {
			h.service.cfg.Pools[i].WebhookRepositories = []string{"Example-Owner/home"}
		}
	}

	rec := h.send("workflow_job", "r1", repoJobBody("queued", 801, "example-owner/home", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("allowed repository: got %d %s", rec.Code, rec.Body)
	}
	rec = h.send("workflow_job", "r2", repoJobBody("queued", 802, "example-owner/public-repo", "self-hosted", "uecb-full"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "repository_not_allowed") {
		t.Fatalf("other repository: got %d %s", rec.Code, rec.Body)
	}
	if _, ok := h.service.findByWorkflowJobID(802); ok {
		t.Fatal("a repository outside webhookRepositories must not get a runner from the pool")
	}
}

func TestMatchWebhookPoolForRepositoryFallsThrough(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		switch pool.Name {
		case model.PoolFull:
			pool.WebhookLabels = []string{"self-hosted", "shared"}
			pool.WebhookRepositories = []string{"owner/home"}
		case model.PoolLite:
			pool.WebhookLabels = []string{"self-hosted", "shared", "linux"}
			pool.WebhookRepositories = []string{"owner/*"}
		}
	})
	if pool, ok := service.MatchWebhookPoolForRepository([]string{"shared"}, "owner/home"); !ok || pool.Name != model.PoolFull {
		t.Fatalf("home: %v %v", pool.Name, ok)
	}
	if pool, ok := service.MatchWebhookPoolForRepository([]string{"shared"}, "OWNER/questkit"); !ok || pool.Name != model.PoolLite {
		t.Fatalf("questkit should fall through to the owner/* pool: %v %v", pool.Name, ok)
	}
	if _, ok := service.MatchWebhookPoolForRepository([]string{"shared"}, "other/questkit"); ok {
		t.Fatal("another owner matches no pool")
	}
}

func TestGitHubWebhookOnlyAllowedActorsGetRunners(t *testing.T) {
	h := newWebhookHarness(t, true)
	h.service.cfg.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "example-owner"}
	runs := map[int64]github.WorkflowRun{
		1: {Actor: "Example-Owner", TriggeringActor: "example-owner"},
		2: {Actor: "collaborator", TriggeringActor: "collaborator"},
		3: {Actor: "collaborator", TriggeringActor: "example-owner"}, // owner re-runs someone else's run
		4: {Actor: "dependabot[bot]", TriggeringActor: "dependabot[bot]"},
		5: {Actor: "dependabot[bot]", TriggeringActor: "example-owner"},
		6: {Actor: "", TriggeringActor: "example-owner"},
	}
	h.server.githubWebhook.opts.RejectForks = true
	h.server.githubWebhook.opts.AllowedActors = []string{"example-owner", "dependabot[bot]"}
	h.server.githubWebhook.opts.WorkflowRun = func(_ context.Context, repo string, runID int64) (github.WorkflowRun, error) {
		run := runs[runID]
		run.HeadRepository = repo
		return run, nil
	}
	body := func(jobID, runID int64) string {
		b := repoJobBody("queued", jobID, "example-owner/questkit", "self-hosted", "uecb-full")
		return strings.Replace(b, fmt.Sprintf(`"id":%d,`, jobID), fmt.Sprintf(`"id":%d,"run_id":%d,`, jobID, runID), 1)
	}
	for runID, allowed := range map[int64]bool{1: true, 2: false, 3: false, 4: true, 5: true, 6: false} {
		jobID := 900 + runID
		rec := h.send("workflow_job", fmt.Sprintf("a%d", runID), body(jobID, runID))
		_, got := h.service.findByWorkflowJobID(jobID)
		if got != allowed {
			t.Errorf("run %d (%+v): allocated=%v want %v (%d %s)", runID, runs[runID], got, allowed, rec.Code, rec.Body)
		}
		if !allowed && !strings.Contains(rec.Body.String(), "actor_not_allowed") {
			t.Errorf("run %d: want actor_not_allowed, got %s", runID, rec.Body)
		}
	}
}

func TestReconcileQueuedGitHubJobsScansAWeekAcrossReposAndSharedPool(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolFull {
			pool.WebhookLabels = []string{"self-hosted", "uecb-shared", "quest-linux"}
		}
	})
	server := newTestServer(t, service)
	var since time.Time
	server.SetGitHubWebhook(GitHubWebhookOptions{
		WorkflowJobs: fakeWorkflowJobLister{
			repos: []string{"my-org/home", "my-org/other"},
			runs:  map[string][]int64{"my-org/other": {4242}},
			jobs: map[string][]github.WorkflowJob{"4242": {
				{ID: 7, Status: "queued", Labels: []string{"quest-linux"}},
			}},
			since: &since,
		},
		WorkflowRun: func(context.Context, string, int64) (github.WorkflowRun, error) {
			return github.WorkflowRun{HeadRepository: "my-org/other"}, nil
		},
	})
	count, err := server.ReconcileQueuedGitHubJobs(context.Background())
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v; a 2-day-old queued job in a non-home repo must be reconciled", count, err)
	}
	if age := service.now().Sub(since); age < 7*24*time.Hour-time.Minute {
		t.Fatalf("reconcile window = %v, want >= 7d", age)
	}
}

// countingLister records every list call and charges one API call per call,
// mimicking github.Client.APICalls.
type countingLister struct {
	repos   []string
	runs    map[string][]int64
	runErr  map[string]error
	jobs    map[string][]github.WorkflowJob
	jobErr  map[int64]error
	calls   *int64
	scanned *[]string
	windows *[]time.Duration
	now     func() time.Time
}

func (l countingLister) APICalls() int64 { return *l.calls }
func (l countingLister) ListRepositories(context.Context, model.GitHubScope) ([]string, error) {
	*l.calls++
	return l.repos, nil
}
func (l countingLister) ListQueuedWorkflowRuns(_ context.Context, repository string, since time.Time) ([]int64, error) {
	*l.calls++
	if l.scanned != nil {
		*l.scanned = append(*l.scanned, repository)
	}
	if l.windows != nil {
		*l.windows = append(*l.windows, l.now().Sub(since))
	}
	return l.runs[repository], l.runErr[repository]
}
func (l countingLister) ListWorkflowJobs(_ context.Context, _ string, runID int64) ([]github.WorkflowJob, error) {
	*l.calls++
	return l.jobs[fmt.Sprint(runID)], l.jobErr[runID]
}

func TestReconcileQueuedGitHubJobsStopsAtCallBudgetAndResumesRoundRobin(t *testing.T) {
	service := newServiceWithConfig(nil)
	server := newTestServer(t, service)
	repos := make([]string, 40)
	runs := map[string][]int64{}
	for i := range repos {
		repos[i] = fmt.Sprintf("my-org/repo-%02d", i)
		// Each repo costs 1 run-list call + 4 job-list calls.
		runs[repos[i]] = []int64{int64(i*10 + 1), int64(i*10 + 2), int64(i*10 + 3), int64(i*10 + 4)}
	}
	var calls int64
	var scanned []string
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: countingLister{
		repos: repos, runs: runs, calls: &calls, scanned: &scanned, now: service.now,
	}})
	budgetBefore := testutil.ToFloat64(server.githubWebhook.metrics.budget)
	seen := map[string]int{}
	for cycle := 0; cycle < 3; cycle++ {
		before := calls
		scanned = nil
		if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		// One repo may straddle the budget: at most budget + one repo's calls.
		if spent := calls - before; spent > reconcileCallBudget+5 {
			t.Fatalf("cycle %d spent %d calls, budget %d", cycle, spent, reconcileCallBudget)
		}
		for _, r := range scanned {
			seen[r]++
		}
	}
	if len(seen) != len(repos) {
		t.Fatalf("round-robin covered %d/%d repos in 3 cycles", len(seen), len(repos))
	}
	if got := testutil.ToFloat64(server.githubWebhook.metrics.budget) - budgetBefore; got < 1 {
		t.Fatalf("budget-exhausted metric delta=%v, want >= 1", got)
	}
}

func TestReconcileQueuedGitHubJobsRunsDeepScanOnSlowerInterval(t *testing.T) {
	service := newServiceWithConfig(nil)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	server := newTestServer(t, service)
	var calls int64
	var windows []time.Duration
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: countingLister{
		repos: []string{"my-org/home"}, calls: &calls, windows: &windows, now: service.now,
	}})
	step := func() time.Duration {
		t.Helper()
		windows = nil
		if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(windows) != 1 {
			t.Fatalf("windows=%v", windows)
		}
		now = now.Add(time.Minute)
		return windows[0]
	}
	if got := step(); got != reconcileRunWindow {
		t.Fatalf("first cycle window=%v, want deep %v", got, reconcileRunWindow)
	}
	for i := 1; i < 10; i++ {
		if got := step(); got != reconcileRecentWindow {
			t.Fatalf("minute %d window=%v, want recent %v", i, got, reconcileRecentWindow)
		}
	}
	if got := step(); got != reconcileRunWindow {
		t.Fatalf("after %v window=%v, want deep scan again", reconcileDeepInterval, got)
	}
}

func TestReconcileQueuedGitHubJobsReportsPageCapAndKeepsTruncatedResults(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolFull {
			pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
		}
	})
	server := newTestServer(t, service)
	var calls int64
	capped := fmt.Errorf("list: %w", github.ErrPageCapReached)
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: countingLister{
		repos:  []string{"my-org/home"},
		runs:   map[string][]int64{"my-org/home": {12}},
		runErr: map[string]error{"my-org/home": capped},
		jobs: map[string][]github.WorkflowJob{"12": {
			{ID: 99, RunID: 12, Status: "queued", Labels: []string{"self-hosted", "uecb-full"}},
		}},
		jobErr: map[int64]error{12: capped},
		calls:  &calls,
		now:    service.now,
	}})
	m := server.githubWebhook.metrics.pageCap
	runsBefore, jobsBefore := testutil.ToFloat64(m.WithLabelValues("runs")), testutil.ToFloat64(m.WithLabelValues("jobs"))
	got, err := server.ReconcileQueuedGitHubJobs(context.Background())
	if err != nil || got != 1 {
		t.Fatalf("allocated=%d err=%v; truncated listings must still be acted on", got, err)
	}
	if d := testutil.ToFloat64(m.WithLabelValues("runs")) - runsBefore; d != 1 {
		t.Fatalf("runs page-cap metric delta=%v", d)
	}
	if d := testutil.ToFloat64(m.WithLabelValues("jobs")) - jobsBefore; d != 1 {
		t.Fatalf("jobs page-cap metric delta=%v", d)
	}
}

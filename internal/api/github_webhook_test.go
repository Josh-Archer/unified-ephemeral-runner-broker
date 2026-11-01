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
// mimicking github.Client.APICalls. Runs come from runsRecent for the 1h
// window and runsDeep for the 7-day window.
type countingLister struct {
	*listerLog
	repos      []string
	repoErr    error
	runsRecent map[string][]int64
	runsDeep   map[string][]int64
	runErr     map[string]error
	jobs       map[string][]github.WorkflowJob
	jobErr     map[int64]error
	now        func() time.Time
	// rateFloorAfter, when > 0, reports remaining=999 once that many calls were made.
	rateFloorAfter int64
	// jobPages, when > 1, charges that many API calls per job listing,
	// mimicking a multi-page listing that overshoots the per-call check.
	jobPages int64
}

type listerLog struct {
	calls     int64
	repoLists int
	runLists  []runListCall
	jobLists  map[int64]int
	// hang counts how many more times a call blocks until ctx ends, keyed by
	// "runs:<repo>:<window>" or "jobs:<runID>"; -1 hangs forever.
	hang map[string]int
}

func (l *listerLog) hangs(ctx context.Context, key string) bool {
	n := l.hang[key]
	if n == 0 {
		return false
	}
	if n > 0 {
		l.hang[key] = n - 1
	}
	<-ctx.Done()
	return true
}

type runListCall struct {
	repo   string
	window time.Duration
}

func newCountingLister(now func() time.Time) countingLister {
	return countingLister{listerLog: &listerLog{jobLists: map[int64]int{}, hang: map[string]int{}}, now: now}
}

func (l countingLister) APICalls() int64 { return l.calls }
func (l countingLister) RateLimit() (int, time.Time, bool) {
	if l.rateFloorAfter > 0 && l.calls >= l.rateFloorAfter {
		return 999, l.now().Add(time.Hour), true
	}
	return 5000, time.Time{}, true
}
func (l countingLister) ListRepositories(context.Context, model.GitHubScope) ([]string, error) {
	l.calls++
	l.repoLists++
	return l.repos, l.repoErr
}
func (l countingLister) ListQueuedWorkflowRuns(ctx context.Context, repository string, since time.Time) ([]int64, error) {
	l.calls++
	window := l.now().Sub(since).Round(time.Hour)
	l.runLists = append(l.runLists, runListCall{repository, window})
	if l.hangs(ctx, fmt.Sprintf("runs:%s:%s", repository, window)) {
		return nil, ctx.Err()
	}
	if window > reconcileRecentWindow {
		return l.runsDeep[repository], l.runErr[repository]
	}
	return l.runsRecent[repository], l.runErr[repository]
}
func (l countingLister) ListWorkflowJobs(ctx context.Context, _ string, runID int64) ([]github.WorkflowJob, error) {
	l.calls++
	if l.jobPages > 1 {
		l.calls += l.jobPages - 1
	}
	l.jobLists[runID]++
	if l.hangs(ctx, fmt.Sprintf("jobs:%d", runID)) {
		return nil, ctx.Err()
	}
	return l.jobs[fmt.Sprint(runID)], l.jobErr[runID]
}

func manyRuns(base, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(base + i)
	}
	return out
}

func reconcileCycle(t *testing.T, server *Server, l countingLister) int64 {
	t.Helper()
	before := l.calls
	if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	spent := l.calls - before
	if spent > reconcileCallBudget {
		t.Fatalf("cycle spent %d calls, budget %d", spent, reconcileCallBudget)
	}
	return spent
}

func TestReconcileQueuedGitHubJobsSingleRepoOverBudgetResumesMidRepo(t *testing.T) {
	service := newServiceWithConfig(nil)
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/big"}
	l.runsDeep = map[string][]int64{"my-org/big": manyRuns(1000, 500)}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	budgetBefore := testutil.ToFloat64(server.githubWebhook.metrics.budget)
	for cycle := 0; cycle < 6; cycle++ {
		spent := reconcileCycle(t, server, l)
		t.Logf("cycle %d spent %d jobs=%d", cycle, spent, len(l.jobLists))
		if spent < reconcileCallBudget-2 && len(l.jobLists) < 500 {
			t.Fatalf("cycle %d spent only %d calls with work pending", cycle, spent)
		}
	}
	if len(l.jobLists) != 500 {
		t.Fatalf("listed jobs for %d/500 runs after 6 cycles", len(l.jobLists))
	}
	for runID, n := range l.jobLists {
		if n != 1 {
			t.Fatalf("run %d listed %d times; mid-repo resume must not restart the repo", runID, n)
		}
	}
	deepLists := 0
	for _, c := range l.runLists {
		if c.window == reconcileRunWindow {
			deepLists++
		}
	}
	if deepLists != 1 {
		t.Fatalf("deep run listing for the big repo ran %d times, want 1 (cursor resumes at run position)", deepLists)
	}
	if got := testutil.ToFloat64(server.githubWebhook.metrics.budget) - budgetBefore; got < 5 {
		t.Fatalf("budget-exhausted metric delta=%v, want >= 5", got)
	}
}

func TestReconcileQueuedGitHubJobsBigRepoDoesNotStarveLaterRepos(t *testing.T) {
	service := newServiceWithConfig(nil)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/big", "my-org/a", "my-org/b", "my-org/c", "my-org/d", "my-org/e"}
	l.runsDeep = map[string][]int64{"my-org/big": manyRuns(1000, 300)}
	for i, r := range l.repos[1:] {
		l.runsDeep[r] = []int64{int64(10 + i)}
	}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	for cycle := 0; cycle < 6 && server.githubWebhook.reconcile.deepOpen || cycle == 0; cycle++ {
		reconcileCycle(t, server, l)
		now = now.Add(time.Minute)
	}
	if server.githubWebhook.reconcile.deepOpen {
		t.Fatal("deep pass never completed: repos after the big one starved")
	}
	for i := range l.repos[1:] {
		if l.jobLists[int64(10+i)] == 0 {
			t.Fatalf("repo %s run never scanned", l.repos[1+i])
		}
	}
}

func TestReconcileQueuedGitHubJobsRecentScanCoversReposAheadOfDeepCursor(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolFull {
			pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
		}
	})
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/big", "my-org/a", "my-org/b", "my-org/late"}
	l.runsDeep = map[string][]int64{"my-org/big": manyRuns(1000, 400)}
	l.runsRecent = map[string][]int64{"my-org/late": {77}}
	l.jobs = map[string][]github.WorkflowJob{"77": {
		{ID: 501, RunID: 77, Status: "queued", Labels: []string{"self-hosted", "uecb-full"}},
	}}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	got, err := server.ReconcileQueuedGitHubJobs(context.Background())
	if err != nil || got != 1 {
		t.Fatalf("allocated=%d err=%v; the 1h scan must reach repos the deep cursor has not", got, err)
	}
	if !server.githubWebhook.reconcile.deepOpen || server.githubWebhook.reconcile.deep.repo != "my-org/big" {
		t.Fatalf("deep cursor = %+v, want still inside my-org/big", server.githubWebhook.reconcile.deep)
	}
}

func TestReconcileQueuedGitHubJobsChecksRateLimitInsideRepo(t *testing.T) {
	service := newServiceWithConfig(nil)
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/big"}
	l.runsDeep = map[string][]int64{"my-org/big": manyRuns(1000, 50)}
	l.rateFloorAfter = 10
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); !errors.Is(err, ErrReconcileRateLimited) {
		t.Fatalf("err=%v, want rate-limit backoff mid-repo", err)
	}
	if l.calls != 10 {
		t.Fatalf("calls=%d, want the scan to stop as soon as remaining fell below the floor", l.calls)
	}
}

func TestReconcileQueuedGitHubJobsCachesRepositoryList(t *testing.T) {
	service := newServiceWithConfig(nil)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/home"}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	for i := 0; i < 4; i++ {
		reconcileCycle(t, server, l)
		now = now.Add(time.Minute)
	}
	if l.repoLists != 1 {
		t.Fatalf("repository list fetched %d times in 4 minutes, want 1", l.repoLists)
	}
	now = now.Add(reconcileReposTTL)
	reconcileCycle(t, server, l)
	if l.repoLists != 2 {
		t.Fatalf("repository list fetched %d times after TTL, want 2", l.repoLists)
	}
}

func TestReconcileQueuedGitHubJobsRunsDeepScanOnSlowerInterval(t *testing.T) {
	service := newServiceWithConfig(nil)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/home"}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	step := func() (deep bool) {
		t.Helper()
		l.runLists = nil
		reconcileCycle(t, server, l)
		now = now.Add(time.Minute)
		recent := false
		for _, c := range l.runLists {
			deep = deep || c.window == reconcileRunWindow
			recent = recent || c.window == reconcileRecentWindow
		}
		if !recent {
			t.Fatalf("cycle without a 1h scan: %+v", l.runLists)
		}
		return deep
	}
	if !step() {
		t.Fatal("first cycle must run the deep scan")
	}
	for i := 1; i < 10; i++ {
		if step() {
			t.Fatalf("minute %d ran the deep scan before %v", i, reconcileDeepInterval)
		}
	}
	if !step() {
		t.Fatalf("no deep scan after %v", reconcileDeepInterval)
	}
}

func TestReconcileQueuedGitHubJobsReportsPageCapAndKeepsTruncatedResults(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolFull {
			pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
		}
	})
	server := newTestServer(t, service)
	capped := fmt.Errorf("list: %w", github.ErrPageCapReached)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/home"}
	l.runsRecent = map[string][]int64{"my-org/home": {12}}
	l.runErr = map[string]error{"my-org/home": capped}
	l.jobs = map[string][]github.WorkflowJob{"12": {
		{ID: 99, RunID: 12, Status: "queued", Labels: []string{"self-hosted", "uecb-full"}},
	}}
	l.jobErr = map[int64]error{12: capped}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	m := server.githubWebhook.metrics.pageCap
	runsBefore, jobsBefore := testutil.ToFloat64(m.WithLabelValues("runs")), testutil.ToFloat64(m.WithLabelValues("jobs"))
	got, err := server.ReconcileQueuedGitHubJobs(context.Background())
	if err != nil || got != 1 {
		t.Fatalf("allocated=%d err=%v; truncated listings must still be acted on", got, err)
	}
	if d := testutil.ToFloat64(m.WithLabelValues("runs")) - runsBefore; d < 1 {
		t.Fatalf("runs page-cap metric delta=%v", d)
	}
	if d := testutil.ToFloat64(m.WithLabelValues("jobs")) - jobsBefore; d < 1 {
		t.Fatalf("jobs page-cap metric delta=%v", d)
	}
}

func TestReconcileQueuedGitHubJobsRefreshesTruncatedRepositoryListSooner(t *testing.T) {
	service := newServiceWithConfig(nil)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/home"}
	l.repoErr = fmt.Errorf("list: %w", github.ErrPageCapReached)
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	reconcileCycle(t, server, l)
	now = now.Add(reconcileTruncatedReposTTL / 2)
	reconcileCycle(t, server, l)
	if l.repoLists != 1 {
		t.Fatalf("truncated repository list fetched %d times within its TTL, want 1", l.repoLists)
	}
	now = now.Add(reconcileTruncatedReposTTL)
	reconcileCycle(t, server, l)
	if l.repoLists != 2 {
		t.Fatalf("truncated repository list fetched %d times after %v, want 2 (must not be cached for %v)", l.repoLists, reconcileTruncatedReposTTL, reconcileReposTTL)
	}
}

// home#4157 (v0.1.40): multi-page recent listings pushed the cycle to the full
// budget before the deep scan took a single call, so the deep cursor never
// moved. The deep scan must get its reserve and advance every cycle.
func TestReconcileQueuedGitHubJobsDeepScanGetsReserveWhenRecentOvershoots(t *testing.T) {
	service := newServiceWithConfig(nil)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.jobPages = 50
	for i := 0; i < 40; i++ {
		r := fmt.Sprintf("my-org/r%02d", i)
		l.repos = append(l.repos, r)
		if l.runsRecent == nil {
			l.runsRecent, l.runsDeep = map[string][]int64{}, map[string][]int64{}
		}
		l.runsRecent[r] = manyRuns(10000+i*100, 5)
		l.runsDeep[r] = []int64{int64(500 + i)}
	}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	state := &server.githubWebhook.reconcile
	prevRemaining := -1
	for cycle := 0; cycle < 3; cycle++ {
		if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if state.lastDeepSpent < reconcileDeepReserve && state.deepOpen {
			t.Fatalf("cycle %d: deep scan got %d calls, want >= %d", cycle, state.lastDeepSpent, reconcileDeepReserve)
		}
		if !state.deepOpen {
			break
		}
		if state.deep.cursor == 0 {
			t.Fatalf("cycle %d: deep cursor never advanced (%+v)", cycle, state.deep)
		}
		if prevRemaining >= 0 && state.deep.remaining >= prevRemaining {
			t.Fatalf("cycle %d: deep remaining %d did not drop from %d", cycle, state.deep.remaining, prevRemaining)
		}
		prevRemaining = state.deep.remaining
		now = now.Add(time.Minute)
	}
	deep := 0
	for _, c := range l.runLists {
		if c.window == reconcileRunWindow {
			deep++
		}
	}
	if deep == 0 {
		t.Fatal("deep scan listed no repositories")
	}
}

func TestDeepScanLimitGuaranteesReserve(t *testing.T) {
	for _, tc := range []struct{ spent, want int64 }{
		{0, reconcileCallBudget},
		{reconcileCallBudget - reconcileDeepReserve, reconcileCallBudget},
		{reconcileCallBudget, reconcileCallBudget + reconcileDeepReserve},
	} {
		if got := deepScanLimit(tc.spent); got != tc.want {
			t.Fatalf("deepScanLimit(%d)=%d want %d", tc.spent, got, tc.want)
		}
	}
}

// An overlapping cycle skips instead of queueing behind the lock.
func TestReconcileQueuedGitHubJobsSkipsWhileCycleRunning(t *testing.T) {
	service := newServiceWithConfig(nil)
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/a"}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	server.githubWebhook.reconcile.mu.Lock()
	done := make(chan struct{})
	go func() {
		_, _ = server.ReconcileQueuedGitHubJobs(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("overlapping cycle blocked on the reconcile lock")
	}
	server.githubWebhook.reconcile.mu.Unlock()
	if l.calls != 0 {
		t.Fatalf("skipped cycle made %d calls", l.calls)
	}
}

func shortDeadlines(server *Server) {
	server.githubWebhook.recentTimeout = 30 * time.Millisecond
	server.githubWebhook.deepTimeout = 30 * time.Millisecond
}

func deepRepoFixture(t *testing.T, n, runsPer int) (*Server, countingLister, *time.Time) {
	t.Helper()
	service := newServiceWithConfig(nil)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.runsRecent, l.runsDeep = map[string][]int64{}, map[string][]int64{}
	for i := 0; i < n; i++ {
		r := fmt.Sprintf("my-org/r%02d", i)
		l.repos = append(l.repos, r)
		l.runsRecent[r] = []int64{int64(90000 + i)}
		l.runsDeep[r] = manyRuns(1000+i*100, runsPer)
	}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	shortDeadlines(server)
	return server, l, &now
}

// A recent scan that hangs every cycle until its deadline must not starve the
// deep scan: the deep scan runs with its own deadline and gets >= 40 calls.
func TestReconcileQueuedGitHubJobsDeepScanRunsWhenRecentHitsDeadline(t *testing.T) {
	server, l, now := deepRepoFixture(t, 20, 20)
	l.hang["jobs:90000"] = -1 // the first repo's recent run hangs forever
	state := &server.githubWebhook.reconcile
	for cycle := 0; cycle < 3; cycle++ {
		if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		// While deep work is left it must get its full share; the last
		// cycle may finish the pass with fewer calls.
		if state.deepOpen && state.lastDeepSpent < reconcileDeepReserve || state.lastDeepSpent == 0 {
			t.Fatalf("cycle %d: deep scan got %d calls with the recent scan hung, want >= %d", cycle, state.lastDeepSpent, reconcileDeepReserve)
		}
		if state.recent.cursor != 1 || state.recent.repo != "my-org/r00" || state.recent.pos != 0 {
			t.Fatalf("cycle %d: recent cursor %+v, want parked on the hung run", cycle, state.recent)
		}
		*now = now.Add(time.Minute)
	}
}

// A deep run cut off by the real scan deadline is retried next cycle, not
// dropped from the pass.
func TestReconcileQueuedGitHubJobsDeadlineKeepsDeepRun(t *testing.T) {
	server, l, now := deepRepoFixture(t, 2, 3)
	hung := int64(1001) // second run of my-org/r00 in the deep window
	l.hang[fmt.Sprintf("jobs:%d", hung)] = 1
	state := &server.githubWebhook.reconcile
	if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state.deep.repo != "my-org/r00" || state.deep.pos != 1 {
		t.Fatalf("deep cursor after deadline = %+v, want my-org/r00 pos 1", state.deep)
	}
	*now = now.Add(time.Minute)
	if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.jobLists[hung] != 2 {
		t.Fatalf("hung run listed %d times, want 2 (cut, then retried)", l.jobLists[hung])
	}
	for _, id := range append(manyRuns(1000, 3), manyRuns(1100, 3)...) {
		if l.jobLists[id] == 0 {
			t.Fatalf("deep run %d dropped from the pass", id)
		}
	}
	if state.deepOpen {
		t.Fatalf("deep pass still open: %+v", state.deep)
	}
}

// A deep repo listing cut off by the deadline is re-listed next cycle.
func TestReconcileQueuedGitHubJobsDeadlineKeepsDeepRepo(t *testing.T) {
	server, l, now := deepRepoFixture(t, 3, 1)
	l.hang[fmt.Sprintf("runs:my-org/r01:%s", reconcileRunWindow)] = 1
	state := &server.githubWebhook.reconcile
	if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state.deep.cursor != 1 || state.deep.remaining != 2 {
		t.Fatalf("deep cursor after deadline = %+v, want cursor 1 with 2 repos left", state.deep)
	}
	*now = now.Add(time.Minute)
	if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.jobLists[1100] == 0 {
		t.Fatal("deep run of the re-listed repo was never scanned")
	}
	if state.deepOpen {
		t.Fatalf("deep pass still open: %+v", state.deep)
	}
}

func TestReconcileTimeoutsDefault(t *testing.T) {
	h := &githubWebhookHandler{}
	if h.timeout(h.recentTimeout, reconcileRecentTimeout) != reconcileRecentTimeout || h.timeout(h.deepTimeout, reconcileDeepTimeout) != reconcileDeepTimeout {
		t.Fatal("default scan deadlines not used")
	}
	if reconcileRecentTimeout+reconcileDeepTimeout >= time.Minute {
		t.Fatal("scan deadlines must fit in the default 1m interval")
	}
}

package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	return fmt.Sprintf(`{"action":%q,"workflow_job":{"id":%d,"conclusion":%s,"labels":[%s]}}`, action, id, concl, strings.Join(quoted, ","))
}

type webhookHarness struct {
	t       *testing.T
	service *Service
	handler http.Handler
	leader  bool
	inbox   store.WebhookInbox
	server  *Server
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
	return strings.TrimSuffix(body, "}") + fmt.Sprintf(`,"repository":{"full_name":%q}}`, repository)
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

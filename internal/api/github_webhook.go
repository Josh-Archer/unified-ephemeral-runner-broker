package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	maxGitHubWebhookBody = 26 << 20 // GitHub caps payloads at 25 MB.
	deliveryTTL          = 24 * time.Hour
	maxTrackedDeliveries = 50000

	inboxBatch  = 50
	inboxMaxAge = 24 * time.Hour // older queued jobs are useless; drop them
)

// inboxLease is the retry delay for a failed inbox delivery (var for tests).
var inboxLease = 30 * time.Second

// GitHubWebhookOptions wires the inbound GitHub webhook endpoint.
type GitHubWebhookOptions struct {
	// Secret returns the shared HMAC secret. It is called per request so a
	// rotated Kubernetes secret is picked up without a restart.
	Secret func(ctx context.Context) (string, error)
	// IsLeader reports whether this replica currently holds reconciliation
	// leadership. Nil means a single replica that always acts.
	IsLeader func(ctx context.Context) (bool, error)
	// Inbox is a durable queue shared by all replicas. A replica that is not
	// the leader persists the delivery here and acknowledges it; the leader
	// drains it (DrainGitHubWebhookInbox). Nil means non-leader deliveries are
	// rejected with 503, which is only safe for a single replica.
	Inbox store.WebhookInbox
	// WorkflowRun reads the run a queued job belongs to. It is required when
	// RejectForks or AllowedActors is set; a failed lookup fails the delivery
	// rather than allocating.
	WorkflowRun  func(ctx context.Context, repository string, runID int64) (github.WorkflowRun, error)
	WorkflowJobs interface {
		ListRepositories(context.Context, model.GitHubScope) ([]string, error)
		ListQueuedWorkflowRuns(context.Context, string, time.Time) ([]int64, error)
		ListWorkflowJobs(context.Context, string, int64) ([]github.WorkflowJob, error)
	}
	// RejectForks gives no runner to jobs whose run comes from another
	// repository (pull requests from forks).
	RejectForks bool
	// AllowedActors, when set, gives runners only to runs whose actor and
	// triggering actor are both listed.
	AllowedActors []string
}

type githubWebhookMetrics struct {
	received *prometheus.CounterVec
	matched  *prometheus.CounterVec
	ignored  *prometheus.CounterVec
	enqueued *prometheus.CounterVec
	expired  prometheus.Counter
	pageCap  *prometheus.CounterVec
	budget   prometheus.Counter
}

func newGitHubWebhookMetrics() *githubWebhookMetrics {
	return &githubWebhookMetrics{
		received: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_github_webhook_events_received_total",
			Help: "GitHub webhook deliveries received with a valid signature, by event type.",
		}, []string{"event"}),
		matched: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_github_webhook_events_matched_total",
			Help: "workflow_job deliveries matched to a webhook-enabled pool and acted on.",
		}, []string{"pool", "action"}),
		ignored: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_github_webhook_events_ignored_total",
			Help: "GitHub webhook deliveries that were not acted on, by reason.",
		}, []string{"reason"}),
		enqueued: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_github_webhook_events_enqueued_total",
			Help: "workflow_job deliveries persisted to the shared inbox by a non-leader replica.",
		}, []string{"action"}),
		expired: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "uecb_github_webhook_inbox_expired_total",
			Help: "Webhook inbox deliveries discarded after exceeding the maximum processing age.",
		}),
		pageCap: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_github_reconcile_page_cap_total",
			Help: "Queued-job reconcile listings truncated at their page cap, by listing (runs or jobs); the oldest entries were skipped.",
		}, []string{"listing"}),
		budget: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "uecb_github_reconcile_budget_exhausted_total",
			Help: "Queued-job reconcile cycles stopped at the per-cycle GitHub API call budget; the next cycle resumes at the next repository.",
		}),
	}
}

func (m *githubWebhookMetrics) register() {
	for _, c := range []prometheus.Collector{m.received, m.matched, m.ignored, m.enqueued, m.expired, m.pageCap, m.budget} {
		if err := prometheus.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				panic(err)
			}
		}
	}
}

// deliveryTracker remembers X-GitHub-Delivery GUIDs so redelivered events are
// acknowledged without acting twice. A GUID is claimed before processing and
// released again on failure so GitHub's redelivery can retry.
type deliveryTracker struct {
	mu   sync.Mutex
	seen map[string]time.Time // GUID -> expiry; zero time means in flight
	now  func() time.Time
}

func newDeliveryTracker() *deliveryTracker {
	return &deliveryTracker{seen: map[string]time.Time{}, now: time.Now}
}

// claim returns false when the delivery is in flight or already processed.
func (t *deliveryTracker) claim(guid string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if expiry, ok := t.seen[guid]; ok && (expiry.IsZero() || expiry.After(now)) {
		return false
	}
	if len(t.seen) >= maxTrackedDeliveries {
		for k, expiry := range t.seen {
			if !expiry.IsZero() && !expiry.After(now) {
				delete(t.seen, k)
			}
		}
	}
	t.seen[guid] = time.Time{}
	return true
}

func (t *deliveryTracker) done(guid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen[guid] = t.now().Add(deliveryTTL)
}

func (t *deliveryTracker) release(guid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.seen, guid)
}

type githubWebhookHandler struct {
	opts       GitHubWebhookOptions
	service    *Service
	metrics    *githubWebhookMetrics
	deliveries *deliveryTracker
	reconcile  reconcileState
}

// reconcileState carries the round-robin cursor and deep-scan schedule across
// reconcile cycles.
type reconcileState struct {
	mu       sync.Mutex
	cursor   int       // next repository index to scan
	lastDeep time.Time // when the last full deep pass completed
	deepOpen bool      // a deep pass started and has not covered every repo yet
}

// SetGitHubWebhook enables POST /webhooks/github. Without it the route
// answers 404 so pools that do not opt in are unaffected.
func (s *Server) SetGitHubWebhook(opts GitHubWebhookOptions) {
	s.githubWebhook = &githubWebhookHandler{
		opts:       opts,
		service:    s.service,
		metrics:    newGitHubWebhookMetrics(),
		deliveries: newDeliveryTracker(),
	}
}

// workflowJobPayload is the subset of GitHub's workflow_job event we use.
type workflowJobPayload struct {
	Action      string `json:"action"`
	WorkflowJob struct {
		ID         int64     `json:"id"`
		RunID      int64     `json:"run_id"`
		RunnerName string    `json:"runner_name"`
		RunnerID   int64     `json:"runner_id"`
		StartedAt  time.Time `json:"started_at"`
		Labels     []string  `json:"labels"`
		Conclusion string    `json:"conclusion"`
	} `json:"workflow_job"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	h := s.githubWebhook
	if h == nil {
		s.writeError(w, http.StatusNotFound, errors.New("github webhook is not enabled"))
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	h.serve(s, w, r)
}

func (h *githubWebhookHandler) serve(s *Server, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxGitHubWebhookBody))
	if err != nil {
		h.metrics.ignored.WithLabelValues("unreadable_body").Inc()
		s.writeError(w, http.StatusBadRequest, errors.New("unable to read request body"))
		return
	}

	secret := ""
	if h.opts.Secret != nil {
		secret, err = h.opts.Secret(ctx)
	}
	if err != nil || secret == "" {
		log.Printf("github webhook secret unavailable: %v", err)
		h.metrics.ignored.WithLabelValues("secret_unavailable").Inc()
		s.writeError(w, http.StatusServiceUnavailable, errors.New("webhook secret unavailable"))
		return
	}
	signature := strings.TrimSpace(r.Header.Get("X-Hub-Signature-256"))
	if signature == "" {
		h.metrics.ignored.WithLabelValues("missing_signature").Inc()
		s.writeError(w, http.StatusUnauthorized, errors.New("missing X-Hub-Signature-256"))
		return
	}
	if !validGitHubSignature(secret, body, signature) {
		h.metrics.ignored.WithLabelValues("invalid_signature").Inc()
		s.writeError(w, http.StatusUnauthorized, errors.New("invalid signature"))
		return
	}

	event := strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
	h.metrics.received.WithLabelValues(metricEventName(event)).Inc()
	if event != "workflow_job" {
		h.ignore(s, w, "unsupported_event")
		return
	}

	var payload workflowJobPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.metrics.ignored.WithLabelValues("invalid_payload").Inc()
		s.writeError(w, http.StatusBadRequest, errors.New("invalid workflow_job payload"))
		return
	}
	action := payload.Action
	if action != "queued" && action != "in_progress" && action != "completed" {
		h.ignore(s, w, "unsupported_action")
		return
	}
	if payload.WorkflowJob.ID == 0 {
		h.metrics.ignored.WithLabelValues("invalid_payload").Inc()
		s.writeError(w, http.StatusBadRequest, errors.New("workflow_job.id is required"))
		return
	}

	// Only the reconciliation leader acts. A non-leader persists the delivery
	// to the shared inbox so the leader processes it; GitHub does not retry
	// failed deliveries, so it must never be dropped.
	if h.opts.IsLeader != nil {
		leader, err := h.opts.IsLeader(ctx)
		if err != nil || !leader {
			h.enqueueForLeader(ctx, s, w, r, payload)
			return
		}
		// Process anything queued earlier first so a completed event cannot
		// overtake the queued event it belongs to.
		if _, err := h.drainInbox(ctx); err != nil {
			log.Printf("github webhook inbox drain failed: %v", err)
		}
	}

	guid := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if guid != "" {
		if !h.deliveries.claim(guid) {
			h.ignore(s, w, "duplicate_delivery")
			return
		}
	}
	status, code, err := h.process(ctx, payload)
	if err != nil {
		if guid != "" {
			h.deliveries.release(guid)
		}
		log.Printf("github webhook %s job=%d failed: %v", action, payload.WorkflowJob.ID, err)
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}
	if guid != "" {
		h.deliveries.done(guid)
	}
	s.writeJSON(w, code, status)
}

func (h *githubWebhookHandler) enqueueForLeader(ctx context.Context, s *Server, w http.ResponseWriter, r *http.Request, payload workflowJobPayload) {
	if h.opts.Inbox == nil {
		h.metrics.ignored.WithLabelValues("not_leader").Inc()
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "ignored", "reason": "not_leader"})
		return
	}
	id := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if id == "" {
		buf := make([]byte, 16)
		_, _ = rand.Read(buf)
		id = "generated-" + hex.EncodeToString(buf)
	}
	raw, err := json.Marshal(payload)
	if err == nil {
		err = h.opts.Inbox.EnqueueWebhook(ctx, id, raw)
	}
	if err != nil {
		log.Printf("github webhook enqueue failed job=%d: %v", payload.WorkflowJob.ID, err)
		h.metrics.ignored.WithLabelValues("enqueue_failed").Inc()
		s.writeError(w, http.StatusServiceUnavailable, errors.New("unable to queue webhook delivery"))
		return
	}
	h.metrics.enqueued.WithLabelValues(payload.Action).Inc()
	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "reason": "not_leader"})
}

// drainInbox processes deliveries queued by non-leader replicas. A delivery
// that fails stays in the inbox and is retried after the lease expires.
func (h *githubWebhookHandler) drainInbox(ctx context.Context) (int, error) {
	if h.opts.Inbox == nil {
		return 0, nil
	}
	processed := 0
	for {
		batch, err := h.opts.Inbox.ClaimWebhooks(ctx, inboxBatch, inboxLease)
		if err != nil {
			return processed, err
		}
		if len(batch) == 0 {
			return processed, nil
		}
		for _, d := range batch {
			var payload workflowJobPayload
			if err := json.Unmarshal(d.Payload, &payload); err != nil {
				slog.Error("dropping undecodable GitHub webhook inbox delivery", "delivery_id", d.DeliveryID, "error", err)
			} else if time.Since(d.ReceivedAt) > inboxMaxAge {
				slog.Error("dropping expired GitHub webhook inbox delivery", "delivery_id", d.DeliveryID, "job_id", payload.WorkflowJob.ID, "attempts", d.Attempts, "age", time.Since(d.ReceivedAt))
				h.metrics.expired.Inc()
			} else {
				result, _, err := h.process(ctx, payload)
				if err != nil {
					log.Printf("github webhook inbox: delivery %s job=%d attempt=%d failed: %v", d.DeliveryID, payload.WorkflowJob.ID, d.Attempts, err)
					continue
				}
				// A completion can race ahead of its queued event. Keep it leased
				// for retry so a later drain can finalize the allocation.
				if payload.Action == "completed" && result["reason"] == "unknown_job" {
					continue
				}
			}
			if err := h.opts.Inbox.AckWebhook(ctx, d.ID); err != nil {
				return processed, err
			}
			processed++
		}
		if len(batch) < inboxBatch {
			return processed, nil
		}
	}
}

// DrainGitHubWebhookInbox processes queued webhook deliveries. Call it only on
// the leader; it is a no-op when the webhook or its inbox is not configured.
func (s *Server) DrainGitHubWebhookInbox(ctx context.Context) (int, error) {
	if s.githubWebhook == nil {
		return 0, nil
	}
	return s.githubWebhook.drainInbox(ctx)
}

// Reconcile bounds. Every cycle scans queued runs from the last
// reconcileRecentWindow; a deep pass over reconcileRunWindow runs at most every
// reconcileDeepInterval (a 24h-only window silently stranded jobs whose queued
// webhook was missed more than a day ago, home#4157). Each cycle spends at most
// reconcileCallBudget GitHub API calls and resumes round-robin at the next
// repository on the following cycle. The GitHub client also caps pages per
// listing, and the scan stops while the rate limit is nearly spent.
const (
	reconcileRunWindow       = 7 * 24 * time.Hour
	reconcileRecentWindow    = time.Hour
	reconcileDeepInterval    = 10 * time.Minute
	reconcileCallBudget      = 100
	reconcileRateLimitFloor  = 1000
	reconcileRateLimitBackof = 5 * time.Minute
)

// ErrReconcileRateLimited reports a reconcile pass skipped or cut short because
// the GitHub rate-limit remaining dropped below reconcileRateLimitFloor.
var ErrReconcileRateLimited = errors.New("queued job reconcile backing off: GitHub rate limit remaining below floor")

// apiCallCounter is implemented by github.Client; listers without it are
// charged one call per list request.
type apiCallCounter interface {
	APICalls() int64
}

type rateLimitReporter interface {
	RateLimit() (remaining int, reset time.Time, ok bool)
}

// reconcileRateLimited reports whether the lister's last observed rate limit
// is below the floor and its reset has not passed yet.
func reconcileRateLimited(lister any, now time.Time) bool {
	reporter, ok := lister.(rateLimitReporter)
	if !ok {
		return false
	}
	remaining, reset, known := reporter.RateLimit()
	if !known || remaining >= reconcileRateLimitFloor {
		return false
	}
	if reset.IsZero() {
		reset = now.Add(reconcileRateLimitBackof)
	}
	return now.Before(reset)
}

// ReconcileQueuedGitHubJobs discovers jobs whose queued webhook was missed.
// AllocateForWorkflowJob makes this safe to overlap with webhook delivery and
// repeated polls; live job allocations are never duplicated, while a job whose
// earlier allocation failed and is still queued is retried.
func (s *Server) ReconcileQueuedGitHubJobs(ctx context.Context) (int, error) {
	if s == nil || s.githubWebhook == nil || s.githubWebhook.opts.WorkflowJobs == nil {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h := s.githubWebhook
	lister := h.opts.WorkflowJobs
	now := h.service.now()
	if reconcileRateLimited(lister, now) {
		return 0, ErrReconcileRateLimited
	}
	h.reconcile.mu.Lock()
	defer h.reconcile.mu.Unlock()
	state := &h.reconcile

	counter, counted := lister.(apiCallCounter)
	var startCalls, fallbackCalls int64
	if counted {
		startCalls = counter.APICalls()
	}
	charge := func() { fallbackCalls++ }
	spent := func() int64 {
		if counted {
			return counter.APICalls() - startCalls
		}
		return fallbackCalls
	}

	deep := state.deepOpen || state.lastDeep.IsZero() || now.Sub(state.lastDeep) >= reconcileDeepInterval
	window := reconcileRecentWindow
	if deep {
		window = reconcileRunWindow
		state.deepOpen = true
	}
	since := now.Add(-window)
	ctx = withRetryFailedWorkflowJob(ctx)
	charge()
	repos, err := h.opts.WorkflowJobs.ListRepositories(ctx, h.service.cfg.GitHub.Scope)
	if err != nil {
		return 0, err
	}
	if len(repos) == 0 {
		state.cursor = 0
		if deep {
			state.deepOpen, state.lastDeep = false, now
		}
		return 0, nil
	}
	start := state.cursor % len(repos)
	allocated := 0
	var firstErr error
	keepErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for i := 0; i < len(repos); i++ {
		idx := (start + i) % len(repos)
		if err := ctx.Err(); err != nil {
			state.cursor = idx
			return allocated, err
		}
		if reconcileRateLimited(lister, h.service.now()) {
			state.cursor = idx
			return allocated, ErrReconcileRateLimited
		}
		if spent() >= reconcileCallBudget {
			state.cursor = idx
			h.metrics.budget.Inc()
			log.Printf("github reconcile: API call budget %d spent; resuming at %s next cycle", reconcileCallBudget, strings.TrimSpace(repos[idx]))
			return allocated, firstErr
		}
		repository := strings.TrimSpace(repos[idx])
		if !h.service.AcceptsWebhookRepository(repository) {
			continue
		}
		charge()
		runs, err := h.opts.WorkflowJobs.ListQueuedWorkflowRuns(ctx, repository, since)
		if err != nil {
			if !errors.Is(err, github.ErrPageCapReached) {
				keepErr(err)
				continue
			}
			h.metrics.pageCap.WithLabelValues("runs").Inc()
			log.Printf("github reconcile WARNING: %s hit the queued-run page cap (%d pages); its oldest queued runs in the %s window were skipped this cycle", repository, github.MaxQueuedRunPages, window)
		}
		for _, runID := range runs {
			charge()
			jobs, err := h.opts.WorkflowJobs.ListWorkflowJobs(ctx, repository, runID)
			if err != nil {
				if !errors.Is(err, github.ErrPageCapReached) {
					keepErr(err)
					continue
				}
				h.metrics.pageCap.WithLabelValues("jobs").Inc()
				log.Printf("github reconcile WARNING: %s run %d hit the job page cap (%d pages); its remaining jobs were skipped this cycle", repository, runID, github.MaxWorkflowJobPages)
			}
			for _, job := range jobs {
				if job.ID <= 0 || job.Status != "queued" {
					continue
				}
				if _, ok := h.service.MatchWebhookPoolForRepository(job.Labels, repository); !ok {
					continue
				}
				if job.RunID == 0 {
					job.RunID = runID
				}
				payload := workflowJobPayload{Action: "queued"}
				payload.WorkflowJob.ID = job.ID
				payload.WorkflowJob.RunID = job.RunID
				payload.WorkflowJob.Labels = job.Labels
				payload.Repository.FullName = repository
				result, _, err := h.process(ctx, payload)
				if err != nil {
					keepErr(fmt.Errorf("reconcile %s job %d: %w", repository, job.ID, err))
					continue
				}
				if result["status"] == "allocated" {
					allocated++
				}
			}
		}
	}
	// Every repository was visited: the next cycle starts over.
	state.cursor = 0
	if deep {
		state.deepOpen, state.lastDeep = false, now
	}
	return allocated, firstErr
}

// process handles an authenticated, leader-owned queued/completed delivery.
func (h *githubWebhookHandler) process(ctx context.Context, payload workflowJobPayload) (map[string]string, int, error) {
	jobID := payload.WorkflowJob.ID
	ignored := func(reason string) (map[string]string, int, error) {
		h.metrics.ignored.WithLabelValues(reason).Inc()
		return map[string]string{"status": "ignored", "reason": reason}, http.StatusOK, nil
	}
	repository := strings.TrimSpace(payload.Repository.FullName)
	if !h.service.AcceptsWebhookRepository(repository) {
		return ignored("foreign_repository")
	}
	runner := WorkflowJobRunner{Repository: repository, Name: payload.WorkflowJob.RunnerName, ID: payload.WorkflowJob.RunnerID, StartedAt: payload.WorkflowJob.StartedAt}
	switch payload.Action {
	case "queued":
		pool, ok := h.service.MatchWebhookPoolForRepository(payload.WorkflowJob.Labels, repository)
		if !ok {
			if _, labelsMatch := h.service.MatchWebhookPool(payload.WorkflowJob.Labels); labelsMatch {
				return ignored("repository_not_allowed")
			}
			return ignored("no_matching_pool")
		}
		if reason, err := h.checkWorkflowRun(ctx, repository, jobID, payload.WorkflowJob.RunID); err != nil {
			return nil, 0, err
		} else if reason != "" {
			return ignored(reason)
		}
		allocation, created, err := h.service.AllocateForWorkflowJob(ctx, pool, jobID, repository)
		if err != nil {
			return nil, 0, err
		}
		if !created {
			return ignored("duplicate_job")
		}
		h.metrics.matched.WithLabelValues(string(pool.Name), "queued").Inc()
		return map[string]string{"status": "allocated", "allocation_id": allocation.ID, "pool": string(pool.Name)}, http.StatusAccepted, nil
	case "in_progress":
		allocation, found, err := h.service.ClaimWorkflowJob(ctx, jobID, payload.WorkflowJob.RunnerName, payload.WorkflowJob.Labels, runner)
		if err != nil {
			return nil, 0, err
		}
		if !found {
			return ignored("unknown_job")
		}
		h.metrics.matched.WithLabelValues(string(allocation.Pool), "in_progress").Inc()
		return map[string]string{"status": "claimed", "allocation_id": allocation.ID, "pool": string(allocation.Pool)}, http.StatusOK, nil
	default: // completed
		allocation, found, err := h.service.FinalizeWorkflowJob(ctx, jobID, payload.WorkflowJob.Conclusion, runner)
		if err != nil {
			return nil, 0, err
		}
		if !found {
			return ignored("unknown_job")
		}
		h.metrics.matched.WithLabelValues(string(allocation.Pool), "completed").Inc()
		return map[string]string{"status": "finalized", "allocation_id": allocation.ID, "state": string(allocation.State)}, http.StatusOK, nil
	}
}

// checkWorkflowRun applies the fork and actor policies to a newly queued job.
// It returns an ignore reason, or an error when the run cannot be read.
func (h *githubWebhookHandler) checkWorkflowRun(ctx context.Context, repository string, jobID, runID int64) (string, error) {
	if !h.opts.RejectForks && len(h.opts.AllowedActors) == 0 {
		return "", nil
	}
	if _, exists := h.service.findByWorkflowJobID(jobID); exists {
		return "", nil
	}
	if h.opts.WorkflowRun == nil {
		return "", errors.New("workflow run lookup is not configured")
	}
	run, err := h.opts.WorkflowRun(ctx, repository, runID)
	if err != nil {
		return "", fmt.Errorf("check workflow run %d of %s: %w", runID, repository, err)
	}
	if h.opts.RejectForks && !strings.EqualFold(strings.TrimSpace(run.HeadRepository), repository) {
		return "fork_pull_request", nil
	}
	if len(h.opts.AllowedActors) > 0 && (!matchActor(run.Actor, h.opts.AllowedActors) || !matchActor(run.TriggeringActor, h.opts.AllowedActors)) {
		return "actor_not_allowed", nil
	}
	return "", nil
}

func (h *githubWebhookHandler) ignore(s *Server, w http.ResponseWriter, reason string) {
	h.metrics.ignored.WithLabelValues(reason).Inc()
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": reason})
}

// metricEventName bounds label cardinality for attacker-chosen event headers.
func metricEventName(event string) string {
	switch event {
	case "workflow_job", "ping", "workflow_run", "push", "pull_request", "check_run", "check_suite":
		return event
	case "":
		return "none"
	default:
		return "other"
	}
}

func validGitHubSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// GitHubWebhookSecretFunc builds a Secret resolver from inline config or a
// Kubernetes secret reference.
func GitHubWebhookSecretFunc(cfg model.GitHubWebhookConfig, read func(ctx context.Context, name string) (map[string]string, error)) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if secret := strings.TrimSpace(cfg.Secret); secret != "" {
			return secret, nil
		}
		ref := strings.TrimSpace(cfg.SecretRef)
		if ref == "" || read == nil {
			return "", errors.New("githubWebhook secret is not configured")
		}
		data, err := read(ctx, ref)
		if err != nil {
			return "", err
		}
		key := strings.TrimSpace(cfg.SecretKey)
		if key == "" {
			key = "webhook_secret"
		}
		value := strings.TrimSpace(data[key])
		if value == "" {
			return "", errors.New("githubWebhook secret key " + key + " is empty")
		}
		return value, nil
	}
}

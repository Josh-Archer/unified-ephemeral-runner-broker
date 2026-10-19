package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	maxGitHubWebhookBody = 26 << 20 // GitHub caps payloads at 25 MB.
	deliveryTTL          = 24 * time.Hour
	maxTrackedDeliveries = 50000
)

// GitHubWebhookOptions wires the inbound GitHub webhook endpoint.
type GitHubWebhookOptions struct {
	// Secret returns the shared HMAC secret. It is called per request so a
	// rotated Kubernetes secret is picked up without a restart.
	Secret func(ctx context.Context) (string, error)
	// IsLeader reports whether this replica currently holds reconciliation
	// leadership. Nil means a single replica that always acts.
	IsLeader func(ctx context.Context) (bool, error)
}

type githubWebhookMetrics struct {
	received *prometheus.CounterVec
	matched  *prometheus.CounterVec
	ignored  *prometheus.CounterVec
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
	}
}

func (m *githubWebhookMetrics) register() {
	for _, c := range []prometheus.Collector{m.received, m.matched, m.ignored} {
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
		ID         int64    `json:"id"`
		Labels     []string `json:"labels"`
		Conclusion string   `json:"conclusion"`
	} `json:"workflow_job"`
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
	if action != "queued" && action != "completed" {
		h.ignore(s, w, "unsupported_action")
		return
	}
	if payload.WorkflowJob.ID == 0 {
		h.metrics.ignored.WithLabelValues("invalid_payload").Inc()
		s.writeError(w, http.StatusBadRequest, errors.New("workflow_job.id is required"))
		return
	}

	// Only the reconciliation leader acts. 503 (rather than 200) keeps the
	// delivery visible as failed in GitHub so it can be redelivered.
	if h.opts.IsLeader != nil {
		leader, err := h.opts.IsLeader(ctx)
		if err != nil || !leader {
			h.metrics.ignored.WithLabelValues("not_leader").Inc()
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "ignored", "reason": "not_leader"})
			return
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

// process handles an authenticated, leader-owned queued/completed delivery.
func (h *githubWebhookHandler) process(ctx context.Context, payload workflowJobPayload) (map[string]string, int, error) {
	jobID := payload.WorkflowJob.ID
	ignored := func(reason string) (map[string]string, int, error) {
		h.metrics.ignored.WithLabelValues(reason).Inc()
		return map[string]string{"status": "ignored", "reason": reason}, http.StatusOK, nil
	}
	switch payload.Action {
	case "queued":
		pool, ok := h.service.MatchWebhookPool(payload.WorkflowJob.Labels)
		if !ok {
			return ignored("no_matching_pool")
		}
		allocation, created, err := h.service.AllocateForWorkflowJob(ctx, pool, jobID)
		if err != nil {
			return nil, 0, err
		}
		if !created {
			return ignored("duplicate_job")
		}
		h.metrics.matched.WithLabelValues(string(pool.Name), "queued").Inc()
		return map[string]string{"status": "allocated", "allocation_id": allocation.ID, "pool": string(pool.Name)}, http.StatusAccepted, nil
	default: // completed
		allocation, found, err := h.service.FinalizeWorkflowJob(ctx, jobID, payload.WorkflowJob.Conclusion)
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

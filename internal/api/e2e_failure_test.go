package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	codebuildbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/codebuild"
	gcebackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/gce"
	kubernetesbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/kubernetes"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/webhook"
)

// e2eKubeCluster simulates the Kubernetes API surface used by the ARC/Kubernetes
// runner backend (node readiness guard + pod lifecycle). Running in-process
// avoids provisioning a heavyweight Kind cluster in CI while exercising the
// production kubernetesbackend.Backend adapter, HTTP routes, store, metrics,
// and webhook dispatcher end to end.
type e2eKubeCluster struct {
	mu        sync.Mutex
	nodeReady bool
	podsCreated []string
	podsDeleted []string
}

func (k *e2eKubeCluster) Namespace() string { return "arc-runners" }

func (k *e2eKubeCluster) SetNodeReady(ready bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.nodeReady = ready
}

func (k *e2eKubeCluster) Do(_ context.Context, method, path string, body any) (int, []byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	cleanPath := strings.SplitN(path, "?", 2)[0]
	switch {
	case method == http.MethodGet && cleanPath == "/api/v1/nodes/arc-node":
		status := "False"
		if k.nodeReady {
			status = "True"
		}
		payload := map[string]any{
			"kind": "Node",
			"metadata": map[string]any{
				"name": "arc-node",
			},
			"spec": map[string]any{},
			"status": map[string]any{
				"conditions": []map[string]any{
					{"type": "Ready", "status": status},
				},
			},
		}
		raw, _ := json.Marshal(payload)
		return http.StatusOK, raw, nil

	case method == http.MethodGet && cleanPath == "/api/v1/namespaces/arc-runners/pods":
		return http.StatusOK, []byte(`{"items":[]}`), nil

	case method == http.MethodPost && cleanPath == "/api/v1/namespaces/arc-runners/pods":
		if !k.nodeReady {
			return http.StatusForbidden, []byte(`{"message":"pods is forbidden: exceeded quota"}`), nil
		}
		var pod struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if raw, err := json.Marshal(body); err == nil {
			_ = json.Unmarshal(raw, &pod)
		}
		k.podsCreated = append(k.podsCreated, pod.Metadata.Name)
		return http.StatusCreated, []byte(`{}`), nil

	case method == http.MethodDelete && strings.HasPrefix(cleanPath, "/api/v1/namespaces/arc-runners/pods/"):
		name := strings.TrimPrefix(cleanPath, "/api/v1/namespaces/arc-runners/pods/")
		k.podsDeleted = append(k.podsDeleted, name)
		return http.StatusOK, []byte(`{}`), nil

	default:
		return http.StatusNotFound, []byte(`{"message":"not found"}`), nil
	}
}

type e2eGitHubRegistrar struct {
	mu         sync.Mutex
	nextID     int64
	registered []string
	deleted    []int64
}

func (r *e2eGitHubRegistrar) GenerateJITConfig(_ context.Context, _ model.GitHubScope, _, name string, _ []string) (github.JITConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.registered = append(r.registered, name)
	return github.JITConfig{
		RunnerID: 1000 + r.nextID,
		Encoded:  "jit-config-" + name,
	}, nil
}

func (r *e2eGitHubRegistrar) DeleteRunner(_ context.Context, _ model.GitHubScope, runnerID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, runnerID)
	return nil
}

type e2eCloudCall struct {
	Backend      string
	Action       string
	AllocationID string
	RunnerLabel  string
	State        string
}

type e2eCloudProvider struct {
	mu         sync.Mutex
	dispatches []e2eCloudCall
	cleanups   []e2eCloudCall
}

func (p *e2eCloudProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"max_runners":    2,
			"active_runners": 0,
			"free_slots":     2,
		})
		return
	}
	var body struct {
		Action       string `json:"action"`
		Backend      string `json:"backend"`
		AllocationID string `json:"allocation_id"`
		RunnerLabel  string `json:"runner_label"`
		State        string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	call := e2eCloudCall{
		Backend:      body.Backend,
		Action:       body.Action,
		AllocationID: body.AllocationID,
		RunnerLabel:  body.RunnerLabel,
		State:        body.State,
	}
	switch body.Action {
	case "dispatch":
		p.dispatches = append(p.dispatches, call)
	case "cleanup":
		p.cleanups = append(p.cleanups, call)
	}
	p.mu.Unlock()

	if body.Action == "cleanup" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"execution_id": "exec-" + body.Backend + "-" + body.AllocationID,
		"metadata": map[string]string{
			"provider": body.Backend,
		},
	})
}

func (p *e2eCloudProvider) CleanupCount(backendName, allocationID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.cleanups {
		if c.Backend == backendName && c.AllocationID == allocationID {
			n++
		}
	}
	return n
}

type e2eWebhookDelivery struct {
	HeaderEvent string
	Signature   string
	RawBody     []byte
	Envelope    webhook.Envelope
}

type e2eWebhookReceiver struct {
	mu         sync.Mutex
	secret     string
	deliveries []e2eWebhookDelivery
}

func (rec *e2eWebhookReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var env webhook.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rec.mu.Lock()
	rec.deliveries = append(rec.deliveries, e2eWebhookDelivery{
		HeaderEvent: r.Header.Get("X-UECB-Event"),
		Signature:   r.Header.Get("X-UECB-Signature"),
		RawBody:     raw,
		Envelope:    env,
	})
	rec.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (rec *e2eWebhookReceiver) EventsForAllocation(t *testing.T, allocationID string) []webhook.Envelope {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []webhook.Envelope
	for _, d := range rec.deliveries {
		if !webhook.VerifySignature(rec.secret, d.Signature, d.RawBody) {
			t.Fatalf("invalid webhook HMAC signature for delivery %s (event=%s)", d.Envelope.ID, d.HeaderEvent)
		}
		if d.HeaderEvent != d.Envelope.Event {
			t.Fatalf("header X-UECB-Event %q != envelope event %q", d.HeaderEvent, d.Envelope.Event)
		}
		if d.Envelope.Allocation.ID == allocationID {
			out = append(out, d.Envelope)
		}
	}
	return out
}

type e2eStaticSecrets map[string]map[string]string

func (s e2eStaticSecrets) ReadSecret(_ context.Context, name string) (map[string]string, error) {
	data, ok := s[name]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	cloned := make(map[string]string, len(data))
	for k, v := range data {
		cloned[k] = v
	}
	return cloned, nil
}

// TestE2EFailureBackendOutageToFallbackExpiryAndQuarantineWebhook exercises the
// full failure chain from issue #148 (covering #119 quarantine webhooks and #141
// registration/lease expiry):
//  1. Primary ARC backend outage & disablement -> automatic fallback to cloud
//     backends (CodeBuild / GCE) via the external-dispatch adapter.
//  2. Non-registering cloud runner injection -> expires after registrationTimeout
//     with reason="unregistered", invokes cloud cleanup, emits signed
//     allocation.expired webhook, and penalizes the non-registering backend
//     after consecutive expiries so subsequent allocations route to a healthy
//     secondary fallback backend.
//  3. Unfinalized runner past job_timeout with orphanCleanup enabled -> enters
//     quarantined state, invokes cloud cleanup, emits signed
//     allocation.quarantined webhook, and after quarantineTTL transitions to
//     expired with signed allocation.expired webhook.
//  4. Explicit runner quarantine via POST /v1/allocations/{id}/complete ->
//     transitions to quarantined and fires signed allocation.quarantined webhook.
func TestE2EFailureBackendOutageToFallbackExpiryAndQuarantineWebhook(t *testing.T) {
	cloudProvider := &e2eCloudProvider{}
	cloudSrv := httptest.NewServer(cloudProvider)
	defer cloudSrv.Close()

	const signingSecret = "e2e-webhook-signing-secret"
	webhookRec := &e2eWebhookReceiver{secret: signingSecret}
	webhookSrv := httptest.NewServer(webhookRec)
	defer webhookSrv.Close()

	kubeCluster := &e2eKubeCluster{nodeReady: true}
	registrar := &e2eGitHubRegistrar{}

	cfg := config.Default()
	cfg.Broker.StateStore = model.StateStoreConfig{
		Type: "file",
		Path: t.TempDir() + "/e2e-allocations.json",
	}
	cfg.Broker.RegistrationTimeout = 5 * time.Minute
	cfg.Broker.DefaultJobTimeout = 20 * time.Minute
	cfg.Broker.OrphanCleanup.Enabled = true
	cfg.Broker.OrphanCleanup.GracePeriod = 0
	cfg.Broker.OrphanCleanup.QuarantineTTL = 2 * time.Minute
	cfg.Broker.Webhooks = model.WebhooksConfig{
		Enabled:     true,
		MaxAttempts: 1,
		Timeout:     2 * time.Second,
		Endpoints: []model.WebhookEndpointConfig{
			{
				URL:           webhookSrv.URL,
				SigningSecret: signingSecret,
				Events:        []string{"ready", "expired", "quarantined", "failed", "completed", "canceled"},
			},
		},
	}
	cfg.GitHub.Scope = model.GitHubScope{
		Type:       model.GitHubScopeRepository,
		Owner:      "example-org",
		Repository: "example-repo",
	}

	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolFull {
			continue
		}
		cfg.Pools[i].Scheduler = "weighted-round-robin"
		for name, bCfg := range cfg.Pools[i].Backends {
			bCfg.Enabled = false
			cfg.Pools[i].Backends[name] = bCfg
		}
		cfg.Pools[i].Backends[model.BackendARC] = model.BackendConfig{
			Enabled:    true,
			Healthy:    true,
			Driver:     "kubernetes",
			MaxRunners: 2,
			Kubernetes: &model.KubernetesBackendConfig{
				Namespace: "arc-runners",
				NodeGuard: &model.KubernetesNodeGuardConfig{
					NodeName: "arc-node",
				},
			},
		}
		cfg.Pools[i].Backends[model.BackendCodeBuild] = model.BackendConfig{
			Enabled:    true,
			Healthy:    true,
			Weight:     10,
			MaxRunners: 2,
			SecretRef:  "uecb-codebuild",
		}
		cfg.Pools[i].Backends[model.BackendGCE] = model.BackendConfig{
			Enabled:    true,
			Healthy:    true,
			Weight:     1,
			MaxRunners: 2,
			SecretRef:  "uecb-gce",
		}
	}

	secrets := e2eStaticSecrets{
		"uecb-codebuild": {
			"dispatch_url":   cloudSrv.URL + "/codebuild/dispatch",
			"cleanup_url":    cloudSrv.URL + "/codebuild/cleanup",
			"capacity_url":   cloudSrv.URL + "/codebuild/capacity",
			"dispatch_token": "cb-token",
		},
		"uecb-gce": {
			"dispatch_url":   cloudSrv.URL + "/gce/dispatch",
			"cleanup_url":    cloudSrv.URL + "/gce/cleanup",
			"capacity_url":   cloudSrv.URL + "/gce/capacity",
			"dispatch_token": "gce-token",
		},
	}

	registry := backend.NewRegistry(
		kubernetesbackend.NewNamed(cfg, kubeCluster, registrar, model.BackendARC),
		codebuildbackend.New(cfg, secrets),
		gcebackend.New(cfg, secrets),
	)

	clock := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	service := NewService(cfg, registry, nil)
	service.now = func() time.Time { return clock }

	dispatcher := webhook.New(cfg.Broker.Webhooks, secrets, webhookSrv.Client())
	defer dispatcher.Wait()
	service.SetLifecycleNotifier(dispatcher)

	server := newTestServer(t, service)
	handler := server.Handler()

	postJSON := func(path, payload string, wantCode int) model.AllocationStatus {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != wantCode {
			t.Fatalf("POST %s status = %d, want %d: %s", path, rec.Code, wantCode, rec.Body.String())
		}
		var status model.AllocationStatus
		if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
			t.Fatalf("decode POST %s response: %v", path, err)
		}
		dispatcher.Wait()
		return status
	}

	getAllocation := func(id string) model.AllocationStatus {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/allocations/"+id, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /v1/allocations/%s status = %d: %s", id, rec.Code, rec.Body.String())
		}
		var status model.AllocationStatus
		if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
			t.Fatalf("decode GET /v1/allocations/%s: %v", id, err)
		}
		return status
	}

	scrapeMetrics := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /metrics status = %d", rec.Code)
		}
		return rec.Body.String()
	}

	setARCEnabled := func(enabled bool) {
		for i := range service.cfg.Pools {
			if service.cfg.Pools[i].Name != model.PoolFull {
				continue
			}
			arcCfg := service.cfg.Pools[i].Backends[model.BackendARC]
			arcCfg.Enabled = enabled
			service.cfg.Pools[i].Backends[model.BackendARC] = arcCfg
		}
	}

	// --- Step 1: Healthy ARC baseline ---
	arcAlloc := postJSON("/v1/allocations", `{"pool":"full","job_timeout":"20m"}`, http.StatusCreated)
	if arcAlloc.SelectedBackend != model.BackendARC || arcAlloc.State != model.StateReady {
		t.Fatalf("expected healthy baseline on arc ready, got backend=%s state=%s", arcAlloc.SelectedBackend, arcAlloc.State)
	}
	completedARC := postJSON("/v1/allocations/"+arcAlloc.ID+"/complete", `{"state":"completed"}`, http.StatusOK)
	if completedARC.State != model.StateCompleted {
		t.Fatalf("expected completed state on arc baseline, got %s", completedARC.State)
	}

	// --- Step 2a: Runtime ARC node outage -> automatic fallback to cloud ---
	kubeCluster.SetNodeReady(false)
	outageFallbackAlloc := postJSON("/v1/allocations", `{"pool":"full","job_timeout":"20m"}`, http.StatusCreated)
	if outageFallbackAlloc.SelectedBackend != model.BackendCodeBuild {
		t.Fatalf("expected ARC node outage to fall back to codebuild, got %s", outageFallbackAlloc.SelectedBackend)
	}
	postJSON("/v1/allocations/"+outageFallbackAlloc.ID+"/complete", `{"state":"completed"}`, http.StatusOK)

	// --- Step 2b: Disable ARC in config -> cloud fallback & non-registering runner expiry (#141) ---
	setARCEnabled(false)
	unregisteredAlloc1 := postJSON("/v1/allocations", `{"pool":"full","job_timeout":"20m"}`, http.StatusCreated)
	if unregisteredAlloc1.SelectedBackend != model.BackendCodeBuild || unregisteredAlloc1.State != model.StateReady {
		t.Fatalf("expected disabled ARC to fall back to codebuild, got backend=%s state=%s", unregisteredAlloc1.SelectedBackend, unregisteredAlloc1.State)
	}
	if unregisteredAlloc1.RegisteredAt != nil {
		t.Fatalf("expected dynamic cloud runner to start unregistered, got RegisteredAt=%v", unregisteredAlloc1.RegisteredAt)
	}

	// Before registrationTimeout (4m < 5m), runner must remain ready.
	clock = clock.Add(4 * time.Minute)
	if swept := service.SweepExpired(clock); swept != 0 {
		t.Fatalf("expected 0 swept before registrationTimeout, got %d", swept)
	}

	// Past registrationTimeout (6m > 5m, well before 20m job_timeout), non-registering
	// runner must expire with reason="unregistered", trigger cloud cleanup, and
	// emit allocation.expired webhook.
	clock = clock.Add(2 * time.Minute)
	if swept := service.SweepExpired(clock); swept != 1 {
		t.Fatalf("expected 1 non-registering allocation swept at 6m, got %d", swept)
	}
	expired1 := getAllocation(unregisteredAlloc1.ID)
	if expired1.State != model.StateExpired {
		t.Fatalf("expected unregistered runner to be expired, got %s", expired1.State)
	}
	if !strings.Contains(expired1.Error, "runner never registered or claimed within registration timeout") {
		t.Fatalf("unexpected expiry error message: %q", expired1.Error)
	}
	if got := cloudProvider.CleanupCount(string(model.BackendCodeBuild), unregisteredAlloc1.ID); got != 1 {
		t.Fatalf("expected 1 cloud cleanup call for expired non-registering runner, got %d", got)
	}

	dispatcher.Wait()
	events1 := webhookRec.EventsForAllocation(t, unregisteredAlloc1.ID)
	if len(events1) != 2 || events1[0].Event != "allocation.ready" || events1[1].Event != "allocation.expired" {
		t.Fatalf("expected [allocation.ready, allocation.expired] webhooks for %s, got %+v", unregisteredAlloc1.ID, events1)
	}

	// Inject a second non-registering runner on codebuild so consecutive
	// registration expiries reach registrationExpiryThreshold (2) and penalize
	// codebuild in favor of the secondary cloud fallback (gce).
	unregisteredAlloc2 := postJSON("/v1/allocations", `{"pool":"full","job_timeout":"20m"}`, http.StatusCreated)
	if unregisteredAlloc2.SelectedBackend != model.BackendCodeBuild {
		t.Fatalf("expected second fallback allocation on codebuild, got %s", unregisteredAlloc2.SelectedBackend)
	}
	clock = clock.Add(6 * time.Minute)
	if swept := service.SweepExpired(clock); swept != 1 {
		t.Fatalf("expected second non-registering allocation swept, got %d", swept)
	}

	metricsAfterUnregistered := scrapeMetrics()
	if !strings.Contains(metricsAfterUnregistered, `uecb_allocation_expired_total{backend="codebuild",pool="full",reason="unregistered"} 2`) {
		t.Fatalf("expected unregistered expiry metric count 2 on codebuild, got:\n%s", metricsAfterUnregistered)
	}

	// --- Step 3: Penalized backend skipped -> secondary fallback -> Quarantine & Quarantine Webhook (#119) ---
	quarantineAlloc := postJSON("/v1/allocations", `{"pool":"full","job_timeout":"20m"}`, http.StatusCreated)
	if quarantineAlloc.SelectedBackend != model.BackendGCE {
		t.Fatalf("expected broker to skip penalized codebuild and route to gce, got %s", quarantineAlloc.SelectedBackend)
	}

	// Runner registers with the broker, satisfying registrationTimeout, but hangs
	// and never finalizes before job_timeout (20m) elapses.
	regStatus := postJSON("/v1/allocations/"+quarantineAlloc.ID+"/register", `{}`, http.StatusOK)
	if regStatus.RegisteredAt == nil {
		t.Fatal("expected RegisteredAt to be recorded after /register")
	}

	// At +6m (past registrationTimeout), registered runner is NOT expired early.
	clock = clock.Add(6 * time.Minute)
	if swept := service.SweepExpired(clock); swept != 0 {
		t.Fatalf("registered runner must not expire at registrationTimeout, swept=%d", swept)
	}

	// Advance past job_timeout (+15m -> 21m total elapsed). With orphanCleanup
	// enabled, SweepExpired transitions the allocation to quarantined, reclaims
	// the cloud runner via Cleanup, and fires allocation.quarantined webhook.
	clock = clock.Add(15 * time.Minute)
	if swept := service.SweepExpired(clock); swept != 1 {
		t.Fatalf("expected 1 allocation quarantined past job_timeout, got %d", swept)
	}

	quarantinedStatus := getAllocation(quarantineAlloc.ID)
	if quarantinedStatus.State != model.StateQuarantined {
		t.Fatalf("expected state quarantined after job_timeout sweep, got %s (%q)", quarantinedStatus.State, quarantinedStatus.Error)
	}
	if !strings.Contains(quarantinedStatus.Error, "stale runner label quarantined") {
		t.Fatalf("unexpected quarantine error message: %q", quarantinedStatus.Error)
	}
	if got := cloudProvider.CleanupCount(string(model.BackendGCE), quarantineAlloc.ID); got != 1 {
		t.Fatalf("expected 1 cloud cleanup call on quarantine entry, got %d", got)
	}

	dispatcher.Wait()
	eventsQuarantined := webhookRec.EventsForAllocation(t, quarantineAlloc.ID)
	if len(eventsQuarantined) != 2 ||
		eventsQuarantined[0].Event != "allocation.ready" ||
		eventsQuarantined[1].Event != "allocation.quarantined" {
		t.Fatalf("expected [allocation.ready, allocation.quarantined] webhooks for %s, got %+v", quarantineAlloc.ID, eventsQuarantined)
	}
	if eventsQuarantined[1].Allocation.State != model.StateQuarantined {
		t.Fatalf("expected quarantined state in webhook payload, got %s", eventsQuarantined[1].Allocation.State)
	}

	metricsInQuarantine := scrapeMetrics()
	for _, needle := range []string{
		`uecb_orphan_cleanup_actions_total{action="quarantined",backend="gce",pool="full"} 1`,
		`uecb_allocations_reaped_total{backend="gce",pool="full",result="quarantined"} 1`,
		`uecb_stale_runner_labels{backend="gce",phase="quarantined",pool="full"} 1`,
	} {
		if !strings.Contains(metricsInQuarantine, needle) {
			t.Fatalf("expected metrics during quarantine to contain %q, got:\n%s", needle, metricsInQuarantine)
		}
	}

	// Advance past quarantineTTL (2m) -> SweepExpired transitions quarantined -> expired
	// and emits allocation.expired webhook.
	clock = clock.Add(3 * time.Minute)
	if swept := service.SweepExpired(clock); swept != 1 {
		t.Fatalf("expected quarantined allocation to expire after quarantineTTL, got %d", swept)
	}
	postQuarantineStatus := getAllocation(quarantineAlloc.ID)
	if postQuarantineStatus.State != model.StateExpired {
		t.Fatalf("expected state expired after quarantineTTL, got %s", postQuarantineStatus.State)
	}
	if !strings.Contains(postQuarantineStatus.Error, "quarantine expired") {
		t.Fatalf("expected quarantine expired error message, got %q", postQuarantineStatus.Error)
	}
	if got := cloudProvider.CleanupCount(string(model.BackendGCE), quarantineAlloc.ID); got != 2 {
		t.Fatalf("expected cleanup retry on quarantine expiry (total 2), got %d", got)
	}

	dispatcher.Wait()
	eventsPostQuarantine := webhookRec.EventsForAllocation(t, quarantineAlloc.ID)
	if len(eventsPostQuarantine) != 3 ||
		eventsPostQuarantine[0].Event != "allocation.ready" ||
		eventsPostQuarantine[1].Event != "allocation.quarantined" ||
		eventsPostQuarantine[2].Event != "allocation.expired" {
		t.Fatalf("expected [ready, quarantined, expired] webhook lifecycle for %s, got %+v", quarantineAlloc.ID, eventsPostQuarantine)
	}

	// --- Step 4: Non-registering runner whose job_timeout elapses before registrationTimeout
	// enters quarantine directly (allocation.quarantined) and then expires after quarantineTTL (allocation.expired) ---
	shortJobUnregistered := postJSON("/v1/allocations", `{"pool":"full","job_timeout":"2m"}`, http.StatusCreated)
	if shortJobUnregistered.SelectedBackend != model.BackendGCE || shortJobUnregistered.RegisteredAt != nil {
		t.Fatalf("expected unregistered gce allocation, got backend=%s registered=%v", shortJobUnregistered.SelectedBackend, shortJobUnregistered.RegisteredAt)
	}
	clock = clock.Add(3 * time.Minute) // 3m > 2m job_timeout, < 5m registrationTimeout
	if swept := service.SweepExpired(clock); swept != 1 {
		t.Fatalf("expected short-job unregistered runner to be quarantined at 3m, got %d", swept)
	}
	dispatcher.Wait()
	if got := getAllocation(shortJobUnregistered.ID); got.State != model.StateQuarantined {
		t.Fatalf("expected short-job unregistered runner state quarantined, got %s", got.State)
	}
	clock = clock.Add(3 * time.Minute) // past 2m quarantineTTL
	if swept := service.SweepExpired(clock); swept != 1 {
		t.Fatalf("expected quarantined short-job runner to expire after quarantineTTL, got %d", swept)
	}
	dispatcher.Wait()
	shortJobEvents := webhookRec.EventsForAllocation(t, shortJobUnregistered.ID)
	if len(shortJobEvents) != 3 ||
		shortJobEvents[0].Event != "allocation.ready" ||
		shortJobEvents[1].Event != "allocation.quarantined" ||
		shortJobEvents[2].Event != "allocation.expired" {
		t.Fatalf("expected [ready, quarantined, expired] webhooks for short-job unregistered runner, got %+v", shortJobEvents)
	}

	// --- Step 5: Explicit quarantine completion callback fires allocation.quarantined ---
	explicitQuarantineAlloc := postJSON("/v1/allocations", `{"pool":"full","job_timeout":"20m"}`, http.StatusCreated)
	explicitDone := postJSON(
		"/v1/allocations/"+explicitQuarantineAlloc.ID+"/complete",
		`{"state":"quarantined","reason":"runner failed self-check"}`,
		http.StatusOK,
	)
	if explicitDone.State != model.StateQuarantined {
		t.Fatalf("expected explicit complete state quarantined, got %s", explicitDone.State)
	}
	dispatcher.Wait()
	explicitEvents := webhookRec.EventsForAllocation(t, explicitQuarantineAlloc.ID)
	if len(explicitEvents) != 2 ||
		explicitEvents[0].Event != "allocation.ready" ||
		explicitEvents[1].Event != "allocation.quarantined" {
		t.Fatalf("expected [allocation.ready, allocation.quarantined] for explicit quarantine, got %+v", explicitEvents)
	}

	finalMetrics := scrapeMetrics()
	for _, needle := range []string{
		`uecb_allocation_expired_total{backend="codebuild",pool="full",reason="unregistered"} 2`,
		`uecb_allocation_expired_total{backend="gce",pool="full",reason="quarantine_ttl_elapsed"} 2`,
		`uecb_orphan_cleanup_actions_total{action="quarantine_expired",backend="gce",pool="full"} 2`,
	} {
		if !strings.Contains(finalMetrics, needle) {
			t.Fatalf("expected final metrics to contain %q, got:\n%s", needle, finalMetrics)
		}
	}
}

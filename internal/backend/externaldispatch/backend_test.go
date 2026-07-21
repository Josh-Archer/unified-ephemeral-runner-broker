package externaldispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type staticSecrets map[string]map[string]string

func (s staticSecrets) ReadSecret(_ context.Context, name string) (map[string]string, error) {
	values, ok := s[name]
	if !ok {
		return nil, context.DeadlineExceeded
	}
	copyValues := make(map[string]string, len(values))
	for key, value := range values {
		copyValues[key] = value
	}
	return copyValues, nil
}

func newRepoScopedConfig() model.BrokerConfig {
	cfg := config.Default()
	cfg.GitHub.Scope.Type = model.GitHubScopeRepository
	cfg.GitHub.Scope.Owner = "example-org"
	cfg.GitHub.Scope.Repository = "example-repo"
	cfg.GitHub.Scope.Organization = ""
	for index := range cfg.Pools {
		if cfg.Pools[index].Name != model.PoolLite {
			continue
		}
		codebuildCfg := cfg.Pools[index].Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		codebuildCfg.SecretRef = "uecb-codebuild"
		cfg.Pools[index].Backends[model.BackendCodeBuild] = codebuildCfg
	}
	return cfg
}

func TestProvisionDispatchesRunnerLaunch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer broker-secret" {
			t.Fatalf("expected authorization header, got %q", got)
		}

		var payload dispatchRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if payload.GitHub.ScopeType != model.GitHubScopeRepository {
			t.Fatalf("expected repository scope, got %q", payload.GitHub.ScopeType)
		}
		if payload.GitHub.TargetURL != "https://github.com/example-org/example-repo" {
			t.Fatalf("unexpected target url %q", payload.GitHub.TargetURL)
		}
		if payload.JobTimeout != "12m0s" {
			t.Fatalf("expected propagated job timeout, got %q", payload.JobTimeout)
		}
		if payload.JobTimeoutSeconds != 720 {
			t.Fatalf("expected propagated timeout seconds, got %d", payload.JobTimeoutSeconds)
		}
		if !contains(payload.RunnerLabels, "uecb-lite") {
			t.Fatalf("expected pool label to be forwarded, got %v", payload.RunnerLabels)
		}
		if payload.LaunchMode != "cold" {
			t.Fatalf("expected launch mode cold, got %q", payload.LaunchMode)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"execution_id":"run-123","details_url":"https://example.invalid/run-123","metadata":{"provider":"cloud-run"}}`))
	}))
	defer server.Close()

	cfg := newRepoScopedConfig()
	backend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL:   server.URL,
			secretKeyDispatchToken: "broker-secret",
		},
	})

	provisioned, err := backend.Provision(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 12 * time.Minute,
		Labels:     []string{"custom-label"},
	}, model.AllocationStatus{
		ID:              "abc123",
		Pool:            model.PoolLite,
		Metadata:        map[string]string{"launch_mode": "cold"},
		RequestedLabels: []string{"custom-label"},
	})
	if err != nil {
		t.Fatalf("provision failed: %v", err)
	}

	if provisioned.RunnerLabel != "uecb-codebuild-abc123" {
		t.Fatalf("unexpected runner label %q", provisioned.RunnerLabel)
	}
	if provisioned.Metadata["provider"] != "cloud-run" {
		t.Fatalf("expected provider metadata, got %+v", provisioned.Metadata)
	}
	if provisioned.Metadata["execution_id"] != "run-123" {
		t.Fatalf("expected execution_id metadata, got %+v", provisioned.Metadata)
	}
}

func TestProvisionPassesLaunchModeFromAllocationMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload dispatchRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if payload.LaunchMode != "warm" {
			t.Fatalf("expected launch_mode warm, got %q", payload.LaunchMode)
		}
	}))
	defer server.Close()

	cfg := newRepoScopedConfig()
	dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL: server.URL,
		},
	})

	_, err := dispatchBackend.Provision(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	}, model.AllocationStatus{
		ID:       "abc123",
		Pool:     model.PoolLite,
		Metadata: map[string]string{"launch_mode": "warm"},
	})
	if err != nil {
		t.Fatalf("provision failed: %v", err)
	}
}

func TestNewUsesBackendSpecificDispatchTimeout(t *testing.T) {
	azureFunctionsBackend := New(model.BackendAzureFunctions, newRepoScopedConfig(), staticSecrets{})
	codebuildBackend := New(model.BackendCodeBuild, newRepoScopedConfig(), staticSecrets{})

	azureFunctionsClient, ok := azureFunctionsBackend.client.(*http.Client)
	if !ok {
		t.Fatalf("expected default HTTP client, got %T", azureFunctionsBackend.client)
	}
	if azureFunctionsClient.Timeout != azureFunctionsDispatchTimeout {
		t.Fatalf("expected azure functions timeout %s, got %s", azureFunctionsDispatchTimeout, azureFunctionsClient.Timeout)
	}

	codebuildClient, ok := codebuildBackend.client.(*http.Client)
	if !ok {
		t.Fatalf("expected default HTTP client, got %T", codebuildBackend.client)
	}
	if codebuildClient.Timeout != defaultDispatchTimeout {
		t.Fatalf("expected default dispatch timeout %s, got %s", defaultDispatchTimeout, codebuildClient.Timeout)
	}
}

func TestProvisionAllowsAzureFunctionsColdStartLatency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"execution_id":"run-slow","metadata":{"provider":"azure-functions"}}`))
	}))
	defer server.Close()

	cfg := newRepoScopedConfig()
	for index := range cfg.Pools {
		if cfg.Pools[index].Name != model.PoolLite {
			continue
		}
		azureCfg := cfg.Pools[index].Backends[model.BackendAzureFunctions]
		azureCfg.Enabled = true
		azureCfg.SecretRef = "uecb-azure-functions"
		cfg.Pools[index].Backends[model.BackendAzureFunctions] = azureCfg
	}

	azureFunctionsBackend := New(model.BackendAzureFunctions, cfg, staticSecrets{
		"uecb-azure-functions": {
			secretKeyDispatchURL: server.URL,
		},
	})
	provisioned, err := azureFunctionsBackend.Provision(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	}, model.AllocationStatus{
		ID:   "azf-001",
		Pool: model.PoolLite,
	})
	if err != nil {
		t.Fatalf("azure functions provision failed: %v", err)
	}
	if provisioned.Metadata["execution_id"] != "run-slow" {
		t.Fatalf("expected execution_id metadata, got %+v", provisioned.Metadata)
	}

	timeoutServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"execution_id":"run-timeout"}`))
	}))
	defer timeoutServer.Close()

	codebuildBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL: timeoutServer.URL,
		},
	})
	codebuildClient, ok := codebuildBackend.client.(*http.Client)
	if !ok {
		t.Fatalf("expected default HTTP client, got %T", codebuildBackend.client)
	}
	codebuildClient.Timeout = time.Millisecond

	_, err = codebuildBackend.Provision(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	}, model.AllocationStatus{
		ID:   "cb-001",
		Pool: model.PoolLite,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout from short-lived controller call, got %v", err)
	}
}

func TestProvisionFailsWhenSecretMissesDispatchURL(t *testing.T) {
	cfg := newRepoScopedConfig()
	backend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchToken: "broker-secret",
		},
	})

	_, err := backend.Provision(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	}, model.AllocationStatus{
		ID:   "abc123",
		Pool: model.PoolLite,
	})
	if err == nil || !strings.Contains(err.Error(), secretKeyDispatchURL) {
		t.Fatalf("expected missing dispatch_url error, got %v", err)
	}
}

func TestProvisionSurfacesRemoteErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"backend rejected request"}`, http.StatusBadGateway)
	}))
	defer server.Close()

	cfg := newRepoScopedConfig()
	backend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL: server.URL,
		},
	})

	_, err := backend.Provision(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	}, model.AllocationStatus{
		ID:   "abc123",
		Pool: model.PoolLite,
	})
	if err == nil || !strings.Contains(err.Error(), "backend rejected request") {
		t.Fatalf("expected remote error, got %v", err)
	}
}

func TestProvisionClassifiesRetriableRemoteErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		reason string
	}{
		{name: "throttled", status: http.StatusTooManyRequests, reason: backend.FailureReasonThrottled},
		{name: "server", status: http.StatusBadGateway, reason: backend.FailureReasonServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"error":"backend unavailable"}`, tc.status)
			}))
			defer server.Close()

			cfg := newRepoScopedConfig()
			dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
				"uecb-codebuild": {
					secretKeyDispatchURL: server.URL,
				},
			})

			_, err := dispatchBackend.Provision(context.Background(), model.AllocationRequest{
				Pool:       model.PoolLite,
				JobTimeout: 5 * time.Minute,
			}, model.AllocationStatus{
				ID:   "abc123",
				Pool: model.PoolLite,
			})
			if err == nil {
				t.Fatal("expected provision to fail")
			}
			reason, ok := backend.FailureReason(err)
			if !ok || reason != tc.reason {
				t.Fatalf("expected reason %s, got %s ok=%v err=%v", tc.reason, reason, ok, err)
			}
		})
	}
}

func TestProvisionClassifiesContextDeadline(t *testing.T) {
	cfg := newRepoScopedConfig()
	dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL: "http://127.0.0.1:1",
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := dispatchBackend.Provision(ctx, model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	}, model.AllocationStatus{
		ID:   "abc123",
		Pool: model.PoolLite,
	})
	if err == nil {
		t.Fatal("expected provision to fail")
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	reason, ok := backend.FailureReason(err)
	if !ok || reason != backend.FailureReasonTransport {
		t.Fatalf("expected transport reason, got %s ok=%v err=%v", reason, ok, err)
	}
}

func TestProbeRequiresSuccessfulHealthEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
		reason  string
	}{
		{name: "ok", status: http.StatusOK},
		{name: "unauthorized", status: http.StatusUnauthorized, wantErr: true},
		{name: "throttled", status: http.StatusTooManyRequests, wantErr: true, reason: backend.FailureReasonThrottled},
		{name: "server", status: http.StatusServiceUnavailable, wantErr: true, reason: backend.FailureReasonServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer broker-secret" {
					t.Fatalf("expected authorization header, got %q", got)
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			cfg := newRepoScopedConfig()
			dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
				"uecb-codebuild": {
					secretKeyDispatchURL:   "https://dispatch.example.invalid",
					secretKeyHealthURL:     server.URL,
					secretKeyDispatchToken: "broker-secret",
				},
			})
			pool := cfg.Pools[1]
			backendCfg := pool.Backends[model.BackendCodeBuild]

			err := dispatchBackend.Probe(context.Background(), pool, backendCfg)
			if tc.wantErr && err == nil {
				t.Fatal("expected probe to fail")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected probe to pass, got %v", err)
			}
			if tc.reason != "" {
				reason, ok := backend.FailureReason(err)
				if !ok || reason != tc.reason {
					t.Fatalf("expected reason %s, got %s ok=%v err=%v", tc.reason, reason, ok, err)
				}
			}
		})
	}
}

func TestCleanupPostsToCleanupURL(t *testing.T) {
	var gotMethod string
	var gotAuth string
	var gotBackendHeader string
	var payload cleanupRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotBackendHeader = r.Header.Get("X-UECB-Backend")
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode cleanup request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := newRepoScopedConfig()
	dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL:   "https://dispatch.example.invalid",
			secretKeyCleanupURL:    server.URL,
			secretKeyDispatchToken: "broker-secret",
		},
	})

	err := dispatchBackend.Cleanup(context.Background(), model.AllocationStatus{
		ID:              "alloc-1",
		CorrelationID:   "corr-9",
		Pool:            model.PoolLite,
		SelectedBackend: model.BackendCodeBuild,
		RunnerLabel:     "uecb-codebuild-alloc-1",
		State:           model.StateCanceled,
		Error:           "client canceled",
		Metadata: map[string]string{
			"execution_id": "run-123",
			"provider":     "codebuild",
		},
	})
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("expected POST, got %s", gotMethod)
	}
	if gotAuth != "Bearer broker-secret" {
		t.Fatalf("expected bearer auth, got %q", gotAuth)
	}
	if gotBackendHeader != string(model.BackendCodeBuild) {
		t.Fatalf("expected backend header %q, got %q", model.BackendCodeBuild, gotBackendHeader)
	}
	if payload.Action != "cleanup" {
		t.Fatalf("expected action cleanup, got %q", payload.Action)
	}
	if payload.AllocationID != "alloc-1" {
		t.Fatalf("expected allocation id alloc-1, got %q", payload.AllocationID)
	}
	if payload.CorrelationID != "corr-9" {
		t.Fatalf("expected correlation id corr-9, got %q", payload.CorrelationID)
	}
	if payload.RunnerLabel != "uecb-codebuild-alloc-1" {
		t.Fatalf("unexpected runner label %q", payload.RunnerLabel)
	}
	if payload.Backend != string(model.BackendCodeBuild) {
		t.Fatalf("unexpected backend %q", payload.Backend)
	}
	if payload.State != string(model.StateCanceled) {
		t.Fatalf("unexpected state %q", payload.State)
	}
	if payload.Metadata["execution_id"] != "run-123" {
		t.Fatalf("expected execution_id metadata, got %+v", payload.Metadata)
	}
}

func TestCleanupTreatsNotFoundAsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"already gone"}`, http.StatusNotFound)
	}))
	defer server.Close()

	cfg := newRepoScopedConfig()
	dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL: "https://dispatch.example.invalid",
			secretKeyCleanupURL:  server.URL,
		},
	})

	err := dispatchBackend.Cleanup(context.Background(), model.AllocationStatus{
		ID:   "alloc-gone",
		Pool: model.PoolLite,
		State: model.StateExpired,
	})
	if err != nil {
		t.Fatalf("expected 404 cleanup to succeed, got %v", err)
	}
}

func TestCleanupSurfacesRemoteFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"cleanup rejected"}`, http.StatusBadGateway)
	}))
	defer server.Close()

	cfg := newRepoScopedConfig()
	dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL: "https://dispatch.example.invalid",
			secretKeyCleanupURL:  server.URL,
		},
	})

	err := dispatchBackend.Cleanup(context.Background(), model.AllocationStatus{
		ID:   "alloc-fail",
		Pool: model.PoolLite,
		State: model.StateCanceled,
	})
	if err == nil || !strings.Contains(err.Error(), "cleanup rejected") {
		t.Fatalf("expected remote cleanup error, got %v", err)
	}
}

func TestCleanupMissingCleanupURLIsNoop(t *testing.T) {
	cfg := newRepoScopedConfig()
	dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL:   "https://dispatch.example.invalid",
			secretKeyDispatchToken: "broker-secret",
		},
	})

	err := dispatchBackend.Cleanup(context.Background(), model.AllocationStatus{
		ID:   "alloc-skip",
		Pool: model.PoolLite,
		State: model.StateCanceled,
	})
	if err != nil {
		t.Fatalf("expected missing cleanup_url to be a no-op, got %v", err)
	}
}

func TestCleanupDefaultsRunnerLabelWhenEmpty(t *testing.T) {
	var payload cleanupRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode cleanup request: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cfg := newRepoScopedConfig()
	dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyCleanupURL: server.URL,
		},
	})

	err := dispatchBackend.Cleanup(context.Background(), model.AllocationStatus{
		ID:   "xyz",
		Pool: model.PoolLite,
		State: model.StateExpired,
	})
	if err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if payload.RunnerLabel != "uecb-codebuild-xyz" {
		t.Fatalf("expected default runner label, got %q", payload.RunnerLabel)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestCapacityReadsProviderSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET, got %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer broker-secret" {
			t.Fatalf("expected bearer token, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"max_runners":5,"active_runners":2,"pending_runners":1,"warm_runners":1}`))
	}))
	defer server.Close()

	cfg := newRepoScopedConfig()
	dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyCapacityURL:   server.URL,
			secretKeyDispatchToken: "broker-secret",
		},
	})

	status, err := dispatchBackend.Capacity(context.Background())
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if status.MaxRunners != 5 || status.ActiveRunners != 2 || status.PendingRunners != 1 || status.WarmRunners != 1 {
		t.Fatalf("unexpected capacity %+v", status)
	}
	if backend.FreeSlots(status) != 1 {
		t.Fatalf("expected 1 free slot, got %d", backend.FreeSlots(status))
	}
}

func TestCapacityMissingURL(t *testing.T) {
	cfg := newRepoScopedConfig()
	dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL: "https://example.invalid/dispatch",
		},
	})
	if _, err := dispatchBackend.Capacity(context.Background()); err == nil {
		t.Fatal("expected error when capacity_url is missing")
	}
}

func TestProvisionCapacityExhaustedConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"capacity exhausted"}`))
	}))
	defer server.Close()

	cfg := newRepoScopedConfig()
	dispatchBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL: server.URL,
		},
	})
	_, err := dispatchBackend.Provision(context.Background(), model.AllocationRequest{
		Pool: model.PoolLite, JobTimeout: time.Minute,
	}, model.AllocationStatus{ID: "abc", Pool: model.PoolLite})
	if !backend.IsCapacityExhausted(err) {
		t.Fatalf("expected capacity exhausted classification, got %v", err)
	}
}

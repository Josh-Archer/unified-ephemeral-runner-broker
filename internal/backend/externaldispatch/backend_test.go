package externaldispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

	codebuildBackend := New(model.BackendCodeBuild, cfg, staticSecrets{
		"uecb-codebuild": {
			secretKeyDispatchURL: server.URL,
		},
	})
	codebuildClient, ok := codebuildBackend.client.(*http.Client)
	if !ok {
		t.Fatalf("expected default HTTP client, got %T", codebuildBackend.client)
	}
	codebuildClient.Timeout = 10 * time.Millisecond

	_, err = codebuildBackend.Provision(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	}, model.AllocationStatus{
		ID:   "cb-001",
		Pool: model.PoolLite,
	})
	if err == nil || !strings.Contains(err.Error(), "Client.Timeout exceeded while awaiting headers") {
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

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

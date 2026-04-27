package azurefunctions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

func TestProvisionDispatchesToAzureFunctions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer broker-secret" {
			t.Fatalf("expected authorization header, got %q", got)
		}

		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if payload["backend"] != string(model.BackendAzureFunctions) {
			t.Fatalf("expected backend %q, got %v", model.BackendAzureFunctions, payload["backend"])
		}

		_, _ = w.Write([]byte(`{"execution_id":"run-azf-123","metadata":{"platform":"azure-functions"}}`))
	}))
	defer server.Close()

	cfg := config.Default()
	poolIndex := 1
	azureCfg := cfg.Pools[poolIndex].Backends[model.BackendAzureFunctions]
	azureCfg.Enabled = true
	azureCfg.SecretRef = "uecb-azure-functions"
	cfg.Pools[poolIndex].Backends[model.BackendAzureFunctions] = azureCfg

	backend := New(cfg, staticSecrets{
		"uecb-azure-functions": {
			"dispatch_url":   server.URL,
			"dispatch_token": "broker-secret",
		},
	})

	provisioned, err := backend.Provision(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 10 * time.Minute,
	}, model.AllocationStatus{
		ID:   "azf-001",
		Pool: model.PoolLite,
	})
	if err != nil {
		t.Fatalf("provision failed: %v", err)
	}
	if provisioned.RunnerLabel != "uecb-azurefunctions-azf-001" {
		t.Fatalf("unexpected runner label %q", provisioned.RunnerLabel)
	}
	if provisioned.Metadata["execution_id"] != "run-azf-123" {
		t.Fatalf("expected execution_id metadata, got %+v", provisioned.Metadata)
	}
	if provisioned.Metadata["platform"] != "azure-functions" {
		t.Fatalf("expected platform metadata, got %v", provisioned.Metadata["platform"])
	}
	if !strings.Contains(provisioned.Metadata["dispatch_url"], "http://") {
		t.Fatalf("expected dispatch_url metadata, got %q", provisioned.Metadata["dispatch_url"])
	}
}

func TestAzureFunctionsHostConfigUsesPlainTextQueueMessages(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	hostPath := filepath.Join(filepath.Dir(filename), "..", "..", "..", "docker", "azure-functions", "host.json")
	content, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("read host.json: %v", err)
	}

	var hostConfig struct {
		Extensions struct {
			Queues struct {
				MessageEncoding string `json:"messageEncoding"`
			} `json:"queues"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal(content, &hostConfig); err != nil {
		t.Fatalf("decode host.json: %v", err)
	}

	if got := hostConfig.Extensions.Queues.MessageEncoding; got != "none" {
		t.Fatalf("expected queue messageEncoding to be %q, got %q", "none", got)
	}
}

func TestAzureFunctionsRunnerWrapperCapturesFailureContext(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	wrapperPath := filepath.Join(filepath.Dir(filename), "..", "..", "..", "docker", "azure-functions", "function_app.py")
	content, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("read function_app.py: %v", err)
	}
	wrapper := string(content)

	for _, expected := range []string{
		`"RUNNER_ALLOW_RUNASROOT": "1"`,
		`"UECB_PROVIDER": BACKEND_NAME`,
		"runner_log=tail_log(log_path)",
		"stderr=subprocess.STDOUT",
		"stdout=log_file",
		"runner_timeout_seconds(payload)",
		`"timeout"`,
		`"--preserve-status"`,
	} {
		if !strings.Contains(wrapper, expected) {
			t.Fatalf("expected Azure Functions wrapper to contain %q", expected)
		}
	}
}

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
)

const mixedBrokerYAML = `
broker:
  allowUnauthenticated: true
  defaultPool: lite
  stateStore:
    type: memory
pools:
  - name: full
    scheduler: round-robin
    backends:
      arc:
        enabled: true
        healthy: true
        maxRunners: 2
        weight: 1
      kubernetes-primary:
        driver: kubernetes
        enabled: true
        healthy: true
        maxRunners: 3
        crossPoolMaxRunners: 3
        warmMin: 1
        warmMax: 1
        kubernetes:
          namespace: arc-runners
  - name: lite
    scheduler: round-robin
    backends:
      arc:
        enabled: true
        healthy: true
        maxRunners: 2
        weight: 1
`

func TestDisabledPoolStartsAndSurfacesHealthAndMetrics(t *testing.T) {
	path := writeBrokerConfig(t, mixedBrokerYAML)
	cfg, disabled, err := config.LoadServing(path)
	if err != nil {
		t.Fatalf("load serving: %v", err)
	}
	if len(cfg.Pools) != 1 || cfg.Pools[0].Name != model.PoolLite {
		t.Fatalf("serving pools = %+v", cfg.Pools)
	}
	if len(disabled) != 1 || disabled[0].Pool != model.PoolFull || disabled[0].Backend != "kubernetes-primary" {
		t.Fatalf("disabled = %+v", disabled)
	}

	service := NewServiceWithStore(cfg, backend.NewRegistry(testBackend{name: model.BackendARC}), nil, store.NewMemory())
	service.SetDisabledPools([]DisabledPool{{
		Pool:    string(disabled[0].Pool),
		Backend: string(disabled[0].Backend),
		Error:   disabled[0].Error(),
	}})
	server := newTestServer(t, service)
	handler := server.Handler()

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health = %d %s", health.Code, health.Body.String())
	}
	var body struct {
		Status        string         `json:"status"`
		DisabledPools []DisabledPool `json:"disabledPools"`
	}
	if err := json.Unmarshal(health.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v body=%s", err, health.Body.String())
	}
	if body.Status != "degraded" || len(body.DisabledPools) != 1 {
		t.Fatalf("health body = %+v", body)
	}
	got := body.DisabledPools[0]
	if got.Pool != "full" || got.Backend != "kubernetes-primary" || !strings.Contains(got.Error, "warm runners are not supported") || !strings.Contains(got.Error, "pools[full].backends[kubernetes-primary]") {
		t.Fatalf("disabled pool = %+v", got)
	}

	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metrics.Code != http.StatusOK {
		t.Fatalf("metrics = %d", metrics.Code)
	}
	text := metrics.Body.String()
	for _, needle := range []string{
		disabledPoolMetricName,
		`pool="full"`,
		`backend="kubernetes-primary"`,
		"warm runners are not supported",
		"pools[full].backends[kubernetes-primary]",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("metrics missing %q\n%s", needle, text)
		}
	}

	lite := postAllocation(t, handler, `{"pool":"lite","job_timeout":"5m"}`)
	if lite.Code != http.StatusCreated {
		t.Fatalf("lite allocation = %d %s", lite.Code, lite.Body.String())
	}
	var created model.AllocationStatus
	if err := json.Unmarshal(lite.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Pool != model.PoolLite || created.SelectedBackend != model.BackendARC {
		t.Fatalf("allocation = %+v", created)
	}

	full := postAllocation(t, handler, `{"pool":"full","job_timeout":"5m"}`)
	if full.Code == http.StatusCreated {
		t.Fatalf("disabled pool accepted an allocation: %s", full.Body.String())
	}
	if !strings.Contains(full.Body.String(), "unknown_pool") && !strings.Contains(full.Body.String(), "not configured") {
		t.Fatalf("full allocation = %d %s", full.Code, full.Body.String())
	}
}

func TestHealthyConfigOmitsDisabledPoolSignal(t *testing.T) {
	cfg, disabled, err := config.LoadServing("")
	if err != nil || len(disabled) != 0 {
		t.Fatalf("default config: err=%v disabled=%v", err, disabled)
	}
	service := NewServiceWithStore(cfg, backend.NewRegistry(testBackend{name: model.BackendARC}), nil, store.NewMemory())
	server := newTestServer(t, service)
	handler := server.Handler()

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"status":"ok"`) {
		t.Fatalf("health = %d %s", health.Code, health.Body.String())
	}
	if strings.Contains(health.Body.String(), "disabledPools") {
		t.Fatalf("healthy body names disabled pools: %s", health.Body.String())
	}
	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(metrics.Body.String(), disabledPoolMetricName) {
		t.Fatalf("healthy metrics exported %s", disabledPoolMetricName)
	}
}

func postAllocation(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/allocations", bytes.NewBufferString(body)))
	return recorder
}

func writeBrokerConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

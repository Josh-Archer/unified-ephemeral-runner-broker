package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	codebuildbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/codebuild"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

type httpMissingSecretReader struct{}

func (httpMissingSecretReader) ReadSecret(context.Context, string) (map[string]string, error) {
	return nil, errors.New("secret not found")
}

func newTestServer(t *testing.T, service *Service) *Server {
	t.Helper()

	previousRegisterer := prometheus.DefaultRegisterer
	previousGatherer := prometheus.DefaultGatherer
	registry := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = registry
	prometheus.DefaultGatherer = registry
	t.Cleanup(func() {
		prometheus.DefaultRegisterer = previousRegisterer
		prometheus.DefaultGatherer = previousGatherer
	})

	return NewServer(service, nil, "", true)
}

func TestHandleAllocationsAcceptsStringJobTimeout(t *testing.T) {
	service := newServiceWithConfig(nil)
	service.now = func() time.Time { return time.Unix(1000, 0) }
	server := newTestServer(t, service)

	request := httptest.NewRequest(http.MethodPost, "/v1/allocations", bytes.NewBufferString(`{"pool":"full","job_timeout":"15m"}`))
	request.Header.Set(correlationIDHeader, "test-correlation")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var allocation model.AllocationStatus
	if err := json.NewDecoder(recorder.Body).Decode(&allocation); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if allocation.SelectedBackend != model.BackendARC {
		t.Fatalf("expected ARC backend, got %s", allocation.SelectedBackend)
	}

	if allocation.CorrelationID != "test-correlation" {
		t.Fatalf("expected response correlation id to be preserved, got %q", allocation.CorrelationID)
	}

	if recorder.Header().Get(correlationIDHeader) != "test-correlation" {
		t.Fatalf("expected response header correlation id to be preserved, got %q", recorder.Header().Get(correlationIDHeader))
	}

	wantExpiry := time.Unix(1000, 0).Add(15 * time.Minute)
	if !allocation.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expected expiry %s, got %s", wantExpiry, allocation.ExpiresAt)
	}
}

func TestMetricsExposeAllocationSignals(t *testing.T) {
	service := newServiceWithConfig(nil)
	server := newTestServer(t, service)
	handler := server.Handler()

	request := httptest.NewRequest(http.MethodPost, "/v1/allocations", bytes.NewBufferString(`{"pool":"full","job_timeout":"15m"}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected allocation to succeed, got %d: %s", recorder.Code, recorder.Body.String())
	}

	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := metrics.Body.String()

	for _, metric := range []string{
		`uecb_allocations_total{backend="arc",pool="full",result="success"} 1`,
		`uecb_queue_depth{pool="full",state="ready"} 1`,
		`uecb_capacity_utilization_ratio{backend="arc",pool="full"} 0.25`,
		`uecb_http_requests_total{method="POST",route="/v1/allocations",status="Created"} 1`,
		`uecb_allocation_latency_seconds_bucket`,
		`uecb_launch_latency_seconds_bucket`,
		`uecb_registration_latency_seconds_bucket`,
	} {
		if !strings.Contains(body, metric) {
			t.Fatalf("expected metrics to contain %q, got:\n%s", metric, body)
		}
	}
}

func TestHandleAllocationsRejectsMissingExternalBackendSecret(t *testing.T) {
	cfg := config.Default()
	for index := range cfg.Pools {
		if cfg.Pools[index].Name != model.PoolLite {
			continue
		}
		codebuildCfg := cfg.Pools[index].Backends[model.BackendCodeBuild]
		codebuildCfg.Enabled = true
		cfg.Pools[index].Backends[model.BackendCodeBuild] = codebuildCfg
	}

	service := NewService(
		cfg,
		backend.NewRegistry(
			testBackend{name: model.BackendARC},
			codebuildbackend.New(cfg, httpMissingSecretReader{}),
		),
		nil,
	)
	server := newTestServer(t, service)

	request := httptest.NewRequest(http.MethodPost, "/v1/allocations", bytes.NewBufferString(`{"pool":"lite","backend":"codebuild","job_timeout":"5m"}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}

	if !strings.Contains(recorder.Body.String(), "secret not found") {
		t.Fatalf("expected secret error, got %s", recorder.Body.String())
	}
}

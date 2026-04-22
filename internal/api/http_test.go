package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	lambdabackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/lambda"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

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

	wantExpiry := time.Unix(1000, 0).Add(15 * time.Minute)
	if !allocation.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expected expiry %s, got %s", wantExpiry, allocation.ExpiresAt)
	}
}

func TestHandleAllocationsRejectsStubBackend(t *testing.T) {
	cfg := config.Default()
	for index := range cfg.Pools {
		if cfg.Pools[index].Name != model.PoolLite {
			continue
		}
		lambdaCfg := cfg.Pools[index].Backends[model.BackendLambda]
		lambdaCfg.Enabled = true
		cfg.Pools[index].Backends[model.BackendLambda] = lambdaCfg
	}

	service := NewService(
		cfg,
		backend.NewRegistry(
			testBackend{name: model.BackendARC},
			lambdabackend.New(),
		),
		nil,
	)
	server := newTestServer(t, service)

	request := httptest.NewRequest(http.MethodPost, "/v1/allocations", bytes.NewBufferString(`{"pool":"lite","backend":"lambda","job_timeout":"5m"}`))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", recorder.Code, recorder.Body.String())
	}

	if !strings.Contains(recorder.Body.String(), "not implemented yet") {
		t.Fatalf("expected not implemented error, got %s", recorder.Body.String())
	}
}

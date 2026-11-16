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
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestCapacitySnapshotOnAllocationExhaustion(t *testing.T) {
	cfg := config.Default()
	// Configure pool with 2 backends having maxRunners=1 each
	for i := range cfg.Pools {
		if cfg.Pools[i].Name == model.PoolLite {
			cfg.Pools[i].Backends = map[model.BackendName]model.BackendConfig{
				model.BackendARC: {
					Enabled:    true,
					Healthy:    true,
					MaxRunners: 1,
				},
				model.BackendLambda: {
					Enabled:    true,
					Healthy:    true,
					MaxRunners: 1,
				},
			}
		}
	}

	service := NewService(
		cfg,
		backend.NewRegistry(
			testBackend{name: model.BackendARC},
			testBackend{name: model.BackendLambda},
		),
		nil,
	)

	// Fill first backend
	_, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("first allocation failed: %v", err)
	}

	// Fill second backend
	_, err = service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("second allocation failed: %v", err)
	}

	// Third allocation should fail with capacity_exhausted and rich capacity_snapshot
	_, err = service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	})
	if err == nil {
		t.Fatal("expected third allocation to fail, got nil")
	}

	var allocErr *AllocationError
	if !errors.As(err, &allocErr) {
		t.Fatalf("expected AllocationError, got %T: %v", err, err)
	}

	if allocErr.ErrorCode != "capacity_exhausted" {
		t.Fatalf("expected error code 'capacity_exhausted', got %q", allocErr.ErrorCode)
	}
	if allocErr.Pool != model.PoolLite {
		t.Fatalf("expected pool %q, got %q", model.PoolLite, allocErr.Pool)
	}
	if len(allocErr.CapacitySnapshot) != 2 {
		t.Fatalf("expected 2 backends in snapshot, got %d", len(allocErr.CapacitySnapshot))
	}

	arcSnap := allocErr.CapacitySnapshot[string(model.BackendARC)]
	if arcSnap.AvailableSlots != 0 || arcSnap.ActiveAllocations != 1 || arcSnap.MaxConcurrency != 1 || arcSnap.Status != "exhausted" {
		t.Fatalf("unexpected ARC snapshot: %+v", arcSnap)
	}

	lambdaSnap := allocErr.CapacitySnapshot[string(model.BackendLambda)]
	if lambdaSnap.AvailableSlots != 0 || lambdaSnap.ActiveAllocations != 1 || lambdaSnap.MaxConcurrency != 1 || lambdaSnap.Status != "exhausted" {
		t.Fatalf("unexpected Lambda snapshot: %+v", lambdaSnap)
	}

	if !strings.Contains(allocErr.SuggestedAction, "exhausted or blocked") {
		t.Fatalf("expected exhausted remediation message, got: %s", allocErr.SuggestedAction)
	}
}

func TestCapacitySnapshotPinnedBackendExhaustedWithFallbacks(t *testing.T) {
	cfg := config.Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name == model.PoolLite {
			cfg.Pools[i].Backends = map[model.BackendName]model.BackendConfig{
				model.BackendARC: {
					Enabled:    true,
					Healthy:    true,
					MaxRunners: 1,
				},
				model.BackendLambda: {
					Enabled:    true,
					Healthy:    true,
					MaxRunners: 5,
				},
			}
		}
	}

	service := NewService(
		cfg,
		backend.NewRegistry(
			testBackend{name: model.BackendARC},
			testBackend{name: model.BackendLambda},
		),
		nil,
	)

	arc := model.BackendARC
	// Fill ARC
	_, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &arc,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("first allocation failed: %v", err)
	}

	// Request pinned ARC again
	_, err = service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		Backend:    &arc,
		JobTimeout: 5 * time.Minute,
	})
	if err == nil {
		t.Fatal("expected allocation to fail for exhausted pinned backend")
	}

	var allocErr *AllocationError
	if !errors.As(err, &allocErr) {
		t.Fatalf("expected AllocationError, got %T: %v", err, err)
	}

	if allocErr.RequestedBackend != "arc" {
		t.Fatalf("expected requested_backend 'arc', got %q", allocErr.RequestedBackend)
	}

	arcSnap := allocErr.CapacitySnapshot["arc"]
	if arcSnap.Status != "exhausted" || arcSnap.AvailableSlots != 0 {
		t.Fatalf("expected arc to be exhausted, got %+v", arcSnap)
	}

	lambdaSnap := allocErr.CapacitySnapshot["lambda"]
	if lambdaSnap.Status != "available" || lambdaSnap.AvailableSlots != 5 {
		t.Fatalf("expected lambda to have 5 available slots, got %+v", lambdaSnap)
	}

	if !strings.Contains(allocErr.SuggestedAction, "backend=auto") || !strings.Contains(allocErr.SuggestedAction, "lambda") {
		t.Fatalf("expected suggestion to include fallback to lambda, got: %s", allocErr.SuggestedAction)
	}
}

func TestCapacitySnapshotHTTPResponseFormat(t *testing.T) {
	cfg := config.Default()
	for i := range cfg.Pools {
		if cfg.Pools[i].Name == model.PoolLite {
			cfg.Pools[i].Backends = map[model.BackendName]model.BackendConfig{
				model.BackendARC: {
					Enabled:    true,
					Healthy:    true,
					MaxRunners: 1,
				},
			}
		}
	}

	service := NewService(
		cfg,
		backend.NewRegistry(testBackend{name: model.BackendARC}),
		nil,
	)
	server := newTestServer(t, service)

	// Fill ARC
	_, err := service.Allocate(context.Background(), model.AllocationRequest{
		Pool:       model.PoolLite,
		JobTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("setup allocation failed: %v", err)
	}

	// Send HTTP request that fails
	reqBody := `{"pool":"lite","backend":"arc","job_timeout":"5m"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/allocations", bytes.NewBufferString(reqBody))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode JSON response: %v", err)
	}

	if resp["error"] != "capacity_exhausted" {
		t.Fatalf("expected error 'capacity_exhausted', got %v", resp["error"])
	}
	if resp["pool"] != "lite" {
		t.Fatalf("expected pool 'lite', got %v", resp["pool"])
	}
	if resp["requested_backend"] != "arc" {
		t.Fatalf("expected requested_backend 'arc', got %v", resp["requested_backend"])
	}

	snapshot, ok := resp["capacity_snapshot"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected capacity_snapshot map, got %T: %v", resp["capacity_snapshot"], resp["capacity_snapshot"])
	}

	arc, ok := snapshot["arc"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected arc snapshot map, got %T", snapshot["arc"])
	}

	if arc["status"] != "exhausted" {
		t.Fatalf("expected status 'exhausted', got %v", arc["status"])
	}
	if arc["available_slots"].(float64) != 0 {
		t.Fatalf("expected 0 available slots, got %v", arc["available_slots"])
	}

	if resp["suggested_action"] == nil || resp["suggested_action"] == "" {
		t.Fatalf("expected suggested_action in response, got nil")
	}
}

func TestFormatSuggestedAction(t *testing.T) {
	arc := model.BackendARC

	// Case 1: Multiple backends available with pinned backend exhausted
	snapshot := map[string]BackendCapacitySummary{
		"arc": {
			AvailableSlots: 0,
			Status:         "exhausted",
		},
		"lambda": {
			AvailableSlots: 5,
			Status:         "available",
		},
		"azure-functions": {
			AvailableSlots: 2,
			Status:         "available",
		},
	}
	action := FormatSuggestedAction(&arc, snapshot)
	if !strings.Contains(action, "Pinned backend \"arc\" is unavailable") || !strings.Contains(action, "azure-functions, lambda") {
		t.Fatalf("unexpected suggested action: %s", action)
	}

	// Case 2: All backends exhausted
	snapshotAllFull := map[string]BackendCapacitySummary{
		"arc": {
			AvailableSlots: 0,
			Status:         "exhausted",
		},
	}
	actionAllFull := FormatSuggestedAction(nil, snapshotAllFull)
	if !strings.Contains(actionAllFull, "exhausted or blocked") {
		t.Fatalf("unexpected suggested action: %s", actionAllFull)
	}
}

// The snapshot must say why a backend cannot take a job, not just that it has
// no free slots: disabled, exhausted, rate-limited, switched off by the
// circuit breaker, or quarantined after runners never registered.
func TestCapacitySnapshotReportsPerBackendStateAndReason(t *testing.T) {
	cfg := config.Default()
	var pool model.PoolConfig
	for i := range cfg.Pools {
		if cfg.Pools[i].Name != model.PoolLite {
			continue
		}
		cfg.Pools[i].Backends = map[model.BackendName]model.BackendConfig{
			model.BackendARC:    {Enabled: true, Healthy: true, MaxRunners: 1},
			model.BackendLambda: {Enabled: false, Healthy: true, MaxRunners: 3},
			model.BackendCodeBuild: {
				Enabled: true, Healthy: true, MaxRunners: 1,
				CircuitBreaker: model.CircuitBreakerConfig{Enabled: true, FailureThreshold: 1, OpenDuration: 10 * time.Minute},
			},
			model.BackendCloudRun: {
				Enabled: true, Healthy: true, MaxRunners: 2,
				RateLimit: model.RateLimitConfig{Enabled: true, Permits: 1, Interval: time.Hour},
			},
			model.BackendGCE: {Enabled: true, Healthy: true, MaxRunners: 1},
			model.BackendEC2: {
				Enabled: true, Healthy: true, MaxRunners: 2,
				CircuitBreaker: model.CircuitBreakerConfig{Enabled: true},
			},
		}
		pool = cfg.Pools[i]
	}

	service := NewService(
		cfg,
		backend.NewRegistry(
			testBackend{name: model.BackendARC},
			testBackend{name: model.BackendLambda},
			failingBackend{
				testBackend: testBackend{name: model.BackendCodeBuild},
				err:         backend.NewClassifiedError(backend.FailureReasonTimeout, context.DeadlineExceeded),
			},
			testBackend{name: model.BackendCloudRun},
			testBackend{name: model.BackendGCE},
			testBackend{name: model.BackendEC2},
		),
		nil,
	)
	now := time.Unix(1000, 0).UTC()
	service.now = func() time.Time { return now }

	allocatePinned := func(name model.BackendName) error {
		pinned := name
		_, err := service.Allocate(context.Background(), model.AllocationRequest{
			Pool: model.PoolLite, Backend: &pinned, JobTimeout: 5 * time.Minute,
		})
		return err
	}
	if err := allocatePinned(model.BackendARC); err != nil {
		t.Fatalf("arc allocation failed: %v", err)
	}
	if err := allocatePinned(model.BackendCodeBuild); err == nil {
		t.Fatal("expected codebuild provisioning to fail and open its circuit")
	}
	if err := allocatePinned(model.BackendCloudRun); err != nil {
		t.Fatalf("cloud-run allocation failed: %v", err)
	}
	for i := 0; i < registrationExpiryThreshold; i++ {
		service.recordRegistrationExpiry(model.PoolLite, model.BackendGCE, now)
	}

	snapshot := service.BuildCapacitySnapshot(pool, model.AllocationRequest{Pool: model.PoolLite})

	want := map[string]struct {
		status       string
		reasonPart   string
		circuitState string
		retryAt      bool
	}{
		"arc":       {status: "exhausted", reasonPart: "maxRunners(1/1)"},
		"lambda":    {status: "disabled", reasonPart: "enabled: false"},
		"codebuild": {status: "circuit-open", reasonPart: "timeout", circuitState: "open", retryAt: true},
		"cloud-run": {status: "rate-limited", reasonPart: "rateLimit(1/1 per 1h0m0s)", retryAt: true},
		"gce":       {status: "quarantined", reasonPart: "never registered", retryAt: true},
		"ec2":       {status: "available", circuitState: "closed"},
	}
	for name, expected := range want {
		got, ok := snapshot[name]
		if !ok {
			t.Fatalf("snapshot missing %s: %+v", name, snapshot)
		}
		if got.Status != expected.status {
			t.Errorf("%s status = %q, want %q", name, got.Status, expected.status)
		}
		if !strings.Contains(got.Reason, expected.reasonPart) {
			t.Errorf("%s reason = %q, want it to contain %q", name, got.Reason, expected.reasonPart)
		}
		if got.CircuitState != expected.circuitState {
			t.Errorf("%s circuit_state = %q, want %q", name, got.CircuitState, expected.circuitState)
		}
		if (got.RetryAt != nil) != expected.retryAt {
			t.Errorf("%s retry_at = %v, want present=%v", name, got.RetryAt, expected.retryAt)
		}
		if expected.status != "available" && got.AvailableSlots != 0 {
			t.Errorf("%s available_slots = %d, want 0", name, got.AvailableSlots)
		}
	}
	if got := snapshot["codebuild"].RetryAt; got != nil && !got.Equal(now.Add(10*time.Minute)) {
		t.Errorf("codebuild retry_at = %v, want next probe at %v", got, now.Add(10*time.Minute))
	}
	if got := snapshot["ec2"]; got.AvailableSlots != 2 || got.Reason != "" {
		t.Errorf("ec2 = %+v, want 2 free slots and no reason", got)
	}

	// The circuit recovers on its own: once the open duration has passed the
	// backend is offered again (the next request is the half-open trial).
	now = now.Add(11 * time.Minute)
	recovered := service.BuildCapacitySnapshot(pool, model.AllocationRequest{Pool: model.PoolLite})
	if got := recovered["codebuild"]; got.Status != "available" || got.AvailableSlots != 1 {
		t.Errorf("codebuild after open duration = %+v, want available for a half-open trial", got)
	}

	encoded, err := json.Marshal(snapshot["ec2"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "reason") || strings.Contains(string(encoded), "retry_at") {
		t.Errorf("available backend should omit reason and retry_at: %s", encoded)
	}
}

func TestCapacitySnapshotReportsCrossPoolCap(t *testing.T) {
	cfg := config.Default()
	var lite model.PoolConfig
	for i := range cfg.Pools {
		switch cfg.Pools[i].Name {
		case model.PoolLite, model.PoolFull:
			cfg.Pools[i].Backends = map[model.BackendName]model.BackendConfig{
				model.BackendARC: {Enabled: true, Healthy: true, MaxRunners: 1, CrossPoolMaxRunners: 1},
			}
			if cfg.Pools[i].Name == model.PoolLite {
				lite = cfg.Pools[i]
			}
		}
	}
	service := NewService(cfg, backend.NewRegistry(testBackend{name: model.BackendARC}), nil)
	if _, err := service.Allocate(context.Background(), model.AllocationRequest{Pool: model.PoolFull, JobTimeout: 5 * time.Minute}); err != nil {
		t.Fatalf("full-pool allocation failed: %v", err)
	}
	got := service.BuildCapacitySnapshot(lite, model.AllocationRequest{Pool: model.PoolLite})["arc"]
	if got.Status != "exhausted" || got.AvailableSlots != 0 || got.Reason != "crossPoolMaxRunners(1/1)" {
		t.Fatalf("arc in lite = %+v, want exhausted by crossPoolMaxRunners(1/1)", got)
	}
}

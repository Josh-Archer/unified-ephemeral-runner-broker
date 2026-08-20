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

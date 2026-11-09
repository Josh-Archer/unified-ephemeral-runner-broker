package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter/adaptertest"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter/dispatch"
)

func TestDispatchIgnoresUnknownJSONFields(t *testing.T) {
	server := httptest.NewServer(dispatch.NewHandler(adaptertest.NewMemory(1), dispatch.HandlerOptions{}))
	t.Cleanup(server.Close)

	body := `{"action":"dispatch","allocation_id":"extra-1","pool":"lite","backend":"conformance","job_timeout_seconds":60,"future_field":{"ok":true},"github":{"target_url":"https://github.com/example/repo","future_scope":"ignore-me"}}`
	resp, err := http.Post(server.URL+dispatch.PathDispatch, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post dispatch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestDispatchRejectsInvalidJSON(t *testing.T) {
	server := httptest.NewServer(dispatch.NewHandler(adaptertest.NewMemory(1), dispatch.HandlerOptions{}))
	t.Cleanup(server.Close)

	resp, err := http.Post(server.URL+dispatch.PathDispatch, "application/json", strings.NewReader("{"))
	if err != nil {
		t.Fatalf("post dispatch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestDispatchRejectsEmptyAllocationID(t *testing.T) {
	server := httptest.NewServer(dispatch.NewHandler(adaptertest.NewMemory(1), dispatch.HandlerOptions{}))
	t.Cleanup(server.Close)

	body := `{"action":"dispatch","allocation_id":"","pool":"lite","backend":"conformance"}`
	resp, err := http.Post(server.URL+dispatch.PathDispatch, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post dispatch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestCleanupRejectsEmptyAllocationID(t *testing.T) {
	server := httptest.NewServer(dispatch.NewHandler(adaptertest.NewMemory(1), dispatch.HandlerOptions{}))
	t.Cleanup(server.Close)

	body := `{"action":"cleanup","allocation_id":"","state":"canceled"}`
	resp, err := http.Post(server.URL+dispatch.PathCleanup, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post cleanup: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestNilAdapterAndDuplicatePathsServeConfigurationError(t *testing.T) {
	for _, handler := range []http.Handler{
		dispatch.NewHandler(nil, dispatch.HandlerOptions{}),
		dispatch.NewHandler(adaptertest.NewMemory(1), dispatch.HandlerOptions{
			DispatchPath: "/same",
			CleanupPath:  "/same",
			HealthPath:   "/health",
			CapacityPath: "/capacity",
		}),
	} {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	}
}

func TestCapacityRejectsNegativeCounters(t *testing.T) {
	handler := dispatch.NewHandler(negativeCapacityAdapter{}, dispatch.HandlerOptions{})
	req := httptest.NewRequest(http.MethodGet, dispatch.PathCapacity, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

// TestDispatchReleasesReservationWhenLaunchCanceled covers the broker client
// timeout case: after Reserve succeeds, a canceled request mid-Launch must
// still Cleanup with a non-canceled context so the reserved slot is freed.
func TestDispatchReleasesReservationWhenLaunchCanceled(t *testing.T) {
	mem := adaptertest.NewMemory(1)
	ctx, cancel := context.WithCancel(context.Background())
	impl := &cancelOnLaunch{Memory: mem, cancel: cancel}
	handler := dispatch.NewHandler(impl, dispatch.HandlerOptions{})

	body := `{"action":"dispatch","allocation_id":"cancel-launch-1","pool":"lite","backend":"conformance","job_timeout_seconds":60,"github":{"target_url":"https://github.com/example/repo"}}`
	req := httptest.NewRequest(http.MethodPost, dispatch.PathDispatch, strings.NewReader(body))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a launch failure", rec.Code)
	}
	payload, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(payload), "context canceled") && !strings.Contains(string(payload), "canceled") {
		t.Fatalf("response body %q should mention cancel", payload)
	}

	status, err := mem.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if status.ActiveRunners+status.PendingRunners+status.WarmRunners != 0 {
		t.Fatalf("reserved slot leaked after canceled Launch: %+v", status)
	}
}

func TestDispatchReportsCleanupFailureAfterLaunchError(t *testing.T) {
	impl := &failingCleanupAdapter{Memory: adaptertest.NewMemory(1)}
	handler := dispatch.NewHandler(impl, dispatch.HandlerOptions{})

	body := `{"action":"dispatch","allocation_id":"cleanup-fail-1","pool":"lite","backend":"conformance","github":{"target_url":"https://github.com/example/repo"}}`
	req := httptest.NewRequest(http.MethodPost, dispatch.PathDispatch, strings.NewReader(body))
	req = req.WithContext(context.Background())
	req.Header.Set("Content-Type", "application/json")
	// Force Launch failure via empty-label adapter path: wrap Launch to fail.
	impl.failLaunch = true
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var payload map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(payload["error"], "cleanup failed") {
		t.Fatalf("error = %q, want cleanup failure mentioned", payload["error"])
	}
}

type cancelOnLaunch struct {
	*adaptertest.Memory
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnLaunch) Launch(ctx context.Context, request adapter.LaunchRequest) (adapter.LaunchResult, error) {
	c.once.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
	})
	select {
	case <-ctx.Done():
		return adapter.LaunchResult{}, ctx.Err()
	case <-time.After(2 * time.Second):
		return adapter.LaunchResult{}, errors.New("expected request context to cancel")
	}
}

type failingCleanupAdapter struct {
	*adaptertest.Memory
	failLaunch bool
}

func (f *failingCleanupAdapter) Launch(ctx context.Context, request adapter.LaunchRequest) (adapter.LaunchResult, error) {
	if f.failLaunch {
		return adapter.LaunchResult{}, errors.New("forced launch failure")
	}
	return f.Memory.Launch(ctx, request)
}

func (f *failingCleanupAdapter) Cleanup(ctx context.Context, request adapter.CleanupRequest) error {
	return errors.New("forced cleanup failure")
}

type negativeCapacityAdapter struct{}

func (negativeCapacityAdapter) Health(ctx context.Context) (adapter.HealthStatus, error) {
	return adapter.HealthStatus{Healthy: true, Checked: time.Now().UTC()}, nil
}
func (negativeCapacityAdapter) Capacity(ctx context.Context) (adapter.CapacityStatus, error) {
	return adapter.CapacityStatus{MaxRunners: 2, ActiveRunners: -1}, nil
}
func (negativeCapacityAdapter) Reserve(ctx context.Context, request adapter.ReservationRequest) (adapter.Reservation, error) {
	return adapter.Reservation{}, errors.New("unused")
}
func (negativeCapacityAdapter) Launch(ctx context.Context, request adapter.LaunchRequest) (adapter.LaunchResult, error) {
	return adapter.LaunchResult{}, errors.New("unused")
}
func (negativeCapacityAdapter) Cleanup(ctx context.Context, request adapter.CleanupRequest) error {
	return nil
}

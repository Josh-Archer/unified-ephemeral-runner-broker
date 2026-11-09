package dispatch_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

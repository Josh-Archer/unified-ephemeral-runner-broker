package adaptertest

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter/dispatch"
)

func TestRunConformanceMemory(t *testing.T) {
	RunConformance(t, func(testing.TB) adapter.Adapter {
		return NewMemory(2)
	}, Options{
		Backend:      "fixture",
		Pool:         "lite",
		Tenant:       "conformance",
		Priority:     "pr",
		Warm:         true,
		Labels:       []string{"self-hosted", "linux"},
		Capabilities: []string{"docker"},
		Metadata:     map[string]string{"team": "ci"},
	})
}

func TestRunConformanceMemoryDefaults(t *testing.T) {
	RunConformance(t, func(testing.TB) adapter.Adapter {
		return NewMemory(1)
	}, Options{})
}

func TestRunHTTPConformanceMemory(t *testing.T) {
	handler := dispatch.NewHandler(NewMemory(2), dispatch.HandlerOptions{Token: "kit-token"})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	RunHTTPConformance(t, server.URL, HTTPOptions{
		Token:   "kit-token",
		Backend: "fixture",
		Warm:    true,
	})
}

func TestExhaustionSkipsWhenCapacityExceedsProbeCap(t *testing.T) {
	err := checkExhaustion(context.Background(), NewMemory(100), Options{}.normalized())
	var skip skipSignal
	if !errors.As(err, &skip) {
		t.Fatalf("error = %v, want skip", err)
	}
}

func TestExhaustionRequiredWhenCapacityIsLarge(t *testing.T) {
	err := checkExhaustion(context.Background(), NewMemory(100), Options{RequireExhaustion: true}.normalized())
	if err == nil {
		t.Fatal("expected exhaustion to fail when the ceiling exceeds the probe cap")
	}
	var skip skipSignal
	if errors.As(err, &skip) {
		t.Fatal("RequireExhaustion skipped the case")
	}
}

func TestCheckHealthRejectsUnhealthyAdapter(t *testing.T) {
	err := checkHealth(context.Background(), unhealthyAdapter{Memory: NewMemory(1)})
	if err == nil {
		t.Fatal("expected unhealthy adapter to fail health")
	}
}

type unhealthyAdapter struct{ *Memory }

func (unhealthyAdapter) Health(context.Context) (adapter.HealthStatus, error) {
	return adapter.HealthStatus{Healthy: false, Reason: "down"}, nil
}

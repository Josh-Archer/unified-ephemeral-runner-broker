package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel"
)

func installTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	return exporter
}

func TestAllocateFinalizeCancelEmitTracesAndMetrics(t *testing.T) {
	exporter := installTestTracer(t)
	service := newServiceWithConfig(nil)
	server := newTestServer(t, service)
	handler := server.Handler()

	allocateReq := httptest.NewRequest(http.MethodPost, "/v1/allocations", bytes.NewBufferString(`{"pool":"full","job_timeout":"15m"}`))
	allocateReq.Header.Set(correlationIDHeader, "trace-correlation-1")
	allocateRec := httptest.NewRecorder()
	handler.ServeHTTP(allocateRec, allocateReq)
	if allocateRec.Code != http.StatusCreated {
		t.Fatalf("allocate expected 201, got %d: %s", allocateRec.Code, allocateRec.Body.String())
	}

	var allocation model.AllocationStatus
	if err := json.NewDecoder(allocateRec.Body).Decode(&allocation); err != nil {
		t.Fatalf("decode allocation: %v", err)
	}

	completeReq := httptest.NewRequest(http.MethodPost, "/v1/allocations/"+allocation.ID+"/complete", bytes.NewBufferString(`{"state":"completed"}`))
	completeReq.Header.Set(correlationIDHeader, "trace-correlation-1")
	completeRec := httptest.NewRecorder()
	handler.ServeHTTP(completeRec, completeReq)
	if completeRec.Code != http.StatusOK {
		t.Fatalf("complete expected 200, got %d: %s", completeRec.Code, completeRec.Body.String())
	}

	// Second allocation so cancel path is exercised independently.
	allocate2 := httptest.NewRequest(http.MethodPost, "/v1/allocations", bytes.NewBufferString(`{"pool":"full","job_timeout":"15m"}`))
	allocate2.Header.Set(correlationIDHeader, "trace-correlation-2")
	allocate2Rec := httptest.NewRecorder()
	handler.ServeHTTP(allocate2Rec, allocate2)
	if allocate2Rec.Code != http.StatusCreated {
		t.Fatalf("second allocate expected 201, got %d: %s", allocate2Rec.Code, allocate2Rec.Body.String())
	}
	var allocation2 model.AllocationStatus
	if err := json.NewDecoder(allocate2Rec.Body).Decode(&allocation2); err != nil {
		t.Fatalf("decode second allocation: %v", err)
	}

	cancelReq := httptest.NewRequest(http.MethodPost, "/v1/allocations/"+allocation2.ID+"/cancel", nil)
	cancelReq.Header.Set(correlationIDHeader, "trace-correlation-2")
	cancelRec := httptest.NewRecorder()
	handler.ServeHTTP(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel expected 200, got %d: %s", cancelRec.Code, cancelRec.Body.String())
	}

	spans := exporter.GetSpans()
	names := map[string]int{}
	var sawCorrelation, sawPool, sawBackend bool
	for _, span := range spans {
		names[span.Name]++
		for _, attr := range span.Attributes {
			switch string(attr.Key) {
			case "uecb.correlation_id":
				if attr.Value.AsString() != "" {
					sawCorrelation = true
				}
			case "uecb.pool":
				if attr.Value.AsString() == "full" {
					sawPool = true
				}
			case "uecb.backend":
				if attr.Value.AsString() == "arc" {
					sawBackend = true
				}
			case "repository", "subject", "owner", "authorization":
				t.Fatalf("PII/secret attribute %q must not appear on spans", attr.Key)
			}
		}
	}

	for _, required := range []string{spanAllocate, spanBackendProvision, spanFinalize, spanCancel} {
		if names[required] == 0 {
			t.Fatalf("expected span %q, got names=%v", required, names)
		}
	}
	if !sawCorrelation || !sawPool || !sawBackend {
		t.Fatalf("expected non-PII operational span attributes (correlation=%v pool=%v backend=%v)", sawCorrelation, sawPool, sawBackend)
	}

	// Parent/child: allocate should parent backend.provision within the same trace.
	var allocateTrace, provisionParent string
	for _, span := range spans {
		if span.Name == spanAllocate {
			allocateTrace = span.SpanContext.TraceID().String()
		}
		if span.Name == spanBackendProvision && span.Parent.HasSpanID() {
			provisionParent = span.Parent.TraceID().String()
		}
	}
	if allocateTrace == "" || provisionParent == "" || allocateTrace != provisionParent {
		t.Fatalf("expected uecb.backend.provision to share allocate trace; allocateTrace=%q provisionParent=%q", allocateTrace, provisionParent)
	}

	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := metrics.Body.String()
	for _, metric := range []string{
		`uecb_allocations_total{backend="arc",pool="full",result="success"}`,
		`uecb_finalizations_total{backend="arc",pool="full",result="completed"} 1`,
		`uecb_cancellations_total{backend="arc",pool="full",result="success"} 1`,
		`uecb_finalization_latency_seconds_bucket`,
		`uecb_cancellation_latency_seconds_bucket`,
		`uecb_allocation_latency_seconds_bucket`,
	} {
		if !strings.Contains(body, metric) {
			t.Fatalf("expected metrics to contain %q, got:\n%s", metric, body)
		}
	}
	for _, forbidden := range []string{`repository=`, `subject=`, `owner=`} {
		// Labels must not include identity dimensions.
		if strings.Contains(body, forbidden) {
			t.Fatalf("metrics must not contain PII label fragment %q", forbidden)
		}
	}
}

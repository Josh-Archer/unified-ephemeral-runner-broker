package telemetry

import (
	"context"
	"testing"
)

func TestSetupNoopWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("UECB_OTEL_EXPORTER_OTLP_ENDPOINT", "")

	shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if shutdown == nil {
		t.Fatal("expected non-nil shutdown func")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestResolveOTLPEndpointStripsSchemeAndPath(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("UECB_OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318/v1/traces")

	endpoint, insecure := resolveOTLPEndpoint()
	if endpoint != "collector:4318" {
		t.Fatalf("endpoint=%q want collector:4318", endpoint)
	}
	if !insecure {
		t.Fatal("expected insecure for http endpoint")
	}
}

func TestResolveOTLPEndpointHTTPS(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("UECB_OTEL_EXPORTER_OTLP_ENDPOINT", "")

	endpoint, insecure := resolveOTLPEndpoint()
	if endpoint != "collector.example:4318" {
		t.Fatalf("endpoint=%q", endpoint)
	}
	if insecure {
		t.Fatal("expected secure for https endpoint")
	}
}

// Package telemetry configures optional OpenTelemetry export for the broker.
//
// Tracing is inactive (noop) unless an OTLP endpoint is configured via the
// standard OpenTelemetry environment variables. Metric labels and span
// attributes intentionally use only non-PII operational dimensions
// (pool, backend, result, launch_mode, correlation_id, allocation_id).
package telemetry

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

const defaultServiceName = "uecb-broker"

// Setup installs a global TracerProvider when OTLP export is configured.
// Returns a shutdown function that flushes remaining spans.
//
// Enable export by setting one of:
//   - OTEL_EXPORTER_OTLP_ENDPOINT
//   - OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
//   - UECB_OTEL_EXPORTER_OTLP_ENDPOINT
//
// Service name defaults to "uecb-broker" and may be overridden with
// OTEL_SERVICE_NAME.
func Setup(ctx context.Context) (func(context.Context) error, error) {
	endpoint, insecure := resolveOTLPEndpoint()
	if endpoint == "" {
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
		return func(context.Context) error { return nil }, nil
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpoint),
	}
	if insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create otlp http trace exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName()),
		),
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, fmt.Errorf("create otel resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio()))),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	log.Printf("otel traces enabled endpoint=%s service=%s", endpoint, serviceName())
	return provider.Shutdown, nil
}

func serviceName() string {
	if name := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); name != "" {
		return name
	}
	return defaultServiceName
}

// resolveOTLPEndpoint returns host[:port][/path] suitable for the OTLP HTTP
// exporter WithEndpoint option, plus whether insecure transport should be used.
func resolveOTLPEndpoint() (endpoint string, insecure bool) {
	raw := firstNonEmpty(
		os.Getenv("UECB_OTEL_EXPORTER_OTLP_ENDPOINT"),
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"),
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	)
	if raw == "" {
		return "", false
	}

	// Accept either bare host:port or full URL.
	insecure = true
	endpoint = raw
	switch {
	case strings.HasPrefix(raw, "https://"):
		insecure = false
		endpoint = strings.TrimPrefix(raw, "https://")
	case strings.HasPrefix(raw, "http://"):
		insecure = true
		endpoint = strings.TrimPrefix(raw, "http://")
	}

	// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT may include the full path; the HTTP
	// exporter appends /v1/traces when only host:port is given. Strip a trailing
	// /v1/traces so operators can paste either form.
	endpoint = strings.TrimSuffix(endpoint, "/v1/traces")
	endpoint = strings.TrimSuffix(endpoint, "/")
	return endpoint, insecure
}

func sampleRatio() float64 {
	raw := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG"))
	if raw == "" {
		return 1.0
	}
	var ratio float64
	if _, err := fmt.Sscanf(raw, "%f", &ratio); err != nil {
		return 1.0
	}
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ForceFlush is a convenience helper for tests and graceful shutdown paths that
// already hold a TracerProvider shutdown function but want a bounded flush.
func ForceFlush(ctx context.Context, shutdown func(context.Context) error) error {
	if shutdown == nil {
		return nil
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	return shutdown(ctx)
}

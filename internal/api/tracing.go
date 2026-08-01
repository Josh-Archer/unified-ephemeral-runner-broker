package api

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"net/http"
)

const tracerName = "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/api"

// Span names for lifecycle operations. Keep stable for dashboard queries.
const (
	spanAllocate         = "uecb.allocate"
	spanBackendProvision = "uecb.backend.provision"
	spanFinalize         = "uecb.finalize"
	spanCancel           = "uecb.cancel"
)

func tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

func startSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

func spanAttrPool(pool string) attribute.KeyValue {
	return attribute.String("uecb.pool", pool)
}

func spanAttrBackend(backend string) attribute.KeyValue {
	return attribute.String("uecb.backend", backend)
}

func spanAttrResult(result string) attribute.KeyValue {
	return attribute.String("uecb.result", result)
}

func spanAttrLaunchMode(mode string) attribute.KeyValue {
	return attribute.String("uecb.launch_mode", mode)
}

func spanAttrCorrelationID(id string) attribute.KeyValue {
	return attribute.String("uecb.correlation_id", id)
}

func spanAttrAllocationID(id string) attribute.KeyValue {
	return attribute.String("uecb.allocation_id", id)
}

func spanAttrState(state string) attribute.KeyValue {
	return attribute.String("uecb.state", state)
}

func endSpan(span trace.Span, err error) {
	if span == nil {
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

func extractTraceContext(ctx context.Context, header http.Header) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(header))
}

func injectTraceContext(ctx context.Context, header http.Header) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(header))
}

package api

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	correlationIDHeader = "X-Correlation-ID"
	correlationIDKey    = "correlation_id"
)

type contextKey string

const correlationContextKey contextKey = correlationIDKey

type Observer interface {
	ObserveAllocationStart(model.PoolName)
	ObserveAllocationResult(model.PoolName, model.BackendName, string, time.Duration)
	ObserveLaunchLatency(model.PoolName, model.BackendName, time.Duration)
	ObserveRegistrationLatency(model.PoolName, model.BackendName, time.Duration)
	ObserveActiveAllocations([]model.AllocationStatus)
	ObserveCapacity(model.BrokerConfig, []model.AllocationStatus)
}

type noopObserver struct{}

func (noopObserver) ObserveAllocationStart(model.PoolName) {}
func (noopObserver) ObserveAllocationResult(model.PoolName, model.BackendName, string, time.Duration) {
}
func (noopObserver) ObserveLaunchLatency(model.PoolName, model.BackendName, time.Duration) {}
func (noopObserver) ObserveRegistrationLatency(model.PoolName, model.BackendName, time.Duration) {
}
func (noopObserver) ObserveActiveAllocations([]model.AllocationStatus)            {}
func (noopObserver) ObserveCapacity(model.BrokerConfig, []model.AllocationStatus) {}

type PrometheusObserver struct {
	allocationLatency   *prometheus.HistogramVec
	launchLatency       *prometheus.HistogramVec
	registrationLatency *prometheus.HistogramVec
	allocations         *prometheus.CounterVec
	queueDepth          *prometheus.GaugeVec
	capacityUtilization *prometheus.GaugeVec
}

func NewPrometheusObserver(registerer prometheus.Registerer) *PrometheusObserver {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}

	return &PrometheusObserver{
		allocationLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "uecb_allocation_latency_seconds",
			Help:    "End-to-end allocation latency from broker admission through backend provisioning.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 90},
		}, []string{"pool", "backend", "result"}),
		launchLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "uecb_launch_latency_seconds",
			Help:    "Backend launch latency for a selected ephemeral runner.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 90},
		}, []string{"pool", "backend"}),
		registrationLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "uecb_registration_latency_seconds",
			Help:    "Observed latency until a provisioned runner registration response is available to the broker.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 90},
		}, []string{"pool", "backend"}),
		allocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "uecb_allocations_total",
			Help: "Allocation attempts by pool, backend, and result.",
		}, []string{"pool", "backend", "result"}),
		queueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "uecb_queue_depth",
			Help: "Current allocation count by state.",
		}, []string{"pool", "state"}),
		capacityUtilization: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "uecb_capacity_utilization_ratio",
			Help: "Active allocations divided by configured backend capacity.",
		}, []string{"pool", "backend"}),
	}
}

func (o *PrometheusObserver) Register(registerer prometheus.Registerer) error {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	for _, collector := range []prometheus.Collector{
		o.allocationLatency,
		o.launchLatency,
		o.registrationLatency,
		o.allocations,
		o.queueDepth,
		o.capacityUtilization,
	} {
		if err := registerer.Register(collector); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				return err
			}
		}
	}
	return nil
}

func (o *PrometheusObserver) ObserveAllocationStart(pool model.PoolName) {
	o.queueDepth.WithLabelValues(string(pool), string(model.StateReserved)).Inc()
}

func (o *PrometheusObserver) ObserveAllocationResult(pool model.PoolName, backend model.BackendName, result string, latency time.Duration) {
	o.allocations.WithLabelValues(string(pool), string(backend), result).Inc()
	o.allocationLatency.WithLabelValues(string(pool), string(backend), result).Observe(latency.Seconds())
}

func (o *PrometheusObserver) ObserveLaunchLatency(pool model.PoolName, backend model.BackendName, latency time.Duration) {
	o.launchLatency.WithLabelValues(string(pool), string(backend)).Observe(latency.Seconds())
}

func (o *PrometheusObserver) ObserveRegistrationLatency(pool model.PoolName, backend model.BackendName, latency time.Duration) {
	o.registrationLatency.WithLabelValues(string(pool), string(backend)).Observe(latency.Seconds())
}

func (o *PrometheusObserver) ObserveActiveAllocations(statuses []model.AllocationStatus) {
	o.queueDepth.Reset()
	for _, status := range statuses {
		o.queueDepth.WithLabelValues(string(status.Pool), string(status.State)).Inc()
	}
}

func (o *PrometheusObserver) ObserveCapacity(cfg model.BrokerConfig, statuses []model.AllocationStatus) {
	o.capacityUtilization.Reset()
	active := map[model.PoolName]map[model.BackendName]int{}
	for _, status := range statuses {
		if status.State != model.StateReady && status.State != model.StateReserved {
			continue
		}
		if active[status.Pool] == nil {
			active[status.Pool] = map[model.BackendName]int{}
		}
		active[status.Pool][status.SelectedBackend]++
	}
	for _, pool := range cfg.Pools {
		for name, backend := range pool.Backends {
			if !backend.Enabled || backend.MaxRunners <= 0 {
				continue
			}
			used := active[pool.Name][name]
			o.capacityUtilization.WithLabelValues(string(pool.Name), string(name)).Set(float64(used) / float64(backend.MaxRunners))
		}
	}
}

func withCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationContextKey, id)
}

func correlationIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(correlationContextKey).(string); ok {
		return id
	}
	return ""
}

func correlationIDFromRequest(r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get(correlationIDHeader))
	if id == "" {
		return newID()
	}
	return id
}

func logAllocationEvent(ctx context.Context, event string, fields map[string]string) {
	log.Printf("event=%s %s=%s%s", event, correlationIDKey, correlationIDFromContext(ctx), formatLogFields(fields))
}

func formatLogFields(fields map[string]string) string {
	if len(fields) == 0 {
		return ""
	}
	var builder strings.Builder
	for key, value := range fields {
		builder.WriteByte(' ')
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(value)
	}
	return builder.String()
}

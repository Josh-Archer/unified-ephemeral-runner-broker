# Observability Pack

The broker exposes Prometheus metrics on `/metrics` and propagates a shared correlation ID through HTTP responses, allocation state, and structured log fields.

Prometheus scrape annotations can be enabled in the Helm chart without changing broker scheduling behavior:

```yaml
observability:
  prometheus:
    scrape: true
```

## Correlation Model

- Clients may send `X-Correlation-ID` on broker requests.
- If the header is absent, the broker generates one.
- The broker returns the same value in the `X-Correlation-ID` response header.
- Allocation responses include `correlation_id`.
- Broker lifecycle logs include `correlation_id=<value>` with allocation, pool, backend, and error fields where available.

Use the correlation ID as the join key across HTTP access logs, broker lifecycle logs, backend controller logs, and trace spans from downstream adapters.

## Metrics

Core metrics:

- `uecb_http_requests_total{route,method,status}`: broker HTTP request count.
- `uecb_allocations_total{pool,backend,result}`: allocation attempts and success or failure results.
- `uecb_allocation_latency_seconds{pool,backend,result}`: end-to-end broker allocation latency.
- `uecb_launch_latency_seconds{pool,backend}`: backend launch handoff latency.
- `uecb_registration_latency_seconds{pool,backend}`: time until the broker has a provisioned runner label.
- `uecb_queue_depth{pool,state}`: current allocations by state.
- `uecb_capacity_utilization_ratio{pool,backend}`: active allocation count divided by configured backend capacity.

The observability pack does not change scheduler selection, capacity reservation, or backend dispatch behavior. It only observes the existing lifecycle.

## Artifacts

- `observability/grafana-dashboard.json`: importable Grafana dashboard for latency, allocation rate, queue depth, and capacity utilization.
- `observability/prometheus-rules.yaml`: Prometheus alert rules for dispatch latency, failure rate, saturated capacity, and stuck allocations.

## Failure Modes

High allocation latency usually means the broker can accept requests but a backend is slow to launch or register a runner. Compare `uecb_allocation_latency_seconds` with `uecb_launch_latency_seconds` and `uecb_registration_latency_seconds` by backend.

High allocation failure rate means requests are being rejected or backend provisioning is failing. Check broker logs by `correlation_id`, then inspect the selected backend controller logs for the same ID.

Saturated capacity means the scheduler has few or no healthy slots available for a pool/backend. Check `maxRunners`, runner cleanup, and whether completed jobs are leaving allocations in `ready` or `reserved`.

Stuck queue depth means allocations are not moving to terminal states. Check expiration sweeps, backend cancellation behavior, and runner completion callbacks in the consuming environment.

## Example SLOs

Dispatch latency SLO:

- Objective: 99 percent of successful allocations complete within 60 seconds over 30 days.
- Indicator: `histogram_quantile(0.99, sum by (le) (rate(uecb_allocation_latency_seconds_bucket{result="success"}[30d])))`.
- Initial alert: p95 over 60 seconds for 15 minutes.

Successful runner completion SLO:

- Objective: 99 percent of allocation attempts reach a usable runner label without broker-side failure over 30 days.
- Indicator: `sum(rate(uecb_allocations_total{result="success"}[30d])) / sum(rate(uecb_allocations_total[30d]))`.
- Initial alert: failure ratio above 5 percent for 10 minutes.

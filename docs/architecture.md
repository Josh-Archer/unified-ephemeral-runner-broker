# Architecture

`unified-ephemeral-runner-broker` uses an allocate-then-run model.

## Control Plane

- The broker runs in Kubernetes.
- GitHub workflows call the `allocate-runner` action.
- The action exchanges OIDC identity for a broker allocation request.
- The broker verifies GitHub Actions OIDC tokens through the issuer discovery
  document and JWKS before authorizing allocation or completion requests.
- The broker selects a backend, reserves capacity, provisions a runner, and returns the label that the heavy job should target.
- External backends read `dispatch_url` and optional `dispatch_token` from their configured `secretRef` and hand off provisioning to a provider-owned controller.
- On cancel, expire, quarantine terminal, or warm recycle, the broker calls `CleanupBackend.Cleanup` when implemented. Shared external-dispatch backends POST to optional secret key `cleanup_url` with `action: "cleanup"`, allocation id, runner label, state, and provision metadata (for example `execution_id`). Auth reuses `dispatch_token` as a Bearer token. Missing `cleanup_url` is a soft no-op so capacity release still succeeds; launchers should treat cleanup as idempotent (HTTP 2xx and 404 are success).
- Provider-owned controllers can use the public `pkg/adapter` SDK and `pkg/adapter/adaptertest` conformance harness to keep health, capacity, reserve, launch, and cleanup behavior aligned with the broker contract.

## Data Plane

- `arc` provisions in-cluster runners.
- `codebuild`, `lambda`, `cloud-run`, `azure-functions`, `ec2`, and `gce` are lite-profile external runners that dispatch into provider-owned launcher controllers using the shared external dispatch contract.
- `azure-vm` is a static-label VM adapter for environments that already operate persistent Azure VM GitHub runners. It reserves broker capacity and returns `runnerLabel` from backend config.
- The public Azure Functions launcher uses an HTTP dispatch endpoint only for admission and status. Actual runner execution happens on a queue-triggered function inside the same container so the HTTP trigger does not have to stay open for the whole job.
- Each runner handles one job and exits.

## Warm Capacity

Each pool backend may define a warm policy:

- `warmMin`: minimum warm instances to keep reserved.
- `warmMax`: maximum warm instances allowed.
- `warmTTL`: maximum idle lifetime for a warm allocation.

The broker keeps warm allocations in the background when enabled and recycles them on TTL expiry or policy violations. Allocation requests consume warm capacity first when available, then fallback to cold launch.

Warm capacity currently applies only to external dispatch backends and intentionally excludes `arc` and `azure-vm`.

## State And Restart Recovery

The default state store is in-memory. A file-backed state store can persist
allocation records to a mounted volume for restart recovery.

On service startup, the broker rehydrates scheduler accounting from persisted
`reserved`, `ready`, and `warm` allocations. Pending allocations remain queued
and are retried by the queue reconciler when their `retryAfter` time is reached.

## Queued Admission

Queued admission is optional and disabled by default.

When enabled, the broker stores retryable allocation failures as `pending`
instead of failing the workflow immediately. Retryable failures include
temporary provider dispatch errors and open backend circuits. Capacity
exhaustion and rate-limited backends are not queued: the broker tries another
eligible backend when rate limits block the selected backend, then fails fast
when no backend can run the allocation.

## Pools

- `full`: full-capability jobs, ARC only in v1
- `lite`: lightweight jobs, ARC plus enabled external and VM backends

## Default Scheduling

Within a selected pool, backends use `round-robin` across healthy backends with available slots.

Pools can opt into `weighted-round-robin` instead. Backend weights are configured per pool and affect selection when that scheduler is enabled.

Pools can also enable `fairShare`. Fair-share **composes** with the pool backend scheduler rather than replacing it:

1. Optional per-tenant `fairShare.quotas` reject over-quota tenants before backend pick.
2. Fair-share ranks eligible backends by active load and per-tenant usage; higher `priority_class` weights reduce the tenant penalty when capacity exists.
3. Among backends with equal fair-share scores and free capacity, the pool scheduler chooses the backend. With `weighted-round-robin`, backend `weight` values still influence the pick; with `round-robin`, each backend has one slot.

Allocation requests may include `tenant` and `priority_class`. Priority only affects dispatch choice when capacity is available. The broker does not preempt active runners. `usageWindow` and `starvationAfter` are reserved and unused today.

Recommended path: `fairShare.enabled: true` with `scheduler: weighted-round-robin` or `round-robin`. `scheduler: priority-fair-share` is a standalone fair-share mode without weight expansion and shares the same fair-share state instance as `fairShare.enabled`.

## Runtime Backend Admission

Backends may opt into runtime admission controls with `circuitBreaker` and `rateLimit` under `pools[].backends.<name>`.

Admission order is deterministic: static `enabled`/`healthy`, capability filtering, requested timeout filtering, runtime circuit and cold-launch rate limiting, optional tier and live-capacity filtering, scheduler reservation, then backend provisioning.

Circuit state is in-memory and scoped to a single `pool/backend` within one broker process. Keep broker replicas at `1` for this feature unless scheduler, allocation, and admission state are moved to shared storage together. Timeout-like provision failures, throttling, server errors, explicit `failure_class` completion callbacks, and allocation expiry can open the circuit for the failing backend only. Open backends are skipped for unpinned requests so another eligible backend can serve the allocation; pinned requests fail fast with a circuit-open error.

Rate limiting only applies to cold launches. The broker consumes permits during
admission, skips rate-limited backends for the current attempt, and may route a
pinned request to another eligible backend when the pinned backend is
throttled. If every remaining backend is rate-limited, the broker returns an
explicit rate-limit exhaustion error instead of creating a pending allocation.

The background backend-health loop probes open circuits and closes them after the configured recovery threshold. Backends without a probe implementation recover through the same success path once the circuit admits a half-open request.

## Live Backend Capacity

Optional live-capacity routing uses provider-reported free slots when selecting a backend, instead of relying only on configured `maxRunners` and local scheduler reservations.

When `broker.liveCapacity.enabled` is true:

1. Backends that implement `Capacity()` (SDK adapters) or publish `capacity_url` (HTTP dispatch secrets) are polled out of band on `refreshInterval`.
2. Snapshots are cached in memory per broker process and marked stale after `staleAfter`.
3. Before scheduler reservation, exhausted backends are filtered out and `MaxRunners` on the pool snapshot is clamped to the lower of configured and provider-reported limits, after combining provider free slots with local active reservations.
4. Local scheduler reservation remains the broker concurrency authority. If a provider still rejects a provision/reserve for capacity, the broker falls back to another eligible backend (unpinned) or returns a deterministic live-capacity error (pinned).
5. Stale or failed capacity reads follow `failureMode`: `pass-through` (default) uses local accounting only; `block` treats the backend as unavailable.

Admission order with live capacity enabled:

static `enabled`/`healthy` → capabilities → timeout → circuit/rate-limit → tier → **live capacity** → scheduler reservation → provision.

See [adapter-sdk.md](adapter-sdk.md#publishing-capacity) for how SDK and HTTP-dispatch adapters publish capacity.

## Tier-Aware Routing

Tier-aware routing is evaluated after static eligibility, capability filtering, timeout filtering, and runtime backend admission, but before scheduler reservation. It uses the same reduced-pool pattern as capability filtering: blocked backends are removed from the pool snapshot passed to the scheduler, and the configured scheduler remains responsible for final selection.

The allocation path only reads cached tier decisions. Prometheus queries and provider budget, free-tier, or credit calls are refreshed out of band and stored in memory per broker process. This keeps allocation latency independent from billing API latency and avoids making `/healthz` depend on cloud billing availability.

Provider-level `broker.tierRouting.providerRules` are evaluated once per provider snapshot and then applied to every matching backend in each pool. This makes spend limits a first-class routing input: if the AWS provider decision is exceeded with `action: disable`, CodeBuild, Lambda, and EC2 are removed before the scheduler sees the candidate pool.

Tier states are normalized to `healthy`, `approaching`, `exceeded`, and `unknown`. Rule actions are `observe-only`, `deprioritize`, and `disable`. `observe-only` never changes routing. `disable` removes an approaching or exceeded backend from scheduler eligibility. Unknown or stale data follows `broker.tierRouting.failureMode`:

- `pass-through-round-robin`: default; ignore tier data and preserve build throughput.
- `block`: fail allocations when tier data is missing, stale, or over policy.
- `fallback-backends`: route through explicit fallback backends, usually `arc` or another self-hosted label.

Pinned backend requests are not silently rerouted when tier policy blocks the pinned backend. The broker returns a deterministic tier-policy error instead.

Persisted allocation state is rehydrated best-effort on startup. Active, unexpired allocations that still fit the current pool/backend config count against scheduler capacity. Terminal, expired, or no-longer-rehydratable allocations are left visible in the state store but marked terminal so stale state cannot make `/healthz` fail after a backend is disabled.

## Capability Filtering

Capability-aware routing is evaluated before scheduler selection.

- Jobs may send `required_capabilities` and `excluded_capabilities` string arrays on the allocation request.
- Each backend advertises a normalized capability set through `pools[].backends.<name>.capabilities`.
- The broker filters the pool down to eligible backends first, then passes only that reduced backend set into the configured scheduler.
- Pinned backend requests still honor capability filters. If the pinned backend is configured for the pool but excluded by the request, the broker returns a clear rejection instead of falling through to another backend.
- Missing backend capability metadata means that backend advertises no extra capabilities.
- Docker workflows should request `required_capabilities: docker`; serverless-only backends should omit that tag so Docker work is routed to ARC, CodeBuild, or VM-style backends.

This keeps scheduling policy isolated in the scheduler registry while making capability eligibility deterministic at the API layer.

## GitHub Targeting

- `github.scope.type=organization` targets an org runner registration surface and can derive per-pool runner groups from `runnerGroupPrefix`.
- `github.scope.type=repository` targets a single repository registration surface and ignores runner groups.

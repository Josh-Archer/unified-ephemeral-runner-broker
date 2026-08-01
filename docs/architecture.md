# Architecture

`unified-ephemeral-runner-broker` uses an allocate-then-run model.

## Control Plane

- The broker runs in Kubernetes.
- GitHub workflows call the `allocate-runner` action.
- The action exchanges OIDC identity for a broker allocation request.
- After the runner job finishes, workflows should call the `finalize-allocation`
  action (cleanup job with `if: always()`) so capacity is released immediately
  via `POST /v1/allocations/{id}/complete`. Orphan expiry remains the fallback
  when the callback cannot run (hard job kill, cancelled workflow, missing
  cleanup job): after the allocation `job_timeout` TTL the leader sweep
  expires or quarantines the allocation, reclaims the runner label, and invokes
  backend `Cleanup` when implemented.
- The broker verifies GitHub Actions OIDC tokens through the issuer discovery
  document and JWKS before authorizing allocation or completion requests.
- The broker selects a backend, reserves capacity, provisions a runner, and returns the label that the heavy job should target.
- External backends read `dispatch_url` and optional `dispatch_token` from their configured `secretRef` and hand off provisioning to a provider-owned controller.
- On cancel, expire, quarantine terminal, or warm recycle, the broker calls `CleanupBackend.Cleanup` when implemented. Shared external-dispatch backends POST to optional secret key `cleanup_url` with `action: "cleanup"`, allocation id, runner label, state, and provision metadata (for example `execution_id`). Auth reuses `dispatch_token` as a Bearer token. Missing `cleanup_url` is a soft no-op so capacity release still succeeds; launchers should treat cleanup as idempotent (HTTP 2xx and 404 are success).
- **Cancel vs provision race:** `POST /v1/allocations/{id}/cancel` is idempotent. If cancel arrives while a backend `Provision` call is still in flight, the allocation is marked `canceled` and capacity is released immediately. When provision later succeeds, the broker does **not** overwrite the terminal cancel with `ready`; instead it runs `Cleanup` again with the freshly provisioned runner label and metadata so cloud runners/labels are not left behind. Allocate returns `allocation canceled` in that case. Clients that cancel should treat the allocation as terminal regardless of an in-flight allocate response.
- Provider-owned controllers can use the public `pkg/adapter` SDK and `pkg/adapter/adaptertest` conformance harness to keep health, capacity, reserve, launch, and cleanup behavior aligned with the broker contract.

## Data Plane

- `arc` provisions in-cluster runners.
- `codebuild`, `lambda`, `cloud-run`, `azure-functions`, `ec2`, and `gce` are lite-profile external runners that dispatch into provider-owned launcher controllers using the shared external dispatch contract.
- `azure-vm` is a static-label VM adapter for environments that already operate persistent Azure VM GitHub runners. It reserves broker capacity and returns `runnerLabel` from backend config.
- The public Azure Functions launcher uses an HTTP dispatch endpoint only for admission and status. Actual runner execution happens on a queue-triggered function inside the same container so the HTTP trigger does not have to stay open for the whole job.
- Each runner handles one job and exits.

## Warm Capacity

Each pool backend may define a warm policy for cold-start cloud backends:

- `warmMin`: minimum warm instances to keep reserved.
- `warmMax`: maximum warm instances allowed.
- `warmTTL`: maximum idle lifetime for a warm allocation.
- `warmSchedule`: optional timezone + windows so warm targets apply only during known CI periods (effective target is zero outside all windows).

The broker keeps warm allocations in the background when enabled and recycles them on TTL expiry, schedule off-window, or policy violations. Auto selection prefers backends that already hold idle warm capacity; allocation then consumes that warm slot before cold launch.

Warm refill integrates with live `Capacity()` / free-slot snapshots when `broker.liveCapacity` is enabled so pre-warm does not intentionally exceed provider headroom.

Warm capacity applies to external dispatch backends (`codebuild`, `lambda`, `cloud-run`, `azure-functions`, `ec2`, `gce`) and intentionally excludes `arc`, `azure-vm`, and `desktop`. Cost controls (`warmMax`, `warmTTL`, `warmSchedule`, live capacity, tier block) are documented in the README Warm Capacity section.

## State And Restart Recovery

The default state store is in-memory. Supported store types:

| Type | Scope | Use case |
|------|--------|----------|
| `memory` | Process-local | Development only (no restart recovery) |
| `file` | Process-local file on a volume | Single-replica restart recovery |
| `postgres` | Shared across replicas | Multi-replica high availability |

`memory` and `file` must run with a single broker replica. The Helm chart rejects
`replicaCount > 1` unless `stateStore.type` is `postgres`, and the broker process
also refuses to start when `UECB_REPLICAS > 1` with a process-local store.

### Failure mode: in-memory / process-local restart

**`memory` (default) does not survive process restart.** Every allocation record is
lost when the broker process exits. That creates a concrete failure mode:

1. Workflow allocates a runner; broker admits capacity and provisions a cloud
   runner (CodeBuild, Lambda, Azure Functions, VM, …).
2. Broker pod restarts (deploy, OOM, node drain) before
   `POST /v1/allocations/{id}/complete` or normal expiry.
3. The new process has an empty store and empty scheduler accounting, so it can
   **over-admit** new work against the same backend while the previous runners
   are still executing.
4. Completions for forgotten allocation IDs return not-found; provider runners
   stay up until their own job timeout, launcher TTL, or a later capacity-based
   signal.

A related **mid-allocate** window exists for durable stores (`file` / `postgres`):
the broker persists `reserved` before `Provision`, then writes `ready` with the
runner label. A crash between those steps leaves a reserved record without a
label (and may leave a provider runner already started).

Mitigations:

| Store | What survives restart | Recommended for |
|-------|----------------------|-----------------|
| `memory` | Nothing | Local dev only |
| `file` | Allocation JSON on a mounted volume | Single-replica production |
| `postgres` | Shared transactional state | Multi-replica / HA |

Prefer `finalize-allocation` in workflow cleanup (`if: always()`) so capacity is
released even when the broker stays up. Prefer `file` or `postgres` whenever
cloud backends are enabled.

### Startup restart reconciliation

On service startup the broker:

1. **Rehydrates** scheduler accounting from persisted `reserved`, `ready`, and
   `warm` allocations (no-op for empty memory). Pending allocations remain
   queued and are retried by the queue reconciler when `retryAfter` is reached.
2. **Terminalizes incomplete reserved records** (empty runner label): reconstructs
   a deterministic default runner label when possible, moves the allocation to
   `quarantined` (when `orphanCleanup.enabled`) or `expired`, releases capacity,
   and best-effort `CleanupBackend.Cleanup`.
3. For **process-local** stores, **probes `Capacity()`** on backends that publish
   live capacity. When the provider reports more active/pending/warm runners
   than the local store accounts for, the broker creates synthetic reserved
   **capacity holds** (`restart-orphan-<backend>-N`) so it does not immediately
   over-admit, and emits orphan metrics (see [observability.md](observability.md)).

Lost allocation IDs under pure memory cannot be reconstructed; capacity holds and
metrics/alerts bound the damage until provider runners exit or holds expire
(`defaultJobTimeout`).

### Shared transactional state (HA)

With `broker.stateStore.type: postgres` the broker:

- Persists allocations in PostgreSQL so GET, complete, and cancel work through any replica.
- Reserves capacity with a transactional `SaveIfCapacity` check so concurrent
  replicas cannot exceed `maxRunners` (or fair-share tenant quotas when set).
- Claims warm runners with compare-and-swap state transitions.
- Shares circuit-breaker and rate-limit runtime state across replicas.
- Runs the stuck-allocation reaper (expiry sweeps), warm-pool, queue, and
  backend-health reconciliation only on the elected leader (lease in PostgreSQL,
  renewed each background tick). The reaper marks active allocations past
  `job_timeout` + optional `orphanCleanup.gracePeriod` terminal, releases
  `maxRunners` capacity, and calls provider cleanup hooks.

```yaml
broker:
  stateStore:
    type: postgres
    dsnEnv: UECB_STATE_STORE_DSN
  ha:
    leaseTTL: 15s
```

Provide the DSN via `UECB_STATE_STORE_DSN` (chart `stateStore.secretRef`) rather
than inline config when possible.

On service startup, the broker rehydrates scheduler accounting from persisted
`reserved`, `ready`, and `warm` allocations. Pending allocations remain queued
and are retried by the queue reconciler when their `retryAfter` time is reached.

## Allocation Lifecycle Webhooks

Optional outbound webhooks notify external systems when an allocation becomes
`ready` or reaches a terminal state (`failed`, `expired`, `completed`,
`canceled`). Configure under `broker.webhooks`:

```yaml
broker:
  webhooks:
    enabled: true
    timeout: 5s
    maxAttempts: 3
    initialBackoff: 500ms
    maxBackoff: 10s
    endpoints:
      - url: https://hooks.example.com/uecb
        signingSecretRef: uecb-webhook   # key signing_secret by default
        # signingSecret: inline-dev-only
        events:                          # empty = all lifecycle events
          - ready
          - failed
          - expired
          - completed
          - canceled
```

Each delivery is an HTTP POST of a signed JSON envelope:

```json
{
  "id": "<delivery-id>",
  "event": "allocation.ready",
  "occurred_at": "2026-07-28T12:00:00Z",
  "allocation": { "...": "AllocationStatus fields" }
}
```

Headers:

| Header | Description |
|--------|-------------|
| `X-UECB-Event` | `allocation.<event>` |
| `X-UECB-Delivery` | Unique delivery id |
| `X-UECB-Timestamp` | Event time (RFC3339) |
| `X-UECB-Signature` | `sha256=<hex HMAC-SHA256 of body>` |

Deliveries are asynchronous and best-effort. Transient failures (network,
408/429/5xx) retry with exponential backoff up to `maxAttempts`. Permanent
client errors (4xx other than 408/429) are not retried. Webhook failures never
block allocation state transitions.

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

1. Optional per-tenant `fairShare.quotas` reject over-quota tenants before backend pick (`tenant` is typically a repo or workflow label).
2. Optional `fairShare.softReserves` hold per-backend slots for higher-weight priority classes / lanes (`deploy` > `pr` > `smoke`). Lower lanes see `effectiveMax = maxRunners - sum(higher softReserves)` so concurrent low-priority load cannot fill shared capacity and block high-priority within policy. Higher lanes still see the full `maxRunners` budget.
3. Fair-share ranks eligible backends by active load and per-tenant usage; higher `priority_class` weights reduce the tenant penalty when capacity exists.
4. Among backends with equal fair-share scores and free capacity, the pool scheduler chooses the backend. With `weighted-round-robin`, backend `weight` values still influence the pick; with `round-robin`, each backend has one slot.

Allocation requests may include `tenant` and `priority_class`. Soft reserves and priority weights shape *new* admissions only; the broker does not preempt active runners. Restart rehydration accounts existing work under hard `maxRunners` (soft reserves are not applied) so already-admitted low-priority runners are not expired. `usageWindow` and `starvationAfter` are reserved and unused today.

Recommended path: `fairShare.enabled: true` with `scheduler: weighted-round-robin` or `round-robin`, named `priorityClasses`, and `softReserves` for high-risk lanes. `scheduler: priority-fair-share` is a standalone fair-share mode without weight expansion and shares the same fair-share state instance as `fairShare.enabled`.

## Runtime Backend Admission

Backends may opt into runtime admission controls with `circuitBreaker` and `rateLimit` under `pools[].backends.<name>`.

Admission order is deterministic: static `enabled`/`healthy`, capability filtering, requested timeout filtering, runtime circuit and cold-launch rate limiting, optional tier and live-capacity filtering, optional quality-aware ranking, scheduler reservation, then backend provisioning.

Circuit and rate-limit runtime state is process-local for `memory`/`file` stores and
shared through the state store when `type: postgres`. Keep broker replicas at `1`
for process-local stores. With postgres HA, admission decisions reload shared state
before consuming permits. Timeout-like provision failures, throttling, server errors,
explicit `failure_class` completion callbacks, and allocation expiry can open the
circuit for the failing backend only. Open backends are skipped for unpinned requests
so another eligible backend can serve the allocation; pinned requests fail fast with
a circuit-open error.

Rate limiting only applies to cold launches. The broker consumes permits during
admission, skips rate-limited backends for the current attempt, and may route a
pinned request to another eligible backend when the pinned backend is
throttled. If every remaining backend is rate-limited, the broker returns an
explicit rate-limit exhaustion error instead of creating a pending allocation.

The background backend-health loop probes open circuits and closes them after the configured recovery threshold. Backends without a probe implementation recover through the same success path once the circuit admits a half-open request.

## Quality-Aware Auto Backend Selection

Optional quality-aware routing ranks eligible backends after capability, admission, tier, and live-capacity filtering. When `broker.qualityAware.enabled` is true and the allocation is not pinned:

1. The broker keeps a process-local rolling window of per-pool/backend outcomes (success, failure, ready latency, capacity errors).
2. Each schedulable candidate is scored from free slots, success rate, p95 ready latency, and recent capacity errors (weights are configurable).
3. The highest-scoring candidate is reserved first. Scheduler fair-share quotas and active accounting still apply through a pinned reserve of that candidate.
4. Provision or capacity rejection still falls back to remaining eligible backends (re-scored without the failed backend). Pinned requests never take the quality path.

Selection reasons and component gauges are exported as Prometheus metrics (`uecb_quality_*`) and structured `quality_selection` logs.

```yaml
broker:
  qualityAware:
    enabled: true
    window: 15m
    minSamples: 3
    weights:
      freeSlots: 1
      successRate: 1
      latency: 1
      capacityErrors: 1
```

With fewer than `minSamples` observations, success/latency/error components are treated as neutral so free slots remain the primary signal until history accumulates.

## Live Backend Capacity

Optional live-capacity routing uses provider-reported free slots when selecting a backend, instead of relying only on configured `maxRunners` and local scheduler reservations.

When `broker.liveCapacity.enabled` is true:

1. Backends that implement `Capacity()` (SDK adapters, built-in `arc` and `desktop`) or publish `capacity_url` (HTTP dispatch secrets, optional ARC secret feed) are polled out of band on `refreshInterval`.
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
- Platform dimensions use canonical tags `os:linux` / `os:windows` / `os:macos` and `arch:amd64` / `arch:arm64`. Bare aliases such as `windows`, `arm64`, and `x64` expand to those forms before matching. Default backends advertise `os:linux` and `arch:amd64`; Windows or arm64 fleets must advertise the matching tags explicitly.
- The `allocate-runner` action exposes first-class `os` and `arch` inputs that merge into `required_capabilities`.

Full schema, aliases, and examples live in [capabilities.md](capabilities.md).

This keeps scheduling policy isolated in the scheduler registry while making capability eligibility deterministic at the API layer.

## GitHub Targeting

- `github.scope.type=organization` targets an org runner registration surface and can derive per-pool runner groups from `runnerGroupPrefix`.
- `github.scope.type=repository` targets a single repository registration surface and ignores runner groups.

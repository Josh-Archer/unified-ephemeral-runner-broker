# Architecture

`unified-ephemeral-runner-broker` uses an allocate-then-run model.

## Control Plane

- The broker runs in Kubernetes.
- GitHub workflows call the `allocate-runner` action.
- The action exchanges OIDC identity for a broker allocation request.
- The broker selects a backend, reserves capacity, provisions a runner, and returns the label that the heavy job should target.
- External backends read `dispatch_url` and optional `dispatch_token` from their configured `secretRef` and hand off provisioning to a provider-owned controller.

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

## Pools

- `full`: full-capability jobs, ARC only in v1
- `lite`: lightweight jobs, ARC plus enabled external and VM backends

## Default Scheduling

Within a selected pool, backends use `round-robin` across healthy backends with available slots.

Pools can opt into `weighted-round-robin` instead. Backend weights are configured per pool and only affect selection when that scheduler is enabled.

Pools can also enable `fairShare` independently from the backend scheduler. Allocation requests may include `tenant` and `priority_class`; fair-share admission uses active allocation counts to prefer lower-loaded backends and avoid repeatedly favoring a tenant that already has active work. Priority only affects dispatch choice when capacity is available. The broker does not preempt active runners.

## Runtime Backend Admission

Backends may opt into runtime admission controls with `circuitBreaker` and `rateLimit` under `pools[].backends.<name>`.

Admission order is deterministic: static `enabled`/`healthy`, capability filtering, requested timeout filtering, runtime circuit and cold-launch rate limiting, scheduler reservation, then backend provisioning.

Circuit state is in-memory and scoped to a single `pool/backend` within one broker process. Keep broker replicas at `1` for this feature unless scheduler, allocation, and admission state are moved to shared storage together. Timeout-like provision failures, throttling, server errors, explicit `failure_class` completion callbacks, and allocation expiry can open the circuit for the failing backend only. Open backends are skipped for unpinned requests so another eligible backend can serve the allocation; pinned requests fail fast with a circuit-open error.

The background backend-health loop probes open circuits and closes them after the configured recovery threshold. Backends without a probe implementation recover through the same success path once the circuit admits a half-open request.

## Tier-Aware Routing

Tier-aware routing is evaluated after static eligibility, capability filtering, timeout filtering, and runtime backend admission, but before scheduler reservation. It uses the same reduced-pool pattern as capability filtering: blocked backends are removed from the pool snapshot passed to the scheduler, and the configured scheduler remains responsible for final selection.

The allocation path only reads cached tier decisions. Prometheus queries and provider budget, free-tier, or credit calls are refreshed out of band and stored in memory per broker process. This keeps allocation latency independent from billing API latency and avoids making `/healthz` depend on cloud billing availability.

Tier states are normalized to `healthy`, `approaching`, `exceeded`, and `unknown`. Rule actions are `observe-only`, `deprioritize`, and `disable`. `observe-only` never changes routing. `disable` removes an approaching or exceeded backend from scheduler eligibility. Unknown or stale data follows `broker.tierRouting.failureMode`:

- `pass-through-round-robin`: default; ignore tier data and preserve build throughput.
- `block`: fail allocations when tier data is missing, stale, or over policy.
- `fallback-backends`: route through explicit fallback backends, usually `arc` or another self-hosted label.

Pinned backend requests are not silently rerouted when tier policy blocks the pinned backend. The broker returns a deterministic tier-policy error instead.

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

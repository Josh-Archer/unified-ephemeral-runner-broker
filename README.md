# unified-ephemeral-runner-broker

`unified-ephemeral-runner-broker` is a public control plane for allocating one-shot GitHub Actions runners across a unified ephemeral capacity pool.

```mermaid
graph TD
    subgraph "GitHub Actions Workflow"
        Step1[allocate-runner action]
        Step2[Job with dynamic label]
        Step3[finalize-allocation action]
    end

    subgraph "Kubernetes (Broker Namespace)"
        Broker[Broker Service]
        SecretAuth[GitHub App/OIDC Secret]
        SecretBackends[Backend Secrets]
    end

    subgraph "Compute Backends"
        ARC[ARC - Action Runner Controller]
        CB[AWS CodeBuild]
        Lambda[AWS Lambda]
        CR[GCP Cloud Run]
        AF[Azure Functions]
        AVM[Azure VM]
        EC2[AWS EC2]
        GCE[GCP Compute Engine]
    end

    Step1 -- "REST API (Allocation)" --> Broker
    Step3 -- "REST API (Complete)" --> Broker
    Broker -- "K8s API" --> SecretAuth
    Broker -- "K8s API" --> SecretBackends
    
    Broker -- "Native Provisioning" --> ARC
    Broker -- "HTTP Dispatch" --> CB
    Broker -- "HTTP Dispatch" --> Lambda
    Broker -- "HTTP Dispatch" --> CR
    Broker -- "HTTP Dispatch" --> AF
    Broker -- "Static Label" --> AVM
    Broker -- "HTTP Dispatch" --> EC2
    Broker -- "HTTP Dispatch" --> GCE

    ARC -. "Runner Label" .-> Step2
    CB -. "Runner Label" .-> Step2
    Lambda -. "Runner Label" .-> Step2
    CR -. "Runner Label" .-> Step2
    AF -. "Runner Label" .-> Step2
    AVM -. "Runner Label" .-> Step2
    EC2 -. "Runner Label" .-> Step2
    GCE -. "Runner Label" .-> Step2
    Step2 --> Step3
```

V1 models these backends:

- `arc`
- `codebuild`
- `lambda`
- `cloud-run`
- `azure-functions`
- `azure-vm`
- `ec2`
- `gce`

The public repo ships ARC provisioning, a static-label VM adapter for existing Azure VM runners, and generic secret-backed external launcher dispatch for `codebuild`, `lambda`, `cloud-run`, `azure-functions`, `ec2`, and `gce`. Each enabled external backend must point at a real launcher controller through a Kubernetes secret in the broker namespace.

It is intentionally split into two capability pools:

- `full`: ARC only in v1
- `lite`: ARC plus the supported external and VM backends

Default multi-backend scheduling is `round-robin`.

Built-in schedulers:

- `round-robin`
- `weighted-round-robin`

## What This Repo Ships

- A Kubernetes broker service with a small REST API
- Reusable GitHub Actions, `allocate-runner` and `finalize-allocation`
- A public backend adapter SDK with a conformance test harness
- An OCI Helm chart for installation
- Generic provider runner images for `launcher`, `lambda`, `cloud-run`, and `azure-functions`
- A generic Kustomize-facing GitOps consumption path
- Generic infrastructure examples for AWS, GCP, and Azure

## What This Repo Does Not Ship

- Homelab-specific manifests, overlays, or secret-store implementations
- Inline credentials or cloud secrets
- Private runner labels, cluster names, or internal network details
- A public release workflow that can touch self-hosted runners

## High-Level Flow

```mermaid
sequenceDiagram
    participant GH as GitHub Workflow
    participant AR as allocate-runner action
    participant B as Broker
    participant BE as Backend (e.g., ARC/Lambda)
    participant R as Ephemeral Runner
    participant FA as finalize-allocation action

    GH->>AR: Run action
    AR->>B: POST /v1/allocations (OIDC Token)
    B->>B: Validate OIDC & Auth
    B->>B: Capability Filtering
    B->>B: Scheduler Selection
    B->>BE: Dispatch Provisioning
    BE-->>B: Admission OK (Label)
    B-->>AR: Allocation Result (Label)
    AR-->>GH: Set outputs (runner_label, allocation_id)
    
    GH->>R: Run Job on label
    R->>R: Execute Job
    R->>GH: Job Complete
    R->>R: Self-Terminate

    GH->>FA: Cleanup job (if: always)
    FA->>B: POST /v1/allocations/{id}/complete (OIDC Token)
    B->>B: Mark terminal and release capacity
    B-->>FA: Terminal allocation status
```

1. A lightweight workflow step calls `allocate-runner`.
2. The broker selects an eligible backend from the chosen pool.
3. The broker sends the request to the selected backend integration. `codebuild`, `lambda`, `cloud-run`, `azure-functions`, `ec2`, and `gce` dispatch through a secret-backed HTTP controller contract. `azure-vm` returns a configured existing runner label.
4. `job_timeout` is accepted as duration strings like `15m`, with numeric nanoseconds still accepted for backward compatibility.
5. The heavy workflow job runs on that exact label.
6. The runner executes one job and exits.
7. A cleanup job (or step) calls `finalize-allocation` so the broker releases scheduler capacity immediately. If the callback never runs, orphan cleanup remains the fallback.

### Allocation API

Machine-readable reference: [docs/openapi.yaml](docs/openapi.yaml)
(`POST`/`GET` `/v1/allocations`, complete, cancel; OIDC auth and correlation headers).

All allocation endpoints require a GitHub OIDC bearer token unless
`allowUnauthenticated` is enabled.

| Operation | Method and path | Success response |
| --- | --- | --- |
| Create | `POST /v1/allocations` | `201 Created` when a runner is ready, or `202 Accepted` with a `Retry-After` header when queued |
| Status | `GET /v1/allocations/{id}` | `200 OK` with the current allocation |
| Cancel | `POST /v1/allocations/{id}/cancel` | `200 OK` with the canceled allocation |
| Complete | `POST /v1/allocations/{id}/complete` | `200 OK` with the terminal allocation |

Create requests include a pool and job timeout:

```json
{
  "pool": "full",
  "job_timeout": "15m"
}
```

Every successful operation returns the allocation status. A ready allocation
has this core response shape:

```json
{
  "allocation_id": "alloc-123",
  "correlation_id": "request-123",
  "pool": "full",
  "selected_backend": "arc",
  "runner_label": "uecb-alloc-123",
  "expires_at": "2026-07-17T12:00:00Z",
  "state": "ready"
}
```

Queued allocations use `state: pending`, may omit the runner label until they
are ready, and include `retry_after` in the response body.

Completion callbacks accept these payload forms:

- `{ "state": "completed" }` (default state)
- `{ "state": "completed" | "failed" | "canceled", "reason": "...", "error": "..." }`
- `{ "state": "expired" }`
- `{ "state": "quarantined" }`

Duplicate callbacks for the same terminal state are idempotent and do not
re-release scheduler capacity.

### Workflow finalization pattern

Without an explicit complete callback, active allocations keep consuming
scheduler capacity until orphan expiry. Use `finalize-allocation` in a cleanup
job that always runs after the runner job, including failure and cancellation.

GitHub `job.result` values map deterministically to broker terminal states:

| GitHub `job.result` / action `result` | Broker `state` |
| --- | --- |
| `success` | `completed` |
| `failure` | `failed` |
| `cancelled` / `canceled` | `canceled` |
| `skipped` | `canceled` (capacity still released) |

You can also pass broker states directly (`completed`, `failed`, `canceled`)
via `result` or the explicit `state` input (`state` wins when both are set).

```yaml
permissions:
  id-token: write   # required for broker OIDC unless allow_unauthenticated
  contents: read

jobs:
  allocate:
    runs-on: ubuntu-latest
    outputs:
      allocation_id: ${{ steps.alloc.outputs.allocation_id }}
      runner_label: ${{ steps.alloc.outputs.runner_label }}
    steps:
      - id: alloc
        uses: Josh-Archer/unified-ephemeral-runner-broker/actions/allocate-runner@main
        with:
          broker_url: https://broker.example.com
          pool: lite
          job_timeout: 15m

  work:
    needs: allocate
    runs-on: ${{ needs.allocate.outputs.runner_label }}
    steps:
      - run: echo "heavy job on ephemeral runner"

  # Always finalize so success, failure, and cancellation release capacity.
  finalize:
    needs: [allocate, work]
    if: ${{ always() && needs.allocate.result == 'success' }}
    runs-on: ubuntu-latest
    steps:
      - uses: Josh-Archer/unified-ephemeral-runner-broker/actions/finalize-allocation@main
        with:
          broker_url: https://broker.example.com
          allocation_id: ${{ needs.allocate.outputs.allocation_id }}
          result: ${{ needs.work.result }}
```

Notes:

- Grant `id-token: write` on the finalize job (or workflow) so OIDC matches
  `allocate-runner`. Local/dev brokers may set `allow_unauthenticated: true`.
- Transient HTTP failures (408/429/5xx and network errors) retry with bounded
  exponential backoff (`max_retries`, `initial_backoff_seconds`,
  `max_backoff_seconds`). Permanent failures (400/401/403/404) fail the step
  with an actionable error.
- Duplicate finalize runs for the same terminal state are safe; the broker
  treats them as idempotent and does not double-release capacity.
- If the finalize job cannot run (for example the workflow was deleted mid-run),
  orphan cleanup still reclaims capacity after expiry—see below.

### Orphan cleanup and quarantine

Stale active allocations are reclaimed during periodic sweep.

```yaml
broker:
  orphanCleanup:
    enabled: false
    quarantineTTL: 15m
```

- `enabled: false` (default): active stale allocations move directly to `expired`.
- `enabled: true`: active stale allocations move to `quarantined` for `quarantineTTL` (or immediately when `0`), then to `expired`.

### Durable State Store

The broker keeps allocation state in memory by default. Environments that need
restart recovery can opt into a file-backed state store on a persistent volume.

```yaml
broker:
  stateStore:
    type: file
    path: /var/lib/uecb/allocations.json
```

On startup, active `reserved`, `ready`, and `warm` allocations are rehydrated
into scheduler accounting so a restarted broker does not over-admit capacity.

### Queued Admission

Queued admission is disabled by default. When enabled, retryable allocation
failures such as open backend circuits or transient provider dispatch failures
are stored as `pending` allocations. Capacity exhaustion and cold-launch rate
limits fail fast instead of entering the queue: rate-limited backends are
skipped in favor of another eligible backend, and the broker returns a direct
error when no backend can admit the request.

```yaml
broker:
  queue:
    enabled: true
    retryAfter: 30s
    maxAttempts: 3
```

`POST /v1/allocations` returns `202 Accepted` with `state: pending` and a
`Retry-After` header for queued allocations. The `allocate-runner` action polls
the allocation until it becomes `ready` or `queue_wait_timeout` expires.

## Project Layout

- `cmd/broker`: broker entrypoint
- `internal/`: broker, scheduler, backend, GitHub, and config packages
- `docker/azure-functions`: published Azure Functions controller and runner container
- `docker/lambda`: published AWS Lambda runner container handler
- `charts/unified-ephemeral-runner-broker`: Helm chart
- `actions/allocate-runner`: public allocate workflow integration surface
- `actions/finalize-allocation`: public complete/finalize workflow integration surface
- `examples/`: generic Terraform and GitOps consumption examples
- `docs/`: architecture notes, security boundary, and [OpenAPI](docs/openapi.yaml) for the allocation API
- `observability/`: reusable Prometheus alert rules and Grafana dashboard artifacts
- `pkg/adapter`: public backend adapter SDK and conformance test helpers

## Public CI and Private Release Boundary

This repository is designed for a split trust model:

- Public CI runs on GitHub-hosted runners only
- A separate private release repository owns the authoritative ARC-backed publish lane
- Public forks and PRs must never reach self-hosted runners or publish credentials

See [docs/architecture.md](docs/architecture.md) and [docs/security-boundary.md](docs/security-boundary.md) for the full model.

## Quick Start

1. Install the Helm chart with external backends disabled.
2. Create the GitHub auth secret and any enabled backend secrets in the same namespace as the broker.
   The broker validates referenced `secretRef` objects via the Kubernetes API and stays unready until they exist.
   External backend secrets should provide:
   `dispatch_url`: the controller endpoint the broker should call for provision.
   `health_url`: health endpoint used by circuit-breaker recovery probes when the backend enables `circuitBreaker`.
   `dispatch_token`: optional bearer token sent to dispatch, health, and cleanup endpoints.
   `cleanup_url` (optional): controller endpoint the broker POSTs on cancel/expire/release so the provider can tear down runners. When omitted, cleanup is skipped (capacity is still released); when set, launchers should treat cleanup as idempotent (2xx and 404 both OK).
3. Point the `allocate-runner` action at the broker URL. The broker accepts `job_timeout` in the same duration-string format used by the action, for example `15m`.
4. Add a cleanup job that always calls `finalize-allocation` with the allocation ID and the runner job result so capacity is released immediately (see [Workflow finalization pattern](#workflow-finalization-pattern)).
5. Start with the `full` pool or ARC-only `lite` pool. Only enable an external backend after you have supplied a real launcher integration for that platform and the matching `secretRef`.

## Broker OIDC Authentication

When `broker.allowUnauthenticated` is false, allocation and completion requests
must use `Authorization: Bearer <token>` with a GitHub Actions OIDC token. The
broker discovers GitHub's JWKS from
`https://token.actions.githubusercontent.com/.well-known/openid-configuration`,
caches signing keys, and accepts only RS256 tokens signed by that issuer.

The token must include:

- `iss`: `https://token.actions.githubusercontent.com`
- `aud`: the configured `broker.api.oidcAudience` value, `uecb-broker` by default
- `sub`: a non-empty GitHub Actions subject such as `repo:OWNER/REPO:ref:...`
- current `exp` and, when present, `nbf` claims

Optional GitHub Actions claims used for authorization and ownership:

- `repository` (`owner/repo`)
- `repository_owner`
- `workflow_ref` / `job_workflow_ref` (retained for future policy)

### Authorization policy (`broker.api.oidcPolicy`)

Authentication alone is not multi-tenant isolation. Configure an allowlist when
the broker is reachable beyond a single trusted tenant:

```yaml
broker:
  api:
    oidcAudience: uecb-broker
    oidcPolicy:
      allowedRepositories:
        - my-org/my-repo
        - my-org/other-*
      allowedOwners:
        - my-org
```

- Empty `allowedRepositories` and `allowedOwners` (default): any **authenticated**
  identity may allocate (backward compatible for single-tenant trusted deploys).
- Non-empty lists: the caller's repository or owner must match at least one entry
  (union). Patterns support a trailing `/*` (or `*`) wildcard.
- Policy denial returns HTTP 403.

### Allocation ownership (IDOR protection)

On allocate, the broker stores the OIDC principal (`subject`, `repository`,
`owner`) on the allocation. Get / cancel / complete require the same `sub` (or
the same `repository` when both sides present it). Cross-tenant access returns
HTTP 403. When `allowUnauthenticated` is true and the request has no bearer
token, ownership checks are skipped so local/test modes keep working.

Set `broker.allowUnauthenticated: true` only behind a separate trusted network
or gateway boundary. For multi-tenant or internet-exposed brokers, keep
authentication required and set a non-empty `oidcPolicy`.

## Azure Functions Launcher

The published Azure Functions launcher image lives in `docker/azure-functions` and is designed for a Linux custom-container Function App.

- The HTTP dispatch endpoint returns quickly and enqueues the allocation.
- The broker waits up to 90 seconds for the Azure Functions dispatch controller so a cold-started Function App can return its admission response.
- A queue-triggered function execution runs the ephemeral GitHub runner inside the same Function App container.
- Use a hosting plan that supports long-running non-HTTP executions, such as Premium or Dedicated with `alwaysOn` enabled. The HTTP request still needs to finish quickly even when the runner job itself can run longer.

## Provider Runner Images

The private release lane should publish these OCI images from one immutable source ref:

- `broker`: Kubernetes broker API
- `launcher`: generic one-shot runner launcher
- `cloud-run`: Cloud Run Job runner image built from the generic launcher
- `lambda`: AWS Lambda container runner image with the Lambda runtime handler
- `azure-functions`: Azure Functions dispatch controller and runner image

Environment-specific repositories can mirror images when a provider requires it. For example, AWS Lambda requires the function image to live in ECR, so a private consumer may mirror the published `lambda` image into its own ECR repository while still treating this repo as the image source of truth.

### Image provenance and SBOM verification

The public `Publish Runtime Images` workflow binds SLSA provenance and SPDX SBOM
attestations to each pushed image digest without changing the Lambda Docker v2
single-manifest format. After a release publish, consumers can verify:

```bash
REPO=Josh-Archer/unified-ephemeral-runner-broker

gh attestation verify oci://ghcr.io/josh-archer/uecb-lambda:TAG -R "${REPO}"
gh attestation verify oci://ghcr.io/josh-archer/uecb-lambda:TAG -R "${REPO}" \
  --predicate-type https://spdx.dev/Document/v2.3
```

Copy-paste commands for all three runtime images (and Lambda manifest checks)
are in [docs/image-attestations.md](docs/image-attestations.md).

## GitHub Scope

`github.scope.type` supports:

- `organization`
- `repository`

Repository scope requires `github.scope.owner` and `github.scope.repository`. Organization scope requires `github.scope.organization`.

## Scheduler Configuration

```mermaid
graph LR
    Req[Allocation Request] --> Cap[Capability Filter]
    Cap -- "Eligible Backends" --> FS{Fair Share?}
    FS -- "Yes" --> FSL[Fair Share Logic]
    FSL --> Sched[Scheduler RR/WRR]
    FS -- "No" --> Sched
    Sched --> BE[Selected Backend]
```

Each pool selects its scheduler with `pools[].scheduler`.

- `round-robin` is the default and ignores backend weights.
- `weighted-round-robin` uses `pools[].backends.<name>.weight`.
- Omitted or non-positive weights are treated as `1`.

Example:

```yaml
pools:
  - name: lite
    scheduler: weighted-round-robin
    backends:
      arc:
        enabled: true
        maxRunners: 2
        weight: 3
      codebuild:
        enabled: true
        maxRunners: 3
        weight: 1
```

`lambda` remains backward-compatible with older pinned requests: if the real `lambda` backend is disabled for a pool but `codebuild` is enabled, the broker treats a pinned `lambda` request as `codebuild`.

Rollback is just a config change: set `scheduler` back to `round-robin` for the pool and redeploy. Leaving `weight` values in place is safe because the default scheduler ignores them.

## Tier-Aware Routing

Tier-aware routing can keep cloud backends from consuming paid capacity once provider free tiers, budgets, or credits are surpassed or close to exhausted. It is disabled by default and reads cached tier decisions during allocation; Prometheus and provider API calls happen outside the allocation path so runner assignment is not delayed by billing APIs.

```yaml
broker:
  tierRouting:
    enabled: true
    refreshInterval: 5m
    staleAfter: 15m
    failureMode: pass-through-round-robin
    fallbackBackends:
      - arc
    prometheus:
      url: https://prometheus.example.invalid
      timeout: 2s
      secretRef: uecb-prometheus
    providers:
      aws-main:
        provider: aws
        mode: free-tier
        secretRef: uecb-aws-billing
    providerRules:
      - name: aws-free-tier
        providerRef: aws-main
        hardLimitRatio: 0.95
        action: disable
pools:
  - name: lite
    backends:
      codebuild:
        tierRules:
          - name: codebuild-free-tier
            providerRef: aws-main
            usageQuery: uecb:backend_usage:ratio{backend="codebuild"}
            burnRateQuery: uecb:backend_usage_burn_rate{backend="codebuild"}
            softLimitRatio: 0.8
            hardLimitRatio: 0.95
            action: observe-only
```

`providerRules` apply one provider budget, free-tier, or credit decision to every matching backend in every pool. The broker maps `aws` to CodeBuild, Lambda, and EC2; `gcp` to Cloud Run and GCE; and `azure` to Azure Functions and Azure VM. Use `backends` on a provider rule when only a subset should be affected.

Supported fallback modes:

- `pass-through-round-robin`: default; unknown or stale tier data does not block builds.
- `block`: fail allocations when tier data is unknown, stale, or over policy.
- `fallback-backends`: route to configured fallback backends such as `arc`.

Use `observe-only` first, then move a provider or backend rule to `deprioritize` or `disable` after validating Prometheus queries and provider snapshots. Pinned requests fail clearly when the requested backend is tier-blocked.

## Runtime Backend Admission

Backends can opt into circuit breaking and cold-launch rate limiting. This is separate from static `enabled` and `healthy`: operator config is still the hard source of truth, while circuit state is learned at runtime per `pool/backend`.

The broker opens a circuit after configured timeout-like failures, transport errors, throttling, server errors, allocation expiry, or completion callbacks with `failure_class: wait-timeout`. Open backends are skipped for unpinned requests so another eligible backend can run the job; pinned requests fail fast.

```yaml
pools:
  - name: lite
    backends:
      azure-vm:
        enabled: true
        healthy: true
        maxRunners: 1
        runnerLabel: replace-with-private-azure-vm-runner-label
        circuitBreaker:
          enabled: true
          failureThreshold: 1
          evaluationWindow: 5m
          openDuration: 2m
          probeInterval: 30s
          probeTimeout: 10s
          recoverySuccessThreshold: 1
```

`rateLimit` applies only to cold provisioning attempts. Warm runner reuse is
not rate limited, and each cold launch attempt consumes a permit even if the
allocation is later canceled or fails downstream. When a cold backend is
rate-limited, the broker tries another eligible backend; if none can run the
allocation, the request fails fast with a rate-limit fallback exhaustion error
instead of waiting in the queue.

Unlike circuit-open or tier-policy rejections, rate limiting can still redirect
a pinned request to another eligible backend. Pinning remains a preference for
the first cold-launch attempt, not a guarantee that a rate-limited backend will
be retried in place.

## Warm Capacity

Backend pools can maintain pre-initialized warm runners to reduce cold-start latency for external backends.

Warm behavior is configured per backend:

- `warmMin`: minimum number of warm allocations to keep for the backend.
- `warmMax`: maximum number of warm allocations to keep for the backend.
- `warmTTL`: how long a warm allocation stays idle before recycle.

Warm allocations are created only for external backends that are enabled and healthy. `arc` and `azure-vm` are not included because they are not external dispatchers and are expected to launch quickly.

```yaml
pools:
  - name: lite
    backends:
      codebuild:
        enabled: true
        maxRunners: 3
        weight: 1
        warmMin: 1
        warmMax: 2
        warmTTL: 10m
        secretRef: uecb-codebuild
```

When warm capacity exists:

- the broker prefers an available warm slot before provisioning cold;
- idle warm runners are recycled on TTL expiry or capacity policy changes;
- warm capacity may consume active runner quota while in warm state.

If a warm slot is unavailable or expired, the broker falls back to cold launch as before.

Use warm pools where external cold-start latency dominates.

### Priority And Fair-Share Scheduling

Pools can opt into tenant-aware dispatch with `fairShare.enabled`. Fair-share **composes** with the pool's backend scheduler (`round-robin` or `weighted-round-robin`):

1. **Tenant admission** — enforce optional per-tenant `quotas`, track active usage by tenant and priority class.
2. **Fair-share ranking** — prefer backends with lower active load and lower active usage for the requesting tenant; higher priority classes reduce the tenant penalty when capacity is available.
3. **Backend pick** — among backends with equal fair-share scores and free capacity, select using the pool scheduler. With `weighted-round-robin`, backend `weight` values still shape selection; with `round-robin`, each eligible backend gets one slot.

Recommended configuration (matches the multi-backend pack):

```yaml
pools:
  - name: lite
    scheduler: weighted-round-robin   # weights apply when fair-share scores tie
    fairShare:
      enabled: true
      priorityClasses:
        normal: 1
        high: 2
      quotas:                         # optional hard caps on concurrent active allocations
        noisy-team: 4
        release: 20
    backends:
      arc:
        enabled: true
        maxRunners: 4
        weight: 3
      codebuild:
        enabled: true
        maxRunners: 4
        weight: 2
```

Allocation requests may include:

- `tenant`: queue, team, or workflow owner used for fair-share accounting
- `priority_class`: priority class such as `normal` or `high`

Fair-share does not preempt active runners. Allocations without a tenant use the `default` tenant bucket. Optional `fairShare.quotas` reject new reservations for a tenant once its concurrent active count reaches the configured limit (other tenants are unaffected).

`usageWindow` and `starvationAfter` are reserved config keys and are not applied yet.

#### Config surface (single path)

| Knob | Role |
|------|------|
| `pools[].fairShare.enabled: true` | **Recommended** enable path for tenant/priority admission and ranking |
| `pools[].scheduler: weighted-round-robin` / `round-robin` | Backend selection among equal fair-share scores (weights only for WRR) |
| `pools[].scheduler: priority-fair-share` | Standalone fair-share backend pick (no weight expansion); same shared scheduler instance as `fairShare.enabled` |

Prefer `fairShare.enabled` plus `weighted-round-robin` or `round-robin`. Setting `scheduler: priority-fair-share` alone is equivalent to fair-share ranking without WRR weight expansion.

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    tenant: release
    priority_class: high
```

## Capability-Aware Routing

Jobs can further narrow backend selection with optional capability filters on the allocation request:

- `required_capabilities`: every listed tag must be advertised by the backend
- `excluded_capabilities`: none of the listed tags may be advertised by the backend
- Capability matching is case-insensitive and uses normalized string tags
- If neither field is set, broker behavior is unchanged

Capability filtering happens before the pool scheduler runs. The scheduler registry stays unchanged and only sees the eligible backends that remain after filtering.

Backend capability tags are configured per pool:

```yaml
pools:
  - name: lite
    scheduler: weighted-round-robin
    backends:
      arc:
        enabled: true
        maxRunners: 2
        capabilities:
          - cluster-local
          - docker
          - region:local
      codebuild:
        enabled: true
        maxRunners: 3
        capabilities:
          - docker
          - region:aws-us-east-1
      azure-vm:
        enabled: true
        maxRunners: 1
        runnerLabel: replace-with-private-azure-vm-runner-label
        capabilities:
          - docker
          - privileged
          - vm
          - cloud:azure
      cloud-run:
        enabled: true
        maxRunners: 2
        capabilities:
          - region:gcp-us-central1
```

Examples:

- Cluster-local routing:

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    required_capabilities: cluster-local
```

- Docker-capable routing:

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    required_capabilities: docker
```

This excludes serverless-only backends such as `lambda`, `cloud-run`, and `azure-functions` unless an environment explicitly advertises Docker support for those backends.

- GPU routing:

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    required_capabilities: gpu
```

This requires at least one backend in the selected pool to advertise `gpu`, for example an ARC template or cloud backend dedicated to GPU jobs.

- Region-specific routing:

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    required_capabilities: region:aws-us-east-1
    excluded_capabilities: cluster-local
```

If no backend matches the requested capability filters, the broker rejects the allocation request before scheduling.

## Observability

The broker exposes Prometheus metrics on `/metrics` and uses a shared `X-Correlation-ID` model across HTTP responses, allocation responses, and structured lifecycle logs. The reusable pack includes:

- `observability/grafana-dashboard.json`
- `observability/prometheus-rules.yaml`
- [docs/observability.md](docs/observability.md)

The pack observes allocation and backend lifecycle events without changing scheduling behavior.

## License

Apache-2.0

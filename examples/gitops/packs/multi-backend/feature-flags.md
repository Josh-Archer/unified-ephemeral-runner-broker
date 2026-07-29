# Feature Flags — multi-backend Pack

## Backend Enable/Disable

Each external backend can be disabled independently without removing its configuration or secret.

### Disable a single cloud backend

```yaml
pools:
  - name: lite
    backends:
      cloud-run:
        enabled: false   # all allocations that would have gone to Cloud Run route to other backends
```

### Disable all external backends (ARC-only fallback)

```yaml
pools:
  - name: lite
    backends:
      codebuild:
        enabled: false
      lambda:
        enabled: false
      cloud-run:
        enabled: false
```

This makes the lite pool behave like the arc-only pack without changing the base configuration.

## Backend Health Override

Use `healthy: false` as a manual circuit breaker:

```yaml
backends:
  lambda:
    enabled: true
    healthy: false   # scheduler skips this backend; re-enable when recovered
```

## Fair-Share Scheduling

Fair-share **composes** with the pool scheduler. With this pack's default (`scheduler: weighted-round-robin` + `fairShare.enabled: true`):

1. Tenant quotas and fair-share scores rank backends.
2. When scores tie, backend `weight` values drive WRR selection.

### Disable fair-share (pure weighted-round-robin only)

```yaml
pools:
  - name: lite
    fairShare:
      enabled: false
```

Backend weights continue to apply via `weighted-round-robin`.

### Adjust priority class / lane weights

Higher weight means a higher lane: better fair-share ranking and protection from lower-lane soft reserves. Use named lanes (for example `deploy` > `pr` > `smoke`) or the built-in `high` / `normal` aliases:

```yaml
pools:
  - name: lite
    fairShare:
      enabled: true
      priorityClasses:
        smoke: 1
        pr: 2
        deploy: 3
        normal: 1
        high: 2
```

Allocation requests must include matching `priority_class` values (for example `priority_class: deploy`).

### Soft-reserve high-risk lanes

Soft reserves hold per-backend slots that lower-weight classes cannot consume, so concurrent PR/smoke traffic does not starve deploy under shared `maxRunners`:

```yaml
pools:
  - name: lite
    fairShare:
      enabled: true
      priorityClasses:
        smoke: 1
        pr: 2
        deploy: 3
      softReserves:
        deploy: 1
        pr: 1
```

With `maxRunners: 4` and the reserves above, smoke sees `effectiveMax = 2` while deploy still sees the full 4. Soft reserves require `fairShare.enabled: true` and do not preempt running jobs.

### Cap concurrent allocations per tenant

```yaml
pools:
  - name: lite
    fairShare:
      enabled: true
      quotas:
        noisy-team: 2
        release: 10
```

Once a tenant reaches its quota of active allocations, further reserves for that tenant fail with a quota error until capacity is released. Other tenants are unaffected. Use the allocate-runner `tenant` input for repo or workflow labels.

### Send tenant and priority in a workflow

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    tenant: release-pipeline
    priority_class: deploy
```

Allocations without a `tenant` value use the `default` tenant bucket. The broker does not preempt active runners. Soft reserves and priority weights shape new admissions only.

## Scheduler Selection

To fall back from `weighted-round-robin` to `round-robin` for a pool (ignoring backend weights, still composed with fair-share when enabled):

```yaml
pools:
  - name: lite
    scheduler: round-robin
```

Weights are preserved in config and take effect again when `weighted-round-robin` is restored. Prefer this dual-knob shape (`fairShare.enabled` + `scheduler`) over `scheduler: priority-fair-share`, which is standalone fair-share without weight expansion.

## Orphan Cleanup

```yaml
broker:
  orphanCleanup:
    enabled: false
    quarantineTTL: 15m
```

Enable quarantine to observe stale allocations before expiry:

```yaml
broker:
  orphanCleanup:
    enabled: true
    quarantineTTL: 10m
```

## Backend Pinning

Pin an allocation to a specific backend, overriding scheduler selection:

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    backend: lambda
```

Pinned requests still honor the `enabled` and `healthy` flags, and capability filters apply. If the pinned backend is disabled or its capabilities do not match, the broker returns a clear rejection.

## Capability-Based Routing

### Route Docker jobs away from serverless backends

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    required_capabilities: docker
```

This routes to ARC or CodeBuild only. Lambda and Cloud Run do not advertise `docker` in this pack.

### Route jobs to a specific cloud region

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    required_capabilities: region:aws-us-east-1
    excluded_capabilities: cluster-local
```

This routes to CodeBuild or Lambda only, excluding ARC.

### Route jobs to GCP only

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    required_capabilities: region:gcp-us-central1
```

This routes to Cloud Run only.

## Warm Capacity Tuning

Adjust warm capacity per backend to balance cost and cold-start latency:

```yaml
backends:
  codebuild:
    warmMin: 0    # disable warm for cost savings
    warmMax: 0
  lambda:
    warmMin: 2    # keep more warm Lambda slots
    warmMax: 4
    warmTTL: 5m
```

Set `warmMin: 0` and `warmMax: 0` to disable warm capacity for a backend without disabling the backend itself.

## Staged Rollout

1. Apply config change to `overlays/staging` via pull request.
2. Validate all backends using `validation.md`.
3. Open pull request to `overlays/production`.
4. Merge after review. ArgoCD or Flux promotes.

## Rollback

Revert the pull request or open a new one restoring the previous config. Never use `kubectl edit` on live ConfigMaps — out-of-band edits are overwritten by the next GitOps sync.

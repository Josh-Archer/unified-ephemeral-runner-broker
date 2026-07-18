# Feature Flags — arc-plus-codebuild Pack

## Backend Enable/Disable

### Disable CodeBuild without removing config

Set `enabled: false` to stop new allocations to CodeBuild. In-flight runners continue to completion.

```yaml
pools:
  - name: lite
    backends:
      codebuild:
        enabled: false
```

All lite pool allocations route to ARC while CodeBuild is disabled. Re-enable by setting `enabled: true` and opening a pull request.

### Disable ARC (drain in-cluster capacity)

```yaml
pools:
  - name: lite
    backends:
      arc:
        enabled: false
```

With ARC disabled and CodeBuild enabled, all lite pool allocations route to CodeBuild. Use this during cluster maintenance windows.

## Backend Health Override

Use `healthy: false` as a manual circuit breaker without removing the backend:

```yaml
backends:
  codebuild:
    enabled: true
    healthy: false   # scheduler skips this backend
```

## Warm Capacity Tuning

Adjust warm capacity without disabling CodeBuild:

```yaml
backends:
  codebuild:
    warmMin: 0    # disable warm capacity
    warmMax: 0
```

Or increase warm capacity for burst absorption:

```yaml
backends:
  codebuild:
    warmMin: 2
    warmMax: 4
    warmTTL: 12m
```

Changes to `warmMin`, `warmMax`, and `warmTTL` take effect after the broker restarts with the updated config.

## Scheduler Selection

This pack uses `weighted-round-robin` for the lite pool. To fall back to plain `round-robin` (ignoring weights):

```yaml
pools:
  - name: lite
    scheduler: round-robin
```

Leaving the `weight` values in place is safe because `round-robin` ignores them. Switch back to `weighted-round-robin` at any time without removing weights.

## Orphan Cleanup

```yaml
broker:
  orphanCleanup:
    enabled: false      # default
    quarantineTTL: 15m
```

Enable quarantine to hold stale allocations before expiry:

```yaml
broker:
  orphanCleanup:
    enabled: true
    quarantineTTL: 10m
```

## Backend Pinning

Pin an allocation to a specific backend:

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    backend: codebuild
```

Useful for testing CodeBuild capacity specifically, or routing specific job types to a known backend. Pinned requests still honor the `enabled` and `healthy` flags.

## Capability-Based Routing

Route Docker-capable jobs to ARC or CodeBuild only (excluding serverless backends):

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    required_capabilities: docker
```

ARC and CodeBuild both advertise `docker` in this pack's config. Lambda and other serverless backends (if added later) would be excluded by this filter.

Route jobs specifically to in-cluster ARC:

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    required_capabilities: cluster-local
```

## Staged Rollout

1. Apply config change to `overlays/staging` via pull request.
2. Validate using `validation.md` against staging.
3. Open pull request to `overlays/production`.
4. Merge after review. ArgoCD or Flux promotes.

## Rollback

Revert the pull request or open a new one restoring the previous config. Never use `kubectl edit` on live ConfigMaps — out-of-band edits are overwritten by the next GitOps sync.

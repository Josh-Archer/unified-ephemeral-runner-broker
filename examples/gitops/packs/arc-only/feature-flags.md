# Feature Flags — arc-only Pack

This document describes how to use broker configuration flags for staged rollout, rollback, and backend pinning in the arc-only pack.

## Backend Enable/Disable

Each backend in a pool has an `enabled` flag. Set it to `false` to stop new allocations to that backend without removing its configuration.

In the arc-only pack, ARC is the only backend. To drain ARC capacity without destroying the deployment:

```yaml
pools:
  - name: full
    backends:
      arc:
        enabled: false   # no new allocations; in-flight runners complete normally
```

Apply this as a ConfigMap patch in your overlay, then re-enable once the drain is complete.

## Backend Health Override

The `healthy` flag is a manual circuit breaker. Set it to `false` to prevent new allocations without disabling the backend permanently.

```yaml
backends:
  arc:
    enabled: true
    healthy: false   # scheduler skips this backend; re-enable when recovered
```

## Orphan Cleanup

Stale allocations that have not received a completion callback are cleaned up during periodic sweeps.

```yaml
broker:
  orphanCleanup:
    enabled: false      # default: stale active allocations move directly to expired
    quarantineTTL: 15m  # only used when enabled: true
```

Enable quarantine when you want to observe stale allocations before they expire:

```yaml
broker:
  orphanCleanup:
    enabled: true
    quarantineTTL: 10m
```

## Scheduler Selection

The arc-only pack uses `round-robin` by default. With a single ARC backend per pool, the scheduler choice has no practical effect, but it is ready to be promoted to `weighted-round-robin` when you add a second backend.

To switch a pool's scheduler, update the overlay patch:

```yaml
pools:
  - name: lite
    scheduler: weighted-round-robin
```

## Backend Pinning

Clients can request a specific backend by including `backend` in the allocation request (action input and JSON field). This overrides scheduler selection for that request.

```yaml
- uses: ./actions/allocate-runner
  with:
    broker_url: https://broker.example.com
    pool: lite
    backend: arc
```

Pinned requests still honor capability filters and the `enabled`/`healthy` flags. If the pinned backend is disabled or unhealthy, the broker returns a clear rejection.

## Staged Rollout

Use the staging and production overlays to test config changes before promoting to production.

1. Apply the config change to `overlays/staging` in your private overlay repo.
2. Validate using the steps in `validation.md`.
3. Open a pull request to `overlays/production` with the same change.
4. Merge after review. ArgoCD or Flux promotes the change.

## Rollback

Revert the pull request that introduced the change, or open a new pull request that restores the previous config values. GitOps reconciliation restores the previous state without direct cluster access.

Do not use `kubectl edit` or `kubectl patch` on live ConfigMaps. Out-of-band edits break the GitOps contract and will be overwritten by the next ArgoCD or Flux sync.

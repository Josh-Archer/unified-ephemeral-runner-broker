# arc-only Reference Pack

This pack deploys the broker in a pure ARC topology. No external cloud backends are enabled. All runners are provisioned in-cluster by the Actions Runner Controller.

## When to Use

- Your entire CI workload fits within cluster capacity.
- You do not yet need overflow capacity from cloud providers.
- You want the simplest possible broker configuration before evaluating hybrid backends.

## Topology

```
GitHub Actions Workflow
        │
        ▼
allocate-runner action
        │  OIDC / REST
        ▼
 ┌─────────────┐
 │   Broker    │  (Kubernetes, arc-systems namespace)
 └─────────────┘
        │
        │  ARC native provisioning
        ▼
 ┌─────────────┐
 │     ARC     │  (Actions Runner Controller)
 └─────────────┘
        │
        ▼
 Ephemeral runner pod (one job, then exits)
```

## Pools

| Pool | Backend | MaxRunners | Scheduler |
|------|---------|-----------|-----------|
| `full` | arc (arc-full template) | 4 | round-robin |
| `lite` | arc (arc-lite template) | 2 | round-robin |

## Files in This Pack

- `base/` — Kustomize base (extends the shared upstream base, patches ConfigMap for ARC-only config)
- `overlays/staging/` — staging overlay (1 broker replica)
- `overlays/production/` — production overlay (2 broker replicas)
- `argocd/application.yaml` — ArgoCD Application manifest
- `secrets.md` — required Kubernetes Secrets and their keys
- `rollout.md` — initial install, update, and rollback steps
- `validation.md` — post-install health checks and optional CronJob
- `feature-flags.md` — staged rollout, backend enable/disable, pinning, and rollback

# multi-backend Reference Pack

This pack deploys the broker in a multi-backend hybrid topology with ARC as the in-cluster backend and AWS CodeBuild, AWS Lambda, and GCP Cloud Run as external backends. The lite pool enables **composed** scheduling: `fairShare` ranks by tenant/priority, then `weighted-round-robin` picks among equal-score backends so backend `weight` values still apply (arc 3, codebuild 2, lambda 1, cloud-run 1).

## When to Use

- Your CI workload spans multiple cloud providers and in-cluster compute.
- You need tenant-aware fair-share scheduling to prevent any single team or workflow from saturating capacity.
- You want Docker-capable overflow in CodeBuild, event-driven short jobs in Lambda, and container job isolation in Cloud Run.

## Topology

```
GitHub Actions Workflow
        │
        ▼
allocate-runner action (with optional tenant + priority_class)
        │  OIDC / REST
        ▼
 ┌──────────────────────────┐
 │  Broker (arc-systems)    │
 │  Fair-share → then WRR   │
 │  (compose, weights apply)│
 └──────────────────────────┘
        │
   ┌────┼──────────────────┬─────────────────┐
   ▼    ▼                  ▼                 ▼
 ARC  CodeBuild         Lambda           Cloud Run
 (w3) (w2, docker)     (w1, serverless)  (w1, serverless)
```

## Pools

| Pool | Backends | Scheduler | Fair Share |
|------|---------|-----------|-----------|
| `full` | arc | round-robin | disabled |
| `lite` | arc (w3), codebuild (w2), lambda (w1), cloud-run (w1) | weighted-round-robin | enabled |

External backends use warm capacity to reduce cold-start latency.

## Files in This Pack

- `base/` — Kustomize base with CodeBuild, Lambda, and Cloud Run enabled
- `overlays/staging/` — staging overlay (1 broker replica, reduced external capacity)
- `overlays/production/` — production overlay (1 broker replica; multi-replica requires postgres HA state store, full external capacity)
- `argocd/application.yaml` — ArgoCD Application manifest
- `secrets.md` — required Kubernetes Secrets for all four backends
- `rollout.md` — phased enablement steps and rollback
- `validation.md` — post-install health checks and optional CronJob
- `feature-flags.md` — backend pinning, fair-share tuning, staged rollout, and rollback

# arc-plus-codebuild Reference Pack

This pack deploys the broker in a hybrid topology with ARC as the primary in-cluster backend and AWS CodeBuild as an external overflow backend. The lite pool uses `weighted-round-robin` scheduling to prefer ARC while routing overflow to CodeBuild.

## When to Use

- Your cluster runs ARC and you need burst capacity into AWS.
- You want to isolate Docker-capable jobs in ARC and allow CodeBuild to absorb overflow.
- You are not yet running workloads across multiple cloud providers.

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
   ┌────┴────────────────────┐
   │                         │
   ▼ (weight 3)              ▼ (weight 1, overflow)
 ┌──────┐            ┌───────────────┐
 │ ARC  │            │  CodeBuild    │
 └──────┘            │  (AWS)        │
   │                 └───────────────┘
   ▼                         │
Ephemeral runner pod    Ephemeral runner
(in-cluster, 1 job)     (CodeBuild build, 1 job)
```

## Pools

| Pool | Backends | Scheduler | Notes |
|------|---------|-----------|-------|
| `full` | arc | round-robin | ARC full-capacity, no external overflow |
| `lite` | arc (weight 3), codebuild (weight 1) | weighted-round-robin | ARC preferred; CodeBuild absorbs overflow |

CodeBuild warm capacity is configured with `warmMin: 1, warmMax: 2` to reduce cold-start latency on burst.

## Files in This Pack

- `base/` — Kustomize base with CodeBuild enabled in the lite pool
- `overlays/staging/` — staging overlay (1 broker replica, lower CodeBuild capacity)
- `overlays/production/` — production overlay (2 broker replicas, full CodeBuild capacity)
- `argocd/application.yaml` — ArgoCD Application manifest
- `secrets.md` — required Kubernetes Secrets including the CodeBuild dispatcher secret
- `rollout.md` — initial install, CodeBuild enablement, and rollback steps
- `validation.md` — post-install health checks and optional CronJob
- `feature-flags.md` — staged rollout, CodeBuild enable/disable, warm capacity tuning, and rollback

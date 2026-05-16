# GitOps Reference Packs

This directory contains three reference deployment packs for running the broker in common topologies using manifest-driven configuration, Kustomize overlays, and secure external integrations.

## Available Packs

| Pack | Topology | External Backends | Scheduler |
|------|----------|------------------|-----------|
| [`arc-only`](arc-only/) | ARC-only | none | round-robin |
| [`arc-plus-codebuild`](arc-plus-codebuild/) | Hybrid ARC + AWS CodeBuild | CodeBuild | weighted-round-robin |
| [`multi-backend`](multi-backend/) | Multi-backend hybrid | CodeBuild, Lambda, Cloud Run | weighted-round-robin + fair-share |

## Choosing a Pack

- Start with **arc-only** if your cluster runs ARC and you do not yet need external cloud runners.
- Move to **arc-plus-codebuild** when you need overflow capacity into AWS without adding multiple cloud providers.
- Use **multi-backend** when jobs span AWS, GCP, and in-cluster compute and you need fair-share tenant scheduling.

## How to Consume a Pack

Each pack ships a Kustomize base that extends the shared upstream base at `examples/gitops/kustomize/base`. Use the pack as a remote base pinned to an immutable release ref in your private overlay repo.

```yaml
# your-private-repo/overlays/production/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - https://github.com/josh-archer/unified-ephemeral-runner-broker//examples/gitops/packs/arc-only/base?ref=v0.1.0

patches:
  - path: patch-org.yaml     # sets github.scope.organization
  - path: patch-replicas.yaml
```

Pin to a tag or commit SHA. Never consume a floating `main` reference in production.

## Pack Layout

```
<pack>/
├── README.md            # pack overview and topology diagram
├── base/
│   ├── kustomization.yaml
│   └── patch-config.yaml   # ConfigMap patch with pack-specific broker.yaml
├── overlays/
│   ├── staging/
│   │   ├── kustomization.yaml
│   │   └── patch-replicas.yaml
│   └── production/
│       ├── kustomization.yaml
│       └── patch-replicas.yaml
├── argocd/
│   └── application.yaml    # ArgoCD Application referencing pack base
├── secrets.md              # secret contract: required Secrets and their keys
├── rollout.md              # initial install, rolling update, and rollback steps
├── validation.md           # post-install health checks
└── feature-flags.md        # staged rollout, backend enable/disable, pinning, rollback
```

## Safe Offload Pattern

External automation in these packs does not use native GitHub Actions `schedule:` triggers for infrastructure operations. Periodic tasks such as capacity health checks or warm-pool status reports are offloaded to a Kubernetes CronJob running in the same cluster as the broker.

Reasons:
- Cluster-level operations require access to the Kubernetes API and broker metrics, not GitHub's compute.
- GitHub Actions scheduled workflows consume Actions minutes and depend on GitHub scheduler availability.
- CronJob execution is scoped to the cluster where the broker runs and does not require repository secrets or outbound GitHub API access.

Each pack's `validation.md` includes an optional Kubernetes CronJob manifest that runs the pack's post-install health checks on a recurring schedule. See the individual pack for the exact CronJob definition.

## Promoting Pack Changes Through GitOps

Never hand-edit live cluster state. All changes flow through pull requests.

### Normal promotion path

1. Open a pull request in your private overlay repo changing the overlay patch (config value, replica count, image tag, or enabled backend).
2. ArgoCD or Flux detects the merged change and syncs the environment. No manual `kubectl apply` is needed.
3. Verify the sync with the validation steps in the pack's `validation.md`.

### Pinning a new pack version

When a new broker release updates the pack base, promote the version change through overlays:

1. Update the `?ref=` pin in your private overlay's `kustomization.yaml` from the old release tag to the new one.
2. Run `kustomize build overlays/staging | kubectl diff -f -` to review the rendered diff before merging.
3. Merge to the staging branch. Confirm health checks pass.
4. Open a pull request to the production branch with the same change.
5. Merge after review. ArgoCD or Flux completes the rollout.

### Rollback

Revert the pull request in your private overlay repo (or open a new one reverting the config change). ArgoCD or Flux will restore the previous state. No direct cluster access is needed.

## Secret Management

Secrets are never stored in the pack manifests or in this repository. See each pack's `secrets.md` for the exact Kubernetes Secret names and keys the broker expects. Provision secrets through your preferred external secret mechanism (External Secrets Operator, Vault agent, cloud provider secret store sync) before deploying the broker.

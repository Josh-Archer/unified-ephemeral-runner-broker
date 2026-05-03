# Kustomize Consumption

This repository ships a plain-manifest Kustomize base under `examples/gitops/kustomize/base`.

Use it as a remote base pinned to a release ref and layer your local secrets, namespace, and backend enablement on top. Keep environment-specific Secret objects and cloud identity wiring in your private repo.

## Required broker secret

The base config references a Kubernetes Secret named `uecb-github-app`.

Create that Secret in your private overlay (or via ExternalSecret/secret manager tooling) in the same namespace as the broker. The broker checks for referenced secrets at runtime and stays unready until they exist.

## Scheduler overlays

The base keeps `round-robin` and `fairShare.enabled=false` as safe defaults. Private overlays can set `pools[].scheduler` to `weighted-round-robin`, enable `pools[].fairShare.enabled`, and tune `fairShare.priorityClasses` without changing live state by hand.

## Reference packs

For complete topology-specific examples with overlays, secret contract documentation, validation steps, and rollout notes, see the reference packs under [`examples/gitops/packs/`](../packs/README.md):

- [`arc-only`](../packs/arc-only/README.md) — ARC-only, round-robin
- [`arc-plus-codebuild`](../packs/arc-plus-codebuild/README.md) — hybrid ARC + CodeBuild, weighted-round-robin
- [`multi-backend`](../packs/multi-backend/README.md) — ARC + CodeBuild + Lambda + Cloud Run, fair-share

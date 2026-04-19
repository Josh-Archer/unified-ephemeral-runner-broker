# Kustomize Consumption

This repository ships a plain-manifest Kustomize base under `examples/gitops/kustomize/base`.

Use it as a remote base pinned to a release ref and layer your local secrets, namespace, and backend enablement on top. Keep environment-specific Secret objects and cloud identity wiring in your private repo.

## Required broker secret

The base config references a Kubernetes Secret named `uecb-github-app`.

Create that Secret in your private overlay (or via ExternalSecret/secret manager tooling) in the same namespace as the broker. The broker checks for referenced secrets at runtime and stays unready until they exist.

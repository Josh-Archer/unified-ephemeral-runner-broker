# Kustomize Consumption

This repository ships a plain-manifest Kustomize base under `examples/gitops/kustomize/base`.

Use it as a remote base pinned to a release ref and layer your local secrets, namespace, and backend enablement on top. Keep environment-specific Secret objects and cloud identity wiring in your private repo.


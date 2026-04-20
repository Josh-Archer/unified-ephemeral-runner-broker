# unified-ephemeral-runner-broker

`unified-ephemeral-runner-broker` is a public control plane for allocating one-shot GitHub Actions runners across a unified ephemeral capacity pool.

V1 supports exactly four backends:

- `arc`
- `lambda`
- `cloud-run`
- `azure-functions`

It is intentionally split into two capability pools:

- `full`: ARC only in v1
- `lite`: ARC plus the supported external backends

Default multi-backend scheduling is `round-robin`.

Built-in schedulers:

- `round-robin`
- `weighted-round-robin`

## What This Repo Ships

- A Kubernetes broker service with a small REST API
- A reusable GitHub Action, `allocate-runner`
- An OCI Helm chart for installation
- A generic Kustomize-facing GitOps consumption path
- Generic infrastructure examples for AWS, GCP, and Azure

## What This Repo Does Not Ship

- Homelab-specific manifests, overlays, or secret-store implementations
- Inline credentials or cloud secrets
- Private runner labels, cluster names, or internal network details
- A public release workflow that can touch self-hosted runners

## High-Level Flow

1. A lightweight workflow step calls `allocate-runner`.
2. The broker selects an eligible backend from the chosen pool.
3. The backend provisions an ephemeral runner and returns a unique runner label.
4. The heavy workflow job runs on that exact label.
5. The runner executes one job and exits.

## Project Layout

- `cmd/broker`: broker entrypoint
- `internal/`: broker, scheduler, backend, GitHub, and config packages
- `charts/unified-ephemeral-runner-broker`: Helm chart
- `actions/allocate-runner`: public workflow integration surface
- `examples/`: generic Terraform and GitOps consumption examples
- `docs/`: architecture and security notes

## Public CI and Private Release Boundary

This repository is designed for a split trust model:

- Public CI runs on GitHub-hosted runners only
- A separate private release repository owns the authoritative ARC-backed publish lane
- Public forks and PRs must never reach self-hosted runners or publish credentials

See [docs/architecture.md](docs/architecture.md) and [docs/security-boundary.md](docs/security-boundary.md) for the full model.

## Quick Start

1. Install the Helm chart with external backends disabled.
2. Create the GitHub auth secret and any enabled backend secrets in the same namespace as the broker.
   The broker validates referenced `secretRef` objects via the Kubernetes API and stays unready until they exist.
3. Point the `allocate-runner` action at the broker URL.
4. Start with the `full` pool or ARC-only `lite` pool, then enable external backends one by one.

## Scheduler Configuration

Each pool selects its scheduler with `pools[].scheduler`.

- `round-robin` is the default and ignores backend weights.
- `weighted-round-robin` uses `pools[].backends.<name>.weight`.
- Omitted or non-positive weights are treated as `1`.

Example:

```yaml
pools:
  - name: lite
    scheduler: weighted-round-robin
    backends:
      arc:
        enabled: true
        maxRunners: 2
        weight: 3
      lambda:
        enabled: true
        maxRunners: 3
        weight: 1
```

Rollback is just a config change: set `scheduler` back to `round-robin` for the pool and redeploy. Leaving `weight` values in place is safe because the default scheduler ignores them.

## License

Apache-2.0

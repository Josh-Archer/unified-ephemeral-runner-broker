# Security Boundary

This public repository must stay free of private environment details.

## Never Commit

- tokens, keys, kubeconfigs, PATs, or cloud credentials
- internal hostnames, node names, runner labels, or secret paths
- private manifests or copied cluster overlays

## Public CI Rules

- GitHub-hosted runners only
- no publish jobs
- no self-hosted runner access
- no privileged environment secrets

## Release Rules

- authoritative releases are built and published from a separate private release lane
- only immutable tags or SHAs are promoted
- images and charts are signed and published with digests


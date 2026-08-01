# Capability Schema

Capability-aware routing lets jobs select backends by advertised tags. Tags are
normalized (trimmed, lowercased, deduplicated) before matching. Filtering runs
before the pool scheduler: only eligible backends are considered.

## Request fields

| Field | Meaning |
| --- | --- |
| `required_capabilities` | Every listed tag must be advertised by the selected backend |
| `excluded_capabilities` | None of the listed tags may be advertised by the selected backend |

If both lists are empty, capability filtering is a no-op and scheduling is
unchanged.

The `allocate-runner` action accepts the same filters as comma-separated inputs,
plus first-class `os` and `arch` inputs that are merged into
`required_capabilities`.

## Platform dimensions (OS and architecture)

Non-default runner platforms are modeled with dedicated dimensions so Windows
and arm64 (and related) fleets can be requested without overloading free-form
tags.

### Canonical tags

| Dimension | Canonical tag | Meaning |
| --- | --- | --- |
| OS | `os:linux` | Linux runners (default for shipped backends) |
| OS | `os:windows` | Windows runners |
| OS | `os:macos` | macOS runners |
| Arch | `arch:amd64` | x86_64 / GitHub `x64` |
| Arch | `arch:arm64` | aarch64 / GitHub `arm64` |

### Accepted aliases

Bare aliases expand to the canonical forms above on both the request and the
backend advertisement path:

| Alias | Canonical |
| --- | --- |
| `linux` | `os:linux` |
| `windows` | `os:windows` |
| `macos`, `darwin` | `os:macos` |
| `amd64`, `x64`, `x86_64` | `arch:amd64` |
| `arm64`, `aarch64` | `arch:arm64` |

Already-prefixed tags such as `os:windows` or `arch:arm64` are left as-is
(after lowercasing). Unknown tags pass through unchanged so operators can keep
using free-form labels (`docker`, `gpu`, `region:…`, `cloud:…`, …).

### Backend configuration

Advertise platform tags on each backend:

```yaml
pools:
  - name: lite
    backends:
      arc:
        enabled: true
        capabilities:
          - os:linux
          - arch:amd64
          - cluster-local
          - docker
          - region:local
      # Example Windows + arm64 fleet (operator-owned runner label / launcher)
      azure-vm:
        enabled: true
        runnerLabel: my-windows-arm64-runners
        capabilities:
          - os:windows
          - arch:arm64
          - vm
          - cloud:azure
          - region:azure-eastus
```

Default chart/config values advertise `os:linux` and `arch:amd64` for the
built-in backends because those fleets are Linux/x64 today. Operators must
explicitly advertise `os:windows` / `arch:arm64` (or aliases) on backends that
actually provide those platforms.

### Job requests

Prefer either the dedicated action inputs or capability tags:

```yaml
- uses: Josh-Archer/unified-ephemeral-runner-broker/actions/allocate-runner@main
  with:
    broker_url: https://broker.example.com
    pool: lite
    os: windows
    arch: arm64
```

Equivalent request body fields:

```json
{
  "pool": "lite",
  "required_capabilities": ["os:windows", "arch:arm64"]
}
```

Aliases also work:

```json
{
  "pool": "lite",
  "required_capabilities": ["windows", "arm64"]
}
```

If no backend matches, the broker rejects the allocation with a clear
capability-mismatch error before scheduling.

## Other common tags

These tags are conventional (not expanded by the platform alias table):

| Tag | Typical use |
| --- | --- |
| `docker` | Docker/container workloads |
| `gpu` | GPU-capable runners |
| `cluster-local` | In-cluster ARC capacity |
| `vm` | Full VM backends |
| `privileged` | Privileged / nested-virt style hosts |
| `cloud:aws` / `cloud:azure` / `cloud:gcp` | Cloud provider |
| `region:<id>` | Region pin (for example `region:aws-us-east-1`) |

## Matching rules

1. Normalize request `required_capabilities` and `excluded_capabilities`.
2. Normalize each backend's configured `capabilities`.
3. Keep a backend only when every required tag is present and no excluded tag is present.
4. Pinned backends still honor the filter; a pin that fails the filter is rejected
   rather than silently rerouted.
5. Missing capability metadata means the backend advertises an empty extra set
   (it will only match requests with no required tags).

See also:

- [architecture.md](architecture.md) — admission order and scheduler boundary
- [openapi.yaml](openapi.yaml) — HTTP request schema
- [examples/workflows/platform-routing.yml](../examples/workflows/platform-routing.yml) — end-to-end workflow

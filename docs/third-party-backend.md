# Add a third-party backend

This guide is for a runner controller the broker does not ship. The contract is Adapter SDK v1: `adapter.APIVersion` is `"v1"`. The normative field rules live in [adapter-sdk.md](adapter-sdk.md). This page is the path from that interface to a broker that can call your controller.

The broker does not load out-of-tree Go plugins. You run your own controller and point a built-in external backend's secret at it. The backend name selects broker behavior (dispatch timeout, warm pool eligibility, cost class). The secret URLs select your controller.

Two extension points are different from this guide:

- `arc`, `azure-vm`, `desktop`, and `kubernetes` are built-in adapters with their own provisioning. A named [`driver: kubernetes`](../README.md#shared-in-cluster-pool-kubernetes-backend) target is an extra in-cluster pod backend, not a hook for an arbitrary cloud API.
- Forking the broker to register a new backend type is outside v1. The frozen surface is the adapter interface plus the HTTP contract below.

## What v1 requires

Implement the lifecycle in [adapter-sdk.md](adapter-sdk.md#lifecycle):

| Call | Required behavior |
| --- | --- |
| `Health` | Healthy controllers return `Healthy: true` and a non-zero `Checked` time. |
| `Capacity` | `MaxRunners > 0`. Counters are never negative. Every reserved, launching, warm, and running slot is visible before the mutating call returns. |
| `Reserve` | Keep a non-empty allocation id. Return a non-empty runner label and a zero or future `ExpiresAt`. Reserving an id you already hold returns that reservation and does not take a second slot. |
| `Reserve` when full | Return `ErrCapacityExhausted` (`adapter.IsCapacityExhausted`). Do not keep a slot for the rejected call. |
| `Launch` | Return a non-empty runner label. `Warm: true` is a warm start. |
| `Cleanup` | Succeed for `canceled`, `expired`, `failed`, and `completed`. A repeat call, including an unknown allocation, returns nil and releases nothing extra. |
| Context | A context that is already canceled returns `context.Canceled` and does not change capacity. |

Run the kit against a fake or local controller. A conformance run reserves real slots up to the controller's free-slot count.

## Path 1 — Go adapter

1. Add a module dependency on this repository (use a `replace` directive until the commit that introduced adapter API v1 is in the release you pin).
2. Implement `adapter.Adapter`. `examples/adapter/mock` is a complete reference, including capacity phases, idempotent reserve, and `ErrCapacityExhausted`.
3. Prove it. Keep `MaxRunners` at or below 4 so exhaustion runs:

```go
func TestAdapterConformance(t *testing.T) {
    adaptertest.RunConformance(t, func(testing.TB) adapter.Adapter {
        return myadapter.New(1)
    }, adaptertest.Options{
        Backend: "my-backend",
        Pool:    "lite",
    })
}
```

4. Serve the broker's HTTP contract with `dispatch.NewHandler`:

```go
func main() {
    handler := dispatch.NewHandler(myadapter.New(4), dispatch.HandlerOptions{
        Token: os.Getenv("UECB_DISPATCH_TOKEN"),
    })
    log.Fatal(http.ListenAndServe(":8080", handler))
}
```

Default routes are `dispatch.PathDispatch` (`/dispatch`), `dispatch.PathCleanup` (`/cleanup`), `dispatch.PathHealth` (`/health`), and `dispatch.PathCapacity` (`/capacity`). The handler maps the broker JSON onto `Reserve` and `Launch`, copies GitHub registration fields into metadata (`github.target_url`, `github.repository`, `github.owner`, `github.organization`, `github.scope_type`, `github.runner_group`), and copies reservation and launch metadata back into the dispatch response. `launch_mode` `warm` sets `LaunchRequest.Warm`. A capacity rejection becomes HTTP 409. Cleanup success is HTTP 204. Unknown JSON fields are ignored.

`examples/adapter/httpdispatch` is this wiring around the mock adapter.

5. Run the HTTP kit against that server:

```go
func TestHTTPConformance(t *testing.T) {
    server := httptest.NewServer(dispatch.NewHandler(myadapter.New(1), dispatch.HandlerOptions{
        Token: "test-token",
    }))
    t.Cleanup(server.Close)
    adaptertest.RunHTTPConformance(t, server.URL, adaptertest.HTTPOptions{
        Token:   "test-token",
        Backend: "my-backend",
    })
}
```

Imports:

```text
github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter
github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter/adaptertest
github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter/dispatch
```

## Path 2 — HTTP controller in any language

Implement the four endpoints yourself. JSON names match `pkg/adapter/dispatch` and the broker's external-dispatch client. Ignore unknown fields.

### Health — `GET health_url`

Return any `2xx` when the controller can accept work. The broker reads the status code only. Send `Authorization: Bearer <dispatch_token>` when the secret sets `dispatch_token`, and read `X-UECB-Backend`.

### Capacity — `GET capacity_url`

```json
{
  "max_runners": 4,
  "active_runners": 1,
  "pending_runners": 0,
  "warm_runners": 1,
  "free_slots": 2
}
```

`free_slots` is optional. When you send it next to `max_runners`, the broker honors the lower ceiling. Conformance requires the counters to move when a dispatch takes or releases a slot. The polling rules (`refreshInterval`, `staleAfter`, `failureMode`) are in [adapter-sdk.md](adapter-sdk.md#publishing-capacity).

### Dispatch — `POST dispatch_url`

```json
{
  "action": "dispatch",
  "backend": "codebuild",
  "allocation_id": "abc123",
  "pool": "lite",
  "runner_label": "uecb-codebuild-abc123",
  "runner_name": "uecb-codebuild-abc123",
  "runner_labels": ["self-hosted", "linux", "x64", "uecb-codebuild-abc123"],
  "requested_labels": ["linux", "x64"],
  "launch_mode": "cold",
  "job_timeout": "15m0s",
  "job_timeout_seconds": 900,
  "github": {
    "scope_type": "repository",
    "owner": "example",
    "repository": "example/repo",
    "target_url": "https://github.com/example/repo",
    "runner_group": ""
  }
}
```

Start one ephemeral runner registered at `github.target_url` with `runner_labels`. Prefer the broker's `runner_label` unless you must mint your own; return the label the job will target:

```json
{
  "runner_label": "uecb-codebuild-abc123",
  "metadata": {"execution_id": "run-1"},
  "status_url": "https://controller.example/runs/run-1",
  "details_url": "https://controller.example/runs/run-1",
  "execution_id": "run-1"
}
```

A second dispatch with the same `allocation_id` returns the same `runner_label` and does not take another slot. `launch_mode` `warm` is a warm start. When the controller is full, return HTTP 409 and an `{"error":"..."}` body. Include the word `capacity` in that error. Do not return `2xx` for a rejected reservation.

`job_timeout_seconds` wins when it is positive. The Go bridge also accepts Go duration strings in `job_timeout` (`15m0s`).

### Cleanup — `POST cleanup_url`

```json
{
  "action": "cleanup",
  "backend": "codebuild",
  "allocation_id": "abc123",
  "correlation_id": "corr-1",
  "pool": "lite",
  "runner_label": "uecb-codebuild-abc123",
  "state": "completed",
  "error": "",
  "metadata": {"execution_id": "run-1"}
}
```

Tear down the runner for that allocation. Treat a repeat call and an unknown allocation as success. HTTP `2xx` and `404` are both success to the broker. States you must accept: `canceled`, `expired`, `failed`, `completed`.

### Prove an HTTP controller from Go

`adaptertest.RunHTTPConformance` is the same kit as the Go path. Point it at the controller origin. Set `HTTPOptions.Token` when the controller requires a bearer token. The kit sends `X-UECB-Backend`, checks health, capacity, dispatch, idempotent dispatch, cleanup, capacity accounting, and HTTP 409 when the free slots are exhausted. It skips the exhaustion case when free slots exceed `MaxProbeReservations` (default 4) unless `RequireExhaustion` is set. Shrink the test controller instead of pointing the kit at a large production ceiling.

## Configure the broker

Create the secret in the broker namespace. Values below are placeholders; do not commit a real token.

```bash
kubectl create secret generic uecb-my-backend \
  --from-literal=dispatch_url=https://<YOUR_CONTROLLER_ORIGIN>/dispatch \
  --from-literal=cleanup_url=https://<YOUR_CONTROLLER_ORIGIN>/cleanup \
  --from-literal=health_url=https://<YOUR_CONTROLLER_ORIGIN>/health \
  --from-literal=capacity_url=https://<YOUR_CONTROLLER_ORIGIN>/capacity \
  --from-literal=dispatch_token=<DISPATCH_TOKEN>
```

| Key | Required | Role |
| --- | --- | --- |
| `dispatch_url` | yes | POST target for provision |
| `health_url` | yes when `circuitBreaker` recovery probes run | GET health |
| `dispatch_token` | no | Bearer token for dispatch, health, cleanup, and capacity |
| `cleanup_url` | no | POST target for cancel, expire, and complete. Omit it and the broker releases its own capacity without calling you |
| `capacity_url` | no | GET target for live free-slot routing when `broker.liveCapacity.enabled` is true |

Enable one existing external backend on the `lite` pool and set `secretRef` to that secret. The URLs decide which controller runs. The name decides broker defaults:

| Backend name | Dispatch timeout | Warm pool |
| --- | --- | --- |
| `codebuild`, `lambda`, `cloud-run`, `ec2`, `gce` | 20s | supported |
| `azure-functions` | 90s | supported |

Use `azure-functions` when cold start can exceed 20 seconds. The controller behind the secret does not have to be that cloud product.

```yaml
pools:
  - name: lite
    backends:
      codebuild:
        enabled: true
        healthy: true
        maxRunners: 4
        secretRef: uecb-my-backend
```

`maxRunners` remains the broker's local concurrency ceiling. Live capacity, when enabled, clamps that ceiling to the provider snapshot. `Reserve` rejection for capacity still falls back to another eligible backend for an unpinned request. See [adapter-sdk.md](adapter-sdk.md#publishing-capacity).

Register the GitHub runner from the dispatch body. The broker does not register runners for external-dispatch backends. The workflow then targets the returned `runner_label`. One job, then exit, then honor cleanup.

## Checklist

- `adapter.APIVersion` is the version you implemented (`v1`).
- `go test` runs `adaptertest.RunConformance` for a Go adapter, with a small ceiling.
- The process you deploy is `dispatch.NewHandler` or an HTTP implementation of the same routes.
- `adaptertest.RunHTTPConformance` passes against that process, including HTTP 409 when full.
- The secret carries `dispatch_url`, `cleanup_url`, `health_url`, and `capacity_url` for the endpoints you implemented.
- Cleanup is safe to call twice. Capacity counters drop when the runner is gone.
- No broker credentials or dispatch tokens are committed with the controller.

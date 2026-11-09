# Backend Adapter SDK

**Status: v1 (frozen).** `pkg/adapter` is Adapter SDK v1. `adapter.APIVersion` is `"v1"`.

Stability promise:

- The `Adapter` method set and `AllocationState` string values do not change within v1.
- Required field semantics in this document do not change within v1.
- Later v1 releases may add optional struct fields and optional helper packages.
- A breaking change ships as a new major API version.

The Go module version may advance while this contract stays at v1.
Step-by-step backend instructions: [third-party backend guide](third-party-backend.md).

The public adapter SDK lives under `pkg/adapter`.

It is for provider-owned runner controllers that want to implement the broker
adapter lifecycle without depending on internal broker packages. The broker
process does not load out-of-tree plugins. Out-of-process controllers implement
this contract behind the external-dispatch HTTP surface in `pkg/adapter/dispatch`.

## Lifecycle

Adapters implement:

- `Health`: report whether the backend can accept runner work. `Checked` is set. A canceled context returns `context.Canceled` and does not report success.
- `Capacity`: report active, pending, warm, and maximum runner counts. Counters include every reserved, launching, warm, and running slot before the mutating call returns.
- `Reserve`: reserve a runner slot and return the runner label contract. A non-empty `AllocationID` is returned unchanged. A repeat `Reserve` for an id that is still held returns the original reservation and does not consume a second slot. `ExpiresAt` is zero or in the future. When no free slot remains, return an error that wraps `ErrCapacityExhausted`.
- `Launch`: start or attach the runner execution and return a non-empty runner label.
- `Cleanup`: release provider-side resources after a terminal allocation state (`canceled`, `expired`, `failed`, `completed`). A second cleanup, including one for an unknown allocation, returns nil.

The conformance kit in `pkg/adapter/adaptertest` is importable by external modules. `RunConformance` checks the in-process lifecycle. `RunHTTPConformance` checks a controller that speaks the external-dispatch HTTP contract. Keep the test adapter's free-slot count at or below `adaptertest.DefaultMaxProbeReservations` (4) so the exhaustion case runs.

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

`Options` zero values use pool `lite`, backend `conformance`, tenant `conformance`, and a 15 minute job timeout. See `examples/adapter/mock` for a minimal reference implementation and `examples/adapter/httpdispatch` for the HTTP wiring. `adaptertest.NewMemory` is an in-memory fixture for local tests, not a production backend.

## Publishing Capacity

`Capacity` is how providers publish live free-slot data for broker routing.

### SDK adapters

Implement `Adapter.Capacity` and return non-negative counters:

| Field | Meaning |
| --- | --- |
| `MaxRunners` | Hard provider ceiling (must be `> 0` for a usable reading) |
| `ActiveRunners` | Runners currently executing work |
| `PendingRunners` | Reserved or starting runners not yet active |
| `WarmRunners` | Idle warm runners held by the provider |

Free slots are computed as
`max(0, MaxRunners - ActiveRunners - PendingRunners - WarmRunners)`.

When `broker.liveCapacity.enabled` is true, the broker polls `Capacity`
out of band (never on the allocation hot path), caches the snapshot with a
TTL (`staleAfter`), and filters exhausted backends before scheduler selection.
Local scheduler reservations remain the broker concurrency authority; provider
`Reserve` rejection for capacity still falls back to another eligible backend
(or returns a deterministic pinned live-capacity error).

Recommended adapter behavior:

1. Count every reserved, launching, warm, and running slot in the counters.
2. Keep `MaxRunners` aligned with the provider’s true concurrency limit.
3. Return an error from `Capacity` only for probe failures (timeouts, auth);
   a full backend should return `ActiveRunners + PendingRunners + WarmRunners >= MaxRunners`, not an error.
4. Reject over-capacity `Reserve` calls with `ErrCapacityExhausted` (`adapter.IsCapacityExhausted`) so concurrent broker admits cannot overrun the provider. On the HTTP path, answer that rejection with HTTP 409. The broker also treats an error body containing `capacity`, `no free`, or `no slot` as exhaustion; v1 conformance requires status 409.

### Built-in ARC and desktop backends

`arc` and `desktop` implement native `Capacity()` so live-capacity routing does
not lag HTTP-dispatch backends:

| Backend | Capacity source |
| --- | --- |
| `arc` | Optional secret key `capacity_url` (same JSON as HTTP-dispatch). When absent, publishes the sum of enabled pool `maxRunners` as the runner-scale ceiling. |
| `desktop` | Configured `maxRunners` (default `1`). Optional `desktop.address` + `desktop.checkPort` host probe: offline hosts report exhaustion (`free_slots = 0`). |

Wire ARC scale/node feeds through the same `capacity_url` secret shape used by
cloud controllers so home GitOps can publish honest free slots without a broker
restart.

### HTTP-dispatch controllers

External dispatch backends (`codebuild`, `lambda`, `cloud-run`,
`azure-functions`, `ec2`, `gce`) read optional secret key `capacity_url`.

```text
GET {capacity_url}
Authorization: Bearer {dispatch_token}   # when configured
X-UECB-Backend: codebuild
```

Successful responses (`2xx`) should return JSON:

```json
{
  "max_runners": 10,
  "active_runners": 4,
  "pending_runners": 1,
  "warm_runners": 2,
  "free_slots": 3
}
```

- `max_runners` is required for a positive usable reading (or omit it and set
  `free_slots` so the broker can reconstruct a ceiling).
- `free_slots` is optional; when `max_runners` is omitted and `free_slots > 0`,
  the broker derives `max_runners = free_slots + active + pending + warm`.
  When specified alongside `max_runners`, the broker honors `free_slots`
  (capping effective capacity so the published free count is respected).
- Missing `capacity_url` on HTTP-dispatch backends means that backend does not
  publish live capacity; the broker uses local `maxRunners` accounting only for
  that backend. Built-in `arc` falls back to configured scale instead of
  treating Capacity as missing.

When a dispatch/provision call is rejected for capacity, return HTTP `409`
and/or an error body containing `capacity` so the broker can classify the
failure as capacity exhaustion and fall back.

### Stale and failed readings

Configure under `broker.liveCapacity`:

```yaml
broker:
  liveCapacity:
    enabled: true
    refreshInterval: 30s
    staleAfter: 2m
    probeTimeout: 2s
    failureMode: pass-through   # or block
    refreshOnStartup: true
```

| `failureMode` | Stale or failed capacity read |
| --- | --- |
| `pass-through` (default) | Ignore live data; use local `maxRunners` only |
| `block` | Treat the backend as unavailable until a fresh reading arrives |

The broker never intentionally admits more than the lower of configured
`maxRunners` and provider-reported limits when a fresh snapshot is available.

### Fake capacity mode (local / CI)

Broker unit and integration tests should not call real AWS, Azure, or GCP
capacity APIs. The first-class fixtures under
`internal/capacity/fakecapacity` cover both capacity publish paths:

| Fixture | Use when |
| --- | --- |
| `fakecapacity.NewBackend` | In-process `Capacity()` for SDK-style backends and refresh-loop tests |
| `fakecapacity.NewServer` | HTTP `capacity_url` server for external-dispatch backends |
| `fakecapacity.LiveSnapshot` / `StaleSnapshot` / `ErrorSnapshot` | Seed `capacity.Manager` without running a refresh |
| `fakecapacity.MapReporter` | Multi-backend `capacity.Refresh` without cloud credentials |

Example: seed routing decisions directly:

```go
manager := capacity.NewManager()
fakecapacity.SeedManager(manager,
    fakecapacity.LiveSnapshot(model.BackendCodeBuild, fakecapacity.Full(2), time.Now().UTC()),
    fakecapacity.LiveSnapshot(model.BackendARC, fakecapacity.Free(4, 2), time.Now().UTC()),
)
service.SetCapacityManager(manager)
```

Example: fake `capacity_url` for HTTP-dispatch controllers:

```go
srv := fakecapacity.NewServer(t,
    fakecapacity.WithStatus(fakecapacity.Detailed(5, 2, 1, 1)),
    fakecapacity.WithBearerAuth("broker-secret"),
)
// Put srv.URL into the backend secret as capacity_url.
```

Golden coverage of the live-capacity decision matrix (skip exhausted, stale
pass-through / block, pin-style error handling, local-only fallback) lives in
`internal/capacity/decision_matrix_test.go` and runs in CI with no outbound
provider network.

## Compatibility

Adapter API v1 is frozen independently of the Go module version. See the status block at the top of this document. `pkg/adapter/dispatch` is part of that v1 surface: its JSON field names match the broker external-dispatch contract, and controllers ignore unknown JSON fields so a later additive field does not break them.

Generic HTTP dispatch backends remain the integration path for a controller that runs outside the broker process. `dispatch.NewHandler` adapts an `adapter.Adapter` to `dispatch_url`, `cleanup_url`, `health_url`, and `capacity_url`. A conformant adapter behind that handler needs no broker core change for reserve, launch, health, capacity, and cleanup. The [third-party backend guide](third-party-backend.md) walks through both the Go and HTTP-only paths.

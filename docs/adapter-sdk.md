# Backend Adapter SDK

The public adapter SDK lives under `pkg/adapter`.

It is for provider-owned runner controllers that want to implement the broker
adapter lifecycle without depending on internal broker packages.

## Lifecycle

Adapters implement:

- `Health`: report whether the backend can accept runner work.
- `Capacity`: report active, pending, warm, and maximum runner counts.
- `Reserve`: reserve a runner slot and return the runner label contract.
- `Launch`: start or attach the runner execution.
- `Cleanup`: release provider-side resources after a terminal allocation state.

The conformance harness in `pkg/adapter/adaptertest` validates that an adapter
honors the minimum lifecycle contract.

```go
func TestAdapterConformance(t *testing.T) {
    adaptertest.RunConformance(t, func(testing.TB) adapter.Adapter {
        return myadapter.New()
    }, adaptertest.Options{})
}
```

See `examples/adapter/mock` for a minimal reference implementation.

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
4. Reject over-capacity `Reserve` calls so concurrent broker admits cannot overrun the provider.

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
- Missing `capacity_url` means the backend does not publish live capacity; the
  broker uses local `maxRunners` accounting only for that backend.

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

## Compatibility

The SDK follows the module version. Minor versions may add optional fields to
request or result structs. Existing interface methods are kept stable within a
minor line. Breaking interface changes require a new major module version.

Generic HTTP dispatch backends remain the lowest-friction integration path. A
conformant SDK adapter can sit behind that dispatch controller without requiring
broker core changes for normal reserve, launch, health, capacity, and cleanup
behavior.

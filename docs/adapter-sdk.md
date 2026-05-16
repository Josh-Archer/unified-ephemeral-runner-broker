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

## Compatibility

The SDK follows the module version. Minor versions may add optional fields to
request or result structs. Existing interface methods are kept stable within a
minor line. Breaking interface changes require a new major module version.

Generic HTTP dispatch backends remain the lowest-friction integration path. A
conformant SDK adapter can sit behind that dispatch controller without requiring
broker core changes for normal reserve, launch, health, capacity, and cleanup
behavior.

# Architecture

`unified-ephemeral-runner-broker` uses an allocate-then-run model.

## Control Plane

- The broker runs in Kubernetes.
- GitHub workflows call the `allocate-runner` action.
- The action exchanges OIDC identity for a broker allocation request.
- The broker selects a backend, reserves capacity, provisions a runner, and returns a unique label.

## Data Plane

- `arc` provisions in-cluster runners.
- `lambda`, `cloud-run`, and `azure-functions` provision lite-profile external runners.
- Each runner handles one job and exits.

## Pools

- `full`: full-capability jobs, ARC only in v1
- `lite`: lightweight jobs, ARC plus enabled external backends

## Default Scheduling

Within a selected pool, backends use `round-robin` across healthy backends with available slots.

Pools can opt into `weighted-round-robin` instead. Backend weights are configured per pool and only affect selection when that scheduler is enabled.

## Capability Filtering

Capability-aware routing is evaluated before scheduler selection.

- Jobs may send `required_capabilities` and `excluded_capabilities` string arrays on the allocation request.
- Each backend advertises a normalized capability set through `pools[].backends.<name>.capabilities`.
- The broker filters the pool down to eligible backends first, then passes only that reduced backend set into the configured scheduler.
- Pinned backend requests still honor capability filters. If the pinned backend is configured for the pool but excluded by the request, the broker returns a clear rejection instead of falling through to another backend.
- Missing backend capability metadata means that backend advertises no extra capabilities.

This keeps scheduling policy isolated in the scheduler registry while making capability eligibility deterministic at the API layer.


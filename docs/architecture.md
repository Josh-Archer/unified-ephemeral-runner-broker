# Architecture

`unified-ephemeral-runner-broker` uses an allocate-then-run model.

## Control Plane

- The broker runs in Kubernetes.
- GitHub workflows call the `allocate-runner` action.
- The action exchanges OIDC identity for a broker allocation request.
- The broker selects a backend, reserves capacity, provisions a runner, and returns a unique label.
- External backends read `dispatch_url` and optional `dispatch_token` from their configured `secretRef` and hand off provisioning to a provider-owned controller.

## Data Plane

- `arc` provisions in-cluster runners.
- `codebuild`, `lambda`, `cloud-run`, and `azure-functions` are lite-profile external runners that dispatch into provider-owned launcher controllers using the shared external dispatch contract.
- The public Azure Functions launcher uses an HTTP dispatch endpoint only for admission and status. Actual runner execution happens on a queue-triggered function inside the same container so the HTTP trigger does not have to stay open for the whole job.
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

## GitHub Targeting

- `github.scope.type=organization` targets an org runner registration surface and can derive per-pool runner groups from `runnerGroupPrefix`.
- `github.scope.type=repository` targets a single repository registration surface and ignores runner groups.


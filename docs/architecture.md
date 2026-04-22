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
- `lambda` and `cloud-run` are lite-profile external runners that dispatch into provider-owned launcher controllers.
- `azure-functions` remains a placeholder backend until a real launcher integration is supplied.
- Each runner handles one job and exits.

## Pools

- `full`: full-capability jobs, ARC only in v1
- `lite`: lightweight jobs, ARC plus enabled external backends

## Default Scheduling

Within a selected pool, backends use `round-robin` across healthy backends with available slots.

Pools can opt into `weighted-round-robin` instead. Backend weights are configured per pool and only affect selection when that scheduler is enabled.

## GitHub Targeting

- `github.scope.type=organization` targets an org runner registration surface and can derive per-pool runner groups from `runnerGroupPrefix`.
- `github.scope.type=repository` targets a single repository registration surface and ignores runner groups.


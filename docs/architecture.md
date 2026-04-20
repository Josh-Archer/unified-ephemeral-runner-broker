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


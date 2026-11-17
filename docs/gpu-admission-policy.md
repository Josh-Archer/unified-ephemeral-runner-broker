# GPU snapshot policy component (not activated)

`internal/gpuadmission` is production-capable pure comparison code with no runtime
caller, collector implementation, enabled configuration or allocation authority.
All thresholds are mandatory explicit values; test numbers are not recommendations.
Nil means missing; zero error/latency allowances are expressible. Typed reasons
avoid logging raw observations. Arithmetic is bounded before subtraction; no
reservation sums can overflow. The initial global limit is one across all nodes.

The Source boundary must be implemented only by a trusted server collector.
The evaluator cannot authenticate supplied structs. Never map request/PR fields
to observations or policy. Resolve node and whole-device UUID, retain each sample's
original timestamp, reject ambiguous mappings and upstream errors. Every sample,
including reservation state, must be present and fresh. Protected latency/error
units and measurement windows must match their operator-approved policy.
Whole-device memory covers host processes, not only pod allocations. Time slicing
provides neither VRAM/fault isolation nor per-container DCGM attribution.

## Existing hooks and separate integration work

Service allocation in internal/api/service.go filters repository/capabilities,
backend admission/tier/live capacity before selection, then saveReservedAllocation
before provisioning. internal/capacity/refresh.go polls CapacityBackend, whose
CapacityStatus is runner-slot counts, not GPU health/identity. The existing
internal/tier/promclient QueryInstant returns a scalar: a new typed collector must
retain labels/timestamps and reject multiple/missing/error series instead.

Store.SaveIfCapacityGroup counts reserved/ready/warm and provides a future
integration point, NOT proof of GPU-safe atomicity. Before wiring this evaluator,
transactionally bind allocation, node, UUID and fencing generation; compare and
reserve global/device capacity in the same durable operation. Count launching,
running, cleanup-pending and orphans, retaining reservations until confirmed
termination. Reconcile restart and uncertain launch outcomes; stale workers may
not launch or release newer reservations. Recheck identity/freshness immediately
before provision. Pure comparison can pass twice; it is deliberately not admission.

Trusted job/attempt/SHA assignment, protected workload eligibility, desktop
interactive guards, image compatibility and effective network isolation remain
separate requirements. No policy approval is inferred from a boolean in PR input.
Whole-device UUID/error-health collection and actual Plex/Jellyfin/DeskQuest
streaming baselines remain unverified. No operational thresholds are selected.
No live GPU load, driver/module/security changes or activation accompanies this
component. CUDA/NVENC capability is not vkms/Mesa compositor coverage.

Collector decoding must distinguish absent fields from explicit zero for EVERY
counter (including memory, slice and reservation members). Reject missing members
before constructing these typed value structs: their zero-valued fields cannot
encode absence. A non-nil Sample is an attestation of complete decoding by the
server collector, not evidence supplied by a job. The current package has no
collector implementation, so this remains a required integration contract.

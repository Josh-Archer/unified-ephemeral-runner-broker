# Disposable local VM lifecycle planner (unconnected)

`internal/vmlifecycle` is production transition-planning code with no I/O, runtime
caller, provider, driver registration or configuration. It does not create a VM,
register a runner, authorize a job, allocate a slot or enforce provider fencing.
It advances the synthetic-graphics design without enabling a hardware lane.

## Integration contract

Before invoking any action, persist `Decision.Next` using a durable atomic CAS
against the entire previous record. Every action increments a durable revision;
only one claimant from the same snapshot may act, and revision exhaustion fails
closed. This is not exclusion across later reconciliations. Inspect, Revoke and
Destroy must be idempotent and provider-fenced under repetition or overlap; the
revision is not the provider ownership fence. Include
all reserved, launch-pending, running and cleanup-pending records in global
capacity. Unexpected/orphan resources remain capacity-consuming until reconciled.
The fake CAS test is not proof of a durable store implementation.

The first durable launch transition can request one launch. After a crash or
uncertain launch result, request inspection instead of retrying creation. Even
if the first request never reached the provider, this conservative planner does
not retry it. A confirmed failed allocation needs fresh admission under a new
identity. This trades availability for avoiding duplicate VMs.

Plain absence never releases a slot: a delayed launch may still complete. The
provider must implement permanent revocation of the allocation/fencing generation,
quiesce every in-flight operation and then prove no resource remains. Only that
`FencedAbsent` evidence releases capacity. A timeout, empty cached inventory or
successful delete request is not sufficient. Destruction targets only a resource
whose provider-reported allocation and generation match the record. Contradictory
resource IDs, generations or post-release orphans are errors, not silent cleanup.
A real provider must enforce the generation at every side effect; copying a number
into metadata does not enforce fencing.

## Still required before any runtime wiring

- Durable transactional reservations/CAS and restart reconciliation, including
  multi-controller concurrency, cleanup failures and orphan inventory.
- Authenticated authoritative provider observations, ownership, revocation and
  quiescence semantics. The planner cannot authenticate caller-supplied structs.
- Verified repository/run/job/attempt/head/checkout/workflow bindings and actual
  runner assignment isolation. Labels are not authorization.
- Approved, pinned synthetic `vgem`/`vkms` guest image and bounded resources, TTL,
  credentials, egress isolation and teardown. No physical GPU, host shares or
  host module loading is implied. No local VM driver currently exists.
- Explicit action-time approval for the concrete host service, KVM/network and
  security changes before enabling any real implementation.

The adapter SDK's reserve/launch/cleanup interface is not changed. An eventual
adapter must fulfill the stronger requirements above, not treat this planner as
an operational provider. GPU CUDA sharing is separate and cannot satisfy these
Mesa/vkms compositor tests by itself.

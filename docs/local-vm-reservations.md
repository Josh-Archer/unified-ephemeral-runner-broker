# Opt-in PostgreSQL VM reservation ledger (unconnected)

`internal/store/vm_reservations.go` adds production PostgreSQL persistence for the
unconnected lifecycle planner. There is no allocator, provider, driver or runtime
caller. `VMReservationSchema` is deliberately NOT included in startup migrations.
The additional CI test step uses the existing disposable PostgreSQL 16 service;
it creates and drops a unique test schema. Never point that test DSN at a live DB.

## Transaction boundaries

A single PostgreSQL guard row is locked `FOR UPDATE` in every reservation,
transition and quarantine transaction. All processes using the same ledger
therefore serialize the global capacity-one decision. The lock is not a
process-local mutex or a filesystem rename. The shared DB and schema are part of
the deployment boundary; two separate databases do not share a cap.

Reservations allocate strictly increasing positive BIGINT fencing generations in
the same transaction as insertion. Exhaustion fails closed. Released allocation
IDs remain tombstones, preventing reuse. Matching live reservations are idempotent;
expiry alone never frees their slot. All nonreleased records consume capacity,
including launch uncertainty and cleanup pending. Malformed data or DB errors
never mean available capacity.

Transitions compare the complete durable record, run the planner and commit the
next record in one transaction. Only a successful return supplies an executable
action. A commit error supplies no action: the commit may have succeeded remotely,
so recovery reads the ledger instead of replaying the stale decision. The planner
will inspect uncertain launches, not request a second creation. Repeated later
reconciliations still require idempotent, provider-fenced actions.

Authoritative provider `FencedAbsent` evidence is still necessary to release a
lease: revoked generation, quiescent in-flight launches and no resource remaining.
This store cannot authenticate that evidence. Unknown/orphan resources are
recorded separately by `Quarantine`; any such record blocks new reservations and
first launches, even after existing leases release. Quarantine has no automatic
clear/delete API. A future independently reviewed reconciler must prove safe
resolution before a removal path exists. Cleanup and cancellation can continue
while quarantined. Quarantine blocks subsequent first-launch decisions, not a
Launch action already committed and handed to a worker. Such an outstanding
action can execute after quarantine commits. Preventing that requires provider
revocation coordinated with launch execution; a last-minute DB read is not an
atomic solution. No such runtime revocation is implemented here.

## Not yet enforced by this component

- Authentication, approved repository/job/attempt/checkout/workflow binding and
  protection against another queued job stealing a runner assignment.
- Real provider ownership/fencing/revocation, orphan discovery and credentials.
- Server-controlled policy for expiry/resource limits and trusted time.
- VM creation, guest image, synthetic DRM, host/network/security setup or GPU lane.
- A live schema migration, application wiring or any activation.

The schema and initializer are source only. Deploying them, connecting a runtime
caller and granting host/VM authority require separately reviewed changes and
applicable action-time approval. The base broker's allocation/pruning tables and
startup schema are unchanged. No VM resources are created by these tests.

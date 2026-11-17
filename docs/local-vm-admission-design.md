# Local VM lane: inert binding contract

Status: design only. `internal/localvmintent` validates candidate syntax and keeps
an immutable copy. It does not authenticate evidence, admit jobs, register
runners, launch VMs, or implement a scheduler. Valid input is still untrusted.
No driver or configuration entry is registered. No production caller exists.

## Trust boundary

A future EvidenceVerifier must independently obtain authenticated GitHub evidence;
never accept caller metadata, runner labels, or a successful syntax check as proof.
Bind repository, run ID, job ID, run attempt, PR head SHA, actual checkout SHA
(including merge SHA where applicable), workflow path and immutable workflow
revision. Verify their relationships and explicit policy approval. Reject stale
attempts, forks and unapproved workflow revisions according to the lane policy.
The current interface has no implementation and supplies no authorization token.

## Hard blockers before any launch integration

* Demonstrate assignment isolation: GitHub labels select runners but are not
  authorization. A different matching queued job must not consume the admission.
* Durable transactional reservation plus monotonically fenced launch ownership;
  restart/retry must not launch twice. An in-memory map is not sufficient.
* Capacity must count reserved, launching, running, cleanup-pending and orphaned
  resources. Reconcile uncertain provider outcomes before reusing capacity.
* Idempotent cleanup and bounded TTL, resource ceilings, image digest and server-
  controlled launch parameters. No arbitrary caller command or device mapping.
* Review credential delivery, guest isolation and host networking independently.
  No cloud spending or VM provisioning is authorized by this document.

The existing externaldispatch adapter offers provision/cleanup/capacity contracts,
but there is no supported disposable local VM implementation. A new named adapter
and independently reviewed local launcher would be required. Desktop reservations
and Kubernetes runner pods are not disposable VMs.

## Next bounded tests (fake implementations only)

Evidence provenance and mismatched repo/job/attempt/revisions; stale admission;
assignment substitution; crash before/after launch acknowledgement; stale fencing
owner; duplicate delivery; cleanup failure and orphan capacity accounting. These
require future verified-evidence and durable-store contracts, not a pretend
in-memory production scheduler. Current tests cover only malformed input and
immutable revision preservation; they do not establish these security properties.

## Test-only relationship model

`relationship_test.go` models comparison through a fake evidence-source seam. It
is not linked into production, implements no EvidenceVerifier, and performs no
network or launch operation. The source receives only lookup identity (repository,
run and job); a real implementation would independently retrieve authenticated
facts. The fake does not prove provenance or GitHub API semantics.

The model compares every immutable candidate field, requires a queued current
attempt and checks an injected freshness policy. Unknown, completed, canceled,
running, superseded, future-dated and stale observations fail closed. Its minute
window is a test fixture, not an operational recommendation. Head checkout must
match the head exactly; synthetic merge checkout must match a separately resolved
merge SHA, not the head. Real API adapters must validate event-specific merge
relationships and workflow provenance rather than trusting these fake facts.

The replay test intentionally succeeds twice: comparison does not consume an
admission. Durable reservation/fencing and actual runner/job assignment isolation
remain mandatory blockers. Rechecking immediately before launch and handling
cancellation between observation and launch are unresolved integration work.

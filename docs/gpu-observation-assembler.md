# GPU observation assembly (no activation)

`internal/gpucollector.Assemble` bridges decoded `gputelemetry.Sample` values to
`gpuadmission.Observation`. It is production pure transformation code, not an
HTTP client, authenticated collector, allocator caller or enabled runner profile.
No endpoint, credentials, query, runtime wiring or default policy is supplied.

## Server-owned contracts

The caller must independently establish authenticated transport, whole-device
node/UUID bindings, query semantics and complete protected-workload coverage.
Neither successful decoding nor `Config` authenticates these facts. Configuration
must originate from the trusted server/operator, never PR content or allocation
metadata. Inputs remain a trusted provider boundary, not a public API accepting
untrusted admission requests.

The configured unit for every metric is mandatory: memory in bytes, counters in
counts and latency in seconds. Other units fail closed; MiB-to-bytes conversion
must not be inferred. The explicit `zero-device-errors-means-healthy` contract is
only appropriate for a server-validated device-health metric whose zero value
really establishes the required error-health coverage. It is not a boolean
approval claim accepted from a payload. Missing/unknown health is not healthy;
nonzero errors yield `HealthFault` for subsequent policy rejection.

The server declares a nonempty, unique set of required protected workload IDs and
the exact label carrying that identity. Each workload must supply distinct latency
and error samples with matching workload, node and physical GPU UUID labels. There
is no caller-supplied `complete` flag or substitute aggregate. Missing, extra or
cross-device coverage fails closed. If workloads span devices, the caller must
resolve the complete applicable set for each device; it must not silently omit
host processes. A matching label does not itself prove completeness or exporter
semantics. The transport/collector must still reject ambiguous original series
using the decoder and establish whole-device rather than per-container queries.

## Exact values and time

Integer fields use the preserved `ValueText`, never its rounded float. Bounded
decimal/exponent parsing accepts exact nonnegative uint64 values and rejects
fractions, overflow, NaN, infinity and oversized representations. Representation
limits are 128 characters and exponent magnitude 128, not operational thresholds.
Latency uses the exact decimal value and rounds upward when float conversion
would round down, preventing an exact value above a policy threshold from becoming
equal to it. Error aggregation is checked for uint64 overflow.

Every input timestamp is checked independently against injected current time and
explicit policy freshness before grouping. Memory and slice groups retain the
oldest constituent timestamp. Protected latency is the maximum across the required
workloads, errors are their checked sum, and each retains the oldest constituent
time, including workloads that did not produce the maximum. This avoids renewing
stale evidence through aggregation. Reservations retain their independent time.

Reservation counts must come separately from the authoritative durable global
ledger, including launching, running, cleanup and orphan states. They cannot be
inferred from Prometheus or supplied as an authorization boolean. Assembly copies
the reservation sample but does not authenticate it, consume it or make an atomic
reservation. Mutating inputs concurrently with assembly is unsupported.

## Required next integration

The caller must invoke `gpuadmission.Evaluate` with the same server policy after
assembly; successful assembly is not pressure admission. Health faults, nonzero
reservations and measured pressure remain in the returned observation rather than
being discarded. Atomic global-one reservation, fencing, provider-side outstanding
action revocation, assignment authorization and launch-time revalidation remain
separate responsibilities. No store or VM provider is changed here.

A future HTTP adapter still requires review of authenticated TLS transport,
server-owned URL allowlisting, redirect policy, SSRF defenses, bounded response
sizes/timeouts, timestamp semantics and complete exporter health/host-process
coverage. No transport is implemented in this milestone. Actual streaming SLO
baselines and approved policy thresholds remain prerequisites; this package does
not invent them or imply time slicing provides memory/fault isolation.

# GPU telemetry decoding boundary (not activated)

`internal/gputelemetry` is a pure production decoder, not an authenticated
collector. It makes no network calls and has no allocator caller, query defaults,
exporter registration or runtime configuration. Supplying JSON does not establish
provenance, eligibility or permission to launch.

## Input contract

The future server collector must authenticate and authorize its configured
Prometheus endpoint over verified transport, bound response sizes, enforce request
timeouts, reject unsuccessful HTTP responses and independently resolve a node to
a whole physical GPU UUID. Neither the endpoint, label schema, expected identity,
queries nor policy may come from a PR or allocation request. Server configuration
must specify exporter metric names, units and query semantics. Queries must retain
physical node/UUID labels and original sample timestamps: aggregation or recording
rules that hide missing components or substitute evaluation time are insufficient.
The JSON timestamp is the returned sample timestamp, not proof of the underlying
exporter's observation time; the collector must establish that separately.

`Decode` accepts only a successful instant vector with no upstream errors,
warnings or infos. It rejects duplicate JSON keys, malformed/nonfinite/negative
values, missing identity labels, absent or ambiguous matching series and stale or
future matching samples. Additional devices are allowed but are never max-reduced
into the requested device. Envelope/result keys are exact and case-sensitive; label case is preserved.
The original value string (`ValueText`) is retained for exact downstream integer
and range checks; the approximate float value must not substitute for that evidence.
Labels and the original numeric timestamp text survive
decoding; fractional timestamps are interpreted exactly to nanosecond resolution.
Unsupported finer precision or timestamps outside positive int64 Unix nanoseconds
fail closed. Freshness and current time are mandatory caller inputs, with no
operational default. The boundary is inclusive at exactly the maximum age.

`DecodeBundle` requires independent memory total/used bytes, slice
capacity/used/pending, device error, protected latency and protected error inputs.
Every input retains its own timestamp. These are semantic keys, not invented
DCGM/Prometheus metric names. A missing protected-workload signal is not zero.
The future collector must resolve all protected workloads and establish the
meaning and completeness of the requested aggregate; one arbitrary matching
workload series does not prove coverage. Missing counters must not be replaced by
zero. Cardinality, metric type, integer range and unit conversion remain required
before constructing `gpuadmission.Observation`; successful decoding alone is not
admission. Nonzero device errors are retained, not interpreted as healthy.

## Deliberately separate integration work

Existing `internal/tier/promclient.Client.QueryInstant` returns a scalar and
reduces vectors to their largest value, losing labels and timestamps. It remains
unchanged and is unsuitable for this collector. The new decoder does not implement
`gpuadmission.Source`. The authenticated server adapter still needs verified
whole-device UUID mappings, individual exporter observation times, health semantics
and complete protected-workload inputs. Durable global reservations come from the
fenced lifecycle store, never a Prometheus gauge or payload approval boolean.

Whole-device telemetry must include desktop host processes outside Kubernetes.
NVIDIA time slicing does not provide VRAM/fault isolation or reliable per-container
DCGM attribution. The prior inventory did not establish UUID/error-health mapping
or actual Plex/Jellyfin/DeskQuest streaming latency/error baselines. Freshness,
headroom and protected SLO policies remain unset pending measurement and approval.

There is no store/vmlifecycle change here. Atomic global-one reservation,
launch-time revalidation, cleanup/orphan accounting and assignment authorization
remain separate requirements described in `gpu-admission-policy.md`. No runner,
GPU workload, cluster configuration or live collector is enabled by this package.

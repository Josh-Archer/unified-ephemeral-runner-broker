# Runner Scale Set listener

Demand detection is webhook-first, with a GitHub queued-run scan as the safety
net. The scan is O(repos × runs), rate-limited, and has to guess a time
window. This listener is the first step off that scan. It follows the
[actions/scaleset](https://github.com/actions/scaleset) v0.5.0 message-session
pattern (public preview, MIT): long-poll a scale-set session, scale from
`statistics.totalAssignedJobs`, and acknowledge a message only after `Scale`
returns nil. A crash replays the message instead of dropping it.

The broker does not import `github.com/actions/scaleset`. That module requires
Go 1.27 and pulls the examples' Docker and Cobra graph. `internal/scaleset`
keeps the same poll and ack loop, the same message batch shape, and a `Client`
interface a later change can satisfy with the upstream client.

## What this slice does

- One listener per pool that sets `scaleSet.name` and `scaleSet.id`, started
  only when `broker.scaleSetListener.enabled` is true.
- The listener reads a message session from a Kubernetes secret. It polls that
  queue. It does not call the queued-workflow-run or workflow-job list APIs.
- `TotalAssignedJobs - (running + allocating)` is the deficit. `running` is
  `ready` and `warm`. `allocating` is `reserved` and `pending`.
- `mode: observe` (the default) logs that delta and leaves webhooks and the
  queued-run reconcile running. A non-zero delta is a disagreement: GitHub
  assigned work the broker has not covered, or the broker is ahead.
- `mode: allocate` places the deficit through the existing allocate path, so
  tier routing, fair-share quotas, and backend fallback still apply. For those
  pools it also stops webhook allocation and, when every webhook pool is in
  this mode, skips the queued-run scan entirely. Other pools keep the scan.
- An empty long poll (HTTP 202) reapplies the last statistics, so a
  capacity-limited scale-up can finish without a new job message.
- Capacity and quota errors acknowledge the message. The next poll retries the
  remainder. Any other error leaves the message unacked.
- With HA, only the reconciliation leader polls. Losing the lease stops the
  poll before the next scale. An in-flight acknowledgement still completes.

Runners are labeled `self-hosted` and the scale set name. Workflows target
that name with `runs-on`.

## Configuration

```yaml
broker:
  scaleSetListener:
    enabled: true
    mode: observe          # observe (default) or allocate
    sessionSecretRef: uecb-scaleset-sessions
pools:
  - name: uecb-shared
    scaleSet:
      name: uecb-shared    # runs-on label and secret key
      id: 17               # id returned when the scale set was registered
    webhookLabels: [self-hosted, uecb-shared]  # still used in observe mode
```

The secret's keys are scale set names. Each value is JSON:

```json
{
  "sessionId": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
  "messageQueueUrl": "https://pipelines.actions.githubusercontent.com/...",
  "messageQueueAccessToken": "<redacted>",
  "statistics": {"totalAssignedJobs": 0}
}
```

The broker never logs that token. A 401 re-reads the secret once. Creating and
refreshing the session against the admin API is not implemented yet: mint the
session with the `actions/scaleset` client (or the Runner Scale Set REST API)
and mount it. Rotate the secret to rotate the token; the listener re-reads it
after a 401 and again whenever it restarts.

Helm: `config.broker.scaleSetListener` in
`charts/unified-ephemeral-runner-broker/values.yaml`. The chart renders it into
the broker config. It does not apply the release.

## Cutover

Keep observe mode on for one release next to webhooks and reconcile.

1. Register one scale set per pool (`uecb-shared`, home-queue, desktop). A
   scale set can be scoped to a personal account, which removes the repo list.
   Confirm that scope, and confirm private-repo access still matches the rules
   from #190 and #193, before pointing production jobs at it. The API is a
   public preview.
2. Mint a message session per scale set and store it in `sessionSecretRef`.
3. Enable `mode: observe`. Compare `scale set decision` logs and
   `uecb_scaleset_decisions_total{outcome="disagree"}` with webhook
   allocations. Jobs that never appeared in the queued-run scan should show up
   here as `totalAssignedJobs`.
4. On a test pool only, switch that pool to `mode: allocate`. Webhook
   `queued` deliveries for it are ignored with reason `scale_set_listener`.
   The queued-run scan no longer allocates it, and if it was the last webhook
   pool the scan does not list runs at all.
5. After JIT registration (below) is in place and the test pool agrees with
   GitHub, move the remaining pools to `allocate` and delete the deep
   queued-run scan. Keep `workflow_job` webhooks for telemetry (`in_progress`,
   `completed`) until job-lifecycle messages replace them.

Do not enable `allocate` on a production pool yet. Runners still register with
the existing registration-token path. They are not scale-set JIT runners, so
GitHub will not hand them jobs that were assigned only to the scale set.
`AcquireJobs` is intentionally not called: acquiring a `JobAvailable` without
a JIT runner strands that job on the scale set. Observe mode never acquires.

## Remaining work

- Admin client: create or adopt the scale set, open the message session, and
  refresh it. Replace the mounted session JSON.
- `GenerateJitRunnerConfig` on allocate, and pass that config to the runner
  instead of a registration token (#120).
- Call `AcquireJobs` for `JobAvailable` only after JIT registration exists.
- Scale down from `JobCompleted` (the listener parses those messages and does
  not act on them). Until then, extra runners expire through the existing
  registration and job timeouts.
- Delete the queued-run reconcile once every pool is on `allocate` and the
  disagreement metric stays at zero.
- Personal-account scope and private-repo visibility against the preview API.

## Metrics

- `uecb_scaleset_decisions_total{pool,mode,outcome}` — `match`, `disagree`,
  `allocated`, or `capacity`. Repeated identical decisions are not counted again.
- `uecb_scaleset_runners_allocated_total{pool}` — runners placed from
  statistics. Stays at zero in observe mode.
- `uecb_github_webhook_events_ignored_total{reason="scale_set_listener"}` —
  webhook `queued` deliveries skipped because allocate mode owns the pool.

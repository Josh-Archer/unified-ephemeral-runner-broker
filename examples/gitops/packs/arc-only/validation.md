# Validation — arc-only Pack

Run these checks after initial install or after any config change to confirm the pack is healthy.

## 1. Broker Deployment

```bash
kubectl rollout status deployment/unified-ephemeral-runner-broker -n arc-systems
# Expected: "deployment "unified-ephemeral-runner-broker" successfully rolled out"
```

## 2. Pod Logs — No Error at Startup

```bash
kubectl logs -n arc-systems -l app.kubernetes.io/name=unified-ephemeral-runner-broker --tail=50
```

Expected: log lines showing `config loaded`, `github auth initialized`, and `broker listening` with no `ERROR` entries.

## 3. Metrics Endpoint

```bash
kubectl port-forward -n arc-systems svc/unified-ephemeral-runner-broker 8080:8080 &
curl -s http://localhost:8080/metrics | grep uecb_
```

Expected: Prometheus metrics including `uecb_http_requests_total`, `uecb_queue_depth`, and `uecb_capacity_utilization_ratio` are present.

## 4. Allocate a Test Runner (Dry Run)

If you have a test workflow that calls `allocate-runner`, trigger it against the staging broker URL and confirm it returns a runner label within 60 seconds.

Alternatively, use `curl` with a valid OIDC token:

```bash
curl -s -X POST http://localhost:8080/v1/allocations \
  -H "Authorization: Bearer <OIDC_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"pool":"full","job_timeout":"5m"}' | jq .
```

Expected: response contains `runner_label` and `correlation_id`.

## 5. Confirm ARC Backends Are Healthy

```bash
curl -s http://localhost:8080/metrics \
  | grep 'uecb_capacity_utilization_ratio{pool="full",backend="arc"}'
```

Expected: metric is present (value 0 when idle is correct).

## Optional: Recurring Health CronJob

Deploy this CronJob to run the core checks automatically every 15 minutes. The CronJob runs inside the cluster and does not require GitHub Actions access.

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: uecb-health-check
  namespace: arc-systems
spec:
  schedule: "*/15 * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          serviceAccountName: unified-ephemeral-runner-broker
          restartPolicy: OnFailure
          containers:
            - name: check
              image: curlimages/curl:8.6.0
              command:
                - /bin/sh
                - -c
                - |
                  set -e
                  echo "=== broker health ==="
                  curl -sf http://unified-ephemeral-runner-broker.arc-systems.svc.cluster.local:8080/metrics \
                    | grep -q uecb_queue_depth \
                    && echo "metrics OK" \
                    || { echo "metrics endpoint failed"; exit 1; }
                  echo "=== done ==="
```

This CronJob follows the Safe Offload Pattern: periodic infrastructure checks run inside the cluster rather than as GitHub Actions scheduled workflows.

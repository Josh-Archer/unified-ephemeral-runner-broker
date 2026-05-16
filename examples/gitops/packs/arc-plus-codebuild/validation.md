# Validation — arc-plus-codebuild Pack

Run these checks after initial install or after any config change.

## 1. Broker Deployment

```bash
kubectl rollout status deployment/unified-ephemeral-runner-broker -n arc-systems
```

Expected: `"deployment "unified-ephemeral-runner-broker" successfully rolled out"`

## 2. Pod Logs — No Error at Startup

```bash
kubectl logs -n arc-systems -l app.kubernetes.io/name=unified-ephemeral-runner-broker --tail=50
```

Expected: log lines showing `config loaded`, `github auth initialized`, `backend codebuild registered`, and `broker listening` with no `ERROR` entries.

## 3. Metrics Endpoint

```bash
kubectl port-forward -n arc-systems svc/unified-ephemeral-runner-broker 8080:8080 &
curl -s http://localhost:8080/metrics | grep uecb_
```

Expected: `uecb_capacity_utilization_ratio` metrics for both `arc` and `codebuild` backends in the `lite` pool are present.

## 4. Confirm Both Backends Are Healthy

```bash
curl -s http://localhost:8080/metrics \
  | grep 'uecb_capacity_utilization_ratio{pool="lite"'
```

Expected output includes lines for both `backend="arc"` and `backend="codebuild"`.

## 5. Confirm Weighted Routing

With multiple allocations, ARC should receive approximately 3 out of every 4 allocations (weight 3 vs weight 1). Check the `uecb_allocations_total` counter:

```bash
curl -s http://localhost:8080/metrics \
  | grep 'uecb_allocations_total{pool="lite"'
```

After a burst of allocations, the ARC count should be roughly 3× the CodeBuild count.

## 6. Confirm Warm Capacity (Optional)

If warm capacity is configured (`warmMin: 1`), check that a warm slot is maintained:

```bash
curl -s http://localhost:8080/metrics \
  | grep 'uecb_queue_depth{pool="lite",state="warm"'
```

Expected: value is at least `1` when the broker has had time to initialize warm slots.

## Optional: Recurring Health CronJob

Deploy this CronJob to run core checks every 15 minutes inside the cluster without GitHub Actions access.

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
                  BASE=http://unified-ephemeral-runner-broker.arc-systems.svc.cluster.local:8080
                  echo "=== broker health ==="
                  curl -sf "${BASE}/metrics" | grep -q uecb_queue_depth \
                    && echo "metrics OK" || { echo "metrics endpoint failed"; exit 1; }
                  echo "=== arc backend ==="
                  curl -sf "${BASE}/metrics" \
                    | grep -q 'uecb_capacity_utilization_ratio{pool="lite",backend="arc"}' \
                    && echo "arc metric present" || { echo "arc metric missing"; exit 1; }
                  echo "=== codebuild backend ==="
                  curl -sf "${BASE}/metrics" \
                    | grep -q 'uecb_capacity_utilization_ratio{pool="lite",backend="codebuild"}' \
                    && echo "codebuild metric present" || { echo "codebuild metric missing"; exit 1; }
                  echo "=== done ==="
```

This CronJob follows the Safe Offload Pattern: periodic checks run inside the cluster rather than as GitHub Actions scheduled workflows.

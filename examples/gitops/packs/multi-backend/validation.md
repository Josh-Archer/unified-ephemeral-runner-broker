# Validation — multi-backend Pack

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

Expected: log lines showing `config loaded`, `github auth initialized`, all four backends registered, and `broker listening` with no `ERROR` entries.

## 3. Metrics Endpoint

```bash
kubectl port-forward -n arc-systems svc/unified-ephemeral-runner-broker 8080:8080 &
curl -s http://localhost:8080/metrics | grep uecb_
```

Expected: `uecb_capacity_utilization_ratio` for all four backends in the lite pool (`arc`, `codebuild`, `lambda`, `cloud-run`).

## 4. Confirm All Backends Are Healthy

```bash
curl -s http://localhost:8080/metrics \
  | grep 'uecb_capacity_utilization_ratio{pool="lite"'
```

Expected output includes four lines, one for each backend.

## 5. Confirm Fair-Share Is Active

Send a few allocations with different tenant values and check that the broker distributes load:

```bash
curl -s http://localhost:8080/metrics \
  | grep 'uecb_allocations_total{pool="lite"'
```

The fair-share admission layer does not have a dedicated metric, but allocation distribution across backends reflects its effect under tenant load.

## 6. Confirm Warm Capacity

All three external backends are configured with `warmMin: 1`. Check that warm slots are initialized:

```bash
curl -s http://localhost:8080/metrics \
  | grep 'uecb_queue_depth{pool="lite",state="warm"'
```

Expected: value is at least `1` per enabled external backend after startup.

## Optional: Recurring Health CronJob

Deploy this CronJob to run core checks every 15 minutes inside the cluster. This follows the Safe Offload Pattern: periodic infrastructure checks run as a Kubernetes CronJob, not as GitHub Actions scheduled workflows.

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
                  for backend in arc codebuild lambda cloud-run; do
                    echo "=== backend: ${backend} ==="
                    curl -sf "${BASE}/metrics" \
                      | grep -q "uecb_capacity_utilization_ratio{pool=\"lite\",backend=\"${backend}\"}" \
                      && echo "${backend} metric present" \
                      || { echo "${backend} metric missing"; exit 1; }
                  done
                  echo "=== done ==="
```

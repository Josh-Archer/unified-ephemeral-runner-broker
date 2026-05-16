# Rollout Notes — multi-backend Pack

## Prerequisites

- ARC is installed in the cluster with `arc-full` and `arc-lite` runner scale set templates.
- CodeBuild, Lambda, and Cloud Run launcher controllers are deployed and reachable from the cluster.
- All four Kubernetes Secrets exist in the broker namespace. See `secrets.md`.
- `kubectl` is configured for the target cluster.
- `kustomize` v5 or later is available.

## Phased Enablement

Enable backends incrementally to isolate issues during rollout. Start with ARC, then enable each external backend after validating the previous phase.

### Phase 1: ARC Only

Deploy the arc-only pack first and validate. See the arc-only pack's `rollout.md`.

### Phase 2: Enable CodeBuild

1. Create the `uecb-codebuild` secret.
2. Apply the arc-plus-codebuild staging overlay and validate.

See the arc-plus-codebuild pack's `rollout.md` for detailed steps.

### Phase 3: Enable Lambda and Cloud Run

3. Create the `uecb-lambda` and `uecb-cloud-run` secrets:

   ```bash
   kubectl create secret generic uecb-lambda \
     --namespace arc-systems \
     --from-literal=dispatch_url=https://<YOUR_LAMBDA_DISPATCHER> \
     --from-literal=dispatch_token=<TOKEN>

   kubectl create secret generic uecb-cloud-run \
     --namespace arc-systems \
     --from-literal=dispatch_url=https://<YOUR_CLOUD_RUN_DISPATCHER> \
     --from-literal=dispatch_token=<TOKEN>
   ```

4. Apply the multi-backend staging overlay:

   ```bash
   kubectl apply -k examples/gitops/packs/multi-backend/overlays/staging
   ```

5. Run the post-install health checks. See `validation.md`.

6. Promote to production. Open a pull request in your private overlay repo targeting the production overlay. After merge, ArgoCD or Flux syncs the production cluster.

## Rolling Update

Config changes follow the GitOps pull request flow. The broker performs a rolling update when the ConfigMap hash changes.

To adjust fair-share priority classes or warm capacity settings, update the overlay patch and open a pull request.

## Updating the Image Tag

Pin the broker image tag in a `patch-image.yaml` in your private overlay:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: unified-ephemeral-runner-broker
  namespace: arc-systems
spec:
  template:
    spec:
      containers:
        - name: broker
          image: ghcr.io/josh-archer/unified-ephemeral-runner-broker/broker:v0.2.0
```

## Rollback

### Rollback a single backend

To stop routing to a specific backend without a full rollback, set that backend's `enabled: false` via a GitOps pull request.

Example: disable Cloud Run only:

```yaml
pools:
  - name: lite
    backends:
      cloud-run:
        enabled: false
```

ARC, CodeBuild, and Lambda continue to serve allocations.

### Full rollback via GitOps

Revert or create a new pull request restoring the previous config. ArgoCD or Flux reconciles the change.

### Emergency rollback via kubectl (last resort)

```bash
kubectl rollout undo deployment/unified-ephemeral-runner-broker -n arc-systems
```

Reconcile the GitOps state as soon as possible after any out-of-band change.

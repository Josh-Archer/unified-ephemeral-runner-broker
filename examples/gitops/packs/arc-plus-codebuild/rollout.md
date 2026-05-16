# Rollout Notes — arc-plus-codebuild Pack

## Prerequisites

- ARC is installed in the cluster with `arc-full` and `arc-lite` runner scale set templates configured.
- The CodeBuild launcher controller is deployed and reachable from the cluster at a known HTTPS endpoint.
- Both required Kubernetes Secrets exist in the broker namespace. See `secrets.md`.
- `kubectl` is configured for the target cluster.
- `kustomize` v5 or later is available.

## Initial Install

### Phase 1: Deploy with ARC only

Deploy first without enabling CodeBuild to confirm the broker and ARC are healthy before adding the external backend.

1. Deploy the arc-only pack to staging:

   ```bash
   kubectl apply -k examples/gitops/packs/arc-only/overlays/staging
   ```

2. Validate using the arc-only pack's `validation.md`. Confirm ARC allocations succeed.

### Phase 2: Enable CodeBuild

3. Create the `uecb-codebuild` secret:

   ```bash
   kubectl create secret generic uecb-codebuild \
     --namespace arc-systems \
     --from-literal=dispatch_url=https://<YOUR_DISPATCHER_ENDPOINT> \
     --from-literal=dispatch_token=<TOKEN>
   ```

4. Apply the arc-plus-codebuild staging overlay:

   ```bash
   kubectl apply -k examples/gitops/packs/arc-plus-codebuild/overlays/staging
   ```

5. Run the post-install health checks. See `validation.md`.

6. Promote to production. Open a pull request in your private overlay repo. After merge, ArgoCD or Flux syncs the production overlay.

## Rolling Update

Config changes follow the same GitOps pull request flow. The broker performs a rolling update when the ConfigMap hash changes.

To change CodeBuild capacity limits or warm capacity settings, update the overlay patch and open a pull request:

```yaml
backends:
  codebuild:
    maxRunners: 5
    warmMin: 2
    warmMax: 3
    warmTTL: 8m
```

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

### Rollback CodeBuild only

To stop routing to CodeBuild without a full rollback, set `backends.codebuild.enabled: false` via a GitOps pull request. ARC continues to serve all allocations.

### Full rollback via GitOps

Revert or create a new pull request in your private overlay repo restoring the previous config. ArgoCD or Flux reconciles the change.

### Emergency rollback via kubectl (last resort)

```bash
kubectl rollout undo deployment/unified-ephemeral-runner-broker -n arc-systems
```

Reconcile the GitOps state as soon as possible after any out-of-band change.

# Rollout Notes — arc-only Pack

## Prerequisites

- ARC (Actions Runner Controller) is installed in the cluster and `arc-full` and `arc-lite` runner scale set templates are configured.
- The `uecb-github-app` Kubernetes Secret exists in the broker namespace. See `secrets.md`.
- `kubectl` is configured for the target cluster.
- `kustomize` v5 or later is available, or you are using `kubectl apply -k`.

## Initial Install

1. **Render and review the manifests.**

   ```bash
   kustomize build examples/gitops/packs/arc-only/overlays/staging
   ```

   Review the output. Confirm the ConfigMap contains the correct `organization` value and ARC template names.

2. **Apply the staging overlay.**

   ```bash
   kubectl apply -k examples/gitops/packs/arc-only/overlays/staging
   ```

3. **Verify the broker is ready.**

   ```bash
   kubectl rollout status deployment/unified-ephemeral-runner-broker -n arc-systems
   ```

4. **Run the post-install health checks.** See `validation.md`.

5. **Promote to production.** Open a pull request in your private overlay repo updating the environment ref to production. After merge, ArgoCD or Flux syncs the production overlay. Confirm health checks pass.

## Rolling Update

When you change the broker config (for example, increasing `maxRunners` or changing the ARC template name):

1. Edit the overlay patch in your private overlay repo.
2. Open a pull request. Review the diff with:
   ```bash
   kustomize build overlays/production | kubectl diff -f -
   ```
3. Merge the pull request. ArgoCD or Flux applies the change. The broker deployment performs a rolling update because the ConfigMap annotation hash changes.

## Updating the Image Tag

Pin image tags in your private overlay using a `patch-image.yaml` strategic merge patch:

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

Never use `latest` in production overlays.

## Rollback

### Immediate rollback via GitOps

Revert or create a new pull request in your private overlay repo restoring the previous config. ArgoCD or Flux reconciles the change.

### Emergency rollback via kubectl (last resort)

If GitOps sync is unavailable and you need an immediate rollback:

```bash
kubectl rollout undo deployment/unified-ephemeral-runner-broker -n arc-systems
```

This reverts the Deployment to the previous revision. The ConfigMap is not rolled back by `kubectl rollout undo`. Update the GitOps state as soon as possible to keep the cluster and repository in sync.

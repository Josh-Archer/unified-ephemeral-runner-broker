# Secret Contract — multi-backend Pack

This pack requires four Kubernetes Secrets in the broker namespace.

## Required Secret: `uecb-github-app`

Same requirement as the arc-only pack. See [arc-only/secrets.md](../arc-only/secrets.md) for the full key list.

## Required Secret: `uecb-codebuild`

See [arc-plus-codebuild/secrets.md](../arc-plus-codebuild/secrets.md) for the full key list.

## Required Secret: `uecb-lambda`

The broker uses this secret to dispatch allocation requests to your AWS Lambda launcher controller.

```bash
kubectl create secret generic uecb-lambda \
  --namespace arc-systems \
  --from-literal=dispatch_url=https://<YOUR_LAMBDA_DISPATCHER_ENDPOINT> \
  --from-literal=cleanup_url=https://<YOUR_LAMBDA_DISPATCHER_ENDPOINT>/cleanup \
  --from-literal=dispatch_token=<BEARER_TOKEN_FOR_DISPATCHER>
```

| Key | Type | Description |
|-----|------|-------------|
| `dispatch_url` | HTTPS URL | The HTTP endpoint of your Lambda launcher controller |
| `cleanup_url` | HTTPS URL | Optional. POST target for cancel/expire teardown; omit to skip provider cleanup |
| `dispatch_token` | string | Optional bearer token; leave blank if the endpoint uses a network boundary instead |

The Lambda launcher is a private controller that wraps the `lambda` OCI image published from this repository. The Lambda function image must be mirrored into ECR because AWS Lambda requires the function image to reside in a registry in the same account. The broker only calls the dispatcher endpoint; it does not interact with ECR or Lambda directly.

## Required Secret: `uecb-cloud-run`

The broker uses this secret to dispatch allocation requests to your GCP Cloud Run launcher controller.

```bash
kubectl create secret generic uecb-cloud-run \
  --namespace arc-systems \
  --from-literal=dispatch_url=https://<YOUR_CLOUD_RUN_DISPATCHER_ENDPOINT> \
  --from-literal=cleanup_url=https://<YOUR_CLOUD_RUN_DISPATCHER_ENDPOINT>/cleanup \
  --from-literal=dispatch_token=<BEARER_TOKEN_FOR_DISPATCHER>
```

| Key | Type | Description |
|-----|------|-------------|
| `dispatch_url` | HTTPS URL | The HTTP endpoint of your Cloud Run launcher controller |
| `cleanup_url` | HTTPS URL | Optional. POST target for cancel/expire teardown; omit to skip provider cleanup |
| `dispatch_token` | string | Optional bearer token |

## Secret Provisioning Order

The broker validates all configured `secretRef` objects at startup. If any required secret is missing, the pod stays unready. Recommended provisioning order:

1. `uecb-github-app` — broker cannot start without this.
2. `uecb-codebuild`, `uecb-lambda`, `uecb-cloud-run` — broker validates these before marking external backends available.

Provision all four secrets before applying the multi-backend overlay. Use External Secrets Operator, Sealed Secrets, or Vault Agent Injector to manage secret lifecycle without storing values in Git.

## Disabling a Backend Without Removing Its Secret

To stop routing to a backend, set `backends.<name>.enabled: false` in the ConfigMap patch. The secret can remain in the cluster. This is the recommended approach for temporarily draining a cloud provider without losing the dispatcher configuration.

# Secret Contract — arc-plus-codebuild Pack

This pack requires two Kubernetes Secrets in the broker namespace.

## Required Secret: `uecb-github-app`

Same requirement as the arc-only pack. See [arc-only/secrets.md](../arc-only/secrets.md) for the full key list.

```bash
kubectl create secret generic uecb-github-app \
  --namespace arc-systems \
  --from-literal=app_id=<YOUR_APP_ID> \
  --from-literal=installation_id=<YOUR_INSTALLATION_ID> \
  --from-file=private_key=<PATH_TO_PEM_FILE>
```

## Required Secret: `uecb-codebuild`

The broker uses this secret to dispatch runner allocation requests to your CodeBuild launcher controller.

```bash
kubectl create secret generic uecb-codebuild \
  --namespace arc-systems \
  --from-literal=dispatch_url=https://<YOUR_CODEBUILD_DISPATCHER_ENDPOINT> \
  --from-literal=cleanup_url=https://<YOUR_CODEBUILD_DISPATCHER_ENDPOINT>/cleanup \
  --from-literal=dispatch_token=<BEARER_TOKEN_FOR_DISPATCHER>
```

| Key | Type | Description |
|-----|------|-------------|
| `dispatch_url` | HTTPS URL | The HTTP endpoint of your CodeBuild launcher controller that accepts broker dispatch requests |
| `cleanup_url` | HTTPS URL | Optional. Endpoint the broker POSTs on cancel/expire/release so the launcher can tear down the runner. Omit to skip provider teardown (broker capacity still releases). Prefer idempotent handlers (2xx and 404 both OK). |
| `dispatch_token` | string | Optional bearer token sent in the `Authorization` header to the dispatcher (and cleanup when set); leave blank if your dispatcher uses a network boundary instead of a token |

Cleanup POST body includes `action: "cleanup"`, `allocation_id`, `runner_label`, `backend`, `pool`, `state`, optional `correlation_id` / `error`, and allocation `metadata` (for example `execution_id` from provision).

The dispatcher endpoint is operated by your private CodeBuild launcher controller (not part of this repository). See `docs/architecture.md` for the external dispatch contract.

## Secret Provisioning

Never store secret values in this repository or any GitOps overlay repository. Provision secrets via External Secrets Operator, Sealed Secrets, or Vault Agent Injector.

The broker validates all configured `secretRef` objects at startup using the Kubernetes API. The pod stays unready until both secrets exist and contain the required keys.

## Disabling CodeBuild Without Removing the Secret

If you need to stop routing to CodeBuild temporarily, set `backends.codebuild.enabled: false` in the ConfigMap patch. The secret can remain in the cluster. Only the `enabled` flag controls whether the broker routes to that backend.

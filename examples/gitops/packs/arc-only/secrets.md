# Secret Contract — arc-only Pack

The arc-only pack requires exactly one Kubernetes Secret in the broker namespace.

## Required Secret: `uecb-github-app`

The broker authenticates to the GitHub API using a GitHub App. The app private key and registration metadata must be present as a Kubernetes Secret before the broker will become ready.

Create the secret in the same namespace as the broker (`arc-systems` by default):

```bash
kubectl create secret generic uecb-github-app \
  --namespace arc-systems \
  --from-literal=app_id=<YOUR_APP_ID> \
  --from-literal=installation_id=<YOUR_INSTALLATION_ID> \
  --from-file=private_key=<PATH_TO_PEM_FILE>
```

| Key | Type | Description |
|-----|------|-------------|
| `app_id` | string | GitHub App numeric ID |
| `installation_id` | string | Installation ID for the target org or repository |
| `private_key` | PEM file | RSA private key downloaded from the GitHub App settings page |

## Secret Provisioning

Do not store secret values in this repository or any GitOps overlay repository. Use one of the following approaches:

- **External Secrets Operator**: define an `ExternalSecret` resource pointing at your secret store (Vault, AWS Secrets Manager, Azure Key Vault, GCP Secret Manager). The operator syncs the Kubernetes Secret before the broker pod starts.
- **Sealed Secrets**: encrypt the secret with your cluster's public key and commit the `SealedSecret` resource to your private overlay repo.
- **Vault Agent Injector**: annotate the broker pod to inject the secret as a projected volume.

The broker validates the `secretRef` at startup using the Kubernetes API. The pod stays in `Pending` or `CrashLoopBackOff` until the secret exists and contains the required keys.

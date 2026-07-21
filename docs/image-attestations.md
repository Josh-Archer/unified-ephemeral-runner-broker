# Runtime Image Provenance and SBOM Attestations

The `Publish Runtime Images` workflow
([`.github/workflows/publish-runtime-images.yml`](../.github/workflows/publish-runtime-images.yml))
publishes digest-bound build provenance and SPDX SBOM attestations for:

| Image | GHCR package |
| --- | --- |
| Cloud Run | `ghcr.io/josh-archer/uecb-cloud-run` |
| Lambda | `ghcr.io/josh-archer/uecb-lambda` |
| Azure Functions | `ghcr.io/josh-archer/uecb-azure-functions` |

## Design constraints

- **Lambda single-manifest contract**: AWS Lambda requires
  `application/vnd.docker.distribution.manifest.v2+json` (not an OCI index or
  multi-manifest list). BuildKit built-in `provenance` / `sbom` outputs stay
  **disabled** on `docker/build-push-action` so the primary image is unchanged.
- **Digest-bound attestations**: After push, the workflow captures the image
  digest and generates SLSA provenance plus an SPDX JSON SBOM with pinned Syft
  (`anchore/sbom-action` + `SYFT_VERSION`). Attestations are signed via
  short-lived Sigstore certificates (OIDC) and stored in the GitHub Attestations
  API. They are also attached as OCI registry referrers without rewriting the
  subject Docker v2 manifest.
- **Least-privilege permissions**: `contents: read`, `packages: write`,
  `id-token: write`, `attestations: write`.

## Publish

```bash
gh workflow run publish-runtime-images.yml \
  --repo Josh-Archer/unified-ephemeral-runner-broker \
  -f tag=vX.Y.Z
```

## Consumer verification

Replace `TAG` (or use `@sha256:…` digests) and authenticate to GHCR if the
package visibility requires it.

### Prerequisites

```bash
# GitHub CLI with attestation support (gh >= 2.49)
gh auth login
docker login ghcr.io
```

### Verify build provenance (default predicate)

```bash
REPO=Josh-Archer/unified-ephemeral-runner-broker

gh attestation verify oci://ghcr.io/josh-archer/uecb-cloud-run:TAG \
  -R "${REPO}"

gh attestation verify oci://ghcr.io/josh-archer/uecb-lambda:TAG \
  -R "${REPO}"

gh attestation verify oci://ghcr.io/josh-archer/uecb-azure-functions:TAG \
  -R "${REPO}"
```

Prefer digests for immutable verification:

```bash
gh attestation verify \
  oci://ghcr.io/josh-archer/uecb-lambda@sha256:DIGEST \
  -R Josh-Archer/unified-ephemeral-runner-broker
```

### Verify SPDX SBOM attestation

```bash
REPO=Josh-Archer/unified-ephemeral-runner-broker
PREDICATE=https://spdx.dev/Document/v2.3

gh attestation verify oci://ghcr.io/josh-archer/uecb-cloud-run:TAG \
  -R "${REPO}" \
  --predicate-type "${PREDICATE}"

gh attestation verify oci://ghcr.io/josh-archer/uecb-lambda:TAG \
  -R "${REPO}" \
  --predicate-type "${PREDICATE}"

gh attestation verify oci://ghcr.io/josh-archer/uecb-azure-functions:TAG \
  -R "${REPO}" \
  --predicate-type "${PREDICATE}"
```

### Inspect the SBOM predicate

```bash
gh attestation verify \
  oci://ghcr.io/josh-archer/uecb-lambda:TAG \
  -R Josh-Archer/unified-ephemeral-runner-broker \
  --predicate-type https://spdx.dev/Document/v2.3 \
  --format json \
  --jq '.[].verificationResult.statement.predicate'
```

### Confirm Lambda still uses a Docker v2 single manifest

```bash
image=ghcr.io/josh-archer/uecb-lambda:TAG
raw="$(docker buildx imagetools inspect --raw "${image}")"
echo "${raw}" | jq -r '.mediaType'
# expect: application/vnd.docker.distribution.manifest.v2+json
echo "${raw}" | jq -e 'has("manifests")' && echo "FAIL multi-manifest" || echo "OK single manifest"
```

## CI guarantees

The publish workflow:

1. Builds and pushes each matrix image with `provenance: false` and `sbom: false`.
2. Verifies the Lambda tag media type before attestations.
3. Generates a pinned-tooling SPDX SBOM for the digest-pinned image ref.
4. Publishes provenance and SBOM attestations bound to that digest.
5. Re-verifies the Lambda tag media type (and digest stability) after registry
   referrer attach.

Static workflow contract checks live in
`internal/imagecontracts/publish_attestations_test.go` and run under `go test ./...`.

## References

- [GitHub artifact attestations](https://docs.github.com/en/actions/how-tos/secure-your-work/use-artifact-attestations/use-artifact-attestations)
- [`gh attestation verify`](https://cli.github.com/manual/gh_attestation_verify)
- [`actions/attest`](https://github.com/actions/attest)

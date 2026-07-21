package imagecontracts

import (
	"strings"
	"testing"
)

// TestPublishRuntimeImagesWorkflowAttestationContract locks the issue #52
// supply-chain contract: digest-bound provenance/SBOM attestations without
// enabling BuildKit provenance/SBOM on the primary (Lambda-critical) image.
func TestPublishRuntimeImagesWorkflowAttestationContract(t *testing.T) {
	path := ".github/workflows/publish-runtime-images.yml"
	content := readFile(t, path)

	required := []string{
		// Least-privilege OIDC + attestation permissions
		"id-token: write",
		"attestations: write",
		"packages: write",
		"contents: read",

		// Primary image must stay Docker v2 single-manifest for Lambda
		"provenance: false",
		"sbom: false",
		"oci-mediatypes=false",

		// Digest capture from build-push
		"steps.build.outputs.digest",
		"id: build",

		// Pinned SBOM tooling
		"anchore/sbom-action@v0.24.0",
		"format: spdx-json",
		"SYFT_VERSION:",
		"syft-version:",

		// Compact package-only SPDX under actions/attest 16 MiB predicate limit
		"Compact SBOM for attestation size limit",
		"16 * 1024 * 1024",

		// GitHub artifact attestation flow bound to digest
		"actions/attest@v4",
		"subject-name:",
		"subject-digest:",
		"sbom-path:",
		"push-to-registry: true",

		// Lambda media-type guard remains
		"application/vnd.docker.distribution.manifest.v2+json",
		"Verify Lambda-compatible image manifest",
		"Re-verify Lambda manifest after attestations",

		// All three runtime packages
		"uecb-cloud-run",
		"uecb-lambda",
		"uecb-azure-functions",
	}

	for _, needle := range required {
		if !strings.Contains(content, needle) {
			t.Fatalf("%s missing required contract fragment %q", path, needle)
		}
	}

	// Must not re-enable BuildKit-embedded attestations that produce indexes.
	forbidden := []string{
		"provenance: true",
		"sbom: true",
	}
	for _, needle := range forbidden {
		if strings.Contains(content, needle) {
			t.Fatalf("%s must not contain %q (breaks Lambda single-manifest)", path, needle)
		}
	}

	// Two attest steps: provenance (no sbom-path on first use of actions/attest
	// is hard to assert precisely) + SBOM — require sbom-path appears once and
	// actions/attest appears at least twice.
	if strings.Count(content, "uses: actions/attest@v4") < 2 {
		t.Fatalf("%s expected at least two actions/attest@v4 steps (provenance + SBOM)", path)
	}
	if strings.Count(content, "sbom-path:") < 1 {
		t.Fatalf("%s expected SBOM attestation sbom-path input", path)
	}
}

func TestImageAttestationsDocHasConsumerVerifyCommands(t *testing.T) {
	path := "docs/image-attestations.md"
	content := readFile(t, path)

	required := []string{
		"gh attestation verify",
		"oci://ghcr.io/josh-archer/uecb-cloud-run",
		"oci://ghcr.io/josh-archer/uecb-lambda",
		"oci://ghcr.io/josh-archer/uecb-azure-functions",
		"--predicate-type",
		"https://spdx.dev/Document/v2.3",
		"application/vnd.docker.distribution.manifest.v2+json",
		"Josh-Archer/unified-ephemeral-runner-broker",
	}
	for _, needle := range required {
		if !strings.Contains(content, needle) {
			t.Fatalf("%s missing %q", path, needle)
		}
	}
}

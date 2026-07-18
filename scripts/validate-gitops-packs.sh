#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
packs_root="${repo_root}/examples/gitops/packs"

if ! command -v kubectl >/dev/null 2>&1; then
  echo "ERROR: kubectl is required to render the GitOps packs." >&2
  exit 1
fi

mapfile -t targets < <(
  find "${packs_root}" -type f -name kustomization.yaml -print |
    LC_ALL=C sort
)

if ((${#targets[@]} == 0)); then
  echo "ERROR: no GitOps pack Kustomizations found under ${packs_root}." >&2
  exit 1
fi

rendered_output="$(mktemp)"
trap 'rm -f "${rendered_output}"' EXIT

status=0
for kustomization in "${targets[@]}"; do
  target="$(dirname "${kustomization}")"
  relative_target="${target#"${repo_root}/"}"

  echo "Rendering ${relative_target}"
  if ! kubectl kustomize "${target}" >"${rendered_output}"; then
    echo "ERROR: failed to render ${relative_target}" >&2
    status=1
  fi
done

if ((status != 0)); then
  exit "${status}"
fi

echo "Successfully rendered ${#targets[@]} GitOps pack targets."

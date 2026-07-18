#!/usr/bin/env bash
# Fails when public docs/examples drift from the live allocation API and
# allocate-runner action contracts (see issue #64 / #54).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

status=0

# Paths scanned for obsolete public-facing literals (not Go sources/tests).
DOC_PATHS=(
  README.md
  docs
  examples
  actions
)

# Action-only inputs that never appear on the allocation request body.
ACTION_ONLY_INPUTS=(
  broker_url
  oidc_audience
  allow_unauthenticated
  queue_wait_timeout
)

fail() {
  echo "error: $*" >&2
  status=1
}

info() {
  echo "$*"
}

# --- 1. Obsolete path: /v1/allocate (must be /v1/allocations) ---------------
# Match /v1/allocate not followed by 's' so /v1/allocations is allowed.
info "Checking for obsolete /v1/allocate path in public docs/examples..."
obsolete_path_hits="$(
  { git grep -nE '/v1/allocate([^s/]|$|/)' -- "${DOC_PATHS[@]}" 2>/dev/null || true; } \
    | { grep -v 'scripts/check-api-docs-contract.sh' || true; }
)"
if [[ -n "${obsolete_path_hits}" ]]; then
  echo "${obsolete_path_hits}" >&2
  fail "found obsolete create path /v1/allocate; use /v1/allocations"
else
  info "  ok: no /v1/allocate create-path literals"
fi

# --- 2. Obsolete action input: pin_backend (correct: backend) ---------------
info "Checking for obsolete pin_backend input in public docs/examples..."
pin_hits="$(
  { git grep -n 'pin_backend' -- "${DOC_PATHS[@]}" 2>/dev/null || true; } \
    | { grep -v 'scripts/check-api-docs-contract.sh' || true; }
)"
if [[ -n "${pin_hits}" ]]; then
  echo "${pin_hits}" >&2
  fail "found obsolete pin_backend; use action/API field backend"
else
  info "  ok: no pin_backend literals"
fi

# --- 3. OpenAPI artifact present --------------------------------------------
OPENAPI="docs/openapi.yaml"
if [[ ! -f "${OPENAPI}" ]]; then
  fail "missing ${OPENAPI}"
else
  info "  ok: ${OPENAPI} present"
fi

# --- 4. Action inputs ⊆ OpenAPI AllocationRequest properties + action-only --
ACTION_YML="actions/allocate-runner/action.yml"
if [[ -f "${ACTION_YML}" && -f "${OPENAPI}" ]]; then
  info "Checking allocate-runner inputs against OpenAPI AllocationRequest..."

  mapfile -t action_inputs < <(
    # Under inputs:, collect top-level keys (two-space indent, name then colon).
    awk '
      /^inputs:[[:space:]]*$/ { in_inputs=1; next }
      in_inputs && /^[^[:space:]#]/ { exit }
      in_inputs && /^  [a-zA-Z0-9_]+:/ {
        line=$0
        sub(/^  /, "", line)
        sub(/:.*/, "", line)
        print line
      }
    ' "${ACTION_YML}"
  )

  mapfile -t openapi_props < <(
    awk '
      /^    AllocationRequest:[[:space:]]*$/ { in_schema=1; next }
      in_schema && /^    [A-Za-z]/ { exit }
      in_schema && /^      properties:[[:space:]]*$/ { in_props=1; next }
      in_props && /^        [a-zA-Z0-9_]+:/ {
        line=$0
        sub(/^        /, "", line)
        sub(/:.*/, "", line)
        print line
      }
      # Leave properties block at the next 6-space key under the schema
      # (for example required already passed; siblings would be rare).
      in_props && /^      [a-zA-Z0-9_]+:/ { exit }
    ' "${OPENAPI}"
  )

  is_action_only() {
    local name="$1"
    local only
    for only in "${ACTION_ONLY_INPUTS[@]}"; do
      if [[ "${name}" == "${only}" ]]; then
        return 0
      fi
    done
    return 1
  }

  is_openapi_prop() {
    local name="$1"
    local prop
    for prop in "${openapi_props[@]}"; do
      if [[ "${name}" == "${prop}" ]]; then
        return 0
      fi
    done
    return 1
  }

  if [[ ${#action_inputs[@]} -eq 0 ]]; then
    fail "could not parse inputs from ${ACTION_YML}"
  elif [[ ${#openapi_props[@]} -eq 0 ]]; then
    fail "could not parse AllocationRequest properties from ${OPENAPI}"
  else
    for input in "${action_inputs[@]}"; do
      if is_action_only "${input}"; then
        continue
      fi
      if ! is_openapi_prop "${input}"; then
        fail "action input '${input}' is not an AllocationRequest property in ${OPENAPI} and is not listed as action-only"
      fi
    done
    if [[ "${status}" -eq 0 ]]; then
      info "  ok: action body inputs match OpenAPI AllocationRequest properties"
    fi
  fi
fi

# --- 5. OpenAPI documents required allocation routes ------------------------
if [[ -f "${OPENAPI}" ]]; then
  info "Checking OpenAPI path coverage..."
  required_paths=(
    '/v1/allocations:'
    '/v1/allocations/{id}:'
    '/v1/allocations/{id}/complete:'
    '/v1/allocations/{id}/cancel:'
  )
  for path_key in "${required_paths[@]}"; do
    if ! grep -qF "  ${path_key}" "${OPENAPI}"; then
      fail "OpenAPI missing path key: ${path_key}"
    fi
  done
  if [[ "${status}" -eq 0 ]]; then
    info "  ok: create/get/complete/cancel paths present"
  fi
fi

if [[ "${status}" -ne 0 ]]; then
  echo "API docs contract check failed." >&2
  exit "${status}"
fi

echo "API docs contract check passed."

#!/usr/bin/env bash
# Detect release drift between published chart/appVersion artifacts and the
# intended public source SHA/tag for unified-ephemeral-runner-broker.
#
# Exit codes:
#   0 - no drift detected
#   1 - drift detected or check failed
#
# Usage:
#   scripts/check-release-drift.sh [--version 0.1.25] [--source-ref v0.1.25|SHA]
#                                  [--create-issue] [--json]
#
# Environment overrides:
#   SOURCE_REPO, PACKAGE_OWNER, CHART_PACKAGE, IMAGE_PACKAGES, GH_TOKEN

set -euo pipefail

SOURCE_REPO="${SOURCE_REPO:-Josh-Archer/unified-ephemeral-runner-broker}"
PACKAGE_OWNER="${PACKAGE_OWNER:-Josh-Archer}"
CHART_PACKAGE="${CHART_PACKAGE:-charts/unified-ephemeral-runner-broker}"
# Comma-separated GHCR package names under PACKAGE_OWNER
IMAGE_PACKAGES="${IMAGE_PACKAGES:-unified-ephemeral-runner-broker/broker,unified-ephemeral-runner-broker/launcher,unified-ephemeral-runner-broker/cloud-run,unified-ephemeral-runner-broker/lambda,unified-ephemeral-runner-broker/azure-functions}"

VERSION=""
SOURCE_REF=""
CREATE_ISSUE=0
JSON_OUT=0
STRICT_APPVERSION=1

usage() {
  sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)
      VERSION="${2:-}"
      shift 2
      ;;
    --source-ref)
      SOURCE_REF="${2:-}"
      shift 2
      ;;
    --create-issue)
      CREATE_ISSUE=1
      shift
      ;;
    --json)
      JSON_OUT=1
      shift
      ;;
    --no-strict-appversion)
      STRICT_APPVERSION=0
      shift
      ;;
    -h|--help)
      usage
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      ;;
  esac
done

if ! command -v gh >/dev/null 2>&1; then
  echo "ERROR: gh CLI is required" >&2
  exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "ERROR: jq is required" >&2
  exit 1
fi

normalize_version() {
  local v="$1"
  v="${v#v}"
  printf '%s' "$v"
}

log() {
  printf '%s\n' "$*"
}

warn() {
  printf '::warning::%s\n' "$*" 2>/dev/null || true
  printf 'WARNING: %s\n' "$*" >&2
}

err_anno() {
  printf '::error::%s\n' "$*" 2>/dev/null || true
  printf 'ERROR: %s\n' "$*" >&2
}

# Resolve the latest public release tag when version is omitted.
resolve_latest_version() {
  local tag
  tag="$(gh api "repos/${SOURCE_REPO}/releases/latest" --jq '.tag_name' 2>/dev/null || true)"
  if [[ -z "${tag}" || "${tag}" == "null" ]]; then
    tag="$(gh api "repos/${SOURCE_REPO}/tags?per_page=1" --jq '.[0].name' 2>/dev/null || true)"
  fi
  if [[ -z "${tag}" || "${tag}" == "null" ]]; then
    echo "ERROR: could not resolve latest public release/tag for ${SOURCE_REPO}" >&2
    exit 1
  fi
  normalize_version "${tag}"
}

# Resolve commit SHA for a ref (tag, branch, or full/short SHA).
resolve_source_sha() {
  local ref="$1"
  local sha
  sha="$(gh api "repos/${SOURCE_REPO}/commits/${ref}" --jq '.sha' 2>/dev/null || true)"
  if [[ -z "${sha}" || "${sha}" == "null" ]]; then
    echo "ERROR: could not resolve public source SHA for ref '${ref}' in ${SOURCE_REPO}" >&2
    exit 1
  fi
  printf '%s' "${sha}"
}

# Read Chart.yaml fields from the public source at a given ref.
read_source_chart_fields() {
  local ref="$1"
  local path="charts/unified-ephemeral-runner-broker/Chart.yaml"
  local content
  content="$(gh api "repos/${SOURCE_REPO}/contents/${path}?ref=${ref}" --jq '.content' 2>/dev/null | tr -d '\n' | base64 --decode 2>/dev/null || true)"
  if [[ -z "${content}" ]]; then
    echo "ERROR: could not read ${path} at ${ref}" >&2
    exit 1
  fi
  local chart_version app_version
  chart_version="$(printf '%s\n' "${content}" | awk '/^version:[[:space:]]+/ {gsub(/"/, "", $2); print $2; exit}')"
  app_version="$(printf '%s\n' "${content}" | awk '/^appVersion:[[:space:]]+/ {gsub(/"/, "", $2); print $2; exit}')"
  printf '%s\t%s' "${chart_version}" "${app_version}"
}

# List tags for a GHCR container package version inventory (all versions, tagged only).
# Prints one tag per line.
package_tags() {
  local package="$1"
  local encoded
  encoded="$(printf '%s' "${package}" | jq -sRr @uri)"
  # Paginate package versions and collect tags.
  gh api --paginate \
    "users/${PACKAGE_OWNER}/packages/container/${encoded}/versions" \
    --jq '.[] | select(.metadata.container.tags != null) | .metadata.container.tags[]' \
    2>/dev/null || true
}

# True if package has the given tag.
package_has_tag() {
  local package="$1"
  local want="$2"
  local tags
  tags="$(package_tags "${package}")"
  printf '%s\n' "${tags}" | grep -Fxq -- "${want}"
}

# Find tags on the same package version that also carries want_tag; returns all co-tags.
package_cotags_for() {
  local package="$1"
  local want="$2"
  local encoded
  encoded="$(printf '%s' "${package}" | jq -sRr @uri)"
  # Pipe through jq so --arg works (gh --jq does not accept --arg).
  gh api --paginate \
    "users/${PACKAGE_OWNER}/packages/container/${encoded}/versions" \
    2>/dev/null \
    | jq -r --arg want "${want}" \
      '.[] | select(.metadata.container.tags != null) | select(.metadata.container.tags | index($want)) | .metadata.container.tags[]' \
    || true
}

DRIFT_REASONS=()
CHECKS_PASSED=0
CHECKS_FAILED=0

record_pass() {
  CHECKS_PASSED=$((CHECKS_PASSED + 1))
  log "  PASS: $*"
}

record_fail() {
  CHECKS_FAILED=$((CHECKS_FAILED + 1))
  DRIFT_REASONS+=("$*")
  err_anno "$*"
}

if [[ -z "${VERSION}" ]]; then
  VERSION="$(resolve_latest_version)"
  log "Resolved latest version: ${VERSION}"
else
  VERSION="$(normalize_version "${VERSION}")"
fi

if [[ -z "${SOURCE_REF}" ]]; then
  SOURCE_REF="v${VERSION}"
fi

SOURCE_SHA="$(resolve_source_sha "${SOURCE_REF}")"
SHORT_SHA="$(printf '%s' "${SOURCE_SHA}" | cut -c1-7)"
# Image sha tags in this project use 7-char prefixes (sha-570c265).
SHA_TAG="sha-${SHORT_SHA}"
VERSION_TAG="v${VERSION}"
PLAIN_VERSION_TAG="${VERSION}"

log "=== Release drift check ==="
log "source_repo=${SOURCE_REPO}"
log "version=${VERSION}"
log "source_ref=${SOURCE_REF}"
log "source_sha=${SOURCE_SHA}"
log "expected_image_sha_tag=${SHA_TAG}"
log "package_owner=${PACKAGE_OWNER}"
log ""

# 1) Public source Chart.yaml version / appVersion vs intended release version.
chart_fields="$(read_source_chart_fields "${SOURCE_SHA}")"
CHART_VERSION="$(printf '%s' "${chart_fields}" | cut -f1)"
APP_VERSION="$(printf '%s' "${chart_fields}" | cut -f2)"
APP_VERSION_NORM="$(normalize_version "${APP_VERSION}")"
CHART_VERSION_NORM="$(normalize_version "${CHART_VERSION}")"

log "Source Chart.yaml at ${SHORT_SHA}:"
log "  version=${CHART_VERSION}"
log "  appVersion=${APP_VERSION}"

if [[ "${STRICT_APPVERSION}" -eq 1 ]]; then
  if [[ "${APP_VERSION_NORM}" == "${VERSION}" ]]; then
    record_pass "source chart appVersion (${APP_VERSION}) matches release version ${VERSION}"
  else
    record_fail "source chart appVersion (${APP_VERSION}) does not match release version ${VERSION} for public SHA ${SOURCE_SHA}"
  fi
  if [[ -n "${CHART_VERSION_NORM}" && "${CHART_VERSION_NORM}" != "${VERSION}" ]]; then
    # Chart version may lag intentionally in some projects; treat as drift when it
    # disagrees with the release version we claim to have published from this SHA.
    record_fail "source chart version (${CHART_VERSION}) does not match release version ${VERSION} for public SHA ${SOURCE_SHA}"
  elif [[ "${CHART_VERSION_NORM}" == "${VERSION}" ]]; then
    record_pass "source chart version (${CHART_VERSION}) matches release version ${VERSION}"
  fi
else
  log "  (strict appVersion check disabled)"
  CHECKS_PASSED=$((CHECKS_PASSED + 1))
fi

# 2) Published Helm chart package must expose the release version tag.
log ""
log "Published chart package: ${CHART_PACKAGE}"
if package_has_tag "${CHART_PACKAGE}" "${PLAIN_VERSION_TAG}"; then
  record_pass "chart package has tag ${PLAIN_VERSION_TAG}"
elif package_has_tag "${CHART_PACKAGE}" "${VERSION_TAG}"; then
  record_pass "chart package has tag ${VERSION_TAG}"
else
  record_fail "published chart package ${CHART_PACKAGE} missing tag ${PLAIN_VERSION_TAG} (or ${VERSION_TAG}) for release ${VERSION}"
fi

# 3) Published images: version tag must exist and be co-tagged with public SHA tag.
log ""
log "Published images:"
IFS=',' read -r -a image_list <<< "${IMAGE_PACKAGES}"
for pkg in "${image_list[@]}"; do
  pkg="$(echo "${pkg}" | xargs)" # trim
  [[ -z "${pkg}" ]] && continue
  log "  package=${pkg}"

  has_version=0
  version_tag_used=""
  if package_has_tag "${pkg}" "${VERSION_TAG}"; then
    has_version=1
    version_tag_used="${VERSION_TAG}"
  elif package_has_tag "${pkg}" "${PLAIN_VERSION_TAG}"; then
    has_version=1
    version_tag_used="${PLAIN_VERSION_TAG}"
  fi

  if [[ "${has_version}" -ne 1 ]]; then
    record_fail "image package ${pkg} missing version tag ${VERSION_TAG} (or ${PLAIN_VERSION_TAG})"
    continue
  fi
  record_pass "image package ${pkg} has version tag ${version_tag_used}"

  cotags="$(package_cotags_for "${pkg}" "${version_tag_used}")"
  if printf '%s\n' "${cotags}" | grep -Fxq -- "${SHA_TAG}"; then
    record_pass "image package ${pkg} version ${version_tag_used} is co-tagged with ${SHA_TAG} (public SHA)"
  else
    # Also accept full-sha style tags if present.
    if printf '%s\n' "${cotags}" | grep -Eiq -- "^(sha-)?${SOURCE_SHA}$"; then
      record_pass "image package ${pkg} version ${version_tag_used} is co-tagged with full public SHA"
    else
      found_sha_tags="$(printf '%s\n' "${cotags}" | grep -E '^sha-' || true)"
      record_fail "image package ${pkg} tag ${version_tag_used} is not co-tagged with public source ${SHA_TAG} (found sha tags: ${found_sha_tags:-none})"
    fi
  fi
done

# Summary
log ""
log "=== Summary ==="
log "passed=${CHECKS_PASSED} failed=${CHECKS_FAILED}"

SUMMARY_FILE="${GITHUB_STEP_SUMMARY:-}"
if [[ -n "${SUMMARY_FILE}" ]]; then
  {
    echo "## Release drift check"
    echo ""
    echo "| Field | Value |"
    echo "| --- | --- |"
    echo "| version | \`${VERSION}\` |"
    echo "| source_ref | \`${SOURCE_REF}\` |"
    echo "| source_sha | \`${SOURCE_SHA}\` |"
    echo "| chart version | \`${CHART_VERSION}\` |"
    echo "| chart appVersion | \`${APP_VERSION}\` |"
    echo "| checks passed | ${CHECKS_PASSED} |"
    echo "| checks failed | ${CHECKS_FAILED} |"
    echo ""
    if [[ "${CHECKS_FAILED}" -gt 0 ]]; then
      echo "### Drift reasons"
      for r in "${DRIFT_REASONS[@]}"; do
        echo "- ${r}"
      done
    else
      echo "No drift detected."
    fi
  } >> "${SUMMARY_FILE}"
fi

if [[ "${JSON_OUT}" -eq 1 ]]; then
  reasons_json="$(printf '%s\n' "${DRIFT_REASONS[@]+"${DRIFT_REASONS[@]}"}" | jq -R . | jq -s .)"
  jq -n \
    --arg version "${VERSION}" \
    --arg source_ref "${SOURCE_REF}" \
    --arg source_sha "${SOURCE_SHA}" \
    --arg chart_version "${CHART_VERSION}" \
    --arg app_version "${APP_VERSION}" \
    --argjson passed "${CHECKS_PASSED}" \
    --argjson failed "${CHECKS_FAILED}" \
    --argjson reasons "${reasons_json}" \
    '{version:$version, source_ref:$source_ref, source_sha:$source_sha, chart_version:$chart_version, appVersion:$app_version, passed:$passed, failed:$failed, drift:($failed>0), reasons:$reasons}'
fi

if [[ "${CHECKS_FAILED}" -gt 0 ]]; then
  if [[ "${CREATE_ISSUE}" -eq 1 ]]; then
    title="Release drift detected for ${VERSION} (public SHA ${SHORT_SHA})"
    body_file="$(mktemp)"
    {
      echo "## Release drift detected"
      echo ""
      echo "- **version**: \`${VERSION}\`"
      echo "- **source_ref**: \`${SOURCE_REF}\`"
      echo "- **source_sha**: \`${SOURCE_SHA}\`"
      echo "- **chart version**: \`${CHART_VERSION}\`"
      echo "- **chart appVersion**: \`${APP_VERSION}\`"
      echo ""
      echo "### Reasons"
      for r in "${DRIFT_REASONS[@]}"; do
        echo "- ${r}"
      done
      echo ""
      echo "Opened by \`scripts/check-release-drift.sh\` (release drift detector)."
      if [[ -n "${GITHUB_SERVER_URL:-}" && -n "${GITHUB_REPOSITORY:-}" && -n "${GITHUB_RUN_ID:-}" ]]; then
        echo ""
        echo "Workflow run: ${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}"
      fi
    } > "${body_file}"

    # Avoid duplicate open issues with the same title.
    existing="$(gh issue list --repo "${GITHUB_REPOSITORY:-Josh-Archer/unified-ephemeral-runner-broker}" \
      --state open --search "in:title ${title}" --json number,title --jq '.[0].number' 2>/dev/null || true)"
    if [[ -n "${existing}" && "${existing}" != "null" ]]; then
      log "Alert issue already open: #${existing}"
      gh issue comment "${existing}" --repo "${GITHUB_REPOSITORY:-Josh-Archer/unified-ephemeral-runner-broker}" \
        --body-file "${body_file}" >/dev/null || true
    else
      issue_url="$(gh issue create \
        --repo "${GITHUB_REPOSITORY:-Josh-Archer/unified-ephemeral-runner-broker}" \
        --title "${title}" \
        --body-file "${body_file}" \
        --label "bug" 2>/dev/null || true)"
      if [[ -n "${issue_url}" ]]; then
        log "Opened alert issue: ${issue_url}"
        warn "Release drift alert issue: ${issue_url}"
      else
        warn "Failed to open alert issue (missing permissions or label); drift still fails the check."
      fi
    fi
    rm -f "${body_file}"
  fi

  log "DRIFT DETECTED"
  exit 1
fi

log "No drift detected."
exit 0

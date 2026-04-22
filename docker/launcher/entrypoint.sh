#!/usr/bin/env bash
set -euo pipefail

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "missing required environment variable: ${name}" >&2
    exit 1
  fi
}

github_api_request() {
  local method="$1"
  local endpoint="$2"
  local payload="${3:-}"
  local curl_args=(
    -fsSL
    -X "${method}"
    -H "Accept: application/vnd.github+json"
    -H "Authorization: Bearer ${GITHUB_PAT}"
    -H "X-GitHub-Api-Version: 2022-11-28"
  )
  if [[ -n "${payload}" ]]; then
    curl_args+=(
      -H "Content-Type: application/json"
      --data "${payload}"
    )
  fi
  curl "${curl_args[@]}" "https://api.github.com/${endpoint}"
}

registration_endpoint() {
  case "${GITHUB_SCOPE_TYPE}" in
    organization)
      printf 'orgs/%s/actions/runners/registration-token' "${GITHUB_ORGANIZATION}"
      ;;
    repository)
      printf 'repos/%s/%s/actions/runners/registration-token' "${GITHUB_OWNER}" "${GITHUB_REPOSITORY}"
      ;;
    *)
      echo "unsupported GITHUB_SCOPE_TYPE: ${GITHUB_SCOPE_TYPE}" >&2
      exit 1
      ;;
  esac
}

remove_endpoint() {
  case "${GITHUB_SCOPE_TYPE}" in
    organization)
      printf 'orgs/%s/actions/runners/remove-token' "${GITHUB_ORGANIZATION}"
      ;;
    repository)
      printf 'repos/%s/%s/actions/runners/remove-token' "${GITHUB_OWNER}" "${GITHUB_REPOSITORY}"
      ;;
    *)
      echo "unsupported GITHUB_SCOPE_TYPE: ${GITHUB_SCOPE_TYPE}" >&2
      exit 1
      ;;
  esac
}

registration_token() {
  github_api_request POST "$(registration_endpoint)" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])'
}

remove_token() {
  github_api_request POST "$(remove_endpoint)" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])'
}

cleanup() {
  if [[ ! -d "${RUNNER_ROOT}" ]] || [[ ! -f "${RUNNER_ROOT}/.runner" ]]; then
    return 0
  fi

  set +e
  local token=""
  token="$(remove_token 2>/dev/null || true)"
  if [[ -n "${token}" ]]; then
    (
      cd "${RUNNER_ROOT}"
      ./config.sh remove --unattended --token "${token}" >/dev/null 2>&1 || true
    )
  fi
}

trap cleanup EXIT

require_env GITHUB_PAT
require_env GITHUB_SCOPE_TYPE
require_env GITHUB_TARGET_URL
require_env RUNNER_NAME
require_env RUNNER_LABELS

if [[ "${GITHUB_SCOPE_TYPE}" == "organization" ]]; then
  require_env GITHUB_ORGANIZATION
fi

if [[ "${GITHUB_SCOPE_TYPE}" == "repository" ]]; then
  require_env GITHUB_OWNER
  require_env GITHUB_REPOSITORY
fi

RUNNER_VERSION="${RUNNER_VERSION:-2.333.1}"
RUNNER_ROOT="${RUNNER_ROOT:-/runner}"
RUNNER_WORKDIR="${RUNNER_WORKDIR:-_work}"
RUNNER_ARCHIVE="actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz"
RUNNER_URL="https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/${RUNNER_ARCHIVE}"

mkdir -p "${RUNNER_ROOT}"
cd "${RUNNER_ROOT}"

if [[ ! -x "${RUNNER_ROOT}/bin/Runner.Listener" ]]; then
  curl -fsSLo "${RUNNER_ARCHIVE}" "${RUNNER_URL}"
  tar -xzf "${RUNNER_ARCHIVE}"
  rm -f "${RUNNER_ARCHIVE}"
fi

token="$(registration_token)"

config_args=(
  --unattended
  --ephemeral
  --replace
  --url "${GITHUB_TARGET_URL}"
  --token "${token}"
  --name "${RUNNER_NAME}"
  --labels "${RUNNER_LABELS}"
  --work "${RUNNER_WORKDIR}"
)

if [[ -n "${RUNNER_GROUP:-}" ]] && [[ "${GITHUB_SCOPE_TYPE}" == "organization" ]]; then
  config_args+=(--runnergroup "${RUNNER_GROUP}")
fi

./config.sh "${config_args[@]}"
./run.sh

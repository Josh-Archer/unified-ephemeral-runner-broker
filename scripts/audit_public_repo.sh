#!/usr/bin/env bash
set -euo pipefail

denylist=(
  "archer.casa"
  "home-gh-runner"
  "homelab"
  "vaultwarden-webhook"
  "ops/arc-github-secret"
  "Josh-Archer/home"
)

status=0
for needle in "${denylist[@]}"; do
  if git grep -n "${needle}" -- . ':!scripts/audit_public_repo.sh'; then
    echo "Found forbidden token: ${needle}" >&2
    status=1
  fi
done

exit "${status}"


#!/usr/bin/env bash
# Stores an application's App LLM proxy key from Agent Control Plane in AWS Secrets Manager.
# The key is read without echo and piped to Secrets Manager, so it isn't in your shell history.
# Usage: ./scripts/store-app-key.sh support-assistant
set -euo pipefail
# shellcheck source=scripts/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

app="${1:?usage: $0 <application>}"
secret="${SECRET_PREFIX}/${app}"
read -rsp "Paste the ${app} proxy key: " key
echo
if aws secretsmanager describe-secret --secret-id "${secret}" >/dev/null 2>&1; then
  printf '%s' "${key}" | aws secretsmanager put-secret-value --secret-id "${secret}" \
    --secret-string file:///dev/stdin >/dev/null
else
  printf '%s' "${key}" | aws secretsmanager create-secret --name "${secret}" \
    --description "Agent Control Plane App LLM proxy key for ${app}" \
    --secret-string file:///dev/stdin >/dev/null
fi
unset key
echo "Stored the ${app} key as ${secret}."

#!/usr/bin/env bash
# Stores the LLM provider's API key from Agent Control Plane in AWS Secrets Manager.
# The key is read without echo and piped to Secrets Manager, so it isn't in your shell history.
# Usage: ./scripts/store-provider-key.sh
set -euo pipefail
# shellcheck source=scripts/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

read -rsp "Paste the provider API key: " key
echo
if aws secretsmanager describe-secret --secret-id "${PROVIDER_KEY_SECRET}" >/dev/null 2>&1; then
  printf '%s' "${key}" | aws secretsmanager put-secret-value --secret-id "${PROVIDER_KEY_SECRET}" \
    --secret-string file:///dev/stdin >/dev/null
else
  printf '%s' "${key}" | aws secretsmanager create-secret --name "${PROVIDER_KEY_SECRET}" \
    --description "Agent Control Plane API key for the bedrock-provider LLM provider" \
    --secret-string file:///dev/stdin >/dev/null
fi
unset key
echo "Stored the provider key as ${PROVIDER_KEY_SECRET}."

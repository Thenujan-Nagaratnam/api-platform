#!/usr/bin/env bash
# Stores the gateway's registration token from Agent Control Plane in AWS Secrets Manager.
# The token is read without echo and piped to Secrets Manager, so it isn't in your shell history.
# Run by deploy-gateway.sh. Usage: ./scripts/store-gateway-token.sh
set -euo pipefail
# shellcheck source=scripts/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

if [ -t 0 ]; then
  read -rsp "Paste the gateway token: " token
  echo
else
  IFS= read -r token || true   # Piped in by deploy-gateway.sh.
fi
[ -n "${token}" ] || { echo "No token entered." >&2; exit 1; }
if aws secretsmanager describe-secret --secret-id "${CONTROL_PLANE_TOKEN_SECRET}" >/dev/null 2>&1; then
  printf '%s' "${token}" | aws secretsmanager put-secret-value --secret-id "${CONTROL_PLANE_TOKEN_SECRET}" \
    --secret-string file:///dev/stdin >/dev/null
else
  printf '%s' "${token}" | aws secretsmanager create-secret --name "${CONTROL_PLANE_TOKEN_SECRET}" \
    --description "Agent Control Plane registration token for WSO2 AI Gateway" \
    --secret-string file:///dev/stdin >/dev/null
fi
unset token
echo "Stored the gateway token as ${CONTROL_PLANE_TOKEN_SECRET}."

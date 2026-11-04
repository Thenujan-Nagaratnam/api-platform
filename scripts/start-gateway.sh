#!/usr/bin/env bash
# Code-only path, without AI Workspace: downloads the open-source WSO2 AI Gateway, sets it up in
# standalone mode, and starts it with your temporary AWS credentials (through run-gateway.sh).
# Then deploy the provider and proxies with ./scripts/deploy-artifacts.sh in a second terminal.
# Usage: ./scripts/start-gateway.sh
set -euo pipefail
# shellcheck source=scripts/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

GATEWAY_VERSION="2026.09.24"
GATEWAY_ZIP_URL="https://github.com/wso2/api-platform/releases/download/ai-gateway/v${GATEWAY_VERSION}/wso2apip-ai-gateway-${GATEWAY_VERSION}.zip"
GATEWAY_ZIP_SHA256="a33772849b4b8ca5393dd9be6facc409846c5ffd857a6c23248a6b28104701fc"
GATEWAY_DIR="${ROOT_DIR}/wso2apip-ai-gateway"

# 1. Download the distribution and verify its checksum.
if [ ! -d "${GATEWAY_DIR}" ]; then
  curl -fsSL -o "${ROOT_DIR}/gateway.zip" "${GATEWAY_ZIP_URL}"
  echo "${GATEWAY_ZIP_SHA256}  ${ROOT_DIR}/gateway.zip" | shasum -a 256 -c -
  # The zip extracts to wso2apip-ai-gateway-<version>; rename it to a version-independent folder.
  (cd "${ROOT_DIR}" && unzip -q gateway.zip && rm gateway.zip \
    && mv "wso2apip-ai-gateway-${GATEWAY_VERSION}" wso2apip-ai-gateway)
fi

# 2. First run only: an admin password in Secrets Manager, then the distribution's setup.
#    --force regenerates the listener certificate and AES key, because the ones in the zip are public.
if [ ! -f "${GATEWAY_DIR}/api-platform.env" ]; then
  if ! aws secretsmanager describe-secret --secret-id "${ADMIN_SECRET}" >/dev/null 2>&1; then
    openssl rand -base64 24 | tr -d '\n' | aws secretsmanager create-secret --name "${ADMIN_SECRET}" \
      --description "WSO2 AI Gateway management API admin password" \
      --secret-string file:///dev/stdin >/dev/null
  fi
  (cd "${GATEWAY_DIR}" && ADMIN_PASSWORD="$(aws secretsmanager get-secret-value --secret-id "${ADMIN_SECRET}" \
    --query SecretString --output text)" ./scripts/setup.sh --force)
fi

# 3. Allowlist, credentials override, start, and logs: the same steps as the AI Workspace path.
exec "${ROOT_DIR}/scripts/run-gateway.sh" "${GATEWAY_DIR}"

#!/usr/bin/env bash
# Builds gateway images that include aws-bedrock-guardrail v1.3.0, using the ap CLI and the build.yaml
# that ships with the gateway. v1.3.0 adds applicableStatusCodes, so a response check skips error
# responses. Run by run-gateway.sh on first start; run it yourself to rebuild.
# Usage: ./scripts/build-gateway-image.sh <gateway-folder>
set -euo pipefail
# shellcheck source=scripts/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
GATEWAY_DIR="$(gateway_dir "${1:-}")"
BUILD_FILE="${GATEWAY_DIR}/build.yaml"

command -v ap >/dev/null || {
  echo "Install the ap CLI v0.9.1 first: https://github.com/wso2/api-platform/releases/tag/ap%2Fv0.9.1" >&2
  exit 1
}
[ -f "${BUILD_FILE}" ] || { echo "No build.yaml in ${GATEWAY_DIR}" >&2; exit 1; }

# 1. Pin the guardrail policy version in build.yaml.
sed -i.bak -E "s#(gateway-controllers/policies/aws-bedrock-guardrail)@[^[:space:]\"]+#\1@${GUARDRAIL_POLICY_VERSION}#" "${BUILD_FILE}"
rm -f "${BUILD_FILE}.bak"
grep -q "aws-bedrock-guardrail@${GUARDRAIL_POLICY_VERSION}" "${BUILD_FILE}" || {
  echo "Couldn't pin aws-bedrock-guardrail@${GUARDRAIL_POLICY_VERSION} in ${BUILD_FILE}" >&2
  exit 1
}

# 2. Build the controller and runtime images.
LOG="$(mktemp)"
trap 'rm -f "${LOG}"' EXIT
(cd "${GATEWAY_DIR}" && ap gateway image build --name bedrock-guardrails) | tee "${LOG}"
CONTROLLER_IMAGE="$(grep -o '[^[:space:]]*-gateway-controller:[^[:space:]]*' "${LOG}" | tail -1)"
RUNTIME_IMAGE="$(grep -o '[^[:space:]]*-gateway-runtime:[^[:space:]]*' "${LOG}" | tail -1)"
[ -n "${CONTROLLER_IMAGE}" ] && [ -n "${RUNTIME_IMAGE}" ] || {
  echo "Couldn't find the built image names in the ap output." >&2
  exit 1
}

# 3. Record the images for run-gateway.sh.
printf 'CONTROLLER_IMAGE=%s\nRUNTIME_IMAGE=%s\n' "${CONTROLLER_IMAGE}" "${RUNTIME_IMAGE}" > "${GATEWAY_DIR}/.sample-images"
echo "Built ${CONTROLLER_IMAGE} and ${RUNTIME_IMAGE} with aws-bedrock-guardrail@${GUARDRAIL_POLICY_VERSION}."

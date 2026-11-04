#!/usr/bin/env bash
# Shared settings for the scripts. Reads the Terraform outputs into environment variables.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

tf_output() {
  terraform -chdir="${ROOT_DIR}/terraform" output -raw "$1"
}

AWS_REGION="$(tf_output region)"
BEDROCK_MODEL_ID="$(tf_output model_id)"
GATEWAY_ROLE_ARN="$(tf_output gateway_role_arn)"
BASELINE_GUARDRAIL_ID="$(tf_output baseline_guardrail_id)"
BASELINE_GUARDRAIL_VERSION="$(tf_output baseline_guardrail_version)"
SUPPORT_GUARDRAIL_ID="$(tf_output support_guardrail_id)"
SUPPORT_GUARDRAIL_VERSION="$(tf_output support_guardrail_version)"
export AWS_REGION BEDROCK_MODEL_ID GATEWAY_ROLE_ARN \
  BASELINE_GUARDRAIL_ID BASELINE_GUARDRAIL_VERSION SUPPORT_GUARDRAIL_ID SUPPORT_GUARDRAIL_VERSION
export AWS_DEFAULT_REGION="${AWS_REGION}"

export SECRET_PREFIX="wso2-ai-gateway/apps"
export ADMIN_SECRET="wso2-ai-gateway/admin-password"
export APPS="support-assistant employee-assistant"

# Only these variables are substituted, so other ${...} text stays intact.
# shellcheck disable=SC2016 # Single quotes are intentional: envsubst expands these names.
export SUBST_VARS='${AWS_REGION} ${GATEWAY_ROLE_ARN} ${BASELINE_GUARDRAIL_ID} ${BASELINE_GUARDRAIL_VERSION} ${SUPPORT_GUARDRAIL_ID} ${SUPPORT_GUARDRAIL_VERSION}'

# The folder where you downloaded the gateway from AI Workspace. Pass it as the first argument.
gateway_dir() {
  local dir="${1:?Pass the folder where you downloaded the gateway from AI Workspace.}"
  [ -f "${dir}/api-platform.env" ] || { echo "No api-platform.env in ${dir}. Run the AI Workspace Get Started commands first." >&2; return 1; }
  (cd "${dir}" && pwd)
}

# Runs docker compose the same way AI Workspace's Get Started does. Docker Compose also merges
# docker-compose.override.yaml from the same folder automatically.
gateway_compose() {
  docker compose --env-file api-platform.env "$@"
}

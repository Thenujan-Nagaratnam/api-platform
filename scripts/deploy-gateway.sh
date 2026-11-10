#!/usr/bin/env bash
# Connects the gateway to Agent Control Plane: reads the host and token from the helm install command that
# the console shows, stores them in Secrets Manager, builds the gateway images, and installs the gateway.
# The sample installs its tested gateway version, whichever chart version the pasted command names.
# Rerun it to apply changes. Set REPLACE_GATEWAY_CONNECTION=1 to paste a new command.
# Usage: ./scripts/deploy-gateway.sh
set -euo pipefail
# shellcheck source=scripts/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

secret_exists() { aws secretsmanager describe-secret --secret-id "$1" >/dev/null 2>&1; }

# Reads the pasted command without displaying it, including lines that continue with a backslash.
read_helm_command() {
  local line command=""
  echo "Paste the helm install command from Agent Control Plane, and press Enter. It won't be displayed:" >&2
  while IFS= read -rs line; do
    line="${line%$'\r'}"
    line="${line%"${line##*[![:space:]]}"}"
    if [ "${line%\\}" != "${line}" ]; then
      command+="${line%\\} "
      continue
    fi
    command+="${line}"
    [ -z "${command//[[:space:]]/}" ] && continue
    break
  done
  echo >&2
  printf '%s' "${command}"
}

# Prints the value of a --set option from the pasted command, without surrounding quotes.
helm_value() {
  sed -nE "s/.*[[:space:]=]$1=([^[:space:]]+).*/\1/p" <<<"$2" | tr -d "\"'"
}

if [ "${REPLACE_GATEWAY_CONNECTION:-0}" != "1" ] \
  && secret_exists "${CONTROL_PLANE_HOST_SECRET}" && secret_exists "${CONTROL_PLANE_TOKEN_SECRET}"; then
  echo "Using the control plane host and token stored in Secrets Manager."
else
  helm_command="$(read_helm_command)"
  host="$(helm_value 'gateway\.controller\.controlPlane\.host' "${helm_command}")"
  token="$(helm_value 'gateway\.controller\.controlPlane\.token\.value' "${helm_command}")"
  unset helm_command
  [ -n "${host}" ] || { echo "Couldn't find controlPlane.host in the command. Copy the whole helm install command." >&2; exit 1; }
  [ -n "${token}" ] || { echo "Couldn't find controlPlane.token.value in the command. Copy the whole helm install command." >&2; exit 1; }
  case "${token}" in
    *'<'* | *'>'*) echo "The command has a placeholder instead of a token. Use the console's Copy button." >&2; exit 1 ;;
  esac
  if secret_exists "${CONTROL_PLANE_HOST_SECRET}"; then
    printf '%s' "${host}" | aws secretsmanager put-secret-value --secret-id "${CONTROL_PLANE_HOST_SECRET}" \
      --secret-string file:///dev/stdin >/dev/null
  else
    printf '%s' "${host}" | aws secretsmanager create-secret --name "${CONTROL_PLANE_HOST_SECRET}" \
      --description "Agent Control Plane host for WSO2 AI Gateway" --secret-string file:///dev/stdin >/dev/null
  fi
  printf '%s\n' "${token}" | "${ROOT_DIR}/scripts/store-gateway-token.sh"
  unset token host
fi

AGENT_CONTROL_PLANE_HOST="$(aws secretsmanager get-secret-value --secret-id "${CONTROL_PLANE_HOST_SECRET}" \
  --query SecretString --output text)"
export AGENT_CONTROL_PLANE_HOST

if [ -f "${GATEWAY_DIR}/.sample-images" ]; then
  echo "Using the gateway images that are already built."
else
  "${ROOT_DIR}/scripts/build-gateway-image.sh"
fi
exec "${ROOT_DIR}/scripts/install-gateway.sh"

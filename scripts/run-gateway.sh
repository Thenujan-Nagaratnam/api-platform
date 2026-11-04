#!/usr/bin/env bash
# Starts the gateway that you downloaded from AI Workspace, with your temporary AWS credentials.
# Run it again to restart the gateway with fresh credentials when your AWS session expires.
# Usage: ./scripts/run-gateway.sh <gateway-folder>
# Works for a gateway downloaded from AI Workspace and for the standalone gateway from start-gateway.sh.
set -euo pipefail
# shellcheck source=scripts/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
GATEWAY_DIR="$(gateway_dir "${1:-}")"
CONFIG="${GATEWAY_DIR}/configs/config.toml"

# 1. Prepend the guardrail allowlist to config.toml (root-level keys go before the first [table]).
if ! grep -q '^awsbedrock_allowed_auth_types' "${CONFIG}"; then
  { envsubst "${SUBST_VARS}" < "${ROOT_DIR}/compose/allowlist.toml"; echo; cat "${CONFIG}"; } > "${CONFIG}.new"
  mv "${CONFIG}.new" "${CONFIG}"
fi

# 2. Add the override that passes your credentials to the runtime. Docker Compose merges
#    docker-compose.override.yaml automatically, so AI Workspace's start command picks it up.
cp "${ROOT_DIR}/compose/docker-compose.override.yaml" "${GATEWAY_DIR}/docker-compose.override.yaml"

# 3. Export temporary credentials for this process only, and start the gateway with
#    AI Workspace's own command. Compose recreates only the containers whose settings changed,
#    such as the runtime when your credentials are new.
eval "$(aws configure export-credentials --format env)"
cd "${GATEWAY_DIR}"
gateway_compose up -d

# 4. Confirm the runtime received the credentials. Prints variable names only, never values.
runtime="$(gateway_compose ps -q gateway-runtime)"
names="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "${runtime}" | cut -d= -f1 | grep '^AWS_' | sort | tr '\n' ' ')"
case "${names}" in
  *AWS_SESSION_TOKEN*) echo "Gateway runtime has: ${names}" ;;
  *) echo "The gateway runtime has no AWS session credentials. Check that the service is named gateway-runtime." >&2; exit 1 ;;
esac

# 5. Follow the logs so you can see the gateway come up. Ctrl+C stops following; the gateway
#    keeps running. Stop it with: docker compose --env-file api-platform.env down
echo "Following the gateway logs. Press Ctrl+C to stop following; the gateway keeps running."
gateway_compose logs -f --tail 20 gateway-controller gateway-runtime

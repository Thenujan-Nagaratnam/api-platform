#!/usr/bin/env bash
# Stops the gateway, removes its data, and deletes the application key secrets.
# If you used AI Workspace, delete the proxies, the provider, and the gateway there first. Then run this script,
# and then run terraform destroy.
# Usage: ./scripts/cleanup.sh <gateway-folder>
set -euo pipefail
# shellcheck source=scripts/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
GATEWAY_DIR="$(gateway_dir "${1:-}")"

(cd "${GATEWAY_DIR}" && gateway_compose down --volumes && rm -f docker-compose.override.yaml)

for secret in $(aws secretsmanager list-secrets --filters Key=name,Values=wso2-ai-gateway/ \
    --query 'SecretList[].Name' --output text); do
  aws secretsmanager delete-secret --secret-id "${secret}" --force-delete-without-recovery >/dev/null
  echo "Deleted secret ${secret}"
done
echo "Done. Now run terraform destroy."

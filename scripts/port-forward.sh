#!/usr/bin/env bash
# Forwards the gateway's traffic port (8443) to this machine. Keep it running while you test.
# Press Ctrl+C to stop.
# Usage: ./scripts/port-forward.sh
set -euo pipefail
# shellcheck source=scripts/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

kubectl -n "${NAMESPACE}" port-forward "svc/${RELEASE}-gateway-runtime" 8443:8443 || true
echo "The port forward stopped, for example because a pod restarted. Run ./scripts/port-forward.sh again." >&2
exit 1

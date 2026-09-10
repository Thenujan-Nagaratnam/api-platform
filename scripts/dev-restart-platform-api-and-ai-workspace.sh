#!/usr/bin/env bash
#
# Stops any previously started instances, builds, then starts:
#   - Platform API      (https://localhost:9243)
#   - AI Workspace BFF  (https://localhost:8081/ai-workspace/ — serves the built SPA directly)
#
# Prerequisite (one-time, not run by this script): platform-api's
#   `make setup-local-dev` — generates local-dev.env + resources/keys/encryption.key.
#
# Usage: ./scripts/dev-restart-platform-api-and-ai-workspace.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLATFORM_API_DIR="$REPO_ROOT/platform-api"
AI_WORKSPACE_DIR="$REPO_ROOT/portals/ai-workspace"

PLATFORM_API_PORT=9243
BFF_PORT=8081

RUN_DIR="/tmp/api-platform-dev"
mkdir -p "$RUN_DIR"
PLATFORM_API_PID_FILE="$RUN_DIR/platform-api.pid"
BFF_PID_FILE="$RUN_DIR/ai-workspace-bff.pid"
PLATFORM_API_LOG="$RUN_DIR/platform-api.log"
BFF_LOG="$RUN_DIR/ai-workspace-bff.log"

log() { echo "[dev-restart] $*"; }

# ---------------------------------------------------------------------------
# Stop
# ---------------------------------------------------------------------------

stop_by_pidfile() {
  local pid_file="$1" label="$2"
  if [[ -f "$pid_file" ]]; then
    local pid
    pid="$(cat "$pid_file")"
    if kill -0 "$pid" 2>/dev/null; then
      log "Stopping $label (pid $pid)..."
      kill "$pid" 2>/dev/null || true
      for _ in $(seq 1 10); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.5
      done
      kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$pid_file"
  fi
}

stop_by_port() {
  local port="$1" label="$2"
  local pids
  pids="$(lsof -ti "tcp:$port" -sTCP:LISTEN 2>/dev/null || true)"
  if [[ -n "$pids" ]]; then
    log "Stopping $label on port $port (pid(s): $pids)..."
    kill $pids 2>/dev/null || true
    sleep 1
    kill -9 $pids 2>/dev/null || true
  fi
}

log "Stopping any existing instances..."
stop_by_pidfile "$PLATFORM_API_PID_FILE" "platform-api"
stop_by_pidfile "$BFF_PID_FILE" "ai-workspace-bff"
stop_by_port "$PLATFORM_API_PORT" "platform-api"
stop_by_port "$BFF_PORT" "ai-workspace-bff"

# ---------------------------------------------------------------------------
# Pre-flight (one-time secrets — not generated here)
# ---------------------------------------------------------------------------

if [[ ! -f "$PLATFORM_API_DIR/local-dev.env" || ! -f "$PLATFORM_API_DIR/resources/keys/encryption.key" ]]; then
  echo "ERROR: platform-api local-dev secrets not found." >&2
  echo "Run once: (cd platform-api && make setup-local-dev)" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

log "Building platform-api..."
( cd "$PLATFORM_API_DIR" && go build -o bin/platform-api ./cmd/main.go )

log "Building AI Workspace SPA..."
( cd "$AI_WORKSPACE_DIR" && [[ -d node_modules ]] || npm install )
( cd "$AI_WORKSPACE_DIR" && npm run build )

log "Building AI Workspace BFF..."
( cd "$AI_WORKSPACE_DIR" && make bff-build )

# ---------------------------------------------------------------------------
# Start
# ---------------------------------------------------------------------------

log "Starting platform-api on :$PLATFORM_API_PORT (log: $PLATFORM_API_LOG)..."
(
  cd "$PLATFORM_API_DIR"
  export $(grep -v '^#' local-dev.env | xargs)
  export APIP_CP_ENCRYPTION_KEY="$(cat resources/keys/encryption.key)"
  nohup ./bin/platform-api -config config/config.local.toml >"$PLATFORM_API_LOG" 2>&1 &
  echo $! >"$PLATFORM_API_PID_FILE"
)

log "Generating BFF dev certs if needed..."
( cd "$AI_WORKSPACE_DIR" && ../scripts/setup.sh --certs-only )

log "Starting AI Workspace BFF on :$BFF_PORT (log: $BFF_LOG)..."
(
  cd "$AI_WORKSPACE_DIR/bff"
  nohup ../target/ai-workspace-bff \
    -config ../configs/config.toml \
    -config ../configs/config-debug.toml \
    -static-dir ../dist \
    >"$BFF_LOG" 2>&1 &
  echo $! >"$BFF_PID_FILE"
)

sleep 1
log "Done."
log "  Platform API:  https://localhost:$PLATFORM_API_PORT   (pid $(cat "$PLATFORM_API_PID_FILE"), log $PLATFORM_API_LOG)"
log "  AI Workspace:  https://localhost:$BFF_PORT/ai-workspace/   (pid $(cat "$BFF_PID_FILE"), log $BFF_LOG)"

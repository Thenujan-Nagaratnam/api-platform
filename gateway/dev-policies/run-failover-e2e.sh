#!/usr/bin/env bash
#
# Runs the LLM model-failover live e2e suite: starts the two mock LLM
# backends, waits for the real gateway stack to be reachable (does NOT
# build or start it — run `make build` + `docker compose up` yourself
# first, see FAILOVER_TESTING.md), runs the Postman collection via
# newman, then tears the mocks down again regardless of outcome.
#
# Usage: ./run-failover-e2e.sh
# Requires: go, newman (npm install -g newman), curl.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MOCK_OPENAI_DIR="$SCRIPT_DIR/mocks/mock-openai"
MOCK_ANTHROPIC_DIR="$SCRIPT_DIR/mocks/mock-anthropic"
COLLECTION="$SCRIPT_DIR/postman/llm-failover-e2e.postman_collection.json"

CONTROLLER_HEALTH_URL="http://localhost:9094/health"
RUNTIME_HEALTH_URL="https://localhost:8443/_gateway-health/healthy"
MOCK_OPENAI_HEALTH_URL="http://localhost:9611/healthz"
MOCK_ANTHROPIC_HEALTH_URL="http://localhost:9612/healthz"

MOCK_OPENAI_PID=""
MOCK_ANTHROPIC_PID=""

log() { echo "[failover-e2e] $*"; }

cleanup() {
  log "cleaning up mock backends..."
  [[ -n "$MOCK_OPENAI_PID" ]] && kill "$MOCK_OPENAI_PID" 2>/dev/null || true
  [[ -n "$MOCK_ANTHROPIC_PID" ]] && kill "$MOCK_ANTHROPIC_PID" 2>/dev/null || true
}
trap cleanup EXIT

wait_for() {
  local url="$1" name="$2" tries=30
  for ((i = 1; i <= tries; i++)); do
    if curl -sk -o /dev/null -w '%{http_code}' "$url" 2>/dev/null | grep -qE '^[23][0-9][0-9]$'; then
      log "$name is up ($url)"
      return 0
    fi
    sleep 1
  done
  log "ERROR: $name never became healthy at $url after ${tries}s"
  return 1
}

command -v newman >/dev/null 2>&1 || {
  log "ERROR: newman is not installed. Install it with: npm install -g newman"
  exit 1
}
command -v go >/dev/null 2>&1 || {
  log "ERROR: go is not installed or not on PATH"
  exit 1
}

log "checking the real gateway stack is already up (run 'make build' + 'docker compose up' yourself first if not)..."
wait_for "$CONTROLLER_HEALTH_URL" "gateway-controller" || {
  log "hint: cd gateway && docker compose --profile redis up -d gateway-controller gateway-runtime sample-backend redis"
  exit 1
}
wait_for "$RUNTIME_HEALTH_URL" "gateway-runtime" || exit 1

log "starting mock-openai on :9611..."
(cd "$MOCK_OPENAI_DIR" && GOWORK=off go run .) &
MOCK_OPENAI_PID=$!

log "starting mock-anthropic on :9612..."
(cd "$MOCK_ANTHROPIC_DIR" && GOWORK=off go run .) &
MOCK_ANTHROPIC_PID=$!

wait_for "$MOCK_OPENAI_HEALTH_URL" "mock-openai"
wait_for "$MOCK_ANTHROPIC_HEALTH_URL" "mock-anthropic"

log "running the Postman collection via newman..."
newman run "$COLLECTION" --insecure --delay-request 200
NEWMAN_EXIT=$?

log "newman exit code: $NEWMAN_EXIT"
exit "$NEWMAN_EXIT"

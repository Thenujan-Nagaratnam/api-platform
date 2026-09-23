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

# A mock left over from an earlier run keeps its port, so `go run` below
# would fail to bind and exit quietly while the health check passes against
# the stale process — silently testing an old mock build.
for port in 9611 9612; do
  if curl -s -o /dev/null "http://localhost:$port/healthz" 2>/dev/null; then
    log "ERROR: something is already serving :$port — likely a mock from an earlier run."
    log "hint: kill \$(lsof -t -nP -iTCP:$port -sTCP:LISTEN) and re-run"
    exit 1
  fi
done

log "starting mock-openai on :9611..."
(cd "$MOCK_OPENAI_DIR" && GOWORK=off go run .) &
MOCK_OPENAI_PID=$!

log "starting mock-anthropic on :9612..."
(cd "$MOCK_ANTHROPIC_DIR" && GOWORK=off go run .) &
MOCK_ANTHROPIC_PID=$!

wait_for "$MOCK_OPENAI_HEALTH_URL" "mock-openai"
wait_for "$MOCK_ANTHROPIC_HEALTH_URL" "mock-anthropic"

log "running the Postman collection via newman..."
NEWMAN_EXIT=0
newman run "$COLLECTION" --insecure --delay-request 200 || NEWMAN_EXIT=$?
log "newman exit code: $NEWMAN_EXIT"

# Half-open probe concurrency can't be proven from Postman (it runs requests
# one at a time), so it's checked here with concurrent curls against the
# failover-circuit-probes proxy registered in the collection's setup (0.15):
# suspendDuration=3; half-open recovery always sends exactly one probe.
PROXY_URL="https://localhost:8443/failover-circuit-probes/chat/completions"
CHAT_BODY='{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'
OUT_DIR="$(mktemp -d)"

control() {
  local args=(-fsS --max-time 5 -o /dev/null -X POST)
  [[ $# -ge 2 ]] && args+=(-H 'Content-Type: application/json' -d "$2")
  curl "${args[@]}" "$1" || { log "ERROR: mock control call failed: $1"; exit 1; }
}

# Fires $1 concurrent chat requests; prints how many were served by openai
# and by anthropic, as "openai anthropic".
burst() {
  local n="$1" i pids=()
  rm -f "$OUT_DIR"/burst-*
  for ((i = 1; i <= n; i++)); do
    curl -sk --max-time 15 -o /dev/null -D "$OUT_DIR/burst-$i" -X POST -H 'Content-Type: application/json' -d "$CHAT_BODY" "$PROXY_URL" &
    pids+=($!)
  done
  # Wait only for these curls — a bare `wait` would also block on the mock
  # servers started in the background above, which never exit.
  wait "${pids[@]}" || true
  echo "$(cat "$OUT_DIR"/burst-* | grep -ci '^x-mock-backend: openai') $(cat "$OUT_DIR"/burst-* | grep -ci '^x-mock-backend: anthropic')"
}

PROBE_EXIT=0
log "half-open probe concurrency check..."
control http://localhost:9611/control/reset
control http://localhost:9612/control/reset
control http://localhost:9611/control/arm-failure '{"count":1,"status":500}'
burst 1 >/dev/null # primary fails -> suspended for 3s
sleep 4            # suspension expires -> half-open

# One slow probe in flight: the other two must be kept off the recovering primary.
control http://localhost:9611/control/arm-delay '{"count":1,"delayMs":2000}'
read -r to_openai to_anthropic <<<"$(burst 3)"
if [[ "$to_openai" == 1 && "$to_anthropic" == 2 ]]; then
  log "PASS half-open: 1 of 3 concurrent requests probed the primary, 2 went to the fallback"
else
  log "FAIL half-open: expected 1 openai + 2 anthropic, got $to_openai openai + $to_anthropic anthropic"
  PROBE_EXIT=1
fi

# That probe succeeded, so the circuit is closed: no more probe gating.
control http://localhost:9611/control/arm-delay '{"count":3,"delayMs":1000}'
read -r to_openai to_anthropic <<<"$(burst 3)"
if [[ "$to_openai" == 3 && "$to_anthropic" == 0 ]]; then
  log "PASS closed: all 3 concurrent requests reached the recovered primary"
else
  log "FAIL closed: expected 3 openai, got $to_openai openai + $to_anthropic anthropic"
  PROBE_EXIT=1
fi
rm -rf "$OUT_DIR"

if [[ "$NEWMAN_EXIT" != 0 ]]; then
  exit "$NEWMAN_EXIT"
fi
exit "$PROBE_EXIT"

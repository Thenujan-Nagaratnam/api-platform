#!/usr/bin/env bash
#
# One-command runner for the body-attempt-echo dev policy's end-to-end test
# suite (postman/body-attempt-echo.postman_collection.json) against a gateway
# stack that is ALREADY running (see gateway/dev-policies/oauth2-generator/e2e/
# TESTING.md Parts B/C for the general build+run pattern - this script does
# not build or start gateway-controller/gateway-runtime itself).
#
# Proves gateway-controller's x-wso2-upstream-attempt: {body: true} opt-in
# (commit 4204a943c on decoupled-retry-source): a PLAIN, same-endpoint
# resilience.retry route (no aggregate cluster / retry-source involved) gets
# its outgoing request body rewritten on each individual Envoy-native retry
# attempt, once a policy on the route explicitly declares the flag.
#
# What it does:
#   1. Sanity-checks the gateway stack is reachable.
#   2. Starts mock-echo-backend natively (go run) on :9720, unless already up.
#   3. Registers the body-attempt-echo-test RestApi, waits for gateway-runtime
#      to actually pick up the new route via xDS (registration returns 201
#      before the route is live - the same gap oauth2-generator's/
#      model-failover's run-e2e.sh scripts document and poll for) before
#      running any flow that invokes it.
#   4. Runs the baseline (attempt 1, no retry) and retry (forced 503 -> Envoy
#      retry -> body rewritten to attempt 2) flows.
#   5. Retries the whole register->test->cleanup cycle if xDS propagation
#      didn't land in time - transient infra timing, not a policy bug.
#   6. Tears down only the mock process this script started, deletes the
#      resource it registered, and reports a single pass/fail exit code.
#
# Usage:
#   ./run-e2e.sh
#
# Env overrides:
#   CONTROLLER_ADMIN_URL   http://localhost:9094
#   CONTROLLER_BASE_URL    http://localhost:9090
#   GATEWAY_URL            https://localhost:8443
#   BACKEND_ADDR           :9720
#   NEWMAN_REPORTERS       cli,junit
#   MAX_ATTEMPTS           3
#
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COLLECTION="$ROOT/postman/body-attempt-echo.postman_collection.json"
REPORT_DIR="$ROOT/postman/reports"

CONTROLLER_ADMIN_URL="${CONTROLLER_ADMIN_URL:-http://localhost:9094}"
CONTROLLER_BASE_URL="${CONTROLLER_BASE_URL:-http://localhost:9090}"
GATEWAY_URL="${GATEWAY_URL:-https://localhost:8443}"
BACKEND_ADDR="${BACKEND_ADDR:-:9720}"
NEWMAN_REPORTERS="${NEWMAN_REPORTERS:-cli,junit}"
MAX_ATTEMPTS="${MAX_ATTEMPTS:-3}"

BACKEND_PORT="${BACKEND_ADDR#:}"

log() { printf '\n==> %s\n' "$1"; }
die() { printf '\nERROR: %s\n' "$1" >&2; exit 1; }

# ─── prerequisites ───────────────────────────────────────────────────────────

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v go >/dev/null 2>&1 || die "go is required to run the mock server"

NEWMAN=(newman)
if ! command -v newman >/dev/null 2>&1; then
  command -v npx >/dev/null 2>&1 || die "neither 'newman' nor 'npx' found on PATH - install newman: npm install -g newman"
  NEWMAN=(npx --yes newman)
fi

# ─── verify the gateway stack is already up (this script never starts it) ───

log "Checking gateway-controller admin API at $CONTROLLER_ADMIN_URL ..."
curl -sf --max-time 5 "$CONTROLLER_ADMIN_URL/api/admin/v1/health" >/dev/null \
  || die "gateway-controller not reachable at $CONTROLLER_ADMIN_URL - start the stack first: cd gateway && make build && docker compose up -d gateway-controller gateway-runtime sample-backend redis"

log "Checking gateway-runtime (Envoy) at $GATEWAY_URL ..."
curl -sk --max-time 5 "$GATEWAY_URL/" -o /dev/null \
  || die "gateway-runtime not reachable at $GATEWAY_URL - start the stack first"

echo "Gateway stack looks up."

# ─── start the mock (skip if already running) ───────────────────────────────

declare -a STARTED_PIDS=()

cleanup() {
  local ec=$?
  if [ "${#STARTED_PIDS[@]}" -gt 0 ]; then
    log "Stopping mocks this script started (pids: ${STARTED_PIDS[*]}) ..."
    for pid in "${STARTED_PIDS[@]}"; do
      kill "$pid" >/dev/null 2>&1 || true
    done
    wait "${STARTED_PIDS[@]}" 2>/dev/null || true
  fi
  exit $ec
}
trap cleanup EXIT INT TERM

wait_healthy() {
  local url="$1" name="$2" tries=30
  until curl -sf --max-time 1 "$url" >/dev/null 2>&1; do
    tries=$((tries - 1))
    if [ "$tries" -le 0 ]; then
      die "$name never became healthy at $url"
    fi
    sleep 0.5
  done
}

mkdir -p "$REPORT_DIR"

if curl -sf --max-time 1 "http://localhost:${BACKEND_PORT}/healthz" >/dev/null 2>&1; then
  echo "mock-echo-backend already running at :${BACKEND_PORT} - reusing it."
else
  log "Starting mock-echo-backend ..."
  (cd "$ROOT/mocks/mock-echo-backend" && GOWORK=off ADDR="$BACKEND_ADDR" exec go run .) \
    >"$REPORT_DIR/mock-echo-backend.log" 2>&1 &
  mock_pid="$!"
  STARTED_PIDS+=("$mock_pid")
  wait_healthy "http://localhost:${BACKEND_PORT}/healthz" "mock-echo-backend"
  echo "mock-echo-backend is up (pid $mock_pid)."
fi

# ─── shared state ────────────────────────────────────────────────────────────

route_is_up() {
  local path="$1"
  local code
  code=$(curl -sk --max-time 2 -o /dev/null -w '%{http_code}' \
    -X POST "$GATEWAY_URL/$path" \
    -H "Content-Type: application/json" -d '{}')
  [ "$code" != "404" ] && [ "$code" != "000" ]
}

wait_for_route() {
  local path="$1" tries="${2:-40}"
  until route_is_up "$path"; do
    tries=$((tries - 1))
    [ "$tries" -le 0 ] && return 1
    sleep 0.5
  done
  return 0
}

delete_rest_api() {
  curl -s -o /dev/null -X DELETE "$CONTROLLER_BASE_URL/api/management/v1/rest-apis/$1" \
    -H "Authorization: Basic YWRtaW46YWRtaW4="
}

cleanup_all_registered_resources() {
  delete_rest_api body-attempt-echo-test
}

run_newman() {
  local label="$1"; shift
  log "$label"
  "${NEWMAN[@]}" run "$COLLECTION" --insecure "$@" || return 1
}

# ─── one full attempt: register -> wait -> test -> cleanup ─────────────────

run_attempt() {
  run_newman "Health checks ..." \
    --folder "00 - Health Checks" \
    --reporters cli --color on || return 1

  run_newman "Registering body-attempt-echo-test ..." \
    --folder "01 - Register body-attempt-echo-test" \
    --reporters cli --color on || return 1

  log "Waiting for gateway-runtime to pick up body-attempt-echo-test via xDS ..."
  wait_for_route "body-attempt-echo-test/v1.0/echo" || { echo "route for 'body-attempt-echo-test' never came up" >&2; return 1; }
  # Route table showing up doesn't guarantee the policy engine's own policy
  # chain for that route has landed yet (separate, async xDS channel - see
  # oauth2-generator's/model-failover's run-e2e.sh for the same observed
  # gap). Fixed grace sleep, since there's no cheap readiness signal for
  # that second channel.
  sleep 3
  echo "body-attempt-echo-test route is live."

  run_newman "Running baseline flow (attempt 1, no retry) ..." \
    --folder "02 - Baseline: attempt 1 body is still rewritten, even with no retry" \
    --reporters "$NEWMAN_REPORTERS" \
    --reporter-junit-export "$REPORT_DIR/junit-baseline.xml" \
    --color on || return 1

  run_newman "Running retry flow (forced 503 -> Envoy retry -> body rewritten to attempt 2) ..." \
    --folder "03 - Retry: forced 503 on attempt 1 triggers Envoy retry, body rewritten to attempt 2" \
    --reporters "$NEWMAN_REPORTERS" \
    --reporter-junit-export "$REPORT_DIR/junit-retry.xml" \
    --color on || return 1

  run_newman "Cleaning up body-attempt-echo-test ..." \
    --folder "04 - Cleanup" \
    --reporters cli --color on || return 1

  return 0
}

# ─── retry loop ──────────────────────────────────────────────────────────────

attempt=1
success=0
while [ "$attempt" -le "$MAX_ATTEMPTS" ]; do
  log "Attempt $attempt/$MAX_ATTEMPTS"
  if run_attempt; then
    success=1
    break
  fi
  echo "Attempt $attempt failed - cleaning up and retrying." >&2
  cleanup_all_registered_resources
  attempt=$((attempt + 1))
  sleep 2
done

if [ "$success" -ne 1 ]; then
  cleanup_all_registered_resources
  die "all $MAX_ATTEMPTS attempts failed - see output above for the actual assertion failures (this is likely a real regression, not just propagation timing, if it fails consistently)"
fi

log "All body-attempt-echo end-to-end flows passed."

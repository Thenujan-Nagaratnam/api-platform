#!/usr/bin/env bash
#
# One-command runner for pii-masking-regex's jsonPath-coverage e2e suite
# (postman/pii-masking-regex.postman_collection.json) against a gateway
# stack that is ALREADY running (mirrors model-failover/e2e/run-e2e.sh's
# pattern — this script does not build or start gateway-controller/
# gateway-runtime itself).
#
# What it tests: the policy's own request/response masking round-trip is
# already covered by unit tests (piimaskingregex_test.go). What unit tests
# CANNOT show is what a full gateway deploy actually forwards upstream for
# a given `jsonPath` value — that's what this suite verifies, using the
# model-failover mock backend (mocks/mock-model-backend) purely as a
# generic OpenAI-shaped echo target whose GET /debug/last-request exposes
# exactly what it received.
#
# NOTE: mock-model-backend only echoes the "model" field in its response,
# not message content, so this suite verifies REQUEST-side masking
# coverage only, not response-side PII restoration.
#
# What it does:
#   1. Sanity-checks the gateway stack is reachable.
#   2. Starts the mock backend natively (go run) on one shared port, unless
#      already up (safe to share since scenarios run strictly sequentially
#      and reset the mock between requests).
#   3. For each scenario: registers its LlmProvider as its own folder,
#      waits for gateway-runtime to pick up the new route via xDS, THEN
#      runs that scenario's separate test folder.
#   4. Cleans up all registered resources and the mock process this script
#      started, and reports a single pass/fail exit code.
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
#
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MOCK_DIR="$ROOT/../../model-failover/e2e/mocks/mock-model-backend"
COLLECTION="$ROOT/postman/pii-masking-regex.postman_collection.json"
REPORT_DIR="$ROOT/postman/reports"

CONTROLLER_ADMIN_URL="${CONTROLLER_ADMIN_URL:-http://localhost:9094}"
CONTROLLER_BASE_URL="${CONTROLLER_BASE_URL:-http://localhost:9090}"
GATEWAY_URL="${GATEWAY_URL:-https://localhost:8443}"
BACKEND_ADDR="${BACKEND_ADDR:-:9720}"
BACKEND_PORT="${BACKEND_ADDR#:}"
NEWMAN_REPORTERS="${NEWMAN_REPORTERS:-cli,junit}"

# Strip schemes so these can feed postman {{...BaseUrl}} host:port variables.
CONTROLLER_BASE_HOST="${CONTROLLER_BASE_URL#http://}"
CONTROLLER_ADMIN_HOST="${CONTROLLER_ADMIN_URL#http://}"
GATEWAY_HOST="${GATEWAY_URL#https://}"

log() { printf '\n==> %s\n' "$1"; }
die() { printf '\nERROR: %s\n' "$1" >&2; exit 1; }

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

echo "Gateway stack looks up. (pii-masking-regex is already wired into gateway/build.yaml.)"

# ─── start (or reuse) the shared mock backend ───────────────────────────────

mkdir -p "$REPORT_DIR"
declare -a STARTED_PIDS=()

cleanup_all_registered_resources() {
  for p in pii-poc-default pii-poc-wildcard pii-poc-messages pii-poc-empty; do
    curl -s -o /dev/null -X DELETE "$CONTROLLER_BASE_URL/api/management/v1/llm-providers/$p" \
      -H "Authorization: Basic YWRtaW46YWRtaW4="
  done
}

cleanup() {
  local ec=$?
  cleanup_all_registered_resources
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
    [ "$tries" -le 0 ] && die "$name never became healthy at $url"
    sleep 0.5
  done
}

if curl -sf --max-time 1 "http://localhost:${BACKEND_PORT}/healthz" >/dev/null 2>&1; then
  echo "mock-model-backend already running on :$BACKEND_PORT - reusing it."
else
  log "Starting mock-model-backend on :$BACKEND_PORT ..."
  (cd "$MOCK_DIR" && GOWORK=off ADDR="$BACKEND_ADDR" exec go run .) \
    >"$REPORT_DIR/mock-model-backend-$BACKEND_PORT.log" 2>&1 &
  STARTED_PIDS+=("$!")
  wait_healthy "http://localhost:${BACKEND_PORT}/healthz" "mock-model-backend"
  echo "mock-model-backend is up (pid ${STARTED_PIDS[-1]})."
fi

cleanup_all_registered_resources

# ─── route readiness helper (same probe convention as model-failover) ──────

route_is_up() {
  local path="$1" code
  code=$(curl -sk --max-time 2 -o /dev/null -w '%{http_code}' \
    -X POST "$GATEWAY_URL/$path" -H "Content-Type: application/json" -d '{}')
  [ "$code" != "404" ] && [ "$code" != "000" ]
}

wait_for_route() {
  local path="$1" tries="${2:-40}"
  until route_is_up "$path"; do
    tries=$((tries - 1))
    [ "$tries" -le 0 ] && return 1
    sleep 0.5
  done
  sleep 2  # policy-chain xDS can lag slightly behind route xDS
  return 0
}

run_newman() {
  # $1 = human label, remaining args = newman flags/folders
  local label="$1"; shift
  log "$label"
  "${NEWMAN[@]}" run "$COLLECTION" --insecure \
    --env-var "controllerBaseUrl=${CONTROLLER_BASE_HOST}" \
    --env-var "controllerAdminUrl=${CONTROLLER_ADMIN_HOST}" \
    --env-var "gatewayBaseUrl=${GATEWAY_HOST}" \
    --env-var "backendBaseUrl=localhost:${BACKEND_PORT}" \
    --env-var "backendPort=${BACKEND_PORT}" \
    "$@" || return 1
}

# ─── one full attempt: register -> wait -> test, per scenario ──────────────

run_attempt() {
  run_newman "Health checks ..." \
    --folder "00 - Health Checks" \
    --reporters cli --color on || return 1

  run_newman "Registering pii-poc-default (\$.messages[-1].content) ..." \
    --folder "01 - Register: pii-poc-default (\$.messages[-1].content)" \
    --reporters cli --color on || return 1
  log "Waiting for gateway-runtime to pick up pii-poc-default via xDS ..."
  wait_for_route "pii-poc-default/latest/chat/completions" || { echo "route for 'pii-poc-default' never came up" >&2; return 1; }

  run_newman "Scenario 1: default jsonPath masks last message only ..." \
    --folder "02 - Test: default jsonPath masks last message only" \
    --reporters "$NEWMAN_REPORTERS" \
    --reporter-junit-export "$REPORT_DIR/junit-scenario-1-default.xml" \
    --color on || return 1

  run_newman "Registering pii-poc-wildcard (\$.messages[*].content) ..." \
    --folder "03 - Register: pii-poc-wildcard (\$.messages[*].content)" \
    --reporters cli --color on || return 1
  log "Waiting for gateway-runtime to pick up pii-poc-wildcard via xDS ..."
  wait_for_route "pii-poc-wildcard/latest/chat/completions" || { echo "route for 'pii-poc-wildcard' never came up" >&2; return 1; }

  run_newman "Scenario 2: wildcard jsonPath is invalid syntax, hard failure ..." \
    --folder "04 - Test: wildcard jsonPath is invalid syntax, hard failure" \
    --reporters "$NEWMAN_REPORTERS" \
    --reporter-junit-export "$REPORT_DIR/junit-scenario-2-wildcard.xml" \
    --color on || return 1

  run_newman "Registering pii-poc-messages (\$.messages) ..." \
    --folder "05 - Register: pii-poc-messages (\$.messages)" \
    --reporters cli --color on || return 1
  log "Waiting for gateway-runtime to pick up pii-poc-messages via xDS ..."
  wait_for_route "pii-poc-messages/latest/chat/completions" || { echo "route for 'pii-poc-messages' never came up" >&2; return 1; }

  run_newman "Scenario 3: \$.messages is non-scalar, silent bypass ..." \
    --folder "06 - Test: \$.messages is a valid key but non-scalar, silent bypass" \
    --reporters "$NEWMAN_REPORTERS" \
    --reporter-junit-export "$REPORT_DIR/junit-scenario-3-messages.xml" \
    --color on || return 1

  run_newman "Registering pii-poc-empty (jsonPath: \"\") ..." \
    --folder "07 - Register: pii-poc-empty (jsonPath: \"\")" \
    --reporters cli --color on || return 1
  log "Waiting for gateway-runtime to pick up pii-poc-empty via xDS ..."
  wait_for_route "pii-poc-empty/latest/chat/completions" || { echo "route for 'pii-poc-empty' never came up" >&2; return 1; }

  run_newman "Scenario 4: empty jsonPath covers the whole payload ..." \
    --folder "08 - Test: empty jsonPath covers the whole payload" \
    --reporters "$NEWMAN_REPORTERS" \
    --reporter-junit-export "$REPORT_DIR/junit-scenario-4-empty.xml" \
    --color on || return 1

  run_newman "Cleanup ..." \
    --folder "09 - Cleanup" \
    --reporters cli --color on || return 1

  return 0
}

if run_attempt; then
  log "PASS - all pii-masking-regex jsonPath-coverage assertions passed."
  exit 0
else
  log "FAIL - see output above and $REPORT_DIR/junit-*.xml for details."
  exit 1
fi

#!/usr/bin/env bash
#
# One-command runner for the model-failover policy's end-to-end test suite
# (postman/model-failover.postman_collection.json) against a gateway stack
# that is ALREADY running (see gateway/dev-policies/oauth2-generator/e2e/
# TESTING.md Parts B/C for the general build+run pattern - this script does
# not build or start gateway-controller/gateway-runtime itself).
#
# This collection targets the response-path retry mechanism (see
# model_failover.go's package doc) - NOT the earlier Envoy
# aggregate-cluster/upstream-ext_proc design, and NOT the still-earlier
# target-level pre-dial redirect this policy also once had (removed
# entirely - see the package doc's own "deliberate simplification" note).
# The client's own primary request is NEVER intercepted, redirected, or
# otherwise modified by this policy: it always reaches whatever the rest of
# the operation's own policy chain (llm-header-router, a translator, Envoy's
# own default routing) already resolves it to. This policy acts only from
# OnResponseHeaders, and only once that primary attempt has genuinely been
# dialed and come back with a status in statusCodes. Every fallback this
# policy then originates - same-provider or cross-provider - is a
# SELF-REDIAL: it dials this same operation's own externally-facing URL
# again, so the attempt re-runs the FULL policy chain (rate limiting,
# analytics, any request/response transformation) exactly like a genuine
# client request, rather than a raw direct call bypassing all of that. This
# policy never carries its own auth/template adapter of any kind - a
# cross-provider attempt gets its credential injection and body conversion
# from whatever real, already-attached translator/upstream-auth policies the
# operator configured for multi-provider routing (e.g. llm-header-router):
#
#   - Same-provider flows (LlmProvider): unmatched-model passthrough, a bare
#     fallback reusing the primary's own backend (self-redial, no selection
#     header), an ordered chain via upstreamDefinitions (self-redial
#     carrying the chosen upstreamDefinition name, applied as an in-process
#     UpstreamName redirect on the redial's own fresh pass), suspend
#     deprioritization, full exhaustion falling through to the original
#     response, a network-unreachable fallback, and maxResponseBytes
#     rejecting an oversized response.
#   - Target-level `provider` is a MATCH qualifier now, never a redirect: it
#     disambiguates which target/fallback-chain applies when the same model
#     name can legitimately reach an operation via more than one provider,
#     read from the PRIMARY request's own x-provider header (never anything
#     this policy sets or routes) - an exact (model, provider) match wins
#     over a provider-agnostic (model, "") target, which itself is used when
#     no more specific target exists or the primary carried no provider
#     header at all; a provider-scoped-only target with no matching header
#     and no catch-all just lets a real failure pass through untouched. A
#     `provider:` reference registers fine on a plain LlmProvider too -
#     gateway-controller never validates it either way, on any resource kind.
#   - Cross-provider flows (LlmProxy), reusing a real additionalProviders
#     entry (anthropic-provider / anthropic-backup):
#       - A FALLBACK's own provider: a self-redial after the primary fails,
#         re-entering the full chain with x-provider set - auth and template
#         conversion both apply for real, via the SAME already-attached
#         translator/upstream-auth policies any other request through that
#         provider would use. No adapter of this policy's own is involved.
#       - Target-level provider matching disambiguating which fallback chain
#         applies, exercised both when the client's own x-provider header
#         hits an exact (model, provider) target and when it falls back to
#         a provider-agnostic one.
#       - A malformed body making the provider's own translator reject the
#         redial, falling through to the original response cleanly.
#
# What it does:
#   1. Sanity-checks the gateway stack is reachable.
#   2. Starts the mock backends natively (go run) on different ports, unless
#      they're already up.
#   3. Registers each API config as its own folder, waits for gateway-runtime
#      to actually pick up the new route via xDS before running any folder
#      that invokes it, then runs the flow/verification folders.
#   4. Retries the whole run if xDS propagation didn't land in time - a
#      transient infrastructure-timing condition, not a policy bug.
#   5. Tears down only the mock processes THIS script started, deletes every
#      resource it registered, and reports a single pass/fail exit code.
#
# Usage:
#   ./run-e2e.sh
#
# Env overrides:
#   CONTROLLER_ADMIN_URL   http://localhost:9094
#   CONTROLLER_BASE_URL    http://localhost:9090
#   GATEWAY_URL            https://localhost:8443
#   BACKEND_A_ADDR         :9711  (mf-poc-provider / mf-poc-chain-test primary)
#   BACKEND_B_ADDR         :9712  (same-provider fallback target)
#   BACKEND_C_ADDR         :9713  (same-provider fallback target)
#   BACKEND_ANTH_ADDR      :9714  (anthropic-provider's own upstream - anthropic-shaped)
#   BACKEND_P_ADDR         :9715  (mf-poc-proxy-provider primary)
#   DEAD_PORT              9799   (nothing listens here - network-error test)
#   NEWMAN_REPORTERS       cli,junit
#   MAX_ATTEMPTS           3
#
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COLLECTION="$ROOT/postman/model-failover.postman_collection.json"
REPORT_DIR="$ROOT/postman/reports"

CONTROLLER_ADMIN_URL="${CONTROLLER_ADMIN_URL:-http://localhost:9094}"
CONTROLLER_BASE_URL="${CONTROLLER_BASE_URL:-http://localhost:9090}"
GATEWAY_URL="${GATEWAY_URL:-https://localhost:8443}"
BACKEND_A_ADDR="${BACKEND_A_ADDR:-:9711}"
BACKEND_B_ADDR="${BACKEND_B_ADDR:-:9712}"
BACKEND_C_ADDR="${BACKEND_C_ADDR:-:9713}"
BACKEND_ANTH_ADDR="${BACKEND_ANTH_ADDR:-:9714}"
BACKEND_P_ADDR="${BACKEND_P_ADDR:-:9715}"
DEAD_PORT="${DEAD_PORT:-9799}"
NEWMAN_REPORTERS="${NEWMAN_REPORTERS:-cli,junit}"
MAX_ATTEMPTS="${MAX_ATTEMPTS:-3}"

BACKEND_A_PORT="${BACKEND_A_ADDR#:}"
BACKEND_B_PORT="${BACKEND_B_ADDR#:}"
BACKEND_C_PORT="${BACKEND_C_ADDR#:}"
BACKEND_ANTH_PORT="${BACKEND_ANTH_ADDR#:}"
BACKEND_P_PORT="${BACKEND_P_ADDR#:}"

log() { printf '\n==> %s\n' "$1"; }
die() { printf '\nERROR: %s\n' "$1" >&2; exit 1; }

# ─── prerequisites ───────────────────────────────────────────────────────────

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v go >/dev/null 2>&1 || die "go is required to run the mock servers"

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

# ─── start the mocks (skip any that's already running) ─────────────────────

declare -a STARTED_PIDS=()

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
    if [ "$tries" -le 0 ]; then
      die "$name never became healthy at $url"
    fi
    sleep 0.5
  done
}

start_go_mock_if_needed() {
  # $1=dir under mocks/  $2=healthz url  $3=addr env value  $4=label
  local dir="$1" healthz_url="$2" addr_env_val="$3" label="$4"
  if curl -sf --max-time 1 "$healthz_url" >/dev/null 2>&1; then
    echo "$label already running at $healthz_url - reusing it."
    return
  fi
  log "Starting $label ..."
  (cd "$ROOT/mocks/$dir" && GOWORK=off ADDR="$addr_env_val" exec go run .) \
    >"$REPORT_DIR/$dir-$(echo "$addr_env_val" | tr -d ':').log" 2>&1 &
  STARTED_PIDS+=("$!")
  wait_healthy "$healthz_url" "$label"
  echo "$label is up (pid $!)."
}

mkdir -p "$REPORT_DIR"

start_go_mock_if_needed "mock-model-backend" "http://localhost:${BACKEND_A_PORT}/healthz" "$BACKEND_A_ADDR" "mock-model-backend (A / mf-poc-provider + mf-poc-chain-test primary)"
start_go_mock_if_needed "mock-model-backend" "http://localhost:${BACKEND_B_PORT}/healthz" "$BACKEND_B_ADDR" "mock-model-backend (B / same-provider fallback)"
start_go_mock_if_needed "mock-model-backend" "http://localhost:${BACKEND_C_PORT}/healthz" "$BACKEND_C_ADDR" "mock-model-backend (C / same-provider fallback)"
start_go_mock_if_needed "mock-anthropic-backend" "http://localhost:${BACKEND_ANTH_PORT}/healthz" "$BACKEND_ANTH_ADDR" "mock-anthropic-backend (anthropic-provider's own upstream)"
start_go_mock_if_needed "mock-model-backend" "http://localhost:${BACKEND_P_PORT}/healthz" "$BACKEND_P_ADDR" "mock-model-backend (P / mf-poc-proxy-provider primary)"

# DEAD_PORT is deliberately never bound - it's the network-unreachable
# fallback target used by folder 08.
if curl -sf --max-time 1 "http://localhost:${DEAD_PORT}/" >/dev/null 2>&1; then
  die "DEAD_PORT ($DEAD_PORT) has something listening on it - the network-error test needs it to stay unbound. Set DEAD_PORT to a genuinely free port."
fi

# ─── shared state ────────────────────────────────────────────────────────────

# route_is_up posts an empty body at the given context path and treats
# anything other than a 404/000 (curl's "no HTTP response at all" code) as
# "the route exists" - a 400/422/500 from an intentionally-malformed probe
# body still proves the route itself is live, which is all this checks.
#
# gateway-runtime syncs a route's own routing table and its policy-chain
# store via separate xDS channels that can land at different times - a
# route can become dispatchable (no longer 404) before its policy chain has
# synced, in which case every request 500s with the fixed, generic
# '{"error":"Internal Server Error"}' body ("Policy chain not found for
# route" in the policy-engine's own logs) regardless of what was posted.
# That generic body is never what an actual attached policy or backend
# produces (compare the mock backends' own distinctly-shaped error bodies),
# so it's a reliable "not really live yet" signal - keep polling on it
# rather than accepting the route as up.
route_is_up() {
  local path="$1"
  local code body
  body=$(curl -sk --max-time 2 -w '\n%{http_code}' \
    -X POST "$GATEWAY_URL/$path" \
    -H "Content-Type: application/json" -d '{}')
  code="${body##*$'\n'}"
  body="${body%$'\n'*}"
  [ "$code" = "404" ] && return 1
  [ "$code" = "000" ] && return 1
  [ "$code" = "500" ] && [ "$body" = '{"error":"Internal Server Error"}' ] && return 1
  return 0
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

delete_llm_provider() {
  curl -s -o /dev/null -X DELETE "$CONTROLLER_BASE_URL/api/management/v1/llm-providers/$1" \
    -H "Authorization: Basic YWRtaW46YWRtaW4="
}

delete_llm_proxy() {
  curl -s -o /dev/null -X DELETE "$CONTROLLER_BASE_URL/api/management/v1/llm-proxies/$1" \
    -H "Authorization: Basic YWRtaW46YWRtaW4="
}

cleanup_all_registered_resources() {
  delete_llm_proxy mf-poc-proxy
  delete_llm_provider mf-poc-proxy-provider
  delete_llm_provider anthropic-provider
  delete_llm_provider mf-poc-sizelimit
  delete_llm_provider mf-poc-chain-test
  delete_llm_provider mf-poc-provider
  # folder 11 already deletes this itself on the happy path; defensive
  # cleanup here in case an earlier assertion in that folder failed first.
  delete_llm_provider mf-poc-unwired-provider
}

run_newman() {
  # $1 = human label, remaining args = newman flags/folders
  local label="$1"; shift
  log "$label"
  "${NEWMAN[@]}" run "$COLLECTION" --insecure "$@" || return 1
}

# ─── one full attempt: register -> wait -> test -> cleanup ─────────────────

run_attempt() {
  run_newman "Health checks ..." \
    --folder "00 - Health Checks" \
    --reporters cli --color on || return 1

  run_newman "Registering mf-poc-provider ..." \
    --folder "01 - LlmProvider: Register mf-poc-provider (bare fallback)" \
    --reporters cli --color on || return 1

  log "Waiting for gateway-runtime to pick up mf-poc-provider via xDS ..."
  wait_for_route "mf-poc-provider/latest/chat/completions" || { echo "route for 'mf-poc-provider' never came up" >&2; return 1; }
  sleep 3
  echo "mf-poc-provider route is live."

  run_newman "Running the bare-fallback flow ..." \
    --folder "02 - LlmProvider: Unmatched model passes through untouched" \
    --folder "03 - LlmProvider: Bare same-provider fallback recovers a transient failure" \
    --reporters "$NEWMAN_REPORTERS" \
    --reporter-junit-export "$REPORT_DIR/junit-bare-fallback.xml" \
    --color on || return 1

  run_newman "Registering mf-poc-chain-test ..." \
    --folder "04 - LlmProvider: Register mf-poc-chain-test (same-provider url overrides via upstreamDefinitions)" \
    --reporters cli --color on || return 1

  log "Waiting for gateway-runtime to pick up mf-poc-chain-test via xDS ..."
  wait_for_route "mf-poc-chain-test/latest/chat/completions" || { echo "route for 'mf-poc-chain-test' never came up" >&2; return 1; }
  sleep 3
  echo "mf-poc-chain-test route is live."

  run_newman "Running the chain/suspend/exhaustion/neterr flows ..." \
    --folder "05 - LlmProvider: Ordered same-provider chain - first fails, second succeeds" \
    --folder "06 - LlmProvider: Suspend deprioritizes a recently-failed fallback" \
    --folder "07 - LlmProvider: All same-provider fallbacks exhausted - original response passes through" \
    --folder "08 - LlmProvider: Network-unreachable fallback falls through to the next" \
    --reporters "$NEWMAN_REPORTERS" \
    --reporter-junit-export "$REPORT_DIR/junit-chain-flows.xml" \
    --color on || return 1

  run_newman "Registering mf-poc-sizelimit ..." \
    --folder "09 - LlmProvider: Register mf-poc-sizelimit (tiny maxResponseBytes)" \
    --reporters cli --color on || return 1

  log "Waiting for gateway-runtime to pick up mf-poc-sizelimit via xDS ..."
  wait_for_route "mf-poc-sizelimit/latest/chat/completions" || { echo "route for 'mf-poc-sizelimit' never came up" >&2; return 1; }
  sleep 3
  echo "mf-poc-sizelimit route is live."

  run_newman "Running maxResponseBytes flow ..." \
    --folder "10 - LlmProvider: Oversized fallback response is rejected" \
    --reporters "$NEWMAN_REPORTERS" \
    --reporter-junit-export "$REPORT_DIR/junit-sizelimit.xml" \
    --color on || return 1

  run_newman "Registering mf-poc-unwired-provider ..." \
    --folder "11 - LlmProvider: Register mf-poc-unwired-provider (provider reference with no selector attached)" \
    --reporters cli --color on || return 1

  log "Waiting for gateway-runtime to pick up mf-poc-unwired-provider via xDS ..."
  wait_for_route "mf-poc-unwired-provider/latest/chat/completions" || { echo "route for 'mf-poc-unwired-provider' never came up" >&2; return 1; }
  sleep 3
  echo "mf-poc-unwired-provider route is live."

  run_newman "Running the unwired-provider flow ..." \
    --folder "11b - LlmProvider: Provider-scoped target with no catch-all - unmatched provider passes a real failure through unchanged" \
    --reporters "$NEWMAN_REPORTERS" \
    --reporter-junit-export "$REPORT_DIR/junit-unwired-provider.xml" \
    --color on || return 1

  run_newman "Registering anthropic-provider ..." \
    --folder "12 - LlmProvider: Register anthropic-provider (real backing target for additionalProviders)" \
    --reporters cli --color on || return 1

  log "Waiting for gateway-runtime to pick up anthropic-provider via xDS ..."
  wait_for_route "anthropic-provider/latest/v1/messages" || { echo "route for 'anthropic-provider' never came up" >&2; return 1; }
  sleep 3
  echo "anthropic-provider route is live."

  run_newman "Registering mf-poc-proxy-provider + mf-poc-proxy ..." \
    --folder "13 - LlmProxy: Register base provider + proxy (additionalProviders + model-failover)" \
    --reporters cli --color on || return 1

  log "Waiting for gateway-runtime to pick up mf-poc-proxy via xDS ..."
  wait_for_route "mf-poc-proxy/chat/completions" || { echo "route for 'mf-poc-proxy' never came up" >&2; return 1; }
  sleep 3
  echo "mf-poc-proxy route is live."

  run_newman "Running LlmProxy provider-reuse flows ..." \
    --folder "14 - LlmProxy: Fallback-level provider - template + auth reuse on a response-path retry" \
    --folder "15 - LlmProxy: Provider-scoped target - exact (model, provider) match wins over the provider-agnostic target" \
    --folder "15b - LlmProxy: No matching provider falls back to the provider-agnostic target" \
    --folder "16 - LlmProxy: Malformed body fails the cross-provider adapter - passthrough" \
    --reporters "$NEWMAN_REPORTERS" \
    --reporter-junit-export "$REPORT_DIR/junit-llmproxy-provider-reuse.xml" \
    --color on || return 1

  # Regression test for the cross-provider model-override bug: openai-to-
  # anthropic-transformer now resolves its model from the request body first,
  # falling back to a configured one only when the request doesn't carry it -
  # so model-failover's own per-fallback/target model reaches Anthropic
  # instead of being silently discarded. See folder 16b's own test-script
  # comment for the full explanation.
  run_newman "Running cross-provider model resolution regression test ..." \
    --folder "16b - LlmProxy: model-failover's own fallback model reaches a cross-provider translator (fix regression test)" \
    --reporters "$NEWMAN_REPORTERS" \
    --reporter-junit-export "$REPORT_DIR/junit-cross-provider-model-resolution.xml" \
    --color on || return 1

  run_newman "Cleanup ..." \
    --folder "17 - Cleanup" \
    --reporters cli --color on || return 1

  return 0
}

# ─── retry the whole attempt on transient xDS-propagation flakiness ────────

attempt=1
while [ "$attempt" -le "$MAX_ATTEMPTS" ]; do
  log "Attempt $attempt/$MAX_ATTEMPTS"
  cleanup_all_registered_resources
  if run_attempt; then
    log "PASSED on attempt $attempt"
    exit 0
  fi
  echo "Attempt $attempt failed." >&2
  attempt=$((attempt + 1))
done

die "model-failover e2e suite failed after $MAX_ATTEMPTS attempts"

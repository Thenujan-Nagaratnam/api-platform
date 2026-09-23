#!/usr/bin/env bash
#
# Live proof test for design doc §19 "Required proof tests before adoption",
# item #3: "Interaction between a skipped member and the next retry attempt."
#
# Answers, against a REAL Envoy (not just unit-tested xDS generation), the
# question the user asked ("can we change consecutive_5xx to use the
# config?"): is the leaf outlier-detection ejection threshold safe to raise
# above 1? Specifically: if an earlier leaf in a composite chain is already
# ejected BEFORE a request starts, does Envoy's within-request retry
# progression correctly skip it and advance monotonically to the next leaf,
# or can it non-deterministically re-dial an already-tried leaf?
#
# This script assumes gateway-controller/pkg/xds/failover_cluster.go's
# TEMPORARY proofTestConsecutiveFailureThreshold override (currently 3) is
# built into the running gateway-controller image. It does NOT build or
# start the stack itself — see FAILOVER_TESTING.md steps 1-3. Run this after
# the mocks and the real gateway stack (rebuilt with that temporary change)
# are both up.
#
# Usage: ./run-outlier-threshold-proof.sh
# Requires: curl, jq.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CONTROLLER_BASE_URL="http://localhost:9090/api/management/v1"
RUNTIME_BASE_URL="https://localhost:8443"
MOCK_OPENAI_BASE_URL="http://localhost:9611"
MOCK_ANTHROPIC_BASE_URL="http://localhost:9612"
ADMIN_AUTH="Basic YWRtaW46YWRtaW4="
PRIMARY_API_KEY="PRIMARY_KEY"
ANTHROPIC_API_KEY="ANTHROPIC_KEY"

PROXY_CONTEXT="outlier-threshold-proof"

log() { echo "[outlier-proof] $*"; }
fail() { echo "[outlier-proof] FAIL: $*" >&2; exit 1; }

curl_json() {
  # curl_json METHOD URL [BODY]
  local method="$1" url="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sk -o /tmp/outlier-proof-resp.json -w '%{http_code}' \
      -X "$method" "$url" \
      -H "Authorization: $ADMIN_AUTH" -H "Content-Type: application/json" \
      -d "$body"
  else
    curl -sk -o /tmp/outlier-proof-resp.json -w '%{http_code}' \
      -X "$method" "$url" -H "Authorization: $ADMIN_AUTH"
  fi
}

register_provider() {
  local name="$1" context="$2" upstream_url="$3"
  local body
  body=$(cat <<EOF
{
  "apiVersion": "gateway.api-platform.wso2.com/v1",
  "kind": "LlmProvider",
  "metadata": { "name": "$name" },
  "spec": {
    "displayName": "$name",
    "version": "v1.0",
    "template": "openai",
    "context": "/$context",
    "upstream": { "url": "$upstream_url" },
    "accessControl": { "mode": "allow_all" }
  }
}
EOF
)
  local code
  code=$(curl_json POST "$CONTROLLER_BASE_URL/llm-providers" "$body")
  if [[ "$code" == "201" || "$code" == "409" ]]; then
    log "provider $name: HTTP $code (201=created, 409=already exists, both fine)"
  elif [[ "$code" == "400" ]] && grep -q "already exists" /tmp/outlier-proof-resp.json; then
    log "provider $name: HTTP 400 already exists (idempotent, fine)"
  else
    fail "register provider $name: HTTP $code: $(cat /tmp/outlier-proof-resp.json)"
  fi
}

register_proxy() {
  local name="$1" body="$2"
  local code
  code=$(curl_json POST "$CONTROLLER_BASE_URL/llm-proxies" "$body")
  if [[ "$code" == "201" || "$code" == "409" ]]; then
    log "proxy $name: HTTP $code (201=created, 409=already exists, both fine)"
  elif [[ "$code" == "400" ]] && grep -q "already exists" /tmp/outlier-proof-resp.json; then
    log "proxy $name: HTTP 400 already exists (idempotent, fine)"
  else
    fail "register proxy $name: HTTP $code: $(cat /tmp/outlier-proof-resp.json)"
  fi
}

wait_for() {
  local url="$1" name="$2" tries=30
  for ((i = 1; i <= tries; i++)); do
    if curl -sk -o /dev/null -w '%{http_code}' "$url" 2>/dev/null | grep -qE '^[23][0-9][0-9]$'; then
      log "$name is up ($url)"
      return 0
    fi
    sleep 1
  done
  fail "$name never became healthy at $url after ${tries}s"
}

mock_control() {
  # mock_control BASE_URL PATH [BODY]
  local base="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sk -X POST "$base$path" -H "Content-Type: application/json" -d "$body" >/dev/null
  else
    curl -sk -X POST "$base$path" >/dev/null
  fi
}

mock_history_count() {
  local base="$1"
  curl -sk "$base/control/history" | jq -r '.callCount'
}

mock_history_models() {
  # last model received per call, in order
  local base="$1"
  curl -sk "$base/control/history" | jq -r '.history[].model // empty' 2>/dev/null || true
}

log "checking gateway stack + mocks are already up..."
wait_for "http://localhost:9094/health" "gateway-controller"
wait_for "$RUNTIME_BASE_URL/_gateway-health/healthy" "gateway-runtime"
wait_for "$MOCK_OPENAI_BASE_URL/healthz" "mock-openai"
wait_for "$MOCK_ANTHROPIC_BASE_URL/healthz" "mock-anthropic"

log "registering providers (idempotent)..."
register_provider "openai-provider" "openai-provider" "http://host.docker.internal:9611/v1"
register_provider "anthropic-provider" "anthropic-provider" "http://host.docker.internal:9612"

PROXY_BODY=$(cat <<EOF
{
  "apiVersion": "gateway.api-platform.wso2.com/v1",
  "kind": "LlmProxy",
  "metadata": { "name": "$PROXY_CONTEXT" },
  "spec": {
    "displayName": "Outlier Threshold Proof (3-member chain)",
    "version": "v1.0",
    "context": "/$PROXY_CONTEXT",
    "provider": {
      "id": "openai-provider",
      "auth": { "type": "api-key", "header": "Authorization", "value": "Bearer $PRIMARY_API_KEY" }
    },
    "additionalProviders": [
      {
        "id": "anthropic-provider",
        "as": "anthropic-upstream",
        "auth": { "type": "api-key", "header": "X-Api-Key", "value": "$ANTHROPIC_API_KEY" },
        "transformer": {
          "type": "openai-to-anthropic-transformer",
          "version": "v0",
          "params": { "model": "claude-3-5-sonnet-20241022" }
        }
      }
    ],
    "operationPolicies": [
      {
        "name": "llm-header-router",
        "version": "v0",
        "paths": [{
          "path": "/chat/completions", "methods": ["POST"],
          "params": {
            "defaultProvider": "openai-provider",
            "mappings": [{ "headerValue": "openai", "provider": "openai-provider" }]
          }
        }]
      },
      {
        "name": "model-failover",
        "version": "v0",
        "paths": [{
          "path": "/chat/completions", "methods": ["POST"],
          "params": {
            "targets": [{
              "model": "gpt-4o",
              "fallbacks": [
                { "model": "gpt-4o-mini" },
                { "model": "claude-3-5-sonnet-20241022", "provider": "anthropic-upstream" }
              ]
            }]
          }
        }]
      }
    ]
  }
}
EOF
)
register_proxy "$PROXY_CONTEXT" "$PROXY_BODY"

CHAT_URL="$RUNTIME_BASE_URL/$PROXY_CONTEXT/chat/completions"

send_chat() {
  curl -sk -o /tmp/outlier-proof-chat.json -w '%{http_code}' \
    -X POST "$CHAT_URL" -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'
}

# ---------------------------------------------------------------------------
# Test 1: within-request retry escalation is attempt-count-driven.
# Leaf0 (primary, gpt-4o) fails once -> attempt 2 must land on leaf1
# (gpt-4o-mini, same provider), NOT re-dial leaf0 and NOT skip to leaf2
# (anthropic). Anthropic must see zero calls.
# ---------------------------------------------------------------------------
log ""
log "=== Test 1: attempt-count-driven progression ==="
mock_control "$MOCK_OPENAI_BASE_URL" "/control/reset"
mock_control "$MOCK_ANTHROPIC_BASE_URL" "/control/reset"
mock_control "$MOCK_OPENAI_BASE_URL" "/control/arm-failure" '{"count":1,"status":500}'

code=$(send_chat)
log "response HTTP $code"
received_model=$(jq -r '.model // empty' /tmp/outlier-proof-chat.json 2>/dev/null || true)
log "client saw response for model: ${received_model:-<none>}"

openai_calls=$(mock_history_count "$MOCK_OPENAI_BASE_URL")
anthropic_calls=$(mock_history_count "$MOCK_ANTHROPIC_BASE_URL")
log "mock-openai callCount=$openai_calls, mock-anthropic callCount=$anthropic_calls"

if [[ "$anthropic_calls" != "0" ]]; then
  fail "Test 1: anthropic was dialed (callCount=$anthropic_calls) — progression skipped leaf1 (gpt-4o-mini) entirely"
fi
if [[ "$openai_calls" != "2" ]]; then
  fail "Test 1: expected 2 calls to mock-openai (leaf0 fail + leaf1 succeed), got $openai_calls"
fi
log "Test 1 PASSED: 2 openai calls (leaf0->leaf1), 0 anthropic calls — attempt-count-driven, no skip-ahead"

# ---------------------------------------------------------------------------
# Test 2: already-ejected-before-request edge case.
#
# Drive leaf0 (primary, gpt-4o) to 3 consecutive failures via 3 separate
# warm-up requests (each: arm openai to fail exactly 1 call, send a request
# for gpt-4o — attempt 1 hits leaf0 and fails, attempt 2 hits leaf1 and
# succeeds). leaf0 never succeeds in any warm-up, so its consecutive-failure
# counter reaches proofTestConsecutiveFailureThreshold (3) and Envoy's
# outlier detector ejects it. leaf1 (gpt-4o-mini) only ever sees successes,
# so it stays healthy.
#
# Then, WITHOUT waiting for leaf0's ejection to expire (base_ejection_time
# defaults to 5s and doubles per repeat ejection — this test must run fast),
# reset call *history* (does not touch Envoy's in-memory ejection state) and
# arm openai to fail the next 2 calls. Send the real test request.
#
# Expected if progression is correct: leaf0 is skipped (ejected), attempt 1
# goes straight to leaf1 (consumes 1 armed failure), attempt 2 advances to
# leaf2 (anthropic) — mock-openai sees exactly 1 NEW call this round.
#
# Expected if design doc §6.3's concern is real: attempt 2 re-dials leaf1
# instead of advancing — mock-openai sees 2 NEW calls this round, both for
# gpt-4o-mini, and anthropic is never reached.
# ---------------------------------------------------------------------------
log ""
log "=== Test 2: already-ejected-leaf retry-skip edge case ==="
log "driving leaf0 (primary) to $((3)) consecutive failures via 3 warm-up requests..."
for i in 1 2 3; do
  mock_control "$MOCK_OPENAI_BASE_URL" "/control/reset"
  mock_control "$MOCK_OPENAI_BASE_URL" "/control/arm-failure" '{"count":1,"status":500}'
  code=$(send_chat)
  log "warm-up $i: HTTP $code"
done

log "resetting call histories only (Envoy ejection state persists in-memory)..."
mock_control "$MOCK_OPENAI_BASE_URL" "/control/reset"
mock_control "$MOCK_ANTHROPIC_BASE_URL" "/control/reset"
mock_control "$MOCK_OPENAI_BASE_URL" "/control/arm-failure" '{"count":2,"status":500}'

code=$(send_chat)
log "test request: HTTP $code"
received_model=$(jq -r '.model // empty' /tmp/outlier-proof-chat.json 2>/dev/null || true)
log "client saw response for model: ${received_model:-<none>}"

openai_calls_after=$(mock_history_count "$MOCK_OPENAI_BASE_URL")
anthropic_calls_after=$(mock_history_count "$MOCK_ANTHROPIC_BASE_URL")
log "mock-openai NEW callCount=$openai_calls_after, mock-anthropic NEW callCount=$anthropic_calls_after"
log "mock-openai models received this round: $(mock_history_models "$MOCK_OPENAI_BASE_URL" | tr '\n' ',')"

if [[ "$openai_calls_after" == "1" && "$anthropic_calls_after" == "1" ]]; then
  log "Test 2 RESULT: SAFE — leaf0 correctly skipped, attempt 1->leaf1 (1 openai call), attempt 2->leaf2 (1 anthropic call)."
  log "Design doc §6.3's concern does NOT reproduce at threshold=3. Raising consecutive_5xx above 1 looks safe for this scenario."
  exit 0
elif [[ "$openai_calls_after" == "2" && "$anthropic_calls_after" == "0" ]]; then
  log "Test 2 RESULT: BUG REPRODUCED — leaf1 was re-dialed twice (2 openai calls, 0 anthropic calls)."
  log "Design doc §6.3's concern is REAL: an already-ejected-before-request leaf can desync retry progression."
  log "Do NOT raise consecutive_5xx above 1 without further mitigation."
  exit 2
else
  log "Test 2 RESULT: INCONCLUSIVE — unexpected call pattern (openai=$openai_calls_after, anthropic=$anthropic_calls_after)."
  log "Inspect /control/history on both mocks manually before drawing a conclusion."
  exit 3
fi

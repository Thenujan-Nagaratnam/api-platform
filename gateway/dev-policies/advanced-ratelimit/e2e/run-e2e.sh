#!/usr/bin/env bash
#
# One-command runner for advanced-ratelimit's Redis-precedence end-to-end test
# (postman/advanced-ratelimit-redis-precedence.postman_collection.json)
# against a gateway stack that is ALREADY running.
#
# Unlike oauth2-generator's run-e2e.sh, this script DOES restart gateway-runtime
# (via `docker compose up -d gateway-runtime`) - backend/redis.* are
# systemParameters (operator/gateway-wide, resolved once from config.toml at
# process startup, uniform across every advanced-ratelimit instance), not
# something a policy attachment's params can set per-API. There is no way to
# exercise "no override" vs "policy-level override" against one running
# process; each phase needs its own env state and a restart. It edits
# gateway/api-platform.env to do this - only inside a clearly delimited,
# idempotent block (see set_env_block/strip_env_block) - and always restores
# the original state (block removed, gateway-runtime restarted once more) on
# exit, success or failure, via the same trap-based cleanup pattern
# oauth2-generator's script uses for its mock processes.
#
# What it does, per phase:
#   1. No override configured -> asserts 200/200/429 via newman AND that the
#      rate-limit key landed in the gateway's default "redis" service, not
#      "redis-override-test" (real redis-cli KEYS check - the Postman
#      collection's own HTTP-only assertions can't see this).
#   2. Policy-level override configured (redis-override-test, reachable) ->
#      asserts the identical 200/200/429 AND that the key now lands in
#      redis-override-test instead.
#   3. Policy-level override configured but unreachable, failureMode: closed
#      -> asserts 500 (the route fails to build) - proving the override is
#      genuinely dialed rather than silently replaced by the healthy gateway
#      default, which is the actual precedence proof.
#
# The test API (ratelimit-test-precedence) is registered once and left
# registered at the end (not deleted) - this script's cleanup only restores
# api-platform.env and gateway-runtime, never the API registration itself.
#
# Usage:
#   ./run-e2e.sh
#
# Env overrides:
#   CONTROLLER_BASE_URL     http://localhost:9090
#   CONTROLLER_ADMIN_URL    http://localhost:9094
#   GATEWAY_URL             http://localhost:8080
#   MAIN_REDIS_PORT         6379   (gateway/docker-compose.yaml's "redis" service, host-mapped)
#   OVERRIDE_REDIS_PORT     6380   (gateway/docker-compose.yaml's "redis-override-test" service, host-mapped)
#   NEWMAN_REPORTERS        cli,junit
#
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATEWAY_DIR="$(cd "$ROOT/../../.." && pwd)"
COLLECTION="$ROOT/postman/advanced-ratelimit-redis-precedence.postman_collection.json"
REPORT_DIR="$ROOT/postman/reports"
ENV_FILE="$GATEWAY_DIR/api-platform.env"

mkdir -p "$REPORT_DIR"

CONTROLLER_BASE_URL="${CONTROLLER_BASE_URL:-http://localhost:9090}"
CONTROLLER_ADMIN_URL="${CONTROLLER_ADMIN_URL:-http://localhost:9094}"
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
MAIN_REDIS_PORT="${MAIN_REDIS_PORT:-6379}"
OVERRIDE_REDIS_PORT="${OVERRIDE_REDIS_PORT:-6380}"
NEWMAN_REPORTERS="${NEWMAN_REPORTERS:-cli,junit}"

log() { printf '\n==> %s\n' "$1"; }
die() { printf '\nERROR: %s\n' "$1" >&2; exit 1; }

# ─── prerequisites ───────────────────────────────────────────────────────────

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v docker >/dev/null 2>&1 || die "docker (with the compose plugin) is required"
command -v redis-cli >/dev/null 2>&1 || die "redis-cli is required (used to verify which Redis instance actually received the rate-limit key - the Postman collection's HTTP-only assertions can't see this)"

NEWMAN=(newman)
if ! command -v newman >/dev/null 2>&1; then
  command -v npx >/dev/null 2>&1 || die "neither 'newman' nor 'npx' found on PATH - install newman: npm install -g newman"
  NEWMAN=(npx --yes newman)
fi

# ─── verify the gateway stack is already up ─────────────────────────────────

log "Checking gateway-controller at $CONTROLLER_BASE_URL ..."
curl -sf --max-time 5 -u admin:admin "$CONTROLLER_BASE_URL/api/management/v1/rest-apis" -o /dev/null \
  || die "gateway-controller not reachable/authenticated at $CONTROLLER_BASE_URL - start the stack first: docker compose up -d gateway-controller gateway-runtime sample-backend redis redis-override-test"

log "Checking gateway-runtime (Envoy) at $GATEWAY_URL ..."
curl -sf --max-time 5 "$GATEWAY_URL/" -o /dev/null
echo "Gateway stack looks up (a 404 for '/' here is fine - it just means no route matches root, which is expected)."

log "Checking redis-override-test is reachable at localhost:$OVERRIDE_REDIS_PORT (phase 2 needs it up; phase 3 needs it present but its 9999 override target absent) ..."
redis-cli -h localhost -p "$OVERRIDE_REDIS_PORT" ping >/dev/null 2>&1 \
  || die "redis-override-test not reachable at localhost:$OVERRIDE_REDIS_PORT - it's a TEST-ONLY service in gateway/docker-compose.yaml; bring it up with: docker compose up -d redis-override-test"

# ─── env-file block management (idempotent, self-cleaning) ─────────────────

BLOCK_BEGIN="# >>> advanced-ratelimit-e2e (managed by run-e2e.sh - do not edit by hand) >>>"
BLOCK_END="# <<< advanced-ratelimit-e2e <<<"

# strip_env_block removes any existing managed block from ENV_FILE, leaving
# everything else (including a human's own settings) untouched.
strip_env_block() {
  if [ -f "$ENV_FILE" ] && grep -qF "$BLOCK_BEGIN" "$ENV_FILE" 2>/dev/null; then
    # macOS/BSD sed needs an explicit (empty) backup suffix for -i; GNU sed
    # accepts the same form, so this works portably either way.
    sed -i '' "/$(printf '%s' "$BLOCK_BEGIN" | sed 's/[.[\*^$/]/\\&/g')/,/$(printf '%s' "$BLOCK_END" | sed 's/[.[\*^$/]/\\&/g')/d" "$ENV_FILE"
  fi
}

# set_env_block replaces the managed block wholesale with the given lines -
# always strips first, so re-running (or moving to a later phase) never
# accumulates stale/duplicate keys from an earlier phase.
set_env_block() {
  strip_env_block
  {
    echo "$BLOCK_BEGIN"
    for line in "$@"; do echo "$line"; done
    echo "$BLOCK_END"
  } >> "$ENV_FILE"
}

restart_gateway_runtime() {
  log "Restarting gateway-runtime to pick up the new env state ..."
  ( cd "$GATEWAY_DIR" && docker compose up -d gateway-runtime ) >/dev/null \
    || die "docker compose up -d gateway-runtime failed"
  # Route rebuild after a restart is async (xDS) - poll rather than a fixed
  # sleep. Polls /ready, NOT /health - /health carries the rate-limit policy
  # under test (2 requests/minute), and this poll loop must never consume
  # part of that quota before the real test requests run. /ready has no
  # policy attached and is unaffected even when /health's own policy chain
  # fails to build (Phase 3) - policy-chain build failures are scoped to
  # the specific route, not the whole RestApi.
  local tries=30 code
  while true; do
    code=$(curl -s --max-time 1 -o /dev/null -w '%{http_code}' "$GATEWAY_URL/ratelimit-test-precedence/v1.0/ready" 2>/dev/null || echo "000")
    [ "$code" = "200" ] && break
    tries=$((tries - 1))
    if [ "$tries" -le 0 ]; then
      die "gateway-runtime never became ready for ratelimit-test-precedence after restart (last status: $code)"
    fi
    sleep 0.5
  done
}

# Always restore a clean env state, whether this script succeeds, fails, or
# is interrupted - the docker-compose stack is shared, not test-owned.
cleanup() {
  local ec=$?
  log "Restoring api-platform.env and gateway-runtime to their pre-test state ..."
  strip_env_block
  ( cd "$GATEWAY_DIR" && docker compose up -d gateway-runtime ) >/dev/null 2>&1 || true
  exit $ec
}
trap cleanup EXIT INT TERM

# ─── register the test API (idempotent) ─────────────────────────────────────

log "Registering ratelimit-test-precedence (idempotent - deletes any pre-existing one first) ..."
curl -s -o /dev/null -u admin:admin -X DELETE "$CONTROLLER_BASE_URL/api/management/v1/rest-apis/ratelimit-test-precedence" || true
"${NEWMAN[@]}" run "$COLLECTION" --folder "01 - Register Test API" \
  --reporters "$NEWMAN_REPORTERS" --reporter-junit-export "$REPORT_DIR/01-register.xml" \
  || die "failed to register ratelimit-test-precedence"

# ─── redis-cli helpers ───────────────────────────────────────────────────────

redis_keys() { redis-cli -h localhost -p "$1" --scan --pattern 'ratelimit:v1:*' 2>/dev/null || true; }
flush_ratelimit_keys() {
  local port="$1" keys
  keys=$(redis_keys "$port")
  [ -n "$keys" ] && echo "$keys" | xargs -I{} redis-cli -h localhost -p "$port" DEL "{}" >/dev/null
  return 0
}

# ─── Phase 1: no override -> gateway-wide default Redis used ───────────────

log "Phase 1: no policy-level override configured ..."
flush_ratelimit_keys "$MAIN_REDIS_PORT"
flush_ratelimit_keys "$OVERRIDE_REDIS_PORT"
set_env_block 'APIP_GW_RATELIMIT_V1_BACKEND=redis'
restart_gateway_runtime

"${NEWMAN[@]}" run "$COLLECTION" --folder "02 - Phase 1: No override -> gateway default used" \
  --reporters "$NEWMAN_REPORTERS" --reporter-junit-export "$REPORT_DIR/02-phase1.xml" \
  || die "Phase 1 (no override) HTTP assertions failed"

main_keys=$(redis_keys "$MAIN_REDIS_PORT")
override_keys=$(redis_keys "$OVERRIDE_REDIS_PORT")
[ -n "$main_keys" ] || die "Phase 1: expected a ratelimit key in the gateway default redis (port $MAIN_REDIS_PORT), found none - the gateway-wide default client was not used"
[ -z "$override_keys" ] || die "Phase 1: expected NO ratelimit key in redis-override-test (port $OVERRIDE_REDIS_PORT), found: $override_keys - a policy-level override is being used even though none is configured"
log "Phase 1 confirmed: key present in the gateway default redis, absent from redis-override-test."

# ─── Phase 2: working policy-level override -> that Redis used instead ─────

log "Phase 2: policy-level override configured (redis-override-test, reachable) ..."
flush_ratelimit_keys "$MAIN_REDIS_PORT"
flush_ratelimit_keys "$OVERRIDE_REDIS_PORT"
set_env_block 'APIP_GW_RATELIMIT_V1_BACKEND=redis' 'APIP_GW_RATELIMIT_V1_REDIS_HOST=redis-override-test'
restart_gateway_runtime

"${NEWMAN[@]}" run "$COLLECTION" --folder "03 - Phase 2: Working override -> policy-level Redis used" \
  --reporters "$NEWMAN_REPORTERS" --reporter-junit-export "$REPORT_DIR/03-phase2.xml" \
  || die "Phase 2 (working override) HTTP assertions failed"

main_keys=$(redis_keys "$MAIN_REDIS_PORT")
override_keys=$(redis_keys "$OVERRIDE_REDIS_PORT")
[ -z "$main_keys" ] || die "Phase 2: expected NO new ratelimit key in the gateway default redis (port $MAIN_REDIS_PORT), found: $main_keys - the policy-level override is not being honored"
[ -n "$override_keys" ] || die "Phase 2: expected a ratelimit key in redis-override-test (port $OVERRIDE_REDIS_PORT), found none - the policy-level override was not actually used"
log "Phase 2 confirmed: key present in redis-override-test, absent from the gateway default redis."

# ─── Phase 3: unreachable override + failureMode closed -> proves precedence ─

log "Phase 3: policy-level override configured but unreachable (port 9999), failureMode: closed ..."
set_env_block 'APIP_GW_RATELIMIT_V1_BACKEND=redis' 'APIP_GW_RATELIMIT_V1_REDIS_HOST=redis-override-test' \
  'APIP_GW_RATELIMIT_V1_REDIS_PORT=9999' 'APIP_GW_RATELIMIT_V1_REDIS_FAILURE_MODE=closed'
restart_gateway_runtime

"${NEWMAN[@]}" run "$COLLECTION" --folder "04 - Phase 3: Unreachable override + failureMode closed -> proves precedence" \
  --reporters "$NEWMAN_REPORTERS" --reporter-junit-export "$REPORT_DIR/04-phase3.xml" \
  || die "Phase 3 (unreachable override) assertion failed - if this passed with a healthy-looking 200 instead of 500, the override is being silently ignored in favor of the gateway default, which is the precedence bug this phase exists to catch"

log "Phase 3 confirmed: route failed to build against the unreachable override - the gateway default was never silently substituted."

log "All advanced-ratelimit Redis-precedence flows passed."

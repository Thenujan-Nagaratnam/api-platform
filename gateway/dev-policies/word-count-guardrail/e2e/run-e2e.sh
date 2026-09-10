#!/usr/bin/env bash
#
# One-command runner for word-count-guardrail's vendor-shaped-rejection e2e
# suite (TESTING.md / postman/word-count-guardrail.postman_collection.json)
# against a gateway stack that is ALREADY running (see TESTING.md Part B —
# this script does not build or start gateway-controller/gateway-runtime
# itself).
#
# What it does:
#   1. Sanity-checks the gateway stack is reachable.
#   2. Registers the 7 vendor-templated test LlmProviders and waits briefly
#      for gateway-runtime to pick up the new routes via xDS (registering
#      returns 200/201 as soon as gateway-controller persists it, but the
#      route only becomes reachable once an async xDS push lands).
#   3. Generates a client API key for each provider.
#   4. Sends a below-minimum-word-count request to each provider and asserts
#      the rejection is rendered in that vendor's own convention, not the
#      default WSO2 shape.
#   5. Deletes all 7 test providers and reports a single pass/fail exit code.
#
# No mock LLM backend is used or needed: every assertion here is a
# request-phase guardrail rejection, which fires before the request would
# ever reach an upstream — upstream.url in the registered providers is a
# placeholder, never dialed.
#
# Usage:
#   ./run-e2e.sh
#
# Env overrides (defaults match TESTING.md / the Postman collection):
#   CONTROLLER_BASE_URL     localhost:9090
#   GATEWAY_URL             localhost:8080
#   NEWMAN_REPORTERS        cli,junit
#   REQUEST_DELAY_MS        800   (per-request delay applied across the whole
#                                  register+keys+trigger run, as a cheap way to
#                                  give gateway-runtime's async xDS push time
#                                  to land before the trigger folder's requests
#                                  reach Envoy — bump if you see a trigger
#                                  request 404/401 instead of hitting the
#                                  guardrail; 800ms was needed even for just 7
#                                  resources when testing this locally)

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COLLECTION="$SCRIPT_DIR/postman/word-count-guardrail.postman_collection.json"
REPORT_DIR="$SCRIPT_DIR/postman/reports"

CONTROLLER_BASE_URL="${CONTROLLER_BASE_URL:-localhost:9090}"
GATEWAY_URL="${GATEWAY_URL:-localhost:8080}"
NEWMAN_REPORTERS="${NEWMAN_REPORTERS:-cli,junit}"
REQUEST_DELAY_MS="${REQUEST_DELAY_MS:-800}"

NEWMAN=(newman)
if ! command -v newman >/dev/null 2>&1; then
  echo "newman not found on PATH — falling back to 'npx --yes newman' (slower, downloads on first run)" >&2
  NEWMAN=(npx --yes newman)
fi

mkdir -p "$REPORT_DIR"

log() { echo "[run-e2e] $*"; }

fail() {
  log "FAILED: $*"
  exit 1
}

log "Checking gateway-controller is reachable at http://${CONTROLLER_BASE_URL} ..."
if ! curl -s -o /dev/null -w '' --max-time 5 "http://${CONTROLLER_BASE_URL}/api/management/v1/rest-apis"; then
  fail "gateway-controller not reachable at http://${CONTROLLER_BASE_URL} — bring up the stack per TESTING.md Part B first"
fi

# Register -> generate keys -> trigger MUST run as a single newman process:
# pm.collectionVariables.set() (used to capture each provider's API key in
# folder 02, consumed in folder 03) only persists in-memory for the lifetime
# of one `newman run` invocation — a separate process for each folder would
# silently lose the captured keys. Folder order here is safe regardless of
# how these three are listed on the command line: newman runs `--folder`
# selections in the COLLECTION's own top-level order (00→04), not
# command-line flag order — confirmed the hard way in oauth2-generator's
# run-e2e.sh (see its comment near the E.21 invocation) — and this
# collection's folders are already declared in the order they need to run.
log "Registering providers, generating keys, and triggering the guardrail (single newman process, in-order) ..."
TEST_EXIT=0
"${NEWMAN[@]}" run "$COLLECTION" \
  --folder "01 - Register Providers" \
  --folder "02 - Generate API Keys" \
  --folder "03 - Trigger Guardrail & Verify Vendor Shape" \
  --env-var "controllerBaseUrl=${CONTROLLER_BASE_URL}" \
  --env-var "gatewayBaseUrl=${GATEWAY_URL}" \
  --delay-request "${REQUEST_DELAY_MS}" \
  --reporters "$NEWMAN_REPORTERS" \
  --reporter-junit-export "$REPORT_DIR/junit.xml" \
  --color on || TEST_EXIT=1

log "Cleaning up test providers ..."
"${NEWMAN[@]}" run "$COLLECTION" \
  --folder "04 - Cleanup" \
  --env-var "controllerBaseUrl=${CONTROLLER_BASE_URL}" \
  --env-var "gatewayBaseUrl=${GATEWAY_URL}" \
  --reporters cli --color on || log "cleanup had failures (non-fatal — providers may already be gone)"

if [ "$TEST_EXIT" -eq 0 ]; then
  log "PASS — all vendor-shaped rejection assertions passed."
else
  log "FAIL — see output above and $REPORT_DIR/junit.xml for details."
fi
exit "$TEST_EXIT"

#!/usr/bin/env bash
# --------------------------------------------------------------------
# Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
#
# WSO2 LLC. licenses this file to you under the Apache License,
# Version 2.0 (the "License"); you may not use this file except
# in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.
# --------------------------------------------------------------------
#
# Runs postman/mcp-auth-manual-test.postman_collection.json end-to-end:
# brings up the minimal IT docker-compose subset it needs, waits for
# readiness, runs it via newman, and tears the stack back down.
#
# Usage:
#   ./run-mcp-auth-e2e.sh [--build] [--keep-stack] [--project-name NAME]
#
#   --build         Run `make build-coverage` first, so the stack reflects
#                    current gateway-controller/gateway-runtime/policy source
#                    (slow, ~5-15 min). Off by default — omit for a quick
#                    smoke test against whatever *-coverage:test images
#                    already exist locally.
#   --keep-stack    Leave the docker compose stack running afterward
#                    instead of tearing it down. Useful for follow-up manual
#                    curl/Postman poking.
#   --project-name  docker compose -p project name (default: gateway-it).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
GATEWAY_DIR="$(cd "$IT_DIR/.." && pwd)"
COMPOSE_FILE="docker-compose.test.yaml"
COLLECTION="$SCRIPT_DIR/mcp-auth-manual-test.postman_collection.json"
SERVICES=(gateway-controller gateway-runtime mcp-server-backend mock-jwks mock-platform-api)
# Resource ids the collection creates — pre-cleaned so re-runs are idempotent
# even when the controller's SQLite volume survives across container recreation.
TEST_PROXY_IDS=(mcp-auth-postman-test mcp-addprops-denied mcp-addprops-allowed)

BUILD=false
KEEP_STACK=false
PROJECT_NAME="gateway-it"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --build) BUILD=true; shift ;;
    --keep-stack) KEEP_STACK=true; shift ;;
    --project-name) PROJECT_NAME="$2"; shift 2 ;;
    -h|--help)
      sed -n '2,26p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) echo "Unknown argument: $1" >&2; exit 1 ;;
  esac
done

log() { printf '\n[run-mcp-auth-e2e] %s\n' "$1"; }

cd "$IT_DIR"

if $BUILD; then
  log "Building gateway-controller/gateway-runtime coverage images (make build-coverage)..."
  make -C "$GATEWAY_DIR" build-coverage
  log "Retagging coverage images -> :test (make ensure-test-tags)..."
  make ensure-test-tags
else
  log "Skipping build (--build not passed). Verifying *-coverage:test images exist..."
  for img in gateway-controller-coverage gateway-runtime-coverage; do
    if ! docker image inspect "ghcr.io/wso2/api-platform/${img}:test" >/dev/null 2>&1; then
      echo "ERROR: ghcr.io/wso2/api-platform/${img}:test not found locally." >&2
      echo "       Run with --build, or run 'make -C $GATEWAY_DIR build-coverage && make ensure-test-tags' first." >&2
      exit 1
    fi
  done
fi

log "Bringing up: ${SERVICES[*]} (project: ${PROJECT_NAME})..."
if ! docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" up -d --force-recreate "${SERVICES[@]}"; then
  echo "" >&2
  echo "ERROR: 'docker compose up' failed — commonly a port conflict (9090/8080/8082/3001)" >&2
  echo "       with another gateway stack. Check what's bound with, e.g.:" >&2
  echo "         lsof -nP -iTCP:9090 -sTCP:LISTEN" >&2
  echo "         docker ps --format '{{.Names}}\\t{{.Ports}}'" >&2
  echo "       and stop the conflicting stack, then re-run this script." >&2
  exit 1
fi

cleanup() {
  if $KEEP_STACK; then
    log "Leaving stack running (--keep-stack). Tear down later with:"
    echo "  docker compose -f $IT_DIR/$COMPOSE_FILE -p $PROJECT_NAME down"
  else
    log "Tearing down stack (including volumes, so the next run starts from a clean DB)..."
    docker compose -f "$COMPOSE_FILE" -p "$PROJECT_NAME" down -v --remove-orphans
  fi
}
trap cleanup EXIT

log "Waiting for gateway-controller REST API to be ready..."
for i in $(seq 1 30); do
  if curl -sf -o /dev/null -u admin:admin "http://localhost:9090/api/management/v1/mcp-proxies"; then
    log "Controller is ready."
    break
  fi
  if [[ "$i" -eq 30 ]]; then
    echo "ERROR: gateway-controller REST API did not become ready in time." >&2
    docker logs it-gateway-controller 2>&1 | tail -50
    exit 1
  fi
  sleep 2
done

log "Waiting for gateway-runtime (Envoy) to report healthy..."
for i in $(seq 1 30); do
  status=$(docker inspect it-gateway-runtime --format '{{.State.Health.Status}}' 2>/dev/null || echo "unknown")
  if [[ "$status" == "healthy" ]]; then
    log "Runtime is healthy."
    break
  fi
  if [[ "$i" -eq 30 ]]; then
    echo "ERROR: gateway-runtime did not report healthy in time (last status: $status)." >&2
    docker logs it-gateway-runtime 2>&1 | tail -50
    exit 1
  fi
  sleep 2
done

if ! command -v newman >/dev/null 2>&1; then
  echo "ERROR: newman is not installed. Install it with: npm install -g newman" >&2
  exit 1
fi

log "Pre-cleaning any leftover test proxies from a prior run (idempotency; a 404 here is expected/fine)..."
for id in "${TEST_PROXY_IDS[@]}"; do
  curl -s -o /dev/null -u admin:admin -X DELETE "http://localhost:9090/api/management/v1/mcp-proxies/${id}" || true
done

log "Running Postman collection via newman..."
newman run "$COLLECTION" --delay-request 500

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
# Runs the model-failover Postman e2e suite with newman against a running
# gateway IT stack. Start the stack first (see README.md).
#
#   ./run-e2e.sh                         # whole suite
#   ./run-e2e.sh --folder "05 Cross-provider failover" --folder "99 Cleanup"
#
# Any extra arguments are passed to newman. Reports go to ./reports/.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HOST="${E2E_HOST:-localhost}"
CTRL_PORT="${E2E_CTRL_PORT:-9090}"
ROUTER_PORT="${E2E_ROUTER_PORT:-8080}"

if command -v newman >/dev/null 2>&1; then
  NEWMAN=(newman)
else
  NEWMAN=(npx --yes newman@6)
fi

check() {
  local what=$1 url=$2
  if ! curl -s -o /dev/null --max-time 3 "$url"; then
    echo "error: $what is not reachable at $url" >&2
    echo "Start the IT stack first: cd gateway/it && docker compose -f docker-compose.test.yaml up -d --build" >&2
    exit 1
  fi
}
check "gateway router" "http://${HOST}:${ROUTER_PORT}/"
check "gateway-controller" "http://${HOST}:${CTRL_PORT}/"
for port in "${E2E_MOCK_A_PORT:-8091}" "${E2E_MOCK_B_PORT:-8092}" "${E2E_MOCK_ANTHROPIC_PORT:-8093}"; do
  check "mock LLM on :$port" "http://${HOST}:${port}/health"
done

mkdir -p "$HERE/reports"
"${NEWMAN[@]}" run "$HERE/model-failover.postman_collection.json" \
  --environment "$HERE/model-failover.postman_environment.json" \
  --env-var "host=${HOST}" \
  --env-var "ctrl_port=${CTRL_PORT}" \
  --env-var "router_port=${ROUTER_PORT}" \
  --env-var "mock_a_port=${E2E_MOCK_A_PORT:-8091}" \
  --env-var "mock_b_port=${E2E_MOCK_B_PORT:-8092}" \
  --env-var "mock_anthropic_port=${E2E_MOCK_ANTHROPIC_PORT:-8093}" \
  --timeout-request 30000 \
  --reporters cli,junit \
  --reporter-junit-export "$HERE/reports/model-failover-junit.xml" \
  "$@"

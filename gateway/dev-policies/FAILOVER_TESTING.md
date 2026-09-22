# LLM Model Failover — Live E2E Verification

Live-verifies the full failover mechanism (config schema, xDS/Envoy wiring,
per-attempt backend+identity resolution, downstream routing + suspension,
request/response transformation, per-attempt auth) against a real deployed
stack, following the same pattern `dev-policies/oauth2/TESTING.md` already
established for the oauth2 policy.

## What's here

Per-attempt upstream policy participation no longer needs a forked policy or
an unreleased SDK field at all: a policy the controller marks for the
upstream-attempt phase runs per upstream attempt using its own, unmodified
`RequestPolicy`/`ResponsePolicy`/`RequestHeaderPolicy`/`ResponseHeaderPolicy`
implementation (see `docs/superpowers/specs/2026-09-21-upstream-policy-reuse-design.md`).
There is no author-facing `upstreamPolicies:` YAML field — the only way a
policy instance reaches this phase today is via `model-failover`'s own
controller-side synthesis (attaching itself plus the credential/transformer
of every provider its chain references).
`build.yaml` now references the released `gomodule:` versions of every
policy — the local `dev-policies/openai-to-anthropic-transformer/`,
`dev-policies/oauth2-generator/`, `dev-policies/llm-upstream-provider-auth/`
copies (and their `filePath:` overrides) that this doc used to describe have
been removed.

- `dev-policies/model-failover/` — the new model-failover policy (see
  `docs/superpowers/specs/2026-09-21-model-failover-policy-design.md`), local-only until it has a real
  release in `github.com/wso2/gateway-controllers`. `build.yaml` has a temporary `filePath:` override
  for it — remove once a `gomodule:` version exists.

`executionCondition` is now evaluated in the upstream-attempt phase (see
`internal/executor/chain.go`'s `ExecuteUpstreamAttempt*` functions), so the
released `oauth2-generator`/`openai-to-anthropic-transformer` self-gate
correctly on which provider an attempt resolved to — attaching one instance
per provider under `upstreamPolicies:` for a multi-provider failover chain no
longer fires every instance on every attempt. Folders 2 and 7 below
(cross-provider credential + translation) exercise exactly that case and are
expected to pass against the released modules.

- `dev-policies/mocks/mock-openai/`, `dev-policies/mocks/mock-anthropic/` —
  standalone, in-memory mock backends. Each exposes its real chat-completion
  endpoint plus a small control API:
  - `POST /control/arm-failure` `{"count": N, "status": 500}` — the next N
    calls to the completion endpoint return that status instead of 200.
  - `POST /control/reset` — clears armed failures and call history.
  - `GET /control/history` — `{"callCount": N, "history": [...]}`.
  - Every real response also carries `X-Mock-Backend` (`openai`/`anthropic`),
    `X-Mock-Received-Auth`, and `X-Mock-Received-Model` headers, so a live
    request can assert exactly which backend served it, with what model, and
    what credential arrived — without needing to poll `/control/history`.
- `postman/llm-failover-e2e.postman_collection.json` — the full e2e suite.

## 1. Start the mocks (host machine, not containers)

```bash
cd gateway/dev-policies/mocks/mock-openai && GOWORK=off go run . &
cd gateway/dev-policies/mocks/mock-anthropic && GOWORK=off go run . &
curl -s localhost:9611/healthz && echo
curl -s localhost:9612/healthz && echo
```

`host.docker.internal` resolves back to these from inside the gateway
containers automatically (Docker Desktop/Colima/Rancher) — no extra config.

## 2. Build (run yourself — not run automatically)

```bash
cd gateway
make build
```

Builds from `build.yaml`'s `gomodule:` entries — no local policy overrides.

## 3. Bring up the stack (run yourself)

```bash
cd gateway
docker compose --profile redis up -d gateway-controller gateway-runtime sample-backend redis
```

- gateway-controller management API: `http://localhost:9090/api/management/v1`
  (admin:admin — `Authorization: Basic YWRtaW46YWRtaW4=`), health on `:9094`.
- gateway-runtime (Envoy, data plane): `https://localhost:8443` — self-signed
  cert, so `curl -k` / disable SSL verification in Postman.

## 4. Run the Postman collection

Import `postman/llm-failover-e2e.postman_collection.json` into Postman (or
run headless: `newman run postman/llm-failover-e2e.postman_collection.json
--insecure`). Its **Setup** folder registers both `LlmProvider`s and the one
`LlmProxy` (idempotent — accepts 201 or 409 on repeat runs), then each
numbered folder exercises one flow. Run the whole collection top to bottom;
folder 4 (suspension expiry) has a ~6s in-script busy-wait to clear the
5s `suspendDuration` configured on the proxy.

## What each folder proves

| Folder | Proves |
|---|---|
| 1. Happy Path | Primary succeeds, no failover engaged, plain pass-through |
| 2. Cross-Provider Failover | Primary 5xx → Envoy retries to Anthropic → request body translated to Anthropic shape → correct per-attempt credential (`X-Api-Key`, not the primary's `Authorization`) → response translated back to OpenAI shape |
| 3. Suspension | A second request within `suspendDuration` skips the suspended primary entirely (bypasses the aggregate) — proven by primary's call count not increasing |
| 4. Suspension Expiry | After the window passes, the primary is tried again |
| 5. Both Fail | Client sees the *last* attempt's error response (Envoy's own exhaustion behavior) |
| 6. No Model Match | A model absent from `model-failover`'s `targets` param never touches the fallback provider at all |
| 7. Streaming | A failover-driven attempt with `"stream": true` gets a real Anthropic SSE response, translated to OpenAI `chat.completion.chunk` events |

Note: the superseded `resilience.failover` schema supported a configurable
`retryOn` (e.g. `retriable-4xx`) that this policy does not — `model-failover`
hardcodes escalation to Envoy's `5xx` retry-on condition only (see the design
doc's discussion of this tradeoff), so there is no "custom retryOn" folder in
this collection.

## Cleanup

```bash
docker compose down
```

Kill the two `go run` mock processes.

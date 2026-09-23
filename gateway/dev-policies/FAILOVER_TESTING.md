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
There is no author-facing `upstreamPolicies:` YAML *list* field — but any
`operationPolicies:`/`globalPolicies:` entry can be marked `upstream: true`
directly to run in this phase (the same generic dispatch model-failover's own
controller-side synthesis uses). `model-failover` is still the only policy
whose upstream attachment is auto-synthesized (it needs controller-computed
values an author can't hand-write); an ordinary policy marked `upstream: true`
by its own author needs no synthesis at all.
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
  - `POST /control/arm-delay` `{"count": N, "delayMs": 500}` (mock-openai
    only) — the next N calls wait that long before responding, to simulate a
    slow-but-responding upstream for the latency rule. Combines with
    `arm-failure`.
  - `POST /control/reset` — clears armed failures, delays and call history.
  - `GET /control/history` — `{"callCount": N, "history": [...]}`.
  - Every real response also carries `X-Mock-Backend` (`openai`/`anthropic`),
    `X-Mock-Received-Auth`, and `X-Mock-Received-Model` headers, so a live
    request can assert exactly which backend served it, with what model, and
    what credential arrived — without needing to poll `/control/history`.
    Each checks its own provider's conventional credential header first
    (mock-openai: `Authorization`, mock-anthropic: `X-Api-Key`) but falls back
    to the other — a request can physically land on either mock while
    carrying the other provider's credential convention (see folder 9,
    `upstreamDefinition`), and `X-Mock-Received-Auth` must still show it.
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
--insecure`). Its **Setup** folder registers every `LlmProvider` and
`LlmProxy` this collection needs (idempotent — accepts 201 or 409 on repeat
runs), then each numbered folder exercises one flow. Run the whole collection
top to bottom; folder 4 (suspension expiry) has a ~6s in-script busy-wait to
clear the 5s `suspendDuration` configured on `failover-default`, and 6.1
(11s) and 8.5 (6s) wait out the suspensions folders 5 and 7 leave on that
same proxy. Those waits matter: once every member of a chain is suspended,
the policy now answers `503 failover_exhausted` without dialing anything, so
stale suspension from an earlier folder fails a later one outright. 6.1 is
11s rather than 6s because the backoff streak stays hot for window + base
(10s here) — a failure inside it doubles the next window.

Folders 12–15 each get their own proxy (`failover-circuit-rate`,
`failover-circuit-latency`, `failover-circuit-halfopen`,
`failover-circuit-chain3`), and
`failover-circuit-probes` backs the concurrency check `run-failover-e2e.sh`
runs with parallel curls after newman — Postman runs requests one at a time,
so it can't prove probe concurrency itself.

Setup registers three proxies, each isolating one concern so folders never
share suspension state or interfere with each other's assertions:
`failover-default` (the default config, `suspendDuration: 5`, no
`statusCodes`), `failover-statuscodes` (`statusCodes: [429]`,
`suspendDuration: 0`), and `failover-upstreamdef` (a fallback whose
`upstreamDefinition` points at a *different* named upstream than its
`provider` — see folder 9). The last of these also needs a second
`LlmProvider`, `anthropic-mirror-provider`, whose `upstream.url` deliberately
points at mock-**openai**'s own address (`:9611`) under a distinct identity —
it exists purely to give `upstreamDefinition` a physically distinguishable
dial target that's provably *not* where `provider: anthropic-upstream` alone
would have routed; its own auth/transformer are never attached or used.

`run-failover-e2e.sh` starts the mocks, runs newman, then runs the probe
concurrency check; it exits non-zero if either fails.

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
| 8. Configurable statusCodes | A `statusCodes: [429]`-configured proxy fails over on 429; the default (`5xx`-only) proxy does not — same primary response, different configured trigger set |
| 9. upstreamDefinition Overrides Dial Target | A fallback with `provider: anthropic-upstream, upstreamDefinition: anthropic-mirror` dials mock-**openai**'s server (proven via `X-Mock-Backend: openai`) while still carrying `anthropic-upstream`'s own credential (`X-Mock-Received-Auth` matches `anthropicApiKey`, not the primary's) — and mock-anthropic's real cluster is never touched (`callCount: 0`) |
| 12. Circuit - Rolling Failure Rate | With `suspendAfterFailures: 100` out of the way, the primary keeps being dialed through 19 failures (the rate rule waits for 20 attempts in its 60s window); the 20th takes the rate to 100%, over `failureRateThresholdPercent: 50`, and the next request skips the primary |
| 13. Circuit - Latency Percentile | Twenty successful-but-400ms responses put the window's p95 over `latencyThresholdMs: 300`; the next request skips a primary that never returned an error |
| 14. Circuit - Half-Open Reopen And Close | After the 3s window, one probe reaches the primary; it fails, the circuit reopens with a doubled 6s window and the next request is skipped; after that window one successful probe closes it |
| 15. Circuit - Three-Member Chain And Exhaustion | Chain `gpt-4o` → Anthropic → `gpt-4o-mini`. With the primary suspended, the request enters at the Anthropic suffix; when Anthropic fails, Envoy still retries on to `gpt-4o-mini` (body rewritten to that model) and the suspended primary is not dialed. With only `gpt-4o-mini` left, its 500 reaches the client. With every member suspended, the policy answers `503 failover_exhausted` locally and neither mock is dialed |
| Probe concurrency (script) | While half-open, 3 concurrent requests send exactly 1 to the slow, recovering primary and 2 to the fallback; once that probe closes the circuit, 3 concurrent requests all reach the primary |

## Cleanup

```bash
docker compose down
```

Kill the two `go run` mock processes.

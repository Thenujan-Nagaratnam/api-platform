# LLM Model Failover — Live E2E Verification

Live-verifies the failover mechanism against a real deployed stack: config
validation, xDS/Envoy wiring, per-attempt provider resolution, downstream
routing and suspension, request/response transformation and per-attempt
auth. The design is in `gateway/spec/prds/llm-model-failover.md`.

## What's here

- `dev-policies/model-failover/` — the model-failover policy. It is attached
  once, in an LlmProxy's `operationPolicies`; the controller adds the
  per-attempt copy to each upstream itself. It is local-only until it has a
  release in `github.com/wso2/gateway-controllers`, so `build.yaml` has a
  temporary `filePath:` override for it — replace it with a `gomodule:` line
  once that release exists.

The controller compiles each provider's `transformer` and `auth` into that
provider's own per-attempt `policies` list (after any policies the author
lists in `spec.provider.policies` / `spec.additionalProviders[].policies`),
gated on the provider the attempt resolved to. Folders 2 and 7 below (cross-provider credential and
translation) exercise exactly that case.

- `dev-policies/mocks/mock-openai/`, `dev-policies/mocks/mock-anthropic/` —
  standalone, in-memory mock backends. Each exposes its real chat-completion
  endpoint plus a small control API:
  - `POST /control/arm-failure` `{"count": N, "status": 500}` — the next N
    calls to the completion endpoint return that status instead of 200.
  - `POST /control/arm-delay` `{"count": N, "delayMs": 500}` (mock-openai
    only) — the next N calls wait that long before responding, to simulate a
    slow-but-responding upstream (e.g. for `perTryTimeout`). Combines with
    `arm-failure`.
  - `POST /control/reset` — clears armed failures, delays and call history.
  - `GET /control/history` — `{"callCount": N, "history": [...]}`.
  - Every real response also carries `X-Mock-Backend` (`openai`/`anthropic`),
    `X-Mock-Received-Auth`, and `X-Mock-Received-Model` headers, so a live
    request can assert exactly which backend served it, with what model, and
    what credential arrived — without needing to poll `/control/history`.
    Each checks its own provider's conventional credential header first
    (mock-openai: `Authorization`, mock-anthropic: `X-Api-Key`) but falls back
    to the other, so `X-Mock-Received-Auth` shows whichever credential
    arrived.
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

Folders 14–15 each get their own proxy (`failover-circuit-halfopen`,
`failover-circuit-chain3`), and
`failover-circuit-probes` backs the concurrency check `run-failover-e2e.sh`
runs with parallel curls after newman — Postman runs requests one at a time,
so it can't prove probe concurrency itself. The half-open and probes proxies
set `recoverAfterSuccesses: 1`, so one successful probe closes the circuit;
the script's "closed" check would fail on the default of 3, which makes it a
live check of that param.

Setup upserts: a proxy that already exists from an earlier run (409) is
updated with `PUT` to the collection's current config, and any other 400 fails
setup instead of passing silently.

Setup registers three proxies, each isolating one concern so folders never
share suspension state or interfere with each other's assertions:
`failover-default` (the default config, `suspendDuration: 5`, no
`statusCodes`), `failover-statuscodes` (`statusCodes: [429]`,
`suspendDuration: 0`), and `failover-samemodel` (a same-provider fallback
that only changes the model — see folder 10).

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
| 10. Same-Provider Fallback (Model Only) | A fallback on the primary's own provider retries the same cluster with the primary's credential, with the body rewritten to the cheaper model |
| 14. Circuit - Half-Open Reopen And Close | After the 3s window, one probe reaches the primary; it fails, the circuit reopens with a doubled 6s window and the next request is skipped; after that window one successful probe closes it |
| 15. Circuit - Three-Member Chain And Exhaustion | Chain `gpt-4o` → Anthropic → `gpt-4o-mini`. With the primary suspended, the request enters at the Anthropic suffix; when Anthropic fails, Envoy still retries on to `gpt-4o-mini` (body rewritten to that model) and the suspended primary is not dialed. With only `gpt-4o-mini` left, its 500 reaches the client. With every member suspended, the policy answers `503 failover_exhausted` locally and neither mock is dialed |
| 16. Deploy-Time Validation | The controller rejects, with a 400 naming the field, an unknown top-level param (`perTryTimeoutMs`), a stale `upstreamDefinition` on a target, and model-failover under `provider.policies`; a proxy using every param, including `recoverAfterSuccesses` and `probeTimeout`, deploys |
| Probe concurrency (script) | While half-open, 3 concurrent requests send exactly 1 to the slow, recovering primary and 2 to the fallback; once that probe closes the circuit, 3 concurrent requests all reach the primary |

## Cleanup

```bash
docker compose down
```

Kill the two `go run` mock processes.

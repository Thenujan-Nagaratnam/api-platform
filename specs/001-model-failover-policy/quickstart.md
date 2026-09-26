# Quickstart: Validate Model Failover End to End

This guide proves the feature works. The data shapes are in [data-model.md](./data-model.md) and the internal wiring is in [contracts/internal-hop-headers.md](./contracts/internal-hop-headers.md).

## Prerequisites

- A gateway stack built from this branch:
  - Build `gateway-runtime` before `gateway-controller`. The controller image copies the runtime's pre-compiled policy output.
  - `gateway/build.yaml` lists `model-failover` (using `filePath: ./dev-policies/model-failover` until it is tagged in `gateway-controllers`).
  - Envoy bootstrap includes the `envoy.bootstrap.internal_listener` extension.
- Mock provider backends from the IT harness (`gateway/it`). Each can be scripted per request:
  - `mock-openai-a` (primary) and `mock-openai-b` (same format, a second "region")
  - `mock-anthropic` (Anthropic Messages format, streaming and non-streaming)
  - A mode switch per mock: `ok`, `status:<code>`, `hang:<seconds>`, `reset`, `refuse` (port closed)
- LlmProviders `openai-a`, `openai-b` and `anthropic` deployed. LlmProxy `mf-proxy` with provider `openai-a` and `additionalProviders: [openai-b, anthropic]`.

## Configure

Attach the policy to `mf-proxy`:

```yaml
policies:
  - name: model-failover
    version: v0
    params:
      targets:
        - { provider: openai-a,  model: gpt-4o }
        - { provider: openai-b,  model: gpt-4o }
        - { provider: anthropic, model: claude-sonnet-4-5 }
      perAttemptTimeout: 2s
      suspendAfterConsecutiveFailures: 2
      suspendDuration: 5s
      recoverAfterSuccessfulProbes: 2
```

**Expected**: the deployment is accepted.

**Negative checks** (each must be rejected with a clear validation error):
- a duplicate `(openai-a, gpt-4o)` target
- a target provider that is not on the proxy
- `perAttemptTimeout: 0s`
- a status code of `404`
- `model-failover` together with `llm-header-router` on the same route

## Scenarios

| # | Setup | Action | Expected |
|---|---|---|---|
| 1 | A=`ok` | POST `/mf-proxy/chat/completions` | 200 from A. B and Anthropic mocks receive 0 requests. |
| 2 | A=`status:429`, B=`ok` | POST | 200 from B. Client sees no 429 and no `x-wso2-failover-*` headers. |
| 3 | A=`status:400` | POST | 400 from A, with no failover (non-eligible). |
| 4 | A=`status:503`, B=`status:503`, Anthropic=`ok` | POST non-streaming | 200 in OpenAI ChatCompletion shape, converted from Anthropic. Anthropic mock got an Anthropic Messages body with its own credentials (no OpenAI key). |
| 5 | Same as 4 | POST `"stream": true` | OpenAI-format SSE (`data: {chunk}` … `data: [DONE]`) converted from the Anthropic stream. |
| 6 | A=`hang:10` | POST | Served by B in about 2s (per-attempt timeout) plus B's latency, under 2.1s + B. |
| 7 | A=`refuse` / A=`reset` | POST | Served by B. Log reason is `connect_failure` / `reset`. |
| 8 | All three failing (`status:503`) | POST | 503 with the exact [exhaustion body](./contracts/exhaustion-response.md). Each mock received exactly 1 request. |
| 9 | A=`status:503` | 2 requests, then a 3rd | A is suspended after the 2nd request. The 3rd request never reaches A (A's count is unchanged) and is served by B. |
| 10 | After 9, wait 5s, A=`ok` | Send 4 sequential requests | The first 2 reach A as probes. Then A is `healthy` (metric gauge = 0) and serves normal traffic. |
| 11 | After 9, wait 5s, A=`status:503` | 1 request | The probe fails and A is suspended again (transition metric `to=suspended`). |
| 12 | A=`ok` streaming, then abort mid-stream (mock closes after 2 chunks) | POST `"stream": true` | Client stream ends with an error. B receives **0** requests (no failover after headers). |
| 13 | All targets suspended | POST | Immediate 503 exhaustion. All mocks receive 0 requests. |
| 14 | Client sends `x-wso2-failover-plan: <random>` | POST | Header is stripped at the front. The request behaves as in scenario 1. |
| 15 | A=`status:503` and `failoverOn.statusCodes: [429]` | POST | Client receives **503** (original status restored, no failover). |

## Verify observability

- `curl <policy-engine-metrics>/metrics | grep wso2_model_failover_` shows `attempts_total`, `failovers_total{reason="status_429"}`, `served_total{position="1"}`, `exhausted_total`, and `target_state` values consistent with the scenarios above.
- Gateway logs contain `model_failover.attempt_failed`, `model_failover.target_suspended`, `model_failover.target_recovered` and `model_failover.exhausted`, with no API keys and no bodies.

## Automated runs

- Unit: `cd gateway-controllers/policies/model-failover && go test ./...` and `cd gateway/gateway-controller && go test ./pkg/xds/... ./pkg/utils/...`
- Integration: `cd gateway/it && IT_FEATURE_PATHS=features/model-failover.feature make test` (new feature file covering scenarios 1–15).

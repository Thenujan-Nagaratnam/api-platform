# openai-error-format — Postman / newman tests

End-to-end tests for the `openai-error-format` fault policy, run against a live gateway. They cover the
same cases as `gateway/it/features/openai-error-format-policy.feature`.

## What it covers

| Folder | Cases |
|---|---|
| 01–07 | Every built-in template (`openai`, `azure-openai`, `azureai-foundry`, `anthropic`, `gemini`, `mistralai`, `awsbedrock`): a guardrail rejection becomes the envelope, and an allowed request is untouched |
| 08 | Status → `error.type` for 400, 401, 403, 404, 405, 409, 413, 418, 422, 429, 451, 499, 500, 501, 502, 503, 504 and 599; POST rejections; 200, 204 and 399 untouched; proxied successes untouched |
| 09 | Message recovery from every body shape: `error.message`, `message`, a guardrail's `message.actionReason`, `error_description`, `detail`, `error`, priority order, non-string and blank values, trimming, unicode, escaping, unknown JSON, arrays, invalid JSON, plain text (with and without a charset), multi-line text, HTML, text over 1024 characters, an empty body |
| 10 | Headers and encoding: `WWW-Authenticate`, `Retry-After` and custom headers kept; `text/plain` becomes JSON; `Content-Encoding: gzip` dropped; `identity` read. An existing OpenAI envelope is left byte-for-byte; near-envelopes are reshaped |
| 11 | `fault.maxInspectBytes`: over the limit, within it, `0`, and an existing envelope kept in both cases |
| 12 | Control: a provider without the policy keeps every body |
| 13 | Real shipped policies: basic-auth (missing, wrong and right credentials), api-key-auth, basic-ratelimit (200 then 429 with `Retry-After`), regex-, json-schema- and content-length-guardrail, and a **response-phase** word-count rejection |
| 14 | Router failures: connection refused (`101503`) and an unresolvable host (`303001`) |
| 15 | Backend errors pass through: OpenAI-shaped, legacy JSON, plain text, empty, and a 429 with `Retry-After` |
| 16 | LLM proxies: opt in at the proxy; inherit the provider's envelope; a provider's router failure reaches the proxy as a backend error |
| 17 | Other API kinds (`RestApi`, `Mcp`) are untouched |
| 18 | Operation-level attachment, API + operation attachment (formatted once), and the policy listed under `globalPolicies` (has no effect) |
| 19 | A request matching no API is untouched |
| 20 | Removing and re-adding the policy at runtime |
| 21 | Parameter validation at deployment: negative, string, fractional, unknown keys, non-object `fault` → 400; a valid entry → 201 |
| 22 | *(optional)* An upstream timeout (`101504`). Needs `httpbinUrl` |

Every envelope response is also checked for:
- exactly one top-level key, `error`;
- only OpenAI fields inside `error`;
- `param` present and `null`;
- the exact `code`;
- no leaked `description`;
- `Content-Type: application/json`;
- a `Content-Length` that matches the body.

Each folder deploys its own APIs and removes them again. Nothing needs to exist beforehand.

The LLM providers' upstream is `oef-mock-backend`, a `respond`-based REST API deployed on the **same gateway** and reached at `http://gateway-runtime:8080`. So the stock `gateway/docker-compose.yaml` stack is all you need.

## Prerequisites

- A gateway built from this branch, with `openai-error-format` in `gateway/build.yaml`.
- newman 6.1 or later (`npm i -g newman`). The optional folder uses `pm.execution.skipRequest`.
- Ports 8080 and 9090 free on the host. Check with `lsof -iTCP:8080 -sTCP:LISTEN` before starting the stack.

## Running

```bash
cd gateway && docker compose up -d     # stock stack

newman run gateway/it/postman/openai-error-format-policy/openai-error-format-policy.postman_collection.json \
  --env-var mgmtUser=<admin user> --env-var mgmtPassword=<admin password> \
  --env-var handleUpstreamFaults=false
```

| Variable | Default | Meaning |
|---|---|---|
| `gatewayUrl` | `http://localhost:8080` | Router ingress |
| `mgmtUrl` | `http://localhost:9090/api/management/v1` | Management API |
| `mgmtUser` / `mgmtPassword` | `admin` / `admin` | Management basic-auth credentials (see `api-platform.env`) |
| `handleUpstreamFaults` | `true` | Must match the gateway's `[policy_engine.fault_policies] handle_upstream_faults`. When `false`, folder 14 asserts that router failures keep the proxy's reply (the documented limitation) |
| `httpbinUrl` | *(empty)* | Base URL of an httpbin the gateway can reach, for folder 22. Empty skips the folder |
| `readyAttempts` | `40` | Readiness probes per endpoint (500 ms apart) |
| `syncDelayMs` | `1500` | Pause after an endpoint becomes ready, for the policy snapshot to settle |

### Router-failure cases with the flag on

The stock config leaves `handle_upstream_faults` off. To run folder 14 with formatting expected, set this in `gateway/configs/config.toml`, restart the gateway, and run with `--env-var handleUpstreamFaults=true`:

```toml
[policy_engine.fault_policies]
handle_upstream_faults = true
```

### Optional timeout folder

```bash
docker run -d --name oef-httpbin --network gateway_gateway-network kennethreitz/httpbin
newman run ... --env-var httpbinUrl=http://oef-httpbin:80
```

## Not covered, and why

- **Ordering against another body-writing fault policy.** No other shipped policy implements `OnFault`, so the policy's unit tests cover this.
- **A failure after a streamed response has started.** No shipped policy can raise one. The policy never touches a committed response, which is unit-tested.

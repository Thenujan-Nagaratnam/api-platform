# Testing the `azure-managed-identity` policy end to end

This doc walks through building the gateway with the `azure-managed-identity`
dev policy included, standing up a mock IMDS server (plus the existing
`mock-ai-backend` from the `oauth2` policy's test harness - it's generic
enough to reuse here unchanged), registering an AI API that uses the policy
for outbound auth via the generic `policies` list, and driving every flow
the policy needs to handle correctly.

Nothing here talks to the real Azure Instance Metadata Service or a real
Azure resource. Everything is local and mocked, so this is safe to run
repeatedly and offline - and it has to be: the real IMDS
(`169.254.169.254`) is only reachable from inside actual Azure compute, so
there is no way to test this policy against the real thing from a laptop or
CI runner.

## Architecture

```
                        curl (you)
                            │
                            │  1. admin API: register LlmProvider
                            │     (port 9090, gateway-controller)
                            │
                            │  2. data-plane traffic: POST /azure-umi-test/latest/chat/completions
                            │     (port 8443, gateway-runtime / Envoy)
                            ▼
                 ┌─────────────────────────┐
                 │   gateway-runtime        │
                 │  (Envoy + policy-engine, │
                 │   azure-managed-identity │
                 │   policy compiled in via │
                 │   dev-policies/)         │
                 └───────────┬─────────────┘
                    (a)       │        (b)
           token fetch/cache  │        forward request with
           (only on miss/     │        Authorization: Bearer <token>
            expiry)           │
                    ▼         │                    ▼
       ┌─────────────────────┐│      ┌───────────────────────────┐
       │  mock-azure-imds     ││      │   mock-ai-backend           │
       │  :9701 (host)        ││      │   :9602 (host, shared with   │
       │  stands in for real  ││      │   the oauth2 policy's own    │
       │  169.254.169.254     ││      │   test harness - unchanged)  │
       └─────────────────────┘│      └───────────────────────────┘
                               ▼
                 gateway-controller (port 9090, admin API)
```

The mock IMDS server runs natively on your host (`go run`, no Docker), and
the gateway containers reach it via `host.docker.internal`. `mock-ai-backend`
is the exact same one used by `gateway/dev-policies/upstream-oauth2-authentication/TESTING.md` -
start it from there rather than duplicating it here.

## Prerequisites

- Go (matching the version in `gateway/gateway-runtime/policy-engine/go.mod`)
- Docker + Docker Compose
- `curl`, and optionally `jq` for prettier output
- The `azure-managed-identity` dev policy already in place at
  `gateway/dev-policies/azure-managed-identity/` with a corresponding
  `filePath` entry in `gateway/build.yaml`.

## Part A — Start the mocks

```bash
cd gateway/dev-policies/azure-managed-identity/mocks/mock-azure-imds
GOWORK=off go run .
# mock-azure-imds listening on :9701 (valid client_id: 11111111-1111-1111-1111-111111111111)
```

```bash
cd gateway/dev-policies/upstream-oauth2-authentication/mocks/mock-ai-backend
GOWORK=off go run .
# mock-ai-backend listening on :9602
```

Sanity check both are up:

```bash
curl -s http://localhost:9701/healthz   # -> ok
curl -s http://localhost:9602/healthz   # -> ok
```

### mock-azure-imds reference

| Endpoint | Purpose |
|---|---|
| `GET /metadata/identity/oauth2/token` | Requires the `Metadata: true` header (matching real IMDS) and a `resource` query param. Optional `client_id` - if omitted, behaves as if exactly one identity is attached (issues a token); if given, must match the configured valid client_id or a special test value (see clients below). Optional `ttl` query param overrides `expires_in`/`expires_on` (seconds, default 300). |
| `GET /debug/stats` | Every token request received so far: timestamp, `clientId`, `resource`, `outcome`, and the issued token (if any). |
| `POST /debug/reset` | Clears the history - call this between test flows. |

Configured identity:

| `client_id` | Behavior |
|---|---|
| `11111111-1111-1111-1111-111111111111` (or omitted) | Issues a valid token (200 OK) |
| `broken-client` | Always `500` - simulates an IMDS/platform outage |
| `malformed-client` | `200 OK` but the body is missing `access_token` |
| anything else | `400 {"error":"identity_not_found"}` |

## Part B — Build the gateway with the policy included

```bash
cd gateway
make build   # builds gateway-runtime, gateway-builder, gateway-controller images
```

`gateway/build.yaml` already has:

```yaml
  - name: azure-managed-identity
    filePath: ./dev-policies/azure-managed-identity
```

## Part C — Run the gateway stack

```bash
cd gateway
docker compose --profile redis up -d gateway-controller gateway-runtime sample-backend redis
```

`--profile redis` is needed for the Redis-cache verification in Part E.4 -
skip it and the policy still works, it just falls back to fetching a fresh
token from IMDS on every cache miss instead of sharing one via Redis.

Confirm it's up:

```bash
curl -s http://localhost:9094/api/admin/v1/health   # -> {"status":"healthy",...}
```

## Part D — Register the AI API

Unlike `oauth2`, this policy is attached via the **generic `policies` list**,
not the typed `upstream.auth` field - matching how `aws-authentication` is
attached today.

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: azure-umi-test
spec:
  displayName: Azure UMI Test
  version: v1.0
  template: openai
  context: /azure-umi-test/latest
  upstream:
    url: http://host.docker.internal:9602
  policies:
    - name: azure-managed-identity
      version: v0
      paths:
        - path: /chat/completions
          methods: [POST]
          params:
            clientId: 11111111-1111-1111-1111-111111111111
            resource: https://cognitiveservices.azure.com/
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF
```

> Unlike `oauth2`'s typed `upstream.auth` field, the generic `policies` list's
> `params` don't sit directly on the policy entry - they're nested under a
> `paths` array (`path`/`methods`/`params` per entry, matching the
> `LLMPolicyPath` schema). Putting `params` directly under `name`/`version`
> is accepted by the API but silently drops the params entirely (no error -
> the request succeeds with an empty policy config), which surfaces at
> request time as an empty/missing `Authorization` header rather than a
> registration-time error. Worth double-checking `GET` on the provider you
> just registered echoes back the `params` you sent if anything looks off.

This won't call the real IMDS - `systemParameters.imdsEndpoint` needs to
point at the mock. Set it via `gateway/configs/config.toml` (already done):

```toml
[policy_configurations.azure_managed_identity_v1]
imds_endpoint = "http://host.docker.internal:9701/metadata/identity/oauth2/token"
```

## Part E — Test flows

Reset the mock's history before each flow:

```bash
curl -s -X POST http://localhost:9701/debug/reset
```

### E.1 — Happy path

```bash
curl -sk -X POST https://localhost:8443/azure-umi-test/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' | jq .
```

**Expect:** `200 OK`, `choices[0].message.content` reads
`received Authorization: "Bearer mock-imds-token-..."`.

```bash
curl -s http://localhost:9701/debug/stats | jq '.history[-1]'
# outcome should be "issued", resource should be the configured resource
```

### E.2 — Token caching (no repeated IMDS calls within TTL)

```bash
curl -s -X POST http://localhost:9701/debug/reset

for i in 1 2 3 4 5; do
  curl -sk -X POST https://localhost:8443/azure-umi-test/latest/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
    | jq -r '.choices[0].message.content'
done

curl -s http://localhost:9701/debug/stats | jq '.tokenRequestCount'
```

**Expect:** all 5 responses embed the same token, `tokenRequestCount` is `1`.

### E.3 — Unreachable identity / platform outage → 502

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: azure-umi-test-broken
spec:
  displayName: Azure UMI Test (simulated platform outage)
  version: v1.0
  template: openai
  context: /azure-umi-test-broken/latest
  upstream:
    url: http://host.docker.internal:9602
  policies:
    - name: azure-managed-identity
      version: v0
      paths:
        - path: /chat/completions
          methods: [POST]
          params:
            clientId: broken-client
            resource: https://cognitiveservices.azure.com/
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF

curl -sk -o - -w "\nHTTP %{http_code}\n" -X POST \
  https://localhost:8443/azure-umi-test-broken/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

**Expect:** `HTTP 502` and body
`{"error":"Bad Gateway","message":"failed to authenticate request to upstream service"}`.

### E.4 — Redis-backed token cache

Requires the stack to have been started with `--profile redis` (Part C).

```bash
curl -s -X POST http://localhost:9701/debug/reset
curl -sk -X POST https://localhost:8443/azure-umi-test/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' > /dev/null

docker exec gateway-redis-1 redis-cli KEYS 'azure-managed-identity:token:v1:*'
# copy a key from the output, then:
docker exec gateway-redis-1 redis-cli GET '<key>'
docker exec gateway-redis-1 redis-cli TTL '<key>'
```

To see the fail-open fallback:

```bash
docker compose stop redis
curl -sk -X POST https://localhost:8443/azure-umi-test/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' | jq .
# still 200 OK - falls back to a direct IMDS call
docker compose start redis
```

## Part F — Cleanup

```bash
cd gateway
docker compose down

pkill -f mock-azure-imds
pkill -f mock-ai-backend
```

```bash
for name in azure-umi-test azure-umi-test-broken; do
  curl -X DELETE "http://localhost:9090/api/management/v1/llm-providers/$name" \
    -H "Authorization: Basic YWRtaW46YWRtaW4="
done
```

or just `docker compose down -v` to wipe the controller's persisted state entirely.

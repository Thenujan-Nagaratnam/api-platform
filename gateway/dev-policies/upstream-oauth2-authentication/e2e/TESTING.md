# Testing the `oauth2` policy end to end

This doc walks through building the gateway with the `oauth2` dev policy
included, standing up two small mock servers (plus an optional Redis
instance), registering an AI API that uses the policy for outbound auth, and
driving every flow the policy needs to handle correctly — happy path, token
caching (both the in-process cache and the shared Redis cache), token
refresh on expiry, and all three failure modes.

Nothing here talks to a real OAuth2 provider or a real LLM. Everything is
local and mocked, so this is safe to run repeatedly and offline.

> **One-command version:** once the gateway stack is up (Parts B/C below),
> `./run-e2e.sh` in this directory starts both mocks and runs every flow in
> `postman/oauth2.postman_collection.json` via newman, reporting a single
> pass/fail for the whole suite. The manual steps below are for driving
> individual flows by hand or debugging a specific one.

## Architecture

```
                        curl (you)
                            │
                            │  1. admin API: register LlmProvider
                            │     (port 9090, gateway-controller)
                            │
                            │  2. data-plane traffic: POST /oauth2-test/chat/completions
                            │     (port 8443, gateway-runtime / Envoy)
                            ▼
                 ┌─────────────────────────┐
                 │   gateway-runtime        │
                 │  (Envoy + policy-engine, │
                 │   oauth2 policy compiled │
                 │   in via dev-policies/)  │
                 └───────────┬─────────────┘
                    (a)       │        (b)
           token fetch/cache  │        forward request with
           (only on miss/     │        Authorization: Bearer <token>
            expiry)           │
                    ▼         │                    ▼
       ┌─────────────────────┐│      ┌───────────────────────────┐
       │  mock-oauth2-idp     ││      │   mock-ai-backend          │
       │  :9601 (host)        ││      │   :9602 (host)              │
       │  client_credentials  ││      │   echoes back the           │
       │  and password grants ││      │   Authorization header it   │
       │  TTL, failure         ││      │   actually received, in an  │
       │  injection            ││      │   OpenAI-chat-completions   │
       └─────────────────────┘│      │   shaped response           │
                               │      └───────────────────────────┘
                               ▼
                 gateway-controller (port 9090, admin API)
```

The two mock servers run natively on your host (`go run`, no Docker), and the
gateway containers reach them via `host.docker.internal` (works out of the
box with Docker Desktop / Rancher Desktop / Colima on macOS).

## Prerequisites

- Go (matching the version in `gateway/gateway-runtime/policy-engine/go.mod`)
- Docker + Docker Compose
- `curl`, and optionally `jq` for prettier output
- The `upstream-oauth2-authentication` dev policy already in place at `gateway/dev-policies/upstream-oauth2-authentication/`
  with a corresponding `filePath` entry in `gateway/build.yaml` (already done
  as part of this policy's development — see `gateway/dev-policies/README.md`
  if you need to redo this from scratch).

## Part A — Start the two mocks

Each mock is a standalone Go module with no third-party dependencies. Run
each in its own terminal (or background them):

```bash
cd gateway/dev-policies/upstream-oauth2-authentication/e2e/mocks/mock-oauth2-idp
GOWORK=off go run .
# mock-oauth2-idp listening on :9601 (valid client: test-client / test-secret)
```

```bash
cd gateway/dev-policies/upstream-oauth2-authentication/e2e/mocks/mock-ai-backend
GOWORK=off go run .
# mock-ai-backend listening on :9602
```

`GOWORK=off` is needed because these two mock modules are intentionally not
listed in the repo's root `go.work` (they're throwaway test tooling, not part
of the product build graph) — without it, Go tries to resolve them through
the workspace and fails with "outside module roots".

Sanity check both are up:

```bash
curl -s http://localhost:9601/healthz   # -> ok
curl -s http://localhost:9602/healthz   # -> ok
```

### mock-oauth2-idp reference

| Endpoint               | Purpose                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| ---------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /oauth2/token` | `client_credentials` and `password` grants. Accepts **both** `client_secret_basic` (HTTP Basic auth) and `client_secret_post` (`client_id`/`client_secret` form fields) — the `oauth2` policy's `clientAuthMethod` param selects between them. Optional `ttl` form field (or query param) overrides the token's `expires_in` (seconds, default 30) — use a small value (e.g. `ttl=2`) to test refresh quickly. Optional `delayMs` form field (or query param) artificially delays the response — use this to test `tokenRequestTimeout` (E.12 below). Optional `omitExpiresIn=true` drops `expires_in` from the response entirely — use this to test `defaultTokenTTL` (E.13 below). Optional `scope` form field is echoed back. |
| `GET /debug/stats`   | Every token request received so far: timestamp,`clientId`, `authStyle` (`basic`/`post`), `scope`, `outcome`, and the issued token (if any). **This is how you prove caching/refresh behavior** — count how many gateway-visible requests translated into how many `/oauth2/token` calls.                                                                                                                                                                                                                                                                                                           |
| `POST /debug/reset`  | Clears the history — call this between test flows below so counts don't carry over.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |

Built-in clients:

| `clientId`         | `clientSecret` | Behavior                                            |
| -------------------- | ---------------- | --------------------------------------------------- |
| `test-client`      | `test-secret`  | Issues a valid token (200 OK)                       |
| `broken-client`    | *(any)*        | Always`500` — simulates the IdP being down       |
| `malformed-client` | *(any)*        | `200 OK` but the body is missing `access_token` |
| anything else        | *(any)*        | `400 {"error":"invalid_client"}`                  |

### mock-ai-backend reference

| Endpoint                                        | Purpose                                                                                                                                                                                                                          |
| ----------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /chat/completions` (or any path/method) | Returns an OpenAI-chat-completions-shaped response whose`choices[0].message.content` literally says `received Authorization: "<value>"` — so a plain curl through the gateway shows you the injected bearer token directly. |
| `GET /debug/last-request`                     | Full headers + body of the most recent request it received — use this for scripted assertions, e.g.`curl .../debug/last-request \| jq -r .headers.Authorization[0]`.                                                           |
| `POST /debug/force-status?code=401`           | Makes the **next request only** (any path/method) return that status instead of the normal 200 - simulates the upstream rejecting an already-cached bearer token, to test `tokenPurgeStatusCodes` (E.16/E.17 below). Consumed on use - the request after that one gets the normal 200 behavior again. |
| `POST /debug/reset`                           | Clears the last-request record and any pending forced status - call this between test flows.                                                                                                                                    |

## Part B — Build the gateway with the policy included

```bash
cd gateway
make build   # builds gateway-runtime, gateway-builder, gateway-controller images
```

This is a normal `make build` — the only thing specific to this policy is
that `gateway/build.yaml` already has:

```yaml
  - name: upstream-oauth2-authentication
    filePath: ./dev-policies/upstream-oauth2-authentication
```

which `gateway-runtime`'s Dockerfile picks up automatically via its
`--build-context dev-policies=../dev-policies` build context. You can
confirm the policy made it into the built image afterwards:

```bash
docker run --rm --entrypoint cat ghcr.io/wso2/api-platform/gateway-runtime:latest /app/build-manifest.yaml | grep -A2 '^\s*- name: upstream-oauth2-authentication$'
```

## Part C — Run the gateway stack

Use the top-level `gateway/docker-compose.yaml` — it's the plain local-dev
stack (no IT-suite dependencies like `mock-platform-api`), and `make build`
(Part B) already tagged your locally-built images as
`ghcr.io/wso2/api-platform/{gateway-controller,gateway-runtime}:$(cat gateway/VERSION)`,
which is exactly the tag this compose file references — so `docker compose up` here uses your local build, not anything pulled from a registry (verify
with `docker image inspect ... --format '{{.Created}}'` if you want to be
sure it's today's build, not a stale local image from a previous pull).

```bash
cd gateway
docker compose --profile redis up -d gateway-controller gateway-runtime sample-backend redis
```

`--profile redis` (and listing `redis` explicitly) is required — the `redis`
service is opt-in so it doesn't start for every developer using this compose
file for unrelated work. `gateway/configs/config.toml` already points the
oauth2 policy's token cache at this service
(`[policy_configurations.upstream_oauth2_v1.redis]`, `host = "redis"`). If you skip
`--profile redis`, the stack still works identically - the policy just falls
back to fetching a fresh token on every cache miss instead of sharing one via
Redis (see Part E.10 below).

Wait for both to be up, then confirm — **note the ports**: this compose file
maps gateway-controller's admin API to host port **9094**, not 9092 (that's
a different compose file's port; don't mix the two up):

```bash
curl -s http://localhost:9094/api/admin/v1/health   # gateway-controller admin -> {"status":"healthy",...}
curl -sk https://localhost:8443/                    # gateway-runtime (Envoy) — a 404 is fine here, it just means no route is registered yet
```

> `host.docker.internal` (used by the mocks in Part D below) resolves
> automatically on Docker Desktop / Rancher Desktop / Colima on macOS — no
> extra config needed for `gateway-runtime`, even though only
> `gateway-controller` has an explicit `extra_hosts` entry in this compose
> file. On plain Docker Engine (Linux), add
> `extra_hosts: ["host.docker.internal:host-gateway"]` to the
> `gateway-runtime` service too, or run the mocks in containers on
> `gateway-network` instead and reference them by service name.

## Part D — Register the AI API with outbound OAuth2 security

This is the core of what we're testing: an `LlmProvider` whose upstream
requires OAuth2, secured via the **typed** `upstream.auth` field — the same
field `api-key` has always used, now also supporting
`type: oauth2` (the transformer attaches the `oauth2` policy under the hood,
the same way it's always attached `set-headers` for `api-key`). No generic
`policies` list entry is needed.

The gateway-controller management API's base path is `/api/management/v1`
(not `v0.9` — an earlier revision of this doc had that wrong).

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-basic
spec:
  displayName: OAuth2 Test
  version: v1.0
  template: openai
  context: /oauth2-test-basic/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      oauth2TokenEndpoint: http://host.docker.internal:9601/oauth2/token
      oauth2ClientId: test-client
      oauth2ClientSecret: test-secret
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF
```

Should return `201 Created`. If it fails with a validation error, re-check
`gateway-controller`'s logs — a typo in `oauth2TokenEndpoint`/`oauth2ClientId`
will only surface once you send real traffic. Note that POSTing the same
`metadata.name` a second time does **not** redeploy it — it returns `400`
with `configuration already exists`; delete it first (Part F) if you need to
re-register with changed spec fields.

`grantType` is optional and defaults to `client_credentials` (you can add
`oauth2GrantType: client_credentials` explicitly; it's accepted and
forwarded to the policy the same way). Client authentication to the token
endpoint always uses `client_secret_basic` (HTTP Basic auth) — there is no
configurable client-auth-method field.

## Part E — Test flows

Reset the IdP's history before each flow so request counts aren't polluted
by earlier ones:

```bash
curl -s -X POST http://localhost:9601/debug/reset
```

### E.1 — Happy path

```bash
curl -sk -X POST https://localhost:8443/oauth2-test-basic/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' | jq .
```

**Expect:** `200 OK`, and `choices[0].message.content` reads
`received Authorization: "Bearer mock-token-1-issued-..."`.

Confirm the IdP was actually called with Basic auth:

```bash
curl -s http://localhost:9601/debug/stats | jq '.history[-1]'
# authStyle should be "basic", outcome "issued"
```

### E.2 — Token caching (no repeated IdP calls within TTL)

```bash
curl -s -X POST http://localhost:9601/debug/reset

for i in 1 2 3 4 5; do
  curl -sk -X POST https://localhost:8443/oauth2-test-basic/latest/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
    | jq -r '.choices[0].message.content'
done

curl -s http://localhost:9601/debug/stats | jq '.tokenRequestCount'
```

**Expect:** all 5 responses embed the **same** `mock-token-N-issued-...`
value, and `tokenRequestCount` is **1** — proving the policy reused the
cached token instead of hitting the IdP on every request.

### E.3 — Token expiry and refresh

This requires a provider configured with a short TTL. Register a second one:

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-shortttl
spec:
  displayName: OAuth2 Test (short TTL, for refresh testing)
  version: v1.0
  template: openai
  context: /oauth2-test-shortttl/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      oauth2TokenEndpoint: http://host.docker.internal:9601/oauth2/token?ttl=2
      oauth2ClientId: test-client
      oauth2ClientSecret: test-secret
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF
```

The `?ttl=2` query parameter on `oauth2TokenEndpoint` is read by the mock via
`r.FormValue`, which checks both the URL query string and the POST body —
this is the practical way to force a short TTL through a real OAuth2 client
library, since such libraries generally don't expose a way to inject an
arbitrary extra body field per grant request.

```bash
curl -s -X POST http://localhost:9601/debug/reset

curl -sk -X POST https://localhost:8443/oauth2-test-shortttl/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  | jq -r '.choices[0].message.content'   # note the token value

sleep 3   # let the 2-second token expire

curl -sk -X POST https://localhost:8443/oauth2-test-shortttl/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  | jq -r '.choices[0].message.content'   # token value should differ

curl -s http://localhost:9601/debug/stats | jq '.tokenRequestCount'
# -> 2 (one fetch, one refresh)
```

### E.4 — Invalid client credentials → 502, no leaked detail

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-badsecret
spec:
  displayName: OAuth2 Test (wrong client secret)
  version: v1.0
  template: openai
  context: /oauth2-test-badsecret/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      oauth2TokenEndpoint: http://host.docker.internal:9601/oauth2/token
      oauth2ClientId: test-client
      oauth2ClientSecret: totally-wrong-secret
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF

curl -sk -o - -w "\nHTTP %{http_code}\n" -X POST \
  https://localhost:8443/oauth2-test-badsecret/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

**Expect:** `HTTP 502` and body
`{"error":"Bad Gateway","message":"failed to authenticate request to upstream service"}`
— the underlying `invalid_client` detail from the IdP must **not** appear in
the response (only in the gateway's own logs).

### E.5 — Token endpoint unreachable → 502

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-unreachable
spec:
  displayName: OAuth2 Test (token endpoint unreachable)
  version: v1.0
  template: openai
  context: /oauth2-test-unreachable/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      # Port 9999 has nothing listening on it — a genuine connection
      # failure, distinct from a 4xx/5xx from a reachable IdP.
      oauth2TokenEndpoint: http://host.docker.internal:9999/oauth2/token
      oauth2ClientId: test-client
      oauth2ClientSecret: test-secret
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF

curl -sk -o - -w "\nHTTP %{http_code}\n" -X POST \
  https://localhost:8443/oauth2-test-unreachable/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

**Expect:** the same generic `502` shape as E.4.

### E.6 — Malformed IdP response → 502

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-malformed
spec:
  displayName: OAuth2 Test (malformed IdP response)
  version: v1.0
  template: openai
  context: /oauth2-test-malformed/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      oauth2TokenEndpoint: http://host.docker.internal:9601/oauth2/token
      oauth2ClientId: malformed-client
      oauth2ClientSecret: whatever
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF

curl -sk -o - -w "\nHTTP %{http_code}\n" -X POST \
  https://localhost:8443/oauth2-test-malformed/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

**Expect:** the same generic `502` shape — the mock returns `200 OK` with no
`access_token`, proving the policy doesn't crash or forward an empty
`Authorization: Bearer` header when the IdP's response is broken.

### E.7 — Confirm the header via the backend's own record (belt-and-suspenders)

For any of the successful flows above, you can also check the backend's own
view of what it received, independent of what the gateway echoed back:

```bash
curl -s http://localhost:9602/debug/last-request | jq '.headers.Authorization'
```

### E.8 — Unsupported `grantType` rejected at configuration time

`grantType` exists so further grants can be added without a breaking schema
change, but only `client_credentials` and `password` are implemented today —
an unrecognized value (e.g. `authorization_code`) must be rejected when the
provider is registered, not accepted and silently ignored:

```bash
curl -s -o - -w "\nHTTP %{http_code}\n" -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-badgrant
spec:
  displayName: OAuth2 Test (unsupported grant type)
  version: v1.0
  template: openai
  context: /oauth2-test-badgrant/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      oauth2GrantType: authorization_code
      oauth2TokenEndpoint: http://host.docker.internal:9601/oauth2/token
      oauth2ClientId: test-client
      oauth2ClientSecret: test-secret
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF
```

**Expect:** a validation error at registration time (not a 502 at request
time) mentioning `oauth2GrantType`.

### E.9 — Resource Owner Password Credentials grant (`grantType: password`)

> **Security note:** the password grant (RFC 6749 §4.3) is supported for
> bridging to legacy identity providers only — it requires the gateway to
> handle the resource owner's raw username/password directly. Prefer
> `client_credentials` (E.1–E.8 above) whenever the upstream identity
> provider supports it.

The mock IdP accepts `username=resource-owner`/`password=hunter2` by default
(override via the `RESOURCE_OWNER_USERNAME`/`RESOURCE_OWNER_PASSWORD` env
vars when starting `mock-oauth2-idp`), in addition to the same
`clientId`/`clientSecret` checks as the `client_credentials` examples above.

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-password
spec:
  displayName: OAuth2 Test (password grant)
  version: v1.0
  template: openai
  context: /oauth2-test-password/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      oauth2GrantType: password
      oauth2TokenEndpoint: http://host.docker.internal:9601/oauth2/token
      oauth2ClientId: test-client
      oauth2ClientSecret: test-secret
      oauth2Username: resource-owner
      oauth2Password: hunter2
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF
```

```bash
curl -s -X POST http://localhost:9601/debug/reset

curl -sk -X POST https://localhost:8443/oauth2-test-password/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' | jq .

curl -s http://localhost:9601/debug/stats | jq '.history[-1]'
```

**Expect:** `200 OK`, the same injected-bearer-token content shape as E.1.

For a failure case, register a second provider with a wrong `oauth2Password`
(e.g. `wrong-password`) — the mock IdP rejects mismatched resource-owner
credentials with `400 invalid_grant`, and the policy should turn that into
the same generic `502` shape as E.4 (never leaking `invalid_grant` to the
caller).

### E.10 — Redis-backed token cache

Requires the stack to have been started with `--profile redis` (Part C).
This proves the token the policy obtained actually lives in Redis, not just
in the gateway-runtime process's own memory.

```bash
curl -s -X POST http://localhost:9601/debug/reset

curl -sk -X POST https://localhost:8443/oauth2-test-basic/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  | jq -r '.choices[0].message.content'

# Ask Redis directly for the cached entry - the key is
# <keyPrefix><apiId>, scoped to the API (not the individual route/resource),
# since oauth2 config lives on upstream.auth - one value for the whole API,
# shared by every resource it exposes. keyPrefix defaults to
# "upstream-oauth2:token:v1:", so scanning for that prefix finds it without knowing
# the exact apiId up front.
docker run --rm --network gateway_gateway-network redis:7-alpine \
  redis-cli -h redis KEYS 'upstream-oauth2:token:v1:*'

docker run --rm --network gateway_gateway-network redis:7-alpine \
  redis-cli -h redis GET '<the key from above>'
# -> {"access_token":"mock-token-...","token_type":"Bearer","expiry":"..."}

docker run --rm --network gateway_gateway-network redis:7-alpine \
  redis-cli -h redis TTL '<the key from above>'
# -> a positive number of seconds, close to (but not exceeding) the mock
#    IdP's default 300s expires_in
```

**Expect:** the key exists, its value is a JSON-encoded token matching what
the mock IdP just issued, and its TTL is positive and roughly matches the
token's remaining lifetime.

To see the fallback behavior when Redis is unreachable, stop the `redis`
service and repeat the chat-completions request — it should still succeed
(same `200 OK`, fresh token each time since there's no cache to hit),
proving the `failureMode: open` default degrades gracefully rather than
breaking authentication:

```bash
docker compose stop redis
curl -sk -X POST https://localhost:8443/oauth2-test-basic/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' | jq .
docker compose start redis   # bring it back for any further testing
```

### E.11 — `client_secret_post` client authentication

`clientAuthMethod` defaults to `client_secret_basic` (all the flows above use
it implicitly). Set it to `client_secret_post` to have `clientId`/
`clientSecret` sent as form fields in the token request body instead of an
HTTP Basic header:

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-clientauthpost
spec:
  displayName: OAuth2 Test (client_secret_post)
  version: v1.0
  template: openai
  context: /oauth2-test-clientauthpost/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      oauth2TokenEndpoint: http://host.docker.internal:9601/oauth2/token
      oauth2ClientId: test-client
      oauth2ClientSecret: test-secret
      oauth2ClientAuthMethod: client_secret_post
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF
```

```bash
curl -s -X POST http://localhost:9601/debug/reset

curl -sk -X POST https://localhost:8443/oauth2-test-clientauthpost/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' | jq .

curl -s http://localhost:9601/debug/stats | jq '.history[-1]'
```

**Expect:** `200 OK` on the chat-completions call, and
`.history[-1].authStyle` reads `"post"` (not `"basic"`) — proving the client
credentials actually arrived as form fields, not a Basic auth header.

### E.12 — `tokenRequestTimeout` bounds a hung token endpoint

Without this, an unresponsive identity provider would block a token fetch
indefinitely (`golang.org/x/oauth2` falls back to `http.DefaultClient`,
which has no timeout at all). Point the token endpoint at the mock's
`delayMs` param to simulate a slow IdP, and set a much smaller
`tokenRequestTimeout`:

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-timeout
spec:
  displayName: OAuth2 Test (tokenRequestTimeout)
  version: v1.0
  template: openai
  context: /oauth2-test-timeout/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      oauth2TokenEndpoint: "http://host.docker.internal:9601/oauth2/token?delayMs=3000"
      oauth2ClientId: test-client
      oauth2ClientSecret: test-secret
      oauth2TokenRequestTimeout: "500ms"
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF
```

```bash
time curl -sk -X POST https://localhost:8443/oauth2-test-timeout/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

**Expect:** `502 Bad Gateway` (`failed to authenticate request to upstream
service`) in well under 3 seconds — the 500ms `tokenRequestTimeout` aborts
the request long before the IdP's simulated 3-second delay completes.

### E.13 — `defaultTokenTTL` fallback when the IdP omits `expires_in`

Without this, an IdP response missing `expires_in` would leave the token's
expiry at the zero value, which `golang.org/x/oauth2`'s `Token.Valid()`
always treats as already-expired — silently disabling caching entirely (a
fresh token fetch on every single request, with no visible error).

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-defaultttl
spec:
  displayName: OAuth2 Test (defaultTokenTTL)
  version: v1.0
  template: openai
  context: /oauth2-test-defaultttl/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      oauth2TokenEndpoint: http://host.docker.internal:9601/oauth2/token
      oauth2ClientId: test-client
      oauth2ClientSecret: test-secret
      oauth2DefaultTokenTTL: "20s"
      oauth2Params:
        omitExpiresIn: "true"
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF
```

```bash
curl -s -X POST http://localhost:9601/debug/reset

for i in 1 2 3; do
  curl -sk -X POST https://localhost:8443/oauth2-test-defaultttl/latest/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
    -o /dev/null -w "req $i: %{http_code}\n"
done

curl -s http://localhost:9601/debug/stats | jq '.tokenRequestCount'
```

**Expect:** all three requests return `200`, and `tokenRequestCount` is `1`
— even though the IdP never sent `expires_in`, the 20s `defaultTokenTTL`
fallback made the token cacheable, so requests 2 and 3 were served from
cache rather than each re-fetching from the IdP.

### E.15 — `oauth2Params.scope` reaches the token endpoint for the password grant

`params` is otherwise `client_credentials`-only (the password grant's
underlying library helper has no `EndpointParams`-style hook), but `scope`
specifically is mapped to `Config.Scopes` for the password grant too - the
one extension point `PasswordCredentialsToken` actually has.

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-password-scope
spec:
  displayName: OAuth2 Test (password grant, custom scope)
  version: v1.0
  template: openai
  context: /oauth2-test-password-scope/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      oauth2GrantType: password
      oauth2TokenEndpoint: http://host.docker.internal:9601/oauth2/token
      oauth2ClientId: test-client
      oauth2ClientSecret: test-secret
      oauth2Username: resource-owner
      oauth2Password: hunter2
      oauth2Params:
        scope: "read write"
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF
```

```bash
curl -s -X POST http://localhost:9601/debug/reset

curl -sk -X POST https://localhost:8443/oauth2-test-password-scope/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  -o /dev/null -w "req: %{http_code}\n"

curl -s http://localhost:9601/debug/stats | jq '.history[0].scope'
```

**Expect:** `200`, and `.history[0].scope` is `"read write"`.

### E.16 — Purge the cached token on an upstream 401 (`tokenPurgeStatusCodes`)

Simulates the upstream backend rejecting an already-cached bearer token
(e.g. revoked out-of-band at the identity provider). The oauth2 policy's
`OnResponseHeaders` purges both cache tiers when the upstream responds with
a status in `tokenPurgeStatusCodes` (default `[401]`) - it does
not retry the request that triggered the purge, only the next one is
guaranteed a fresh token.

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-purge
spec:
  displayName: OAuth2 Test (purge on upstream 401, default)
  version: v1.0
  template: openai
  context: /oauth2-test-purge/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      oauth2TokenEndpoint: http://host.docker.internal:9601/oauth2/token
      oauth2ClientId: test-client
      oauth2ClientSecret: test-secret
      oauth2TokenPurgeStatusCodes: [401]
      oauth2Params:
        testId: purge-401-default
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF
```

```bash
curl -s -X POST http://localhost:9601/debug/reset
curl -s -X POST http://localhost:9602/debug/reset

# Prime the cache with a real token.
curl -sk -X POST https://localhost:8443/oauth2-test-purge/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  -o /dev/null -w "prime: %{http_code}\n"
curl -s http://localhost:9601/debug/stats | jq '.tokenRequestCount'   # expect 1

# Make mock-ai-backend reject the NEXT request only.
curl -s -X POST "http://localhost:9602/debug/force-status?code=401"

# This request gets the forced 401 (passed straight through, not retried) -
# and is also the one whose OnResponseHeaders call purges the cache.
curl -sk -X POST https://localhost:8443/oauth2-test-purge/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  -o /dev/null -w "after force-401: %{http_code}\n"

# mock-ai-backend is back to normal; the cache was purged, so this must
# fetch a genuinely fresh token rather than reusing the purged one.
curl -sk -X POST https://localhost:8443/oauth2-test-purge/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  -o /dev/null -w "post-purge: %{http_code}\n"
curl -s http://localhost:9601/debug/stats | jq '.tokenRequestCount'   # expect 2
```

**Expect:** `prime` and `post-purge` are `200`; `after force-401` is `401`;
`tokenRequestCount` is `1` after priming and `2` after the purge - if it
were still `1`, the purge did not actually force a refetch.

### E.17 — `tokenPurgeStatusCodes: []` disables purging entirely

Same flow as E.16, but against a provider that explicitly sets an empty
list - proves a publisher can opt out, and that `OnResponseHeaders` doesn't
purge (or, per `Mode()`, even run) when there's nothing to purge on.

```bash
curl -X POST http://localhost:9090/api/management/v1/llm-providers \
  -H "Content-Type: application/yaml" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: oauth2-test-purge-disabled
spec:
  displayName: OAuth2 Test (purge explicitly disabled)
  version: v1.0
  template: openai
  context: /oauth2-test-purge-disabled/latest
  upstream:
    url: http://host.docker.internal:9602
    auth:
      type: oauth2
      oauth2TokenEndpoint: http://host.docker.internal:9601/oauth2/token
      oauth2ClientId: test-client
      oauth2ClientSecret: test-secret
      oauth2TokenPurgeStatusCodes: []
      oauth2Params:
        testId: purge-disabled
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF
```

```bash
curl -s -X POST http://localhost:9601/debug/reset
curl -s -X POST http://localhost:9602/debug/reset

curl -sk -X POST https://localhost:8443/oauth2-test-purge-disabled/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  -o /dev/null -w "prime: %{http_code}\n"
curl -s http://localhost:9601/debug/stats | jq '.tokenRequestCount'   # expect 1

curl -s -X POST "http://localhost:9602/debug/force-status?code=401"

curl -sk -X POST https://localhost:8443/oauth2-test-purge-disabled/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  -o /dev/null -w "after force-401: %{http_code}\n"

curl -sk -X POST https://localhost:8443/oauth2-test-purge-disabled/latest/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
  -o /dev/null -w "after (should still be cached): %{http_code}\n"
curl -s http://localhost:9601/debug/stats | jq '.tokenRequestCount'   # expect still 1
```

**Expect:** `tokenRequestCount` stays `1` throughout, even after the forced
`401` - if it becomes `2`, disabling `tokenPurgeStatusCodes`
(set to `[]`) failed to actually disable it.

## Part F — Cleanup

```bash
cd gateway
docker compose down

# Ctrl-C both `go run` mock processes, or:
pkill -f mock-oauth2-idp
pkill -f mock-ai-backend
```

If you don't want to keep the test `LlmProvider`s around, delete them via the
same admin API:

```bash
for name in oauth2-test-basic oauth2-test-shortttl \
            oauth2-test-badsecret oauth2-test-unreachable oauth2-test-malformed \
            oauth2-test-badgrant oauth2-test-password oauth2-test-password-wrong \
            oauth2-test-clientauthpost oauth2-test-timeout oauth2-test-defaultttl \
            oauth2-test-purge oauth2-test-purge-disabled oauth2-test-password-scope; do
  curl -X DELETE "http://localhost:9090/api/management/v1/llm-providers/$name" \
    -H "Authorization: Basic YWRtaW46YWRtaW4="
done
```

or just `docker compose down -v` to wipe the controller's persisted state
entirely.

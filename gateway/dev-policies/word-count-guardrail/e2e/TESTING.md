# Testing word-count-guardrail's vendor-shaped rejection end to end

This doc walks through building the gateway with the modified
`word-count-guardrail` dev policy, standing up gateway-controller +
gateway-runtime locally, and driving the flow that matters: a guardrail
rejection on an `openai`-templated provider should come back shaped like
OpenAI's own `finish_reason: content_filter` convention, an
`anthropic`-templated provider should come back as a genuine Anthropic error
envelope, and so on for the other five bundled templates — instead of the
generic WSO2 `{"type":"WORD_COUNT_GUARDRAIL",...}` shape.

No real LLM backend, and no mock LLM backend, is needed. Every assertion here
is a **request-phase** guardrail rejection — it fires before the request
would ever reach an upstream, so `upstream.url` on the test providers is a
placeholder that's never dialed.

> **One-command version:** once the gateway stack is up (Part B below),
> `./run-e2e.sh` in this directory registers the 7 vendor-templated test
> providers, generates a key for each, sends a below-minimum-word-count
> request, and asserts the vendor-shaped rejection — reporting a single
> pass/fail for the whole suite. The manual steps below are for driving
> individual flows by hand or debugging a specific one.

## Architecture

```
                        curl / newman (you)
                            │
                            │  1. admin API: register LlmProvider,
                            │     generate an API key (port 9090)
                            │
                            │  2. data-plane traffic: POST /wcg-openai/chat/completions
                            │     (port 8080, gateway-runtime / Envoy)
                            ▼
                 ┌─────────────────────────┐
                 │   gateway-runtime        │
                 │  (Envoy + policy-engine, │
                 │   word-count-guardrail   │
                 │   compiled in via        │
                 │   dev-policies/)         │
                 └───────────┬─────────────┘
                              │ live lookup: policy.GetLazyResourceStoreInstance()
                              │   .GetResourceByIDAndType(template_handle, "LlmProviderTemplate")
                              ▼
                 ┌─────────────────────────┐
                 │   gateway-controller     │
                 │  (loads default-llm-     │
                 │   provider-templates/*   │
                 │   at startup, pushes as  │
                 │   LazyResources via xDS) │
                 └─────────────────────────┘
```

The guardrail never calls an upstream and never calls gateway-controller
directly per-request — it reads the already-synced `LlmProviderTemplate`
document straight out of the policy-engine's in-process lazy resource store,
which the controller populated once at startup (and keeps in sync via xDS).

## Prerequisites

- `gateway/dev-policies/word-count-guardrail/` contains the modified policy
  source, and `gateway/build.yaml`'s `word-count-guardrail` entry is a
  `filePath` pointing at it (not a `gomodule` reference) — see the top-level
  `dev-policies/README.md` for the general pattern. If this repo's
  `build.yaml` still has the `gomodule` entry, swap it:
  ```yaml
  - name: word-count-guardrail
    filePath: ./dev-policies/word-count-guardrail
  ```
- `gateway/gateway-controller/api/management-openapi.yaml`'s
  `LLMProviderTemplateData` schema has a `guardrailResponse` property, and
  `pkg/api/management/generated.go` has been regenerated
  (`make generate-server-code` in `gateway/gateway-controller`) — otherwise
  the controller's template loader silently drops the field and every
  vendor-shape assertion below falls back to the default WSO2 shape. This
  is a real, previously-hit failure mode — see "Known gotchas" below.
- `gateway/gateway-controller/default-llm-provider-templates/*.yaml` (and the
  `platform-api/resources/` mirror, if you also touch AI Workspace) declare a
  `guardrailResponse` block for the templates you're testing.
- `newman` (`npm install -g newman`) for the Postman collection, or let
  `run-e2e.sh` fall back to `npx --yes newman`.

## Part A — Build the gateway with the policy included

```bash
REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT/gateway/gateway-builder"
go run ./cmd/builder \
  -build-file        ../build.yaml \
  -system-build-lock ../system-policies/system-build-lock.yaml \
  -policy-engine-src ../gateway-runtime/policy-engine \
  -out-dir           ./target/output \
  -log-level         info
```

Confirm:
- `ls "$REPO_ROOT/gateway/gateway-builder/target/output/gateway-controller/policies/" | grep word-count` shows `word-count-guardrail-v1.0.2.yaml`.
- `grep "word-count-guardrail" "$REPO_ROOT/gateway/gateway-runtime/policy-engine/go.mod"` shows a `replace` line pointing at `../../dev-policies/word-count-guardrail`.

## Part B — Run the gateway stack locally

This follows the `gateway-debug` skill's Path B (Envoy in Docker, controller
+ policy-engine as local Go processes) — reused here because it's the only
way Envoy actually picks up freshly-compiled, uncommitted dev-policy source.

**Two separate, local-only config overlays are required — do not add these
settings to the shared `configs/config.toml`.** That file is also mounted
into the Docker container's own co-located controller/policy-engine (via
`env_file`/volume mounts), so anything you add there affects the container
too. Concretely:
- Adding `[policy_engine.server] mode = "tcp"` to the shared file breaks the
  **container's own internal policy-engine**, which expects its default UDS
  socket and fails its entrypoint health check within 10s, crash-looping the
  container.
- Skipping the controller-side overlay entirely (see below) is worse and
  silent: Envoy keeps talking to the **container's own internal
  policy-engine** over UDS instead of your local process. Every test still
  "works" — requests get rejected, response shapes look plausible — but
  you're exercising the old code baked into the Docker image, never your
  local changes. This was hit for real while building this suite: every
  guardrail response kept coming back in the default WSO2 shape long after
  the actual bug (the template-placeholder delimiter collision below) was
  fixed, because the request was never reaching the modified policy engine
  at all.

```bash
REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT/gateway"

# One-time provisioning if you haven't already (safe to re-run):
ADMIN_USERNAME=admin ADMIN_PASSWORD=admin ./scripts/setup.sh --force

# Point Envoy (in Docker) at the local controller process, and free the PE
# admin/metrics ports for the local policy-engine process:
#   gateway-runtime:
#     environment:
#       - GATEWAY_CONTROLLER_HOST=host.docker.internal
#     ports: comment out "9002:9002" and "9003:9003"
# Remember to `git checkout -- docker-compose.yaml` when done.

docker compose up -d gateway-runtime sample-backend

# Local-only overlay for the CONTROLLER: tells it to generate Envoy's ext_proc
# cluster config pointing at the HOST policy-engine over TCP, instead of the
# default same-container UDS socket (router.policy_engine.mode defaults to
# "uds", host to "policy-engine" -- see pkg/config/config.go). This is the
# critical, easy-to-miss piece -- see the callout above.
cp configs/config.toml /tmp/local-controller-config.toml
cat >> /tmp/local-controller-config.toml <<'EOF'

[router.policy_engine]
mode = "tcp"
host = "host.docker.internal"
port = 9001
EOF

# Local-only overlay for the POLICY ENGINE itself: switches its own ext_proc
# and ALS server modes to TCP so it can actually listen on a host port
# (default is also UDS, at a socket path that only makes sense inside a
# container).
cp configs/config.toml /tmp/local-policy-engine-config.toml
cat >> /tmp/local-policy-engine-config.toml <<'EOF'

[policy_engine.server]
mode = "tcp"
extproc_port = 9001

[collector.server]
mode = "tcp"
EOF

# Controller — note the literal `read` loop, NOT `source`: sourcing
# api-platform.env directly corrupts the bcrypt password hash (bash treats
# "$2y$10$..." as parameter expansion). See "Known gotchas" below.
cd gateway-controller
mkdir -p data/aesgcm-keys && cp aesgcm-keys/default-aesgcm256-v1.bin data/aesgcm-keys/
while IFS='=' read -r key value; do
  [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
  export "$key=$value"
done < "$REPO_ROOT/gateway/api-platform.env"
export APIP_GW_CONTROLLER_CONTROLPLANE_HOST="" APIP_GW_CONTROLLER_CONTROLPLANE_TOKEN=""
export APIP_GW_CONTROLLER_STORAGE_TYPE=sqlite
export APIP_GW_CONTROLLER_STORAGE_SQLITE_PATH="$REPO_ROOT/gateway/gateway-controller/data/gateway.db"
export APIP_GW_CONTROLLER_LLM_TEMPLATE__DEFINITIONS__PATH="$REPO_ROOT/gateway/gateway-controller/default-llm-provider-templates"
export APIP_GW_CONTROLLER_ROUTER_DOWNSTREAM__TLS_CERT__PATH="$REPO_ROOT/gateway/gateway-controller/listener-certs/default-listener.crt"
export APIP_GW_CONTROLLER_ROUTER_DOWNSTREAM__TLS_KEY__PATH="$REPO_ROOT/gateway/gateway-controller/listener-certs/default-listener.key"
export APIP_GW_ROUTER_DOWNSTREAM__TLS_CERT__PATH="$REPO_ROOT/gateway/gateway-controller/listener-certs/default-listener.crt"
export APIP_GW_ROUTER_DOWNSTREAM__TLS_KEY__PATH="$REPO_ROOT/gateway/gateway-controller/listener-certs/default-listener.key"
export APIP_GW_ROUTER_LUA_REQUEST__TRANSFORMATION_SCRIPT__PATH="$REPO_ROOT/gateway/gateway-controller/lua/request_transformation.lua"
# NOTE: single underscore between DEFINITIONS and PATH — see "Known gotchas".
export APIP_GW_CONTROLLER_POLICIES_DEFINITIONS_PATH="$REPO_ROOT/gateway/gateway-builder/target/output/gateway-controller/policies"
export APIP_GW_IMMUTABLE__GATEWAY_ENABLED=false
nohup go run ./cmd/controller -config /tmp/local-controller-config.toml > /tmp/local_controller.log 2>&1 &
disown

# Policy engine
cd "$REPO_ROOT/gateway/gateway-runtime/policy-engine"
nohup go run ./cmd/policy-engine -config /tmp/local-policy-engine-config.toml -xds-server localhost:18001 \
  > /tmp/local_policy_engine.log 2>&1 &
disown
```

Verify:
```bash
curl -s -o /dev/null -w "controller: %{http_code}\n" -u admin:admin http://localhost:9090/api/management/v1/rest-apis   # expect 200
curl -s -o /dev/null -w "policy-engine: %{http_code}\n" http://localhost:9002/health                                    # expect 200
```

## Part C — Run the e2e suite

```bash
cd "$REPO_ROOT/gateway/dev-policies/word-count-guardrail/e2e"
./run-e2e.sh
```

Or drive one vendor by hand:

```bash
curl -s -u admin:admin -X POST 'http://localhost:9090/api/management/v1/llm-providers' \
  -H 'Content-Type: application/yaml' --data-binary @- <<'EOF'
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: wcg-openai
spec:
  displayName: Word Count Guardrail Test (openai)
  version: v1.0
  template: openai
  context: /wcg-openai
  upstream:
    url: https://example.invalid
    auth:
      type: none
  policies:
    - name: api-key-auth
      version: v1
      paths:
        - path: /chat/completions
          methods: [POST]
          params: { key: Authorization, in: header, valuePrefix: "Bearer " }
    - name: word-count-guardrail
      version: v1
      paths:
        - path: /chat/completions
          methods: [POST]
          params:
            request: { enabled: true, min: 5, max: 1000, jsonPath: "$.messages[-1].content" }
  accessControl:
    mode: deny_all
    exceptions:
      - path: /chat/completions
        methods: [POST]
EOF

KEY=$(curl -s -u admin:admin -X POST 'http://localhost:9090/api/management/v1/llm-providers/wcg-openai/api-keys' \
  -H 'Content-Type: application/json' -d '{"name":"test-key"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["apiKey"]["apiKey"])')

curl -s -X POST http://localhost:8080/wcg-openai/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"hi"}]}' | python3 -m json.tool
```

Expect `finish_reason: "content_filter"` inside a normal chat-completion-shaped
body at HTTP 200 — not `{"type":"WORD_COUNT_GUARDRAIL",...}`.

## Part D — Clean up

```bash
REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT/gateway"
lsof -nP -iTCP:9090 -sTCP:LISTEN -t 2>/dev/null | xargs -r kill -9   # controller
lsof -nP -iTCP:9002 -sTCP:LISTEN -t 2>/dev/null | xargs -r kill -9   # policy-engine
git checkout -- docker-compose.yaml   # configs/config.toml was never touched -- see Part B
docker compose down -v --remove-orphans
rm -f gateway-controller/data/gateway.db*
rm -rf gateway-controller/data/aesgcm-keys
rm -f /tmp/local-controller-config.toml /tmp/local-policy-engine-config.toml
```

## Known gotchas (hit and fixed while building this suite)

- **Never `source` `api-platform.env` directly in bash.** It contains a
  bcrypt hash (`$2y$10$...`); `source`/plain shell evaluation treats `$2` as
  positional-parameter expansion and silently truncates the hash, producing a
  401 with no error logged anywhere. Read it literally with a `while IFS='='
  read -r key value; do export "$key=$value"; done` loop instead (`read`
  does not perform parameter expansion). Docker Compose's own `env_file:
  ... format: raw` avoids this by design — the bug only bites a bare `source`.
- **`APIP_GW_CONTROLLER_POLICIES_DEFINITIONS_PATH` has a single underscore**
  between `DEFINITIONS` and `PATH`, not the double underscore the
  `gateway-debug` skill's Step 3a block currently shows. Getting this wrong
  fails silently too: the controller starts, logs `"Policy definitions
  loaded" count=0`, and every `LlmProvider` registration referencing any
  policy then 400s with `"Required policy definition is missing"`.
- **The management API base path is `/api/management/v1`**, not the `v0.9`
  the `gateway-debug` skill's examples currently use.
- **The AES-GCM encryption key's default relative path** is
  `./data/aesgcm-keys/default-aesgcm256-v1.bin` (relative to the controller's
  CWD), not `./aesgcm-keys/...` — the Docker image's volume mount papers over
  this by mounting straight to `/app/data/aesgcm-keys/...`, but a local `go
  run` process needs the file physically copied to `data/aesgcm-keys/`
  first, or it fails to start with `"encryption key file not found"`.
- **A `guardrailResponse` field added only to the `LlmProviderTemplate` YAML
  files does nothing on its own.** The controller's loader
  (`pkg/utils/llm_provider_template_loader.go`) round-trips the parsed YAML
  through the strictly-typed, OpenAPI-generated `LLMProviderTemplateData`
  struct — any field not declared in `api/management-openapi.yaml` is
  silently dropped before the template ever reaches the lazy resource store
  the guardrail reads from at runtime. The guardrail's own live-lookup code
  needs zero changes for a new template field to work; the OpenAPI schema +
  `make generate-server-code` regen do.
- **`guardrailResponse.body`/`.streamingBody` use `<<`/`>>` delimiters, not
  Go text/template's default `{{`/`}}` — this is deliberate, not a stylistic
  choice.** The controller has its own, unrelated `{{ secret "handle" }}` /
  `{{ env "NAME" }}` templating pass (`pkg/templateengine/spec.go`) that
  recursively renders **any** spec string containing `"{{"`, with a `nil`
  data root, before the template is ever published to the lazy resource
  store. A `guardrailResponse.body` written with `{{.Message}}` gets silently
  swept into that pass and comes out empty — indistinguishable from
  "guardrailResponse not configured at all," so this fails with no error
  anywhere. `word-count-guardrail`'s `renderGuardrailResponseTemplate` uses
  `template.New(...).Delims("<<", ">>")` to keep its own templating fully
  independent of the controller's.
- **The single most time-consuming one: Envoy silently keeps talking to the
  Docker container's own co-located policy-engine unless you explicitly
  redirect it.** `router.policy_engine.mode` defaults to `"uds"` and `.host`
  to `"policy-engine"` (`pkg/config/config.go`) — controller-side settings
  that decide what ext_proc cluster config the controller generates for
  Envoy. Neither is tokenized in `configs/config.toml`, so an
  `APIP_GW_ROUTER_POLICY__ENGINE_MODE=tcp`-style env var (as `gateway-debug`
  suggests) has **no effect** — same silent-no-op failure mode as the two
  `{{ env }}`-token gaps above. Symptom: every test "works" (registration
  succeeds, requests get rejected with plausible-looking status codes) but
  none of your local `word-count-guardrail` source changes ever show up in
  the response, no matter how many times you rebuild and restart — because
  the request never reaches your process at all; it's served by the
  unmodified policy engine baked into the `gateway-runtime` image. The fix is
  the `[router.policy_engine]` block in the controller's local-only overlay
  config in Part B — there is no way to do this via environment variables
  with the current `config.toml`.

---
description: "Task list for the Model Failover Policy (Envoy-native retry, dispatch hop, transformer reuse)"
---

# Tasks: Model Failover Policy

**Input**: Design documents from `specs/001-model-failover-policy/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: Included. plan.md commits to Go unit tests for the policy, controller and SDK, plus godog integration tests for every scenario in quickstart.md.

**Organization**: Tasks are grouped by user story (spec.md US1–US6) so each story can be implemented and verified on its own.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependency on an unfinished task)
- **[Story]**: The user story the task belongs to (US1–US6)

## Implementation notes (2026-09-26 run)

- **Worktrees**: api-platform → `/Users/thenujan/Desktop/Git-Repos/api-platform-worktrees/model-failover` (branch `001-model-failover-policy`, from `upstream/main`). gateway-controllers → `/Users/thenujan/Desktop/Git-Repos/gateway-controllers-worktrees/model-failover-envoy-retry` (from `upstream/main`). All `api-platform/` and `gateway-controllers/` paths below resolve to these worktrees.
- **Design changes found while implementing** (recorded in research.md):
  - R5: `retry_on: retriable-headers,connect-failure,reset`, with no status masking.
  - R6: the hop secret is moved to dynamic metadata on the client-facing listener, so it is never forwarded to providers.
  - R11: the policy is detected by name.
  - R13: fail-closed is already the engine's behaviour.
  - The dispatch route is emitted as an ordinary extra Operation (header match) by `transformProxy`, and the xDS translator relocates it to the internal listener.
- **Controller files**: `pkg/failover/` (new), `pkg/utils/llm_failover.go`, `pkg/transform/llm_failover.go`, `pkg/xds/failover_listener.go`, `pkg/config/llm_validator_failover.go`, plus hooks in `llm_transformer.go`, `transform/llm.go`, `xds/translator.go`, `config/config.go`, `models/runtime_deploy_config.go`.
- **Do not commit** the `filePath: ./dev-policies/model-failover` line in `gateway/build.yaml` as-is (`dev-policies/` is gitignored). T060 switches it to a `gomodule:` entry.

## Path Conventions

Three roots. Paths are relative to each root unless stated otherwise:

| Prefix | Root |
|---|---|
| `api-platform/` | `/Users/thenujan/Desktop/Git-Repos/api-platform` (this repo) |
| `gateway-controllers/` | `/Users/thenujan/Desktop/Git-Repos/gateway-controllers` (separate repo; the user commits there in parallel, so check `git status` before editing) |
| `GC/` | shorthand for `api-platform/gateway/gateway-controller/` |
| `PE/` | shorthand for `api-platform/gateway/gateway-runtime/policy-engine/` |
| `POL/` | shorthand for `gateway-controllers/policies/model-failover/` |

**Build rules**:
- The build order is sdk/core, then the policy, then gateway-runtime, then gateway-controller. The controller image copies the runtime's pre-compiled policy output.
- Do **not** run docker builds or restarts yourself. Give the user the exact commands (a user preference).
- `api-platform/gateway/dev-policies/` is gitignored. It is a local build mirror only; the source of truth is `POL/`.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Scaffolding and the verification spikes that lock in the Envoy mechanics (research.md S1–S5).

- [X] T001 Create the policy module skeleton in `POL/`:
  - `go.mod` with module `github.com/wso2/gateway-controllers/policies/model-failover`. Pin `github.com/wso2/api-platform/sdk/core` to the version the round-robin policies use (v0.3.4), unless T010's SDK metrics tag forces a bump.
  - `policy-definition.yaml`, copied verbatim from `api-platform/specs/001-model-failover-policy/contracts/policy-definition.yaml`.
  - An empty `model_failover.go` with package `modelfailover` and a `GetPolicy` stub.
- [X] T002 [P] Mirror `POL/` into `api-platform/gateway/dev-policies/model-failover/`. Add a `filePath: ./dev-policies/model-failover` entry named `model-failover` to `api-platform/gateway/build.yaml`, in alphabetical order after `model-weighted-round-robin`.
- [X] T003 [P] Create a scriptable mock LLM provider in `api-platform/tests/mock-servers/mock-llm-provider/` (Go, with a Dockerfile following the `mock-interceptor-service` layout).
  - Serves OpenAI `POST */chat/completions` (non-streaming and SSE) and Anthropic `POST */v1/messages` (non-streaming and SSE).
  - A control API: `PUT /__mode` with body `{"mode":"ok"|"status:<code>"|"hang:<seconds>"|"reset"|"stream-abort:<chunks>"}`, `GET /__requests` returning the received count plus the last body and headers, and `DELETE /__requests`.
  - The `refuse` mode is done by stopping the container, so it isn't a server mode.
- [X] T004 [P] Add three services to `api-platform/gateway/it/docker-compose.test.yaml`, all built from `../../tests/mock-servers/mock-llm-provider`: `mock-llm-openai-a`, `mock-llm-openai-b` and `mock-llm-anthropic`, with container names `it-mock-llm-*`.
- [X] T005 Spike S1 (research.md R2), in `api-platform/specs/001-model-failover-policy/spikes/s1-internal-listener.yaml` plus a findings note appended to research.md:
  - Hand-write an Envoy v1.39 config with the `envoy.bootstrap.internal_listener` bootstrap extension, an internal listener whose HCM runs ext_proc, Lua and the router, and an `internal_upstream` cluster pointing at it.
  - Confirm that a request routed front → internal listener → loopback reaches ext_proc on both hops.
  - If it fails, record the decision to use the fallback: a secret-header-gated dispatch route on the main listener.
- [X] T006 Spike S2 (research.md R5) in `.../spikes/`. **Result**: `retry_on: retriable-headers,connect-failure,reset` with `retriable_headers: [x-wso2-failover-retry present]` and `per_try_timeout` works:
  - a tagged response retries, and an untagged one (including 500/502) does not;
  - `reset` retries a per-try timeout;
  - the per-try timer stops once headers arrive (a 3s stream is not cut by a 1s `per_try_timeout`).
  `gateway-error` and status masking are **not** needed (research.md R5, revised).
- [X] T007 [P] Spike S3 (research.md R6) in `.../spikes/s3-local-reply-mapper.yaml`. Confirm that a `local_reply_config` mapper with AND(`response_flag_filter` {UF,URX,UH,UO,UT,UC,DC,LR}, `header_filter` on request header `x-wso2-failover-hop` equal to the secret) adds `x-wso2-upstream-failure: %RESPONSE_FLAGS%` only on matching requests. Record the results in research.md.
- [X] T008 [P] Spike S4 (research.md R12) in `.../spikes/s4-buffer-limit.yaml`. Confirm that a request body larger than `request_body_buffer_limit` is still forwarded once with retries turned off (`retry_or_shadow_abandoned`) and is not rejected. Record the results in research.md.
- [X] T009 Spike S5 (research.md S5): read `PE/internal/kernel/translator.go` (`applyDefaultUpstream` and related code, around :162-200 and :383-398) and `api-platform/gateway/gateway-controller/lua/request_transformation.lua`. Determine whether a route with a fixed `cluster` (no `cluster_header`) still gets a base-path `:path` rewrite. Record the answer in research.md, and if a rewrite happens, record which fallback T020 must apply.

**Checkpoint**: Spikes recorded. Where a fallback was chosen, the later tasks that name that decision use the fallback.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The front hop → dispatch hop pipeline, end to end, with no failover semantics yet. A request through a failover-enabled proxy reaches the primary target via both hops.

**⚠️ CRITICAL**: No user story work can begin until this phase is complete.

### SDK and policy engine

- [X] T010 [P] Add package `api-platform/sdk/core/metrics/metrics.go`:
  - `SetRegisterer(prometheus.Registerer)`, called once by the engine.
  - `Registerer() prometheus.Registerer`, which returns a no-op registerer before `SetRegisterer` is called.
  - `MustRegisterOnce(collectors ...prometheus.Collector)`, which is idempotent per collector name so that re-creating a policy instance on config reload doesn't panic.
  - Tests in `api-platform/sdk/core/metrics/metrics_test.go`: no-op before set, idempotent registration, concurrent safety.
- [ ] T011 **(Blocked: needs an `sdk/core` tag containing `metrics`. The Docker build resolves sdk/core v0.4.1 from go.mod, while go.work masks this locally; see KB `gateway-go-work-masks-sdk-core-replace-gap`. Implement as a `metrics.Provider` backed by the engine registry.)** Wire the engine's private registry (`PE/internal/metrics/metrics.go:528-605`) into `sdkmetrics.SetRegisterer` in `PE/cmd/policy-engine/main.go`, next to the existing `utils.SharedHTTPClient` initialisation (:196). Add a test in `PE/internal/metrics/` asserting that a collector registered through the SDK appears on the metrics handler output.
- [X] T012 Make chain-build failure fail closed for failover routes (research.md R13):
  - In `PE/internal/xdsclient/handler.go:234` (currently "Failed to build policy chain for route, skipping"), when the route's metadata carries `wso2.route/failover` = `front` or `dispatch`, register a sentinel chain that answers every request with the 503 body from `contracts/exhaustion-response.md`, instead of skipping.
  - Plumb the route metadata key through `GC/pkg/policyxds/snapshot.go` if it isn't already sent in `RouteMetadata`.
  - Test in `PE/internal/xdsclient/handler_test.go`.

### Policy module core (`POL/`)

- [X] T013 [P] Implement config parsing in `POL/config.go`, with tests in `POL/config_test.go`:
  - Parse authored params into `FailoverPolicyConfig` with the exact defaults and bounds from data-model.md §1: `targets` 1–10, no duplicate `(provider, model)`; `failoverOn.statusCodes` default `[429,500,502,503,504]`, each `429` or `500`–`599`, unique; `connectFailure`/`reset`/`timeout` default `true`; `perAttemptTimeout` default `30s`, `1s`–`300s`; `suspendAfterConsecutiveFailures` default `3`, `1`–`100`; `suspendDuration` default `30s`, `1s`–`3600s`; `probeConcurrency` default `1`, `1`–`10`; `recoverAfterSuccessfulProbes` default `2`, `1`–`20`.
  - Fall back to the schema default when a key is omitted, and tell omission apart from an explicit empty value by checking whether the key is present in the map (KB `decoupled-retry-source-task10-schema-default-not-materialized`).
  - Parse the controller-internal keys `_role` (`front`|`dispatch`), `_chainId`, `_targetIds`, `_targetBasePaths`, `_hopSecret` (research.md R10). `GetPolicy` returns an error if `_role` or `_chainId` is missing.
- [X] T014 [P] Implement the attempt-plan registry in `POL/plan.go`, with tests in `POL/plan_test.go` covering concurrent advance, expiry, and release on expiry. Per data-model.md §2:
  - `nonce` is 128-bit `crypto/rand` hex.
  - `cursor` is advanced atomically via `Advance(nonce, chainId) (targetIndex, ok)`.
  - `outcomes map[index]Outcome` is guarded by a mutex.
  - `expiresAt` is `front route timeout + 5s`.
  - A background sweeper runs every 1s, removes expired plans and calls a release hook for `probeClaims`.
  - The registry is package-level and shared by all instances in the process.
- [X] T015 Implement `GetPolicy` and a role-aware `Mode()` in `POL/model_failover.go`:
  - **front**: request headers PROCESS, request body SKIP, response headers PROCESS, response body SKIP.
  - **dispatch**: request headers PROCESS, request body BUFFER (needed to rewrite `model` for OpenAI-native targets), response headers PROCESS, response body SKIP.
  - Add stub hooks that route to `front.go` and `dispatch.go`.

### Front and dispatch plumbing, no failover semantics yet

- [X] T016 Implement the minimal front role in `POL/front.go`, `OnRequestHeaders`:
  1. Build an attempt plan containing all configured targets in order (health filtering comes in US4).
  2. Store it in the registry.
  3. Set request header `x-wso2-failover-plan: <nonce>`.
  4. Save the nonce in `SharedContext.Metadata["model_failover_nonce"]`.

  In `OnResponseHeaders`, close the plan.
- [X] T017 Implement the minimal dispatch role in `POL/dispatch.go`, `OnRequestHeaders`:
  1. Read and remove `x-wso2-failover-plan`, then call `Advance(nonce, _chainId)`.
  2. If the nonce is unknown or expired, or the chain doesn't match, return `ImmediateResponse` 500 with a generic OpenAI-format error body, and log `model_failover.plan_rejected` (fields per contracts/observability.md).
  3. Otherwise set `mods.UpstreamName = "failover-" + targetId` and `Metadata["selected_provider"] = "failover-" + targetId`.
  4. Set `x-wso2-failover-hop: <secret>` (the secret comes from the `_hopSecret` internal param).
- [X] T018 Add a dispatch-role `OnRequestBody` in `POL/dispatch.go`: for an OpenAI-native target, replace the JSON body's `model` with the target's `model` and leave every other field byte-preserved. Transformer targets are left alone, because their transformer pins the model.

### Controller: parse, validate, generate

- [X] T019 Add `GC/pkg/utils/llm_failover.go` with tests in `llm_failover_test.go`:
  - Detect a policy whose definition carries `x-wso2-envoy-retry-chain: true` (research.md R11).
  - Parse its params into a controller-side `FailoverChain` with fields `chainId = <proxyId>:<routeKey>`, `targetId = t<index>`, and each target's resolved LlmProvider and base path.
  - Map a provider template to the transformer policy name, reusing the existing template→transformer mapping in `GC/pkg/utils/llm_transformer.go` (:961-1005): openai → none; azure-openai, anthropic, awsbedrock, gemini, mistral → `openai-to-<x>-transformer`.
- [X] T020 Split the LlmProxy chain in `GC/pkg/utils/llm_transformer.go`, `transformProxy`. When a route has a retry-chain policy:
  - **Front chain**: user policies plus `model-failover` with `_role=front`.
  - **Dispatch chain**: `model-failover` with `_role=dispatch`, then one transformer per non-OpenAI target (`providerId = failover-t<k>`, `model = target.model`), then one loopback-auth policy per target, gated by CEL `request.Metadata['selected_provider'] == 'failover-t<k>'` (the gate is required for **every** target, including t0).
  - Per-target loopback upstream definitions named `failover-t<k>` (research.md R14; this also covers the primary, whose own cluster has an empty name).
  - Append the synthesized dispatch-role instance before the Phase-3 provider loops (KB `gateway-controller-llm-policy-attachment-two-stage-split`).
  - Deep-copy each `Params` map.
  - Remove the proxy's default provider mechanics from the front chain.
  - Front route: see T023, which has no `RegexRewrite` (S5 result).
- [X] T021 In `GC/pkg/transform/` (where route keys first exist, per the same KB), merge the internal keys `_chainId`, `_targetIds`, `_targetBasePaths` and `_hopSecret` into both instances' `Params`, key by key and never by replacement (preserve `attachedTo`). Add route metadata `wso2.route/failover: front|dispatch`.
- [X] T022 Add gateway config in `GC/pkg/config/config.go`, with a test in `config_test.go`:
  - `router.failover.max_request_body_bytes`, default `4194304`, validated `> 0`.
  - `router.failover.internal_listener_name`, default `failover_dispatch`.
  - A per-boot random hop secret: 32 bytes from `crypto/rand`, hex-encoded, never logged.
- [X] T023 Generate the front route in `GC/pkg/xds/translator.go` for routes marked `wso2.route/failover=front`. Follow `contracts/internal-hop-headers.md` § Front route exactly:
  - Fixed `cluster: failover_dispatch`, with no `cluster_header` and **no `RegexRewrite`**; `x-target-upstream` is in `request_headers_to_remove` (S5 result, research.md).
  - `timeout = max(configured, n × perAttemptTimeout + 2s)`.
  - `retry_policy` with `num_retries = n-1`, `retry_on: retriable-headers,connect-failure,reset` (omit `reset` when `failoverOn.timeout=false`), `per_try_timeout`, `retriable_headers`, and `retry_back_off` with base 1ms and max 10ms.
  - `request_body_buffer_limit` from config.
  - `request_headers_to_remove` for any client-supplied `x-wso2-failover-*` headers.
  - `request_headers_to_add` for `x-wso2-failover-chain`.
  - `response_headers_to_remove` for the four internal response headers.

  Add snapshot tests in `GC/pkg/xds/translator_failover_test.go`.
- [X] T024 Add `GC/pkg/xds/failover_listener.go` with tests in `failover_listener_test.go`. It generates:
  - The `failover_dispatch` internal listener. Its HCM has the same ext_proc and Lua filters as the main listener, plus `NormalizePath: true`, `MergeSlashes: true` and `PathWithEscapedSlashesAction: UNESCAPE_AND_REDIRECT` (xDS rule directive 6).
  - One dispatch route per chain. It clones the proxy route's `RegexRewrite` and `cluster_header` generation (S5 result), matching `prefix: "/"` plus header `x-wso2-failover-chain == <chainId>`, with `cluster_header: x-target-upstream`, route timeout `0s`, the idle timeout kept, and route metadata `wso2.route/failover=dispatch`.
  - The `failover_dispatch` cluster, with an `internal_upstream` transport socket and `circuit_breakers.thresholds[0].max_retries: 1024`.

  Use the S1 fallback if T005 chose it.
- [X] T025 Register the internal listener and cluster in the xDS snapshot build (the caller of the listener and cluster generators in `GC/pkg/xds/`). Emit them only when at least one failover chain exists. Test: no failover chains produces no internal listener.
- [X] T026 [P] Add the `envoy.bootstrap.internal_listener` bootstrap extension to `api-platform/gateway/gateway-runtime/router/config/envoy-bootstrap.yaml`.

**Checkpoint**: With a `model-failover` chain of one target (openai-a), a request through the proxy returns 200 from openai-a through front → dispatch → provider hop. Verify manually from quickstart.md scenario 1; build commands are handed to the user.

---

## Phase 3: User Story 1 — Ordered fallback when the primary fails (Priority: P1) 🎯 MVP

**Goal**: An eligible status or a transport failure on target k sends the request to target k+1. Non-eligible responses pass through unchanged.

**Independent Test**: quickstart.md scenarios 1, 2, 3, 7, 14 and 15. Mock A returns 429, mock B returns 200, and the client gets B's 200 with no internal headers.

### Tests for User Story 1

- [X] T027 [P] [US1] Unit-test response classification in `POL/dispatch_test.go`. The status is eligible if it is in `failoverOn.statusCodes`; transport reasons come from `x-wso2-upstream-failure` flags (UF/URX/UH/LR → `connect_failure`, UC/DC → `reset`, UT/UO → `timeout`), each subject to its toggle. Cover:
  - an eligible failure gets tagged `x-wso2-failover-retry: <reason>`;
  - a non-eligible 502/503/504 passes through untagged and unchanged;
  - 2xx and 4xx are untouched;
  - `x-wso2-upstream-failure` is always removed.
- [X] T028 [P] [US1] Unit-test the front-role header hygiene in `POL/front_test.go`: every internal `x-wso2-failover-*` and `x-wso2-upstream-failure` header is removed from the client response, and non-eligible statuses (for example 503 when not configured) reach the client unchanged.
- [X] T029 [P] [US1] Add `api-platform/gateway/it/features/model-failover.feature` with scenarios 1, 2, 3, 7 (`reset` mode, and `refuse` by stopping `mock-llm-openai-a`), 14 and 15 from quickstart.md. Add step definitions in `api-platform/gateway/it/steps_model_failover.go` to set mock modes, read mock request counts, and assert that no `x-wso2-failover-*` header reaches the client.

### Implementation for User Story 1

- [X] T030 [US1] Implement dispatch `OnResponseHeaders` classification in `POL/dispatch.go` exactly as tested in T027. Record the attempt outcome (`success` / `non_eligible` / `eligible_failure` + reason) into the plan's `outcomes[index]`.
- [X] T031 [US1] Implement front `OnResponseHeaders` internal-header stripping in `POL/front.go`, as tested in T028. The front route's `response_headers_to_remove` (T023) is a second layer of defence, not a replacement.
- [X] T032 [US1] Add the failover local-reply mapper to the main listener's HCM in `GC/pkg/xds/translator.go`: AND(response flags {UF,URX,UH,UO,UT,UC,DC,LR}, request header `x-wso2-failover-hop` equal to the per-boot secret), adding header `x-wso2-upstream-failure: %RESPONSE_FLAGS%`.
  - Place it **first** among mappers.
  - If the OpenAI error-format mapper (`pkg/xds/llm_errors.go`, KB `ai-gateway-openai-error-format-llm-routes`) exists on the branch when this task runs, give this mapper the same OpenAI `body_format`.
  - Snapshot test in `GC/pkg/xds/translator_failover_test.go`.
- [X] T033 [US1] Add synchronous registration-time validation in `GC/pkg/config/llm_validator.go`, with tests in `llm_validator_failover_test.go`, returning 400 with a field path per error. Checks:
  - T013's bounds;
  - each `targets[].provider` is the proxy's `provider` or in `additionalProviders`;
  - the provider template has a supported conversion (FR-005c);
  - no authored `_`-prefixed keys;
  - `model-failover` is attached at most once per route;
  - it is mutually exclusive with `llm-header-router`, `model-round-robin`, `model-weighted-round-robin`, `intelligent-model-routing` and `cost-based-model-routing`, by extending the existing `validatePolicyListExclusivity` (:520).
- [ ] T034 [US1] Run the US1 IT scenarios. Hand the user the build and run commands (`IT_FEATURE_PATHS=features/model-failover.feature make test` in `api-platform/gateway/it`), then fix failures.

**Checkpoint**: MVP. Ordered failover works for OpenAI-format targets on status and transport failures.

---

## Phase 4: User Story 2 — Same-model regional failover (Priority: P1)

**Goal**: The same model on two provider configurations. Each uses its own credentials, and health is isolated per target.

**Independent Test**: quickstart.md scenario 2 with the assertion that `mock-llm-openai-b` received openai-b's credential and never openai-a's.

- [X] T035 [P] [US2] Controller test in `GC/pkg/utils/llm_failover_test.go`: for targets `(openai-a, gpt-4o)` and `(openai-b, gpt-4o)`, assert two distinct upstream definitions and two loopback-auth policies, each CEL-gated on its own `failover-t<k>`, and that neither gate matches when `selected_provider` is absent.
- [X] T036 [P] [US2] Add a regional scenario to `api-platform/gateway/it/features/model-failover.feature`: A=`status:503`, B=`ok`. Assert B's recorded `Authorization`/api-key header equals openai-b's configured key, via `GET /__requests` on the mock.
- [X] T037 [US2] Fix any credential cross-over found by T035 or T036 in the dispatch-chain construction in `GC/pkg/utils/llm_transformer.go` (FR-004). Credentials must never come from the primary's ungated condition (KB `failover-aggregate-cluster-upstreamname-two-hidden-couplings`, third coupling).

**Checkpoint**: Regional failover is verified, and credentials are isolated per target.

---

## Phase 5: User Story 3 — Cross-provider failover (Priority: P2)

**Goal**: A fallback on a different provider format (Anthropic, Azure OpenAI, Bedrock, Gemini, Mistral), with two-way conversion including streaming, using the existing transformers unchanged.

**Independent Test**: quickstart.md scenarios 4 and 5.

- [X] T038 [P] [US3] Controller test in `GC/pkg/utils/llm_failover_test.go`: for each supported template, the dispatch chain contains the matching `openai-to-<x>-transformer` with `providerId=failover-t<k>` and `model=<target.model>`, ordered after `model-failover[dispatch]` and before loopback auth. An unsupported template is rejected by T033's validation.
- [X] T039 [P] [US3] Add scenarios 4 (non-streaming) and 5 (`"stream": true`) to `api-platform/gateway/it/features/model-failover.feature`. Assert:
  - an OpenAI ChatCompletion shape, and OpenAI SSE ending in `data: [DONE]`;
  - the Anthropic mock received an Anthropic Messages body (`/v1/messages`, `anthropic-version` header) with the Anthropic credential only.
- [ ] T040 **(Needs your live run: IT scenarios "An Anthropic fallback…" and "A streamed Anthropic fallback…" exercise it.)** [US3] Verify the transformers run correctly as dispatch-hop members. Specifically, check that `UpstreamName = providerId` resolves to the per-target upstream `failover-t<k>` and that the base-path rewrite composes with the transformer `Path` (for example `/<anthropic-context>/v1/messages`). Fix any mismatch in `GC/pkg/utils/llm_transformer.go` or `GC/pkg/transform/`. Do **not** fork transformer code (FR-005d). If a transformer change is unavoidable, make it in `gateway-controllers/policies/openai-to-<x>-transformer/` and mirror it into `api-platform/gateway/dev-policies/`.
- [X] T041 **(Controller side done: Bedrock, Gemini, Azure and Mistral transformers land on the dispatch route per target, `TestTransformProxy_FailoverUsesEachAttachmentsOwnTransformer`. A live Bedrock or Gemini fallback isn't in the IT because there's no mock for those formats yet.)** [US3] Confirm that path-located templates (Bedrock `/model/{id}/converse`, Gemini `/{v}/models/{m}:generateContent`) work as fallbacks. Add Bedrock and Gemini cases to T038's tests and a Gemini mock mode to `api-platform/tests/mock-servers/mock-llm-provider/` if needed.

**Checkpoint**: Cross-provider failover works with conversion both ways, including streaming.

---

## Phase 6: User Story 4 — Suspension and probe-limited recovery (Priority: P2)

**Goal**: Suspend a target after N consecutive failures, skip it while suspended, then recover it only after M successful probes.

**Independent Test**: quickstart.md scenarios 9, 10, 11 and 13.

### Tests for User Story 4

- [X] T042 [P] [US4] Unit-test the state machine in `POL/health_test.go`, exactly per data-model.md §3 transitions:
  - healthy → suspended at the Nth consecutive `eligible_failure`;
  - `success` or `non_eligible` resets `consecutiveFailures`;
  - suspended → probing when `now >= suspendedUntil`, evaluated at plan build;
  - probing → healthy after M consecutive probe successes;
  - probing → suspended on any probe failure (with a new `suspendDuration`);
  - `probesInFlight` never exceeds `probeConcurrency`;
  - a config revision resets only targets whose `(provider, model)` changed.
  Use an injectable clock.
- [X] T043 [P] [US4] Unit-test plan building in `POL/front_test.go`:
  - suspended targets are excluded;
  - a probing target is included only if it claimed a probe slot;
  - an empty plan returns `ImmediateResponse` 503 with the exact body from `contracts/exhaustion-response.md`, and no nonce is created;
  - claims are released on outcome and on plan expiry.

### Implementation for User Story 4

- [X] T044 [US4] Implement `POL/health.go`: a package-level registry keyed by `(chainId, targetId)` holding `TargetHealth{state, consecutiveFailures, suspendedUntil, probeSuccesses, probesInFlight}` (data-model.md §3), guarded by a mutex per entry, with an injectable clock.
- [X] T045 [US4] Integrate health into the front plan build (`POL/front.go`) and outcome recording (`POL/dispatch.go` via T030). Wire the plan-expiry release hook from T014.
- [X] T046 [US4] Add scenarios 9, 10, 11 and 13 to `api-platform/gateway/it/features/model-failover.feature`, using `suspendDuration: 5s` as in quickstart.md.

**Checkpoint**: Unhealthy targets cost no latency, and recovery is gated by probes.

---

## Phase 7: User Story 5 — Bounded, time-boxed attempts (Priority: P2)

**Goal**: The per-attempt timeout abandons a slow target and moves on. Each target is tried at most once, suspended targets are skipped, and there is no failover once the response has started streaming.

**Independent Test**: quickstart.md scenarios 6 and 12, plus chain-skip ordering.

- [X] T047 [P] [US5] Unit-test timeout attribution in `POL/dispatch_test.go` and `POL/front_test.go` (research.md R7):
  - when attempt k+1 arrives and `outcomes[k]` is empty, record `k` as `eligible_failure`/`timeout`;
  - when the front sees a final 504 carrying `x-wso2-upstream-failure` flag `UT` or Envoy's per-try-timeout local reply, record the last attempted target as `timeout`;
  - a plan that expires with no outcome and no later attempt records nothing (client disconnect).
- [X] T048 [US5] Implement the timeout attribution from T047 in `POL/dispatch.go` and `POL/front.go`.
- [X] T049 [P] [US5] Add to `api-platform/gateway/it/features/model-failover.feature`:
  - scenario 6 (A=`hang:10`, `perAttemptTimeout: 2s`; served by B in under 2.1s plus B's latency);
  - scenario 12 (A=`stream-abort:2`; B receives 0 requests);
  - a 3-target chain where t1 is suspended and t0 fails: t2 serves, and t1's mock count is unchanged;
  - an all-fail 3-target chain: each mock count is exactly 1 (SC-004).
- [ ] T050 **(Open: Envoy's behaviour is verified by spike S4 on a single hop. An end-to-end IT needs a decision first on how `router.http_listener.per_connection_buffer_limit_bytes` (1 MiB default) interacts with the 4 MiB retry buffer and the dispatch hop's buffered ext_proc body.)** [US5] Add a buffer-limit IT scenario in the same feature file: a body larger than `router.failover.max_request_body_bytes` is sent to the primary only. Apply the S4 outcome from T008 (either this, or 413 if the S4 fallback was chosen).

**Checkpoint**: Attempts are bounded and time-boxed, and streaming is safe.

---

## Phase 8: User Story 6 — Exhaustion response and observability (Priority: P3)

**Goal**: A fixed 503 exhaustion response, plus the metrics and logs in contracts/observability.md.

**Independent Test**: quickstart.md scenario 8 and the § Verify observability checks.

- [X] T051 [P] [US6] Unit-test exhaustion in `POL/front_test.go`: a final response that carries `x-wso2-failover-retry`, or `x-wso2-failover-exhausted: true`, or is a per-try-timeout 504 (when `failoverOn.timeout=true`), is replaced by `ImmediateResponse` 503. The body must be byte-identical to `contracts/exhaustion-response.md` and contain no internal headers.
- [X] T052 [US6] Implement exhaustion replacement in `POL/front.go`, and `x-wso2-failover-exhausted` (cursor past end of plan: status 500, untagged) in `POL/dispatch.go`.
- [ ] T053 **(Blocked with T011: needs the `sdk/core` tag containing `metrics`.)** [P] [US6] Implement `POL/metrics.go` with exactly the seven metrics in `contracts/observability.md` (names, types, labels, bounded label values), registered through `sdkmetrics.MustRegisterOnce`. Emit them from `front.go`, `dispatch.go` and `health.go`. Tests in `POL/metrics_test.go` use a test registry.
- [X] T054 [P] [US6] Implement the structured `slog` events in `contracts/observability.md` (`model_failover.attempt_failed`, `.served`, `.exhausted`, `.target_suspended`, `.target_probing`, `.target_recovered`, `.plan_rejected`) with exactly the listed fields. Test in `POL/logging_test.go` that no header values, bodies or `_hopSecret` are ever logged (FR-021).
- [X] T055 **(Metrics scrape step deferred with T053.)** [US6] Add to `api-platform/gateway/it/features/model-failover.feature`: scenario 8 (exact body match), and a metrics assertion step that scrapes the policy-engine metrics endpoint for `wso2_model_failover_attempts_total` and `wso2_model_failover_exhausted_total`.

**Checkpoint**: All user stories are complete.

---

## Phase 9: Polish & Cross-Cutting Concerns

- [X] T056 [P] Write policy docs at `gateway-controllers/policies/model-failover/README.md` (or wherever the repo's policy docs live; check the existing transformer docs under `gateway-controllers/docs`). Cover:
  - the parameters table from data-model.md §1;
  - supported target formats (FR-005b);
  - the OpenAI-format client requirement;
  - known limits: bodies over the buffer limit are tried once; health is per gateway instance; no failover after streaming starts.
- [X] T057 [P] Document `router.failover.max_request_body_bytes` in the gateway configuration reference (find it via `grep -rn "per_connection_buffer_limit" api-platform/gateway/configs api-platform/docs`).
- [X] T058 **(Policy CPU: ~2.5 µs and 65 allocations per healthy request, `BenchmarkHealthyPrimaryRequest`. The extra internal-listener hop and ext_proc exchange need a live measurement.)** [P] Add a benchmark for the healthy-primary path overhead in `PE/internal/kernel/` or `POL/`, targeting under 5 ms median added (plan.md Performance Goals). Record the result in research.md.
- [ ] T059 **(Code and test checks done in `checklists/security.md`; three live checks remain.)** Security pass against `.claude/rules/`:
  - client-supplied `x-wso2-failover-*` headers are stripped on the front route;
  - the internal listener is unreachable from the host (`curl` against every published port shows no dispatch route);
  - the hop secret is never logged;
  - the exhaustion body has no upstream detail;
  - retry settings are all bounded.
  Record the results in `api-platform/specs/001-model-failover-policy/checklists/security.md`.
- [ ] T060 Release path: after `POL/` is tagged in `gateway-controllers`, switch the `api-platform/gateway/build.yaml` entry from `filePath:` to `gomodule: github.com/wso2/gateway-controllers/policies/model-failover@v0`. Bump the sdk/core requirement in `POL/go.mod` to the tag that contains `sdk/core/metrics` (KB `gateway-go-work-masks-sdk-core-replace-gap`: verify without a `go.work` replace).
- [ ] T061 Run the full quickstart.md scenario table (1–15) and the unit suites (`go test ./...` in `POL/`, `GC/`, `PE/` and `api-platform/sdk/core`), handing the build commands to the user, and record pass/fail per scenario in `api-platform/specs/001-model-failover-policy/checklists/verification.md`.

---

## Dependencies & Execution Order

### Phase dependencies

- **Setup (Phase 1)**: none. T005–T009 (spikes) must finish before the tasks that consume their decisions (T020, T023, T024, T032, T050).
- **Foundational (Phase 2)**: depends on Phase 1 and blocks every story.
  - T010 → T011.
  - T013, T014 → T015 → T016, T017 → T018.
  - T019 → T020 → T021.
  - T022 → T023, T024 → T025.
  - T012 depends on T021 (route metadata).
- **US1 (Phase 3)**: depends on Phase 2.
- **US2 (Phase 4)**: depends on Phase 2. Independent of US1's code, but reuses US1's IT steps (T029).
- **US3 (Phase 5)**: depends on Phase 2 and T033 (template validation).
- **US4 (Phase 6)**: depends on US1's T030 (outcome recording).
- **US5 (Phase 7)**: depends on US1's T030 and T031. T049's suspended-skip case depends on US4.
- **US6 (Phase 8)**: T051–T052 depend on US1. T053–T054 depend on T010–T011 (SDK metrics) and on US4 for health events.
- **Polish (Phase 9)**: after the desired stories.

### User story completion order

```text
Setup ─► Foundational ─► US1 (MVP) ─┬─► US2
                                    ├─► US3
                                    ├─► US4 ─► US5 (suspended-skip case)
                                    └─► US6 (metrics need US4 for health events)
```

### Parallel opportunities

- **Phase 1**: T002, T003, T004, T007 and T008 in parallel (T005, T006 and T009 are sequential spikes by a single owner).
- **Phase 2**: T010, T013, T014 and T026 in parallel. Controller T019–T025 and policy T015–T018 proceed as two parallel tracks.
- **US1**: T027, T028 and T029 in parallel; then T030 and T031 (different files) in parallel; T032 and T033 in parallel (different files).
- **After US1**: US2, US3 and US4 can be staffed in parallel.
- **US6**: T053 and T054 in parallel.

### Parallel example: User Story 1

```text
Task: "T027 [P] [US1] Unit-test response classification in POL/dispatch_test.go"
Task: "T028 [P] [US1] Unit-test front status restoration in POL/front_test.go"
Task: "T029 [P] [US1] Add features/model-failover.feature scenarios 1,2,3,7,14,15 + steps_model_failover.go"
```

---

## Implementation Strategy

### MVP first (User Story 1 only)

1. Phase 1 (spikes decide internal listener vs fallback, and retry-trigger semantics).
2. Phase 2 (two-hop pipeline, one target).
3. Phase 3 / US1: ordered failover on status and transport failures for OpenAI-format targets.
4. **Stop and validate**: quickstart.md scenarios 1, 2, 3, 7, 14, 15.

### Incremental delivery

1. US1 is the MVP.
2. US2 adds regional credential isolation (mostly verification).
3. US3 adds cross-provider conversion (transformer reuse).
4. US4 adds suspension and probing (the latency win at scale).
5. US5 adds timeouts, bounded attempts and streaming safety.
6. US6 adds the exhaustion contract and metrics.

### Notes

- Never resolve a gap with a `// TODO`/`FIXME` (repo rules). Fix it, or raise it.
- The user commits to `gateway-controllers` in parallel. Re-check `git status` there before editing and after long pauses.
- Commit after each task or logical group. No Co-Authored-By trailer on commits (a user preference).

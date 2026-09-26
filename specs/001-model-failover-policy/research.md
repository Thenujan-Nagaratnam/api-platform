# Phase 0 Research: Model Failover Policy

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-09-26

Direction from the user: use **Envoy's native retry policy**, reuse the existing **transformer policies**, and feel free to change the existing architecture.

## Baseline: what exists today (verified in code)

| Area | Current state | Reference |
|---|---|---|
| Envoy | v1.39.0 | `gateway/gateway-runtime/Dockerfile:23` |
| Retry config | None. No `retry_policy`, `per_try_timeout`, aggregate cluster, outlier detection, upstream HTTP filters, or `local_reply_config` is generated. | `gateway/gateway-controller/pkg/xds/translator.go` (routes set only `Timeout`/`IdleTimeout`, :1729-1739) |
| Downstream filter chain | ext_proc → Lua → router. Lua rewrites `:path`/host/method from dynamic metadata *after* routing. | `translator.go:1328-1339`, `gateway-controller/lua/request_transformation.lua:102-182` |
| Upstream selection | Routes use `cluster_header: x-target-upstream`. The policy engine maps `UpstreamName` to `upstream_<Kind>_<apiId>_<name>` and always clears the route cache. | `translator.go:1115-1123,1742-1748`; `policy-engine/internal/kernel/translator.go:88-200,1263-1270` |
| LlmProxy providers | Every provider (primary and `additionalProviders`) is a **loopback** upstream to `127.0.0.1:<listener>` with the provider's context as base path. The request re-enters Envoy on that LlmProvider's own route, which adds the real provider credentials. | `gateway-controller/pkg/utils/llm_transformer.go:119-137,233-277,514-565` |
| Per-provider mechanics on a proxy | Transformers (`providerId` param) and loopback-hop auth (`set-headers`/`oauth2-generator`) are auto-injected, each gated by CEL on `request.Metadata['selected_provider']`. | `llm_transformer.go:315-351,907-1017` |
| Transformers | Five modules (`openai-to-{anthropic,azure-openai,bedrock,gemini,mistral}-transformer`). Request body is converted in `OnRequestBody` (body, `Path`, `UpstreamName = providerId`). Responses are converted buffered and streamed (anthropic, gemini, bedrock). Stream state lives in `SharedContext.Metadata`. None of them touch credentials. | `gateway-controllers/policies/openai-to-*/` |
| SDK retry concept | None. There is no retry or attempt hook, and response-phase actions cannot re-send a request. | `sdk/core/policy/v1alpha2/action.go` |
| Health-tracking precedent | `model-round-robin` keeps `suspendedModels map[string]time.Time` on the policy instance and suspends on ≥500 or 429 from `OnResponseHeaders`. There is no recovery probing. | `gateway-controllers/policies/model-round-robin/roundrobin.go:75-90,360-386` |
| Policy metrics | None. The engine uses a private Prometheus registry and the SDK offers nothing to policies. | `policy-engine/internal/metrics/metrics.go:528-605` |
| Known bug | `additionalProviders` dispatch on an LlmProxy returns 404: after a transformer rewrites `:path` and the route cache is cleared, the re-match fails against the proxy's own route table. | Prior live debugging (see R4) |

## Decisions

### R1: How the chain walk happens — Envoy native retry on a new "front hop"

- **Decision**: The client-facing LlmProxy route (the *front hop*) gets an Envoy `retry_policy`. It retries against a single internal cluster that points at a new **dispatch hop**, and each retry is a fresh request into the dispatch hop. Settings:
  - `num_retries = len(targets) - 1`
  - `retry_on: "retriable-headers,connect-failure,reset"` (`reset` is dropped when `failoverOn.timeout` is false)
  - `retriable_headers: [x-wso2-failover-retry (present)]`
  - `per_try_timeout = perAttemptTimeout`
  - small `retry_back_off`
  - a raised `max_retries` circuit breaker on the dispatch cluster
- **Rationale**: Envoy supplies the safety properties natively:
  - request-body buffering and replay of the *original* body on each attempt
  - a per-try timeout that stops once response headers arrive (`upstream_request.cc:208`), so streams are never cut off
  - **no retry after response headers have been forwarded downstream** (streaming safety, FR-012)
  - client-disconnect cancellation
  - a bounded attempt count (FR-010)
  - retry stats
- **Alternatives considered**:
  - *Aggregate cluster of per-provider clusters*: rejected. Envoy documents that PriorityLoad retry plugins don't work with aggregate clusters, so a strict ordered walk isn't guaranteed.
  - *One cluster with one priority per target plus `previous_priorities`, and upstream ext_proc for per-attempt adaptation*: rejected for v1. Every provider is a loopback to the same host, so priorities and outlier detection can't tell targets apart. It would also need a new upstream-ext_proc processing path in the policy engine, credential application moved into upstream mode, and unverified target identification per attempt.
  - *Policy makes its own HTTP calls and substitutes the response via `ImmediateResponse`*: rejected. `ImmediateResponse` is fully buffered (no SSE), it bypasses the provider chain's credentials, and it isn't Envoy-native.

### R2: Per-attempt target adaptation — a "dispatch hop" on an Envoy internal listener

- **Decision**: For every LlmProxy route with `model-failover` attached, the controller generates a **dispatch route** on a new Envoy **internal listener** (`envoy.bootstrap.internal_listener`, reached through an `internal_upstream` cluster). The dispatch route:
  - mirrors the proxy route's context and path semantics
  - matches on the chain id header (any path)
  - carries the per-target machinery that today sits on the proxy route: `model-failover` in its *dispatch* role, one transformer per non-OpenAI target, one loopback-auth policy per target, and one loopback upstream definition per target

  Each attempt is a separate dispatch request, so each gets a **fresh `SharedContext`**. The transformers run completely unchanged, and their stream state can't leak between attempts (FR-005a).
- **Rationale**:
  - Reuses the transformers, the loopback provider routes, and the existing credential injection exactly as they are (FR-005d).
  - Per-attempt request conversion *and* response conversion both happen in the dispatch hop's ordinary downstream ext_proc, which already supports streaming.
  - An internal listener can't be reached from outside the gateway, so no new external attack surface is added.
- **Alternatives considered**:
  - *A secret-header-gated dispatch route on the main listener*: kept as the **fallback** if spike S1 fails. It works, but it adds an externally matchable route.
  - *Importing transformer conversion functions into `model-failover`*: rejected. They are unexported, and importing them would duplicate the conversion paths.

### R3: Which target each attempt uses — an in-process attempt plan keyed by a nonce

- **Decision**:
  1. In the request phase, the front-hop `model-failover` instance builds an **attempt plan**: the configured targets in order, minus suspended targets, plus any probing target that claimed a probe slot.
  2. It stores the plan in a package-level registry keyed by a random 128-bit nonce, and sends only the nonce, in `x-wso2-failover-plan`.
  3. Each dispatch-hop arrival atomically advances the plan's cursor and uses that target.
  4. If the plan is empty, the front hop returns the exhaustion response immediately without contacting any upstream.
  5. If the cursor runs past the end of the plan, the dispatch hop returns an untagged "plan exhausted" response and Envoy stops retrying.
- **Rationale**:
  - Both hops call the same policy-engine process (one per gateway-runtime pod, same ext_proc cluster), so shared memory is safe.
  - Skipping suspended targets while trying each target at most once is guaranteed by construction (FR-010, FR-011).
  - Clients can't spoof a plan: an unknown nonce gets a non-retriable rejection, and the front route strips any client-supplied `x-wso2-failover-*` headers.
- **Alternatives considered**:
  - *`x-envoy-attempt-count` mapped to target index*: rejected. It is VirtualHost-scoped, so it would leak into other APIs' upstream requests, and it can't express skip-ahead.
  - *Full plan in a header*: rejected because it is spoofable.

### R4: Avoiding the known `additionalProviders` 404 bug

- **Decision**: Dispatch routes match on `prefix: "/"` **plus** the chain id header. When a transformer rewrites `:path` and the route cache is cleared, the re-match still hits the same dispatch route, which then forwards to the selected per-target loopback cluster.
- **Rationale**: The bug happens because the rewritten path isn't registered in the *proxy's* route table. Owning the dispatch route table sidesteps it by design. The bug is not fixed on plain (non-failover) proxies; that stays tracked separately.

### R5: Deciding what counts as a failover condition (revised after spike S2)

- **Decision**: The dispatch hop classifies every attempt's response in `OnResponseHeaders`:
  - **Eligible failure** (status in the configured `failoverOn.statusCodes`, or a transport failure the publisher enabled): set `x-wso2-failover-retry: <reason>`. Envoy retries through `retriable-headers`.
  - **Everything else**: passed through untouched, including non-eligible 502/503/504. Nothing in the front `retry_on` retries on status alone.
- **Front `retry_on`**: `retriable-headers,connect-failure,reset`.
  - `reset` retries a **per-try timeout**. The router resets the upstream stream with `LocalReset`, and `RETRY_ON_RESET` retries that. Verified live on Envoy 1.36.4 (spike S2j) and in the v1.39.0 source (`router.cc:1465` `maybeRetryReset` → `retry_state_impl.cc:473`).
  - The front→dispatch hop is in-process, so a real reset there is an internal failure. Provider-side resets and connect failures reach the front as tagged responses (R6), not as resets.
  - When `failoverOn.timeout` is `false`, the controller drops `reset` and the per-try timeout becomes Envoy's final 504.
- **Superseded**: the earlier design used `gateway-error` in `retry_on` and masked non-eligible 502/503/504 as 500 with `x-wso2-failover-status`. That came from misreading a source comment about *hedged* per-try timeouts. The spike showed it isn't needed, so the masking header and the `_maskGatewayErrors` param are removed.
- **Alternatives considered**: hedging on per-try timeout was rejected, because the slow primary stays in flight and could still win.

### R6: Telling transport failures apart from provider-sent 5xx (revised after spike S3b)

- **Decision**: The dispatch hop sends the per-boot secret in `x-wso2-failover-hop` on its loopback request to a provider route. On the client-facing listener, a small inline Lua filter (`wso2.failover.hop`, placed after the request-transformation Lua and before the router) **moves that header into dynamic metadata** (`wso2.failover` / `hop`) **and strips it**, so the secret is never forwarded to a real provider. An HCM `local_reply_config` mapper on that listener adds `x-wso2-upstream-failure: %RESPONSE_FLAGS%` only when both hold:
  - the response flags include any of `UF, URX, UH, UO, UT, UC, DC, LR`;
  - the dynamic metadata equals the secret, with `match_if_key_not_found: false`.

  The dispatch hop maps the flags to `connect_failure`, `reset` or `timeout` and removes the header. The internal dispatch listener never strips the header, so its own mapper matches on the header directly.
- **Why not match on the header**: the header must be stripped before the provider hop forwards the request, and Envoy applies route header removal to the live request-header map *before* a local reply is generated, so a `header_filter` would never match. `match_if_key_not_found` defaults to **true**; spike S3b showed that without setting it to false, every request without the secret was also annotated.
- **Rationale**: Native Envoy features only, no secret leak to providers, and it gives the per-reason toggles in FR-007.
- **Interaction with the OpenAI error-format work** (KB `ai-gateway-openai-error-format-llm-routes`, not on this branch yet): that work adds an HCM mapper, AND(upstream-failure flags, `api_kind` is LLM), which rewrites local-reply bodies into OpenAI format. Envoy applies only the **first matching** mapper. So the failover mapper must come first and carry the same OpenAI `body_format` in addition to the header, or the two must be merged into one mapper. Whichever of the two changes lands second owns that merge.

### R7: Health tracking, suspension, and probing — policy-engine state

- **Decision**: A package-level `healthRegistry` keyed by `(chainId, targetId)` holds a small state machine per target: `healthy → suspended → probing → healthy`. The dispatch hop records outcomes:
  - 2xx and non-eligible responses reset the failure count.
  - Eligible failures increment it.
  - Timeouts are recorded as follows: when attempt *k+1* arrives and attempt *k* has no outcome, *k* is marked `timeout`. If the *final* attempt times out, the front hop sees Envoy's 504 local reply (`UT`) and records it.

  Probe admission is a counting semaphore per target (`probeConcurrency`, default 1). A probe slot is released on its outcome or when the plan expires.
- **Rationale**: Envoy outlier detection can't count 429 (the HTTP-code `monitors` are not implemented in 1.39), has no probe-limited half-open recovery, and every target shares one loopback host anyway. Per-instance scope matches the spec assumption.
- **Alternatives considered**:
  - *Outlier detection with 429 rewritten to 503*: rejected. It is a hack, and the client would see the wrong code.
  - *Active health checks for probing*: rejected. They spend provider quota and need credentials.

### R8: Where the exhaustion response comes from

- **Decision**: The front hop's `OnResponseHeaders` replaces any final response that still carries `x-wso2-failover-retry`, is a plan-exhausted marker, or is a 504 `UT` local reply with an `ImmediateResponse`: `503` and a fixed OpenAI-format error body (see [contracts/exhaustion-response.md](./contracts/exhaustion-response.md)). An empty plan short-circuits in the request phase with the same response.
- **Rationale**: A single consistent response that doesn't leak raw upstream bodies (FR-018). It matches the gateway's OpenAI-format error convention for LLM routes.

### R9: Policy metrics — small SDK addition

- **Decision**: Add a `sdk/core/metrics` package that exposes a `prometheus.Registerer`, injected by the policy engine at startup (same pattern as `utils.SharedHTTPClient()`). It wraps the engine's private registry so policy metrics are exported on the existing metrics endpoint. Every `model-failover` metric has bounded labels (chain id, target id, reason, state).
- **Rationale**: FR-020 needs metrics per target, and nothing a policy can use exists today. The addition is reusable by any policy.
- **Alternatives considered**: Envoy cluster stats only. Rejected: they can't see targets, because all targets share one loopback cluster.

### R10: One policy, two roles

- **Decision**: A single `model-failover` policy module. When it builds the two chains, the controller injects internal keys into each instance's **`params`**: `_role` (`front` | `dispatch`), `_chainId`, `_targetIds`, `_hopSecret`, and for the dispatch role the per-target resolved `basePath`. It merges them key-wise, the same way `attachedTo` is injected today. Authored params that contain any `_`-prefixed key are rejected at validation. These keys are **not** `systemParameters`: per KB `gateway-policy-systemparameters-cel-default-hard-fails`, `systemParameters` are resolved from `${config.*}` before params are merged, and a missing key hard-fails chain build for every route.
- **Rationale**: Follows the team rule of one policy per capability, where `Mode()` is computed per instance. The front role needs only headers; the dispatch role buffers the request body for native-target model rewriting.

### R11: Ownership of the retry settings in the controller

- **Decision (as implemented)**: the controller recognises the policy by name (`failover.PolicyName`) in `pkg/utils/llm_failover.go`. The `x-wso2-envoy-retry-chain` definition marker was dropped because nothing read it, and `LLMProviderTransformer` has no access to policy definitions. Generalising to a marker is future work, if a second chain-style policy appears.
- **Rationale**: Simplest correct option for one policy. A definition marker can replace the name check later without any wire change.
- **Validation runs synchronously at registration** (`POST`/`PUT /llm-proxies`, returning 400 with field errors), not only in the async xDS transform. This fixes the earlier gap recorded in KB `model-failover-registration-time-validation-was-async-only`.

### R13: Fail closed when a chain fails to build (verified: already the case)

- **Finding (code read, current main)**: when a route's policy chain fails to build, `PE/internal/xdsclient/handler.go:232-237` drops the route from the applied set. Requests to it then get an immediate `500 {"error":"Internal Server Error"}` with terminal reason "no policy chain" (`PE/internal/kernel/extproc.go:339-366`). The fail-open behaviour recorded in KB `gateway-policy-systemparameters-cel-default-hard-fails` no longer applies.
- **Consequence for failover**: a front route without a chain returns 500 and never forwards. A dispatch route without a chain returns an untagged 500, which Envoy does not retry, so the front route returns it. No engine change is needed.

### R14: Why per-attempt `:path` isn't an issue in this design

- Envoy replays the first attempt's `:path` verbatim on retries and never recomputes it (KB `failover-aggregate-cluster-upstreamname-two-hidden-couplings`). That was the core flaw of the earlier aggregate-cluster approach.
- Here, the front hop forwards the client's original path unchanged on every attempt. Each dispatch request computes its own path from scratch: the target's loopback base path plus the transformer path.
- Per-target upstream definitions are **named explicitly** (`failover-t<k>`), including one for the primary. This sidesteps the known gap that the primary provider's cluster has an empty name and can't be addressed through `upstream_definition_paths`.

### R12: Limits and defaults

- **Decision**:

  | Setting | Default | Range |
  |---|---|---|
  | `perAttemptTimeout` | `30s` | `1s`–`300s` |
  | `suspendAfterConsecutiveFailures` | `3` | `1`–`100` |
  | `suspendDuration` | `30s` | `1s`–`3600s` |
  | `probeConcurrency` | `1` | `1`–`10` |
  | `recoverAfterSuccessfulProbes` | `2` | `1`–`20` |
  | `failoverOn.statusCodes` | `[429,500,502,503,504]` | `429` and `500`–`599` only |
  | `targets` | — | 1–10 |

  - The front route timeout is `max(configured route timeout, n × perAttemptTimeout + 2s)`.
  - The retry buffer (`request_body_buffer_limit`) defaults to 4 MiB and comes from gateway config `router.failover.max_request_body_bytes`. A larger body turns off retries for that request (Envoy `retry_or_shadow_abandoned`); this is documented and logged.
  - Retry back-off: base 1ms, max 10ms.
- **Rationale**: Safe defaults let a publisher enable the policy with only `targets` (FR-023). The limits bound retry amplification, following the network-service-hardening rule.

## Measurements

- **Policy overhead** (`BenchmarkHealthyPrimaryRequest`, Apple M-series, Go 1.26): about 2.5 µs and 65 allocations per request served by a healthy primary, covering both roles' hooks. The Envoy internal-listener hop and the dispatch hop's ext_proc exchange are not included and need a live measurement against the < 5 ms target.

## Spikes (verification, not open questions)

S1–S4 were run on 2026-09-26 with local Envoy 1.36.4 (brew) against `spikes/spike-envoy.yaml`, `spikes/harness/` and `spikes/run-spikes.sh`. The gateway ships 1.39.0; the retry and reset behaviour was cross-checked against the 1.39.0 source. Re-run `spikes/run-spikes.sh` against a 1.39 binary before release.

Each spike has a decided fallback, so none of them blocks planning.

| ID | Verify | Fallback if it fails |
|---|---|---|
| S1 | ✅ **Done**. The internal listener plus an `internal_upstream` cluster works, and ext_proc runs on both hops (spike S1: the backend saw both hops' header mutations). | — |
| S2 | ✅ **Done**. `retriable-headers` + `reset` + `per_try_timeout` retry as described in R5; untagged responses are not retried; streaming isn't cut by `per_try_timeout`. | — |
| S3 | ✅ **Done** (superseded in part by S3b). A `local_reply_config` mapper with AND(`response_flag_filter`, request `header_filter` secret) adds `x-wso2-upstream-failure: UF` only when the secret header is present. The formatter works in `headers_to_add`, and the local reply passes through the HCM's encoder filters (Lua saw it), so ext_proc in the dispatch hop will too. | — |
| S3b | ✅ **Done**. `spikes/spike-s3b-hop-metadata.yaml`: an inline Lua filter moves `x-wso2-failover-hop` into dynamic metadata and strips it; the backend never sees it; the mapper's `metadata_filter` (with `match_if_key_not_found: false`) annotates only requests that carried the correct secret. Without that setting, requests with no secret matched too. | — |
| S4 | ✅ **Done**. A body over the buffer limit is forwarded once, returns the primary's response, and has retries turned off (not 413). The field is `per_request_buffer_limit_bytes` on 1.36; check whether 1.39 prefers `request_body_buffer_limit`. | — |
| S5 | ✅ **Done (code read)**. Paths are rewritten in two places. (1) The route-level `RegexRewrite` (`GC/pkg/xds/translator.go:1809-1861`) strips the API context and prepends the default upstream base path. (2) Lua (`request_transformation.lua:102+`) rewrites `:path` **only** when the engine sets `metadata["path"]`, which `applyUpstreamRedirect` does whenever an `UpstreamName` resolves with a known base path (`PE/internal/kernel/translator.go:115-157`). `applyDefaultUpstream` sets no path, only `x-target-upstream`. **Result**: the front route must be generated **without** `RegexRewrite` and without `cluster_header` (fixed `cluster: failover_dispatch`), and must also strip `x-target-upstream`. The front chain sets no `UpstreamName`, so Lua does nothing and the client path reaches the dispatch hop unchanged. The dispatch route clones today's proxy-route rewrite semantics (same `RegexRewrite` + `cluster_header` + Lua), so it behaves exactly like a proxy route today, except that it matches on the chain header. | — |

# LLM Model Failover — Design

**Status:** Draft, awaiting review
**Scope:** `LlmProxy` only (not `RestApi`)
**Depends on:** the upstream-scoped policy execution mechanism fixed in this same session (`gateway-runtime/policy-engine/internal/kernel/upstream_extproc.go`, `gateway-controller/pkg/xds/upstream_policy_filter.go`) — this design extends that mechanism, it does not replace it.

## 1. Problem

There is no way today to declare "if this model/provider fails on a request, retry the same request against a different model/provider" with correct per-attempt authentication and payload transformation. The two existing "model-*" policies (`model-round-robin`, `model-weighted-round-robin`) pick a target *before* dispatch and never retry within a request; they are load-distribution policies, not failover. Genuine mid-request failover requires an Envoy-level retry across real backend clusters, which today's session proved is mechanically possible but has no declarative config surface and no way for a retry attempt to run the right transformer.

## 2. Non-goals (v1)

- `RestApi`-level generic backend failover. `model`/`provider`/transformer selection are LLM-proxy concepts; a generic "retry to a different plain backend" feature is a separate, later effort.
- Per-fallback suspend-duration overrides. One `suspendDuration` per `failover` block.
- Updating every transformer policy. The mechanism is implemented end-to-end against `openai-to-anthropic-transformer` first; `openai-to-mistral`/`-azure-openai`/`-gemini`/`-bedrock` get the same treatment as a mechanical follow-up once the pattern is proven, not as part of this change.
- Cross-replica shared suspension state. Suspension tracking is in-memory, per policy-engine replica — the same consistency level `model-round-robin` already has today. A Redis-backed shared tracker is a future hardening step, not part of this design.

## 3. Config surface

New `failover` field nested under the existing `resilience` block, settable at API level and operation level with the same override precedence `timeout`/`idleTimeout` already have (operation overrides API).

```yaml
resilience:
  timeout: 15s                    # existing, unchanged
  failover:
    targets:
      - target:
          model: gpt-4o            # provider omitted -> the LlmProxy's primary provider
        fallbacks:
          - model: claude-sonnet-4-5-20250929
            provider: anthropic-upstream   # matches an additionalProviders[].as (or .id)
      - target:
          model: gpt-4o-mini
        fallbacks:
          - model: claude-3-5-haiku
            provider: anthropic-upstream
    suspendDuration: 900           # seconds; 0 disables suspension tracking entirely
```

- `failover.targets` is a list because a single LlmProxy operation can serve multiple client-requested models (the client's `model` field in the OpenAI-shaped request body), and different requested models can legitimately need different fallback chains.
- Each entry's `target` is the primary attempt for that model; `fallbacks` is its ordered retry chain. Both `target` and each `fallbacks[]` entry are `{model, provider?}`, `provider` defaulting to the LlmProxy's primary provider.
- `provider` (when set) must match an `additionalProviders[].as` (or `.id` when `as` is omitted) — the same validation `model-round-robin`'s `provider` field already does. A `provider`/`model` pair that resolves to no configured `additionalProviders` entry (or the primary) is a deploy-time validation error, not a runtime failure.
- Omitting `failover` entirely (only `timeout`/`idleTimeout` set, as today) must produce byte-identical xDS output to before this change — no aggregate cluster, no retry policy, no attempt-count header, zero cost when unused. This is the same "pay nothing when the feature isn't attached" discipline `clusterNeedsUpstreamPolicyFilter` already applies to the upstream ext_proc filter.

## 4. Control-plane translation (gateway-controller)

New logic in `gateway-controller/pkg/transform/llm.go` and `gateway-controller/pkg/xds/translator.go`, gated on `resilience.failover.targets` being non-empty:

- **One real cluster per distinct `{model, provider}` pair** referenced anywhere in `failover.targets` (a `target` or a `fallbacks[]` entry) that doesn't already have one from the LlmProxy's normal `provider`/`additionalProviders` cluster creation — in the common case every referenced provider already has a cluster (it's also the provider's normal upstream), so this is mostly bookkeeping, not new cluster creation.
- **One `envoy.clusters.aggregate` cluster per `failover.targets[]` entry**, named deterministically and uniquely (e.g. `failover_agg_<routeKey-safe-hash>_<index>`) so the policy-engine can map an observed `xds.cluster_name` back to the exact entry — this name is synced to the policy-engine as part of the route's metadata (§6), not just used inside the Envoy config. Members are `[target-cluster, fallback-cluster-1, ...]` in priority order (aggregate cluster ordering assigns ascending priority by list position — confirmed live this session).
- **The route's action** gets `retry_policy: { retry_on: "5xx", retry_priority: { name: envoy.retry_priorities.previous_priorities } }`, `auto_host_rewrite: true`, and `include_attempt_count_in_request: true`. `auto_host_rewrite` and the attempt-count header are required for §6 to resolve a real per-attempt URL/identity at all — this is not optional hardening, the mechanism does not work without them (confirmed live: without `auto_host_rewrite`, the resolved `:authority` is the original downstream host, not the real backend, producing an internally-consistent but wrong signature).
- **The upstream ext_proc filter is attached to each aggregate cluster itself**, not to its real members — confirmed live this session that attaching to real members never fires when reached through an aggregate, while attaching to the aggregate fires correctly on every attempt.
- Since a route can now resolve to one of *several* aggregate clusters (one per `failover.targets[]` entry) depending on which the downstream phase selects, the route's dispatch to a specific aggregate is done via the **existing dynamic `x-target-upstream` / `cluster_header` mechanism** (`TargetUpstreamHeader`/`TargetUpstreamClusterKey` in `gateway-controller/pkg/constants/constants.go`), the same one that already selects among `additionalProviders`. `useClusterHeader` must be forced on for any route carrying a `failover` block.

## 5. Downstream phase — target selection and suspension pre-emption

New logic in the policy-engine's kernel (downstream `ExternalProcessorServer`, request-headers/body phase), running only for routes whose synced `RouteConfig` carries a non-empty failover-targets list:

1. Parse the client-requested `model` out of the request body (the same body the resolver/transformer path already has access to for a body-resolved route).
2. Match it against `failover.targets[].target.model` for this route. No match → failover is inert for this request; it proceeds through the LlmProxy's normal primary-provider path unchanged (this is a deliberate fallback-to-today's-behavior, not an error — a proxy can serve models with no failover chain declared alongside ones that have one).
3. On a match, consult the suspension tracker (§7) for `(routeKey, matched entry's target)`. If not suspended → set `x-target-upstream` to the matched entry's aggregate cluster. If suspended → walk `fallbacks` in order and set `x-target-upstream` to the aggregate cluster **but request dispatch starting from the first non-suspended fallback's priority**, not the suspended target's. (Open implementation detail, not an open design question: Envoy's aggregate cluster always starts at priority 0 and only escalates on failure — routing "start at fallback 2" needs either a per-request priority-set override or, more simply, treating "target is suspended" as "skip straight to that fallback's *own* single-cluster route instead of the aggregate," bypassing the aggregate entirely for a known-bad primary. The second option is simpler and is the one to implement.)
4. Run the matched target's own client-facing auth/transform policies exactly as `additionalProviders` selection already does today — no new downstream transform logic, this reuses the existing conditional-execution-by-selected-provider pattern.

## 6. Upstream phase — attempt identity resolution

Extends `UpstreamExternalProcessorServer.resolveBackend` (`gateway-runtime/policy-engine/internal/kernel/upstream_extproc.go`), which today resolves a single real cluster's `DefaultUpstream`. New resolution order, checked before falling through to today's existing checks (route-scoped `DefaultUpstream` match, then the global cluster-name index, then `:authority`) — those remain as the fallback for every route *without* a failover block, so this is additive, not a replacement:

1. Look up `xds.cluster_name` against the route's synced failover-entry map (keyed by the aggregate cluster names assigned in §4). No match → this route has no failover block, or the request never went through the aggregate at all (e.g. a normal non-failover route) — fall through to the existing resolution chain unchanged.
2. On a match, read `x-envoy-attempt-count` from the RequestHeaders message. Compute `index = max(attemptCount, 1) - 1`; treat a missing or unparseable header as attempt 1 (`index = 0`) rather than failing closed — see the reasoning in the conversation: index 0 is always the declared primary, the safe default.
3. `index == 0` → the entry's `target`; `index >= 1` → `fallbacks[index-1]`. `index` past the end of `fallbacks` (more retries than configured fallbacks — shouldn't happen since `retry_policy` has no `num_retries` larger than `len(fallbacks)`, but defensively) → leave URL empty, fail closed, exactly like today's unknown-cluster case.
4. The resolved list entry carries `{model, providerId, resolvedURL, resolvedBasePath}` — this is what feeds both the AWS-style auth policy's signing (`resolvedURL`, same as today) and the new transformer dispatch (§7): the policy chain's transformer policy (if any) needs `providerId`/`model` to pick its own transform direction, the same way `execution_condition`-based provider matching already tells a downstream transformer whether to run.

## 7. Transformer policy changes and suspension recording

- `openai-to-anthropic-transformer` (chosen as the first, proving implementation) gains `OnUpstreamRequestBody`: reads `UpstreamAttemptContext.OriginalRequestRaw` (never the possibly-already-mutated `Body` — same discipline `aws-authentication`'s upstream signing already follows) and rewrites it into Anthropic's shape when the resolved target (§6) is the anthropic provider. When the resolved target is the OpenAI primary (a retry landed back on `target` rather than a fallback — not expected given retry_priority semantics, but the check must exist), it is a no-op passthrough.
- The same policy gains `OnUpstreamResponseBody`: on a successful response, transforms Anthropic's response shape back to OpenAI's before it flows downstream to the client. On a failing response (5xx, matching `retry_on`), this is also where suspension gets **recorded** — mark `(routeKey, resolved target)` suspended until `now + suspendDuration` in the tracker from §5, mirroring where `model-round-robin` already suspends today (`OnResponseHeaders`), just moved to the upstream phase since that's where failure is now observed for a failover-driven attempt.
- New small kernel component: an in-memory `map[string]time.Time` tracker, same shape as `model-round-robin`'s `suspendedModels`, but living in the kernel (not inside one policy instance) since both the downstream phase (§5, checks it) and the upstream phase (§7, writes it) need it, and it must survive across the many short-lived per-attempt contexts a single failover-eligible route creates. Keyed by `routeKey + model + providerId`, TTL-cleared lazily on read (same pattern `model-round-robin` uses — check-and-delete-if-expired, no background sweep).

## 8. Error handling

- All targets in a chain exhausted (target + every fallback failed) → Envoy's own retry-exhaustion behavior applies unchanged: the last attempt's response is what the client sees. No new error-shaping logic.
- Unknown/unmapped `xds.cluster_name` for a route that does have a failover block, or an attempt index past the end of the resolved list → leave URL empty, fail closed (existing pattern, §6.3).
- A `provider` in `target`/`fallbacks[]` that doesn't resolve to a configured `additionalProviders` entry or the primary → deploy-time validation error (config rejected), never a runtime condition.

## 9. Testing strategy

- **gateway-controller** (`pkg/transform`, `pkg/xds`): table tests asserting (a) no `failover` block → xDS output unchanged from before this change, (b) one aggregate cluster per `targets[]` entry with correctly-ordered members, (c) `retry_policy`/`auto_host_rewrite`/`include_attempt_count_in_request` present only on failover-carrying routes, (d) the upstream ext_proc filter attached to the aggregate cluster's own `TypedExtensionProtocolOptions`, validated via `HttpProtocolOptions.ValidateAll()` the same way this session's CDS-rejection bug was caught.
- **policy-engine kernel**: unit tests for `resolveBackend`'s new failover-entry-map + attempt-count-index resolution (missing header → index 0; index past list end → empty URL; unknown cluster_name → falls through to existing resolution), and for the suspension tracker's check/record/expire behavior, following the RED→GREEN pattern used throughout this session.
- **Live verification**: repeat this session's hand-crafted-Envoy test methodology (a standalone Envoy instance inside the running `gateway-runtime` container, dialing the real `policy-engine-upstream.sock`) but against a *real* deployed `LlmProxy` with a `failover` block — the config-generation half (§4) becomes real once implemented, removing the need to hand-craft the aggregate-cluster YAML as was necessary today.

## 10. Open items carried into implementation planning

- §5.3 decides to route a suspended target's traffic directly to its first non-suspended fallback's own single cluster, bypassing the aggregate entirely. That decision trades away retry protection on the fallback itself (if the fallback also fails, there's no further retry within that request, since the aggregate was never entered) — confirm this tradeoff is acceptable, or the alternative (a per-request priority-set override into the aggregate, starting past the suspended member) needs real implementation-time investigation into whether Envoy exposes that lever at all.
- Exact deterministic naming scheme for aggregate clusters (§4) needs to be collision-free across concurrently-deployed LlmProxies and stable across redeploys (so a redeploy doesn't spuriously version-bump every route's xDS resource). A hash of `routeKey + target-entry-index` is the working assumption; confirm against existing cluster-naming collision-avoidance code before implementing.

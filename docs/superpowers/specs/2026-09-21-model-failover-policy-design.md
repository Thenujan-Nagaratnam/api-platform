# Model failover as a policy — Design

**Status:** Draft, awaiting review
**Supersedes:** `docs/superpowers/specs/2026-09-17-llm-model-failover-design.md` (config surface and
runtime ownership only — the Envoy-level mechanics that design proved, aggregate clusters,
`retry_policy`, `auto_host_rewrite`, attempt-count header, per-attempt upstream ext_proc dispatch,
are unchanged and reused as-is).
**Depends on:** `docs/superpowers/specs/2026-09-21-upstream-policy-reuse-design.md` (attachment-point
selection of downstream vs. upstream policy execution) — this design is built entirely on top of that
mechanism.

## 1. Problem

`resilience.failover` is a dedicated LlmProxy schema section, parsed and acted on by bespoke
gateway-controller and gateway-runtime-kernel code (`resolveBackend`'s failover-entry matching, a
kernel-owned suspension tracker, unconditional per-attempt metadata seeding). That bespoke code only
existed because, until today, there was no generic way for an upstream-attempt policy to know which
provider a specific attempt was for — so the mechanism was built directly into the kernel instead of
into a policy, and the credential/transform policies needed dev-fork changes to consume the kernel's
resolution.

Now that any policy can run per upstream attempt via its existing interfaces (attachment via
`upstreamPolicies:`), and CEL `executionCondition` can gate that participation the same way it already
gates downstream `additionalProviders` selection, none of that bespoke kernel logic is structurally
necessary anymore. This design moves model failover out of a special schema section and bespoke kernel
code, into an ordinary policy — `model-failover` — attached and executed the same way
`model-round-robin`/`model-weighted-round-robin` already are.

## 2. Goal

- `model-failover` is a real policy: same attachment syntax (`operationPolicies:`/`upstreamPolicies:`),
  same params-from-config pattern, same `Mode()`-declared phase participation as any other policy.
- It owns the full failover lifecycle: downstream target selection and suspension pre-emption, and
  per-attempt chain-position resolution, provider/model identity seeding, and suspension recording —
  the runtime logic `resilience.failover` currently spreads across the kernel.
- Provider-scoped credential/transform policies (`oauth2-generator`,
  `openai-to-anthropic-transformer`, and future ones) need **zero code changes** — they gate on the
  same `executionCondition` + `selected_provider` metadata mechanism they already use downstream today,
  now also evaluated upstream.
- The Envoy-level plumbing (aggregate clusters, `retry_policy`, `auto_host_rewrite`, attempt-count
  header, upstream ext_proc filter attachment) is unchanged — still controller-generated — just
  triggered by detecting a `model-failover` attachment instead of a `resilience.failover` schema field.

## 3. Config surface

`resilience.failover` is removed from the LlmProxy schema. In its place, `model-failover` is attached
like any other policy, with the same params shape the old schema carried:

```yaml
operationPolicies:
  - name: model-failover
    version: v0
    paths: [{ path: /chat/completions, methods: [POST] }]
    params:
      targets:
        - target: { model: gpt-4o }
          fallbacks:
            - { model: claude-sonnet-4-5-20250929, provider: anthropic-upstream }
        - target: { model: gpt-4o-mini }
          fallbacks:
            - { model: claude-3-5-haiku, provider: anthropic-upstream }
      suspendDuration: 900
```

Same validation rules as before (`provider` must resolve to `additionalProviders[].as`/`.id` or the
primary; deploy-time error otherwise). The policy is also attached under `upstreamPolicies:` with the
*same instance* — the controller synthesizes this second attachment automatically when it sees
`model-failover` under `operationPolicies:`, the same way it already synthesizes provider-scoped
`upstreamPolicies:` attachments for credential/transform policies (§6).

## 4. Downstream phase

`model-failover`'s `OnRequestBody`:

1. Parses the client-requested `model` from the JSON body.
2. Matches it against its own `params.targets[].target.model`. No match → no-op (`UpstreamRequestModifications{}`) — the request proceeds through the proxy's normal primary-provider path unchanged, exactly like today.
3. Checks its own suspension state (§7) for `(routeKey, matched target)`. If suspended, walks
   `fallbacks` for the first non-suspended entry and targets that instead (same "bypass the aggregate
   for a known-bad primary" behavior §5.3 of the superseded design settled on).
4. Returns `UpstreamRequestModifications{UpstreamName: &aggregateClusterName}` to route Envoy to the
   matched target's aggregate cluster — reusing the *existing* upstream-selection mechanism
   `openai-to-anthropic-transformer` already uses today (`UpstreamRequestModifications.UpstreamName`),
   not a new one.

## 5. Controller: xDS generation

Unchanged from the superseded design's §4, except triggered by policy detection instead of a schema
field: when a route has a `model-failover` `PolicyInstance` attached, the controller reads its `params`
(same shape as before) and generates:

- One real cluster per distinct `{model, provider}` pair referenced in `targets`, if one doesn't
  already exist from normal provider/`additionalProviders` cluster creation.
- One `envoy.clusters.aggregate` cluster per `targets[]` entry, deterministically named
  (`failover_agg_<routeKey-hash>_<index>`), members in priority order.
- `retry_policy: { retry_on: "5xx", retry_priority: previous_priorities }`, `auto_host_rewrite: true`,
  `include_attempt_count_in_request: true` on the route.
- The upstream ext_proc filter attached to each aggregate cluster.
- **New:** the controller also synthesizes one `upstreamPolicies:` `PolicyInstance` per provider
  referenced in `targets` for each attached credential/transform policy (see §6), each carrying an
  `executionCondition` — the same `selectedProviderExecutionCondition`-style helper
  (`gateway-controller/pkg/utils/llm_transformer.go`) already used to gate their downstream instances.

No `RouteConfig.Metadata.FailoverTargets` sync is needed anymore — `model-failover`'s own `params`
(passed to its `GetPolicy` the same way any policy's params are) *are* the failover-chain declaration;
nothing needs to be pre-resolved and pushed into kernel route metadata separately.

## 6. Upstream-attempt phase

### New SDK surface: the raw dialed cluster name

Today `RequestContext.Upstream.Name`/`ResponseContext.Upstream.Name` (populated for an upstream-attempt
invocation) is the *resolved* real backend cluster — already overwritten by the kernel's existing
member-cluster resolution. For an attempt routed through an aggregate cluster, Envoy's `xds.cluster_name`
attribute reports the *aggregate's own name* on every attempt, unchanged since the design was first
proven live — that's the signal `model-failover` needs to know which `targets[]` entry's chain it's
walking, and it isn't exposed to policies today.

New field, added to both `UpstreamRequestContext` and `UpstreamResponseContext`:

```go
type UpstreamRequestContext struct {
    Name     string // resolved real backend cluster (unchanged)
    URL      string
    BasePath string
    // RouteCluster is the raw xds.cluster_name Envoy reported for this
    // attempt, before any kernel-side member resolution — for an
    // aggregate-routed attempt, this is the aggregate cluster's own name,
    // stable across every attempt against it. Empty for a route with no
    // aggregate/failover involvement.
    RouteCluster string
}
```

Populated in `kernel.BuildUpstreamAttemptRequestContext`/`BuildUpstreamAttemptResponseContext` from
`upstream_extproc.go`'s already-captured raw `xds.cluster_name` attribute (`extractAttribute(req,
"xds.cluster_name")`), read *before* today's existing `resolveBackend` substitution — that capture
point already exists, this only means preserving the pre-substitution value instead of discarding it.

### `model-failover`'s per-attempt role

Attached via `upstreamPolicies:` (§3), `model-failover`'s `OnRequestHeaders`:

1. Matches `reqCtx.Upstream.RouteCluster` against its own `params.targets[]`'s assigned aggregate
   cluster names (deterministically computable from the same naming scheme the controller uses in §5 —
   `model-failover` and the controller must agree on this scheme; see open items).
2. No match → no-op (this attempt isn't on a chain this instance owns).
3. On a match, reads `x-envoy-attempt-count` from `reqCtx.Headers`. `index = max(attemptCount, 1) - 1`.
   `index == 0` → the entry's `target`; `index >= 1` → `fallbacks[index-1]`; past the end → no-op
   (fail closed, matches today's behavior).
4. Writes `reqCtx.SharedContext.Metadata["selected_provider"]`/`["selected_model"]` to the resolved
   entry's `provider`/`model` — the same metadata key/convention `llm-header-router` already writes
   downstream (see `docs/superpowers/specs/2026-09-21-upstream-policy-reuse-design.md`), just written
   by a different policy, in a different phase.

`model-failover`'s `OnResponseHeaders` reads the response status; on a failure matching the configured
retry condition, records suspension (§7). This mirrors where the superseded design put suspension
*recording* (upstream phase, since that's where attempt failure is actually observed), just moved from
kernel code into the policy.

## 7. Suspension tracking moves into the policy

Today's kernel-owned `map[string]time.Time` tracker (shared between the downstream pre-emption check
and the upstream-phase recording) is replaced by `model-failover` self-managing its own suspension
state — the exact same pattern `model-round-robin` already uses today for its own suspended-model
tracking, just applied to failover targets instead of round-robin candidates. This removes the
kernel's dedicated tracker entirely; `model-failover` becomes the only place suspension state lives.

## 8. Provider-scoped credential/transform policies: zero changes

`oauth2-generator`, `openai-to-anthropic-transformer` (and any future provider-scoped policy) are
attached under `upstreamPolicies:` once per provider referenced in `targets`, each with an
`executionCondition` like:

```
selected_provider != '' && selected_provider == 'anthropic-upstream'
```

— evaluated by the *same* CEL mechanism and metadata key already used for their downstream
`additionalProviders`-scoped instances. No policy code changes: the released, unmodified
implementations already do the right thing when the executor simply doesn't call them for an attempt
whose condition doesn't match.

## 9. Executor change

`internal/executor/chain.go`'s four upstream-attempt dispatch functions
(`ExecuteUpstreamAttemptRequestHeaderPolicies`/`RequestPolicies`/`ResponseHeaderPolicies`/`ResponsePolicies`)
gain CEL `executionCondition` evaluation, mirroring the existing downstream blocks exactly:

```go
if chain.HasExecutionConditions && spec.ExecutionCondition != nil && *spec.ExecutionCondition != "" {
    conditionMet, err := c.celEvaluator.EvaluateRequestBodyCondition(*spec.ExecutionCondition, reqCtx)
    ...
    if !conditionMet { continue }
}
```

Verified this session: the real `CELEvaluator` is already shared between the downstream and upstream
servers (`cmd/policy-engine/main.go`), the upstream-attempt context types are the exact same
`*policy.RequestContext`/etc. the evaluator already accepts, `ExecutionCondition` already survives into
`UpstreamPolicySpecs` through all three chain-builders, and `chain.HasExecutionConditions` already
covers upstream-attached entries. This is a pure, self-contained addition — no other plumbing changes.

**Ordering requirement:** `model-failover`'s `upstreamPolicies:` instance must execute before the
provider-scoped credential/transform instances in `chain.UpstreamPolicies`, so the metadata it writes
exists when their conditions are checked — same requirement `llm-header-router` already has downstream.
The controller must attach `model-failover`'s upstream instance first in attachment order (array order
in the synced config determines execution order, same as today).

## 10. What this removes from the kernel

- `RouteConfig.Metadata.FailoverTargets` and its xDS sync (`gateway-controller/pkg/xds/translator.go`
  → `gateway-runtime` route metadata) — `model-failover`'s own policy params replace it.
- `resolveBackend`'s failover-entry-matching branch (`kernel/upstream_extproc.go`) — the *real* backend
  URL/basePath for an aggregate member is already resolvable through the existing generic
  cluster-name-based resolution (global cluster index / route `DefaultUpstream`), since every failover
  target is also a normally-registered provider cluster; the failover-specific branch only ever existed
  to produce `{model, provider}` *labels*, which `model-failover` now produces itself.
- The kernel's dedicated suspension tracker (§7).
- `ResolvedFailoverProviderHeader`'s kernel-side emission (`upstream_extproc.go`) — `model-failover`
  can set this itself in its own `OnResponseHeaders` when it detects `index > 0` for the current
  attempt, using the same header/analytics-attribution contract, no kernel involvement needed.

## Non-goals (unchanged from the superseded design)

- `RestApi`-level generic backend failover.
- Per-fallback suspend-duration overrides.
- Updating every transformer policy — `openai-to-anthropic-transformer` first, others as a mechanical
  follow-up.
- Cross-replica shared suspension state.

## Open items carried into implementation planning

1. **Aggregate cluster naming agreement.** `model-failover` must compute the same deterministic
   aggregate-cluster name the controller assigns (§5/§6) purely from its own params + route identity,
   with no side-channel sync — needs a naming scheme both sides can derive independently (e.g. a pure
   function of `routeKey`/policy-instance-identity + `targets[]` index), not a hash the controller
   invents and never communicates.
2. **Does `model-failover` need `routeKey` at all**, or is matching purely on `RouteCluster` against its
   own params sufficient? (Probably yes — the per-route uniqueness lives in the cluster name, which
   already encodes it.)
3. **Attachment-order guarantee.** Confirm `orderedLLMPolicyAttachments` (or its successor) has a
   deterministic way to force `model-failover` first among `upstreamPolicies:` entries, not just an
   accident of iteration order.
4. **Downstream suspension check needs body access before routing.** `model-failover`'s downstream
   role reads the body (to parse `model`) *and* needs to decide `UpstreamName` before the body-phase
   action returns — confirm this fits `RequestPolicy.OnRequestBody`'s existing single-pass shape
   cleanly (it should; `openai-to-anthropic-transformer` already does body-parse + `UpstreamName` in one
   `OnRequestBody` today).
5. **Migration path for existing `resilience.failover` config.** Removing the schema field is a
   breaking change for any already-deployed LlmProxy using it — needs a deprecation/migration story
   (config transformer emitting the policy attachment automatically from the old field for one release?
   hard cutover?) before this ships, not decided in this design.

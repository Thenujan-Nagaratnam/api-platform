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

- `model-failover` is a real policy: same author-facing attachment syntax (`operationPolicies:`),
  same params-from-config pattern, same `Mode()`-declared phase participation as any other policy.
  Its second, upstream-attempt attachment is controller-synthesized, not author-written — see §3's
  post-implementation note on why `upstreamPolicies:` has no author-facing schema field at all.
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

That's the *author-facing* shape — what an operator (or the LlmProxy transformer, if this stays
generated from higher-level provider config rather than hand-written) writes. The controller expands
it before attaching: each `targets[]` entry gets an `aggregateCluster` field injected with the exact
name the controller assigned that entry's `envoy.clusters.aggregate` cluster in §5 — the *same* params
map is used for both the `operationPolicies:` and `upstreamPolicies:` attachments, so `model-failover`
never needs to compute or guess a cluster name; it only ever reads one it was handed. This removes the
"two independently-written naming schemes must agree" risk entirely — there is exactly one source of
truth (the controller), and matching in §6 is a plain string comparison against `params`, not a
recomputation.

Implementation additionally injects, per target/fallback member and discovered only once the kernel's
`:path`-correction and aggregate-dispatch gaps below were found (§10): `primaryProvider` (the entry's
own resolved provider id, so a member authored with no `provider:` — meaning "the primary" — can still
be told apart from an explicit one when seeding `selected_provider`); each member's own `basePath` and
the route's `operationPath` (so the upstream phase can reconstruct the per-attempt `:path`, see §6);
and each member's own resolved Envoy `clusterName` (so the upstream phase can recognize a dispatch that
bypassed the aggregate entirely — the suspended-primary case in §4 — by matching the cluster Envoy
actually reports rather than the aggregate name it will never see on that path). All of these are
controller-computed and always overwrite any author-supplied value of the same name; none of them are
part of the author-facing shape above.

Same validation rules as before (`provider` must resolve to `additionalProviders[].as`/`.id` or the
primary; deploy-time error otherwise). The policy is also attached under `upstreamPolicies:` with the
*same expanded params* — the controller synthesizes this second attachment automatically when it sees
`model-failover` under `operationPolicies:`, the same way it already synthesizes provider-scoped
`upstreamPolicies:` attachments for credential/transform policies (§6).

**Post-implementation correction:** `upstreamPolicies:` has no author-facing schema field at all —
removed from `LLMProxyConfigData`/`LLMProviderConfigData` after this session established that nothing,
model-failover included, ever needed an author to hand-write it (every real consumer is
controller-synthesized), and that keeping it open invited exactly the kind of downstream/upstream
credential double-attachment §8 had to fix. `upstreamPolicies:` throughout this document names the
*internal* wire representation the controller populates programmatically — never something an operator
writes in a proxy/provider's YAML. The underlying execution mechanism (`models.Policy.Upstream bool`,
the runtime chain's upstream-policy list, the four `ExecuteUpstreamAttempt*` executor functions) is
unaffected; only the schema door for hand-authoring it was closed. A future policy that also needs
per-attempt execution gets its own controller-side synthesis, the same way Step 3.6 does for
`model-failover`, rather than a generic author-facing attachment point.

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
  (`failover_agg_<routeKey-hash>_<index>`), members in priority order — this same name is written back
  into that entry's `aggregateCluster` param (§3) before the policy instance is attached.
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

1. Matches `reqCtx.Upstream.RouteCluster` against its own `params.targets[].aggregateCluster` (the name
   the controller injected — §3) with a plain string comparison. No independent computation.
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

**Post-implementation correction:** a provider referenced by a `model-failover` chain (the primary
included, via the implicit-target rule) no longer *also* gets its ordinary downstream instance
attached. Initially it did — the downstream instance (gated on "nothing else claimed this provider
yet") and the upstream-attempt instance above coexisted. That downstream instance fires
unconditionally, before `model-failover`'s own routing decision even runs, since nothing has set
`selected_provider` yet at that point. On a cross-provider retry or the suspended-primary bypass, its
header was never removed — since a different provider typically uses a different header name
(`Authorization` vs `X-Api-Key`), `HeadersToSet`'s overwrite semantics never collided with it, so it
rode along to the wrong backend as a leaked, spurious credential. The fix: the controller now skips
the downstream attachment entirely for any provider a `model-failover` chain references, since the
upstream-attempt instance already covers every attempt including the first (Envoy's upstream ext_proc
phase runs on attempt 1 too, not only retries) — one attachment point owns each such provider's
credential/transform decision, not two racing against each other. A provider used only through some
other downstream mechanism (`llm-header-router`, `model-round-robin`) and never named in any
`model-failover` chain is unaffected and keeps its downstream attachment as before.

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

Verified this session: ordering in this whole pipeline is pure Go code-execution order, not a
role-aware sort — `llm-header-router`'s downstream ordering works today only because it's attached via
a code block (`collectOperationLevelLLMPolicies`/`orderedLLMPolicyAttachments` in
`llm_transformer.go`) that appends before the separate loops building the provider-scoped
`transformerPolicies`/`upstreamAuthPolicies` attachments later in the same function. The one sort that
exists (`shouldAttachPathBefore`) never touches the provider-scoped policies at all, so it can't
reorder them relative to the router. `model-failover`'s `upstreamPolicies:` instance must be appended
by a code block that runs before the loop(s) building the provider-scoped `upstreamPolicies:`
attachments — the same pattern, nothing new required in the executor or the SDK for this.

## 10. What this removes from the kernel

- `RouteConfig.Metadata.FailoverTargets` and its xDS sync (`gateway-controller/pkg/xds/translator.go`
  → `gateway-runtime` route metadata) — `model-failover`'s own policy params replace it.
- `resolveBackend`'s failover-entry-matching branch (`kernel/upstream_extproc.go`) — but **this claim
  was corrected during implementation, not confirmed as written**: the aggregate cluster is *not*
  resolvable through the existing generic cluster-name-based resolution. It is not a normally-registered
  provider cluster and appears in no cluster index or route `DefaultUpstream` — a failover aggregate's
  Envoy name (`failover_agg_<routeKey>_<i>`, `pkg/xds/failover_cluster.go`) follows none of the naming
  conventions that resolution assumes, so a plain removal of this branch left `resolveUpstreamRedirect`
  re-prefixing the aggregate name into a cluster that doesn't exist, and left `state.basePath` empty for
  every aggregate-routed attempt. The actual fix: the xDS wire field `upstream_definition_paths`
  (name → base path) was widened to a name → `{cluster_name, base_path}` registry entry per aggregate
  (the primary member's base path), so `resolveUpstreamRedirect` can use a registered cluster name
  verbatim instead of re-deriving one — dual-emitted alongside the original narrow shape under its own
  key (`upstream_definition_targets`) so a gateway-runtime instance one release behind the controller
  during a rolling upgrade still resolves every *ordinary* named upstream's base path from the
  unchanged legacy key, rather than silently losing the whole registry to a type-assertion failure on
  the new shape. The kernel also gained one small, permanent, non-failover-specific addition: it adopts
  `state.basePath` from `Upstream.BasePath` if a header-phase policy set one, generically — this is what
  lets `model-failover` (or anything else) correct the resolved base path from the upstream-attempt
  phase without the kernel knowing what failover is.
  The failover-specific branch's *other* job — producing `{model, provider}` labels — is now produced by
  `model-failover` itself, confirmed as originally claimed.
- The kernel's per-attempt `:path` correction (`upstream_extproc.go`) — **also not a clean removal**.
  `additionalProviders` are loopback upstreams sharing one host:port (`AutoHostRewrite` makes
  `:authority` identical across every chain member), so `:path` is the *only* discriminator between
  providers on a cross-provider retry, and Envoy never recomputes it per attempt. Removing the kernel's
  `:path`-rewrite left every retry past the first reusing attempt 1's path against whichever backend the
  aggregate happened to route it to. Restored as a policy-owned mechanism instead of a kernel one:
  `model-failover`'s upstream `OnRequestHeaders` computes `joinBasePathAndOperation(member.basePath,
  operationPath)` (both now controller-injected params, §3) and returns a `:path` header modification
  when it differs from the request's current path — byte-identical join semantics to the removed kernel
  helper (including the same pre-existing behavior of dropping the query string, which is parity, not a
  regression).
- The kernel's dedicated suspension tracker (§7).
- `ResolvedFailoverProviderHeader`'s kernel-side emission (`upstream_extproc.go`) — `model-failover`
  can set this itself in its own `OnResponseHeaders` when it detects `index > 0` for the current
  attempt, using the same header/analytics-attribution contract, no kernel involvement needed. This
  also covers the suspended-primary bypass case in §4 (dispatch straight at a fallback's own cluster,
  never through the aggregate): the upstream phase identifies which chain member an attempt belongs to
  either by the aggregate cluster name (attempt-count-indexed, the normal path) or, when that doesn't
  match, by a controller-injected `clusterName` on each fallback member (§3) — never on a chain's
  primary member, whose cluster is the route's own default cluster and would otherwise mis-attribute an
  ordinary non-failover request landing on it.

## Non-goals (unchanged from the superseded design)

- `RestApi`-level generic backend failover.
- Per-fallback suspend-duration overrides.
- Updating every transformer policy — `openai-to-anthropic-transformer` first, others as a mechanical
  follow-up.
- Cross-replica shared suspension state.

## Open items

None outstanding. Aggregate-cluster naming, attachment ordering, and the downstream body+routing shape
were all verified against the existing codebase this session (§3, §9, §4) — see each section for the
resolution and citations.

**Post-implementation correction:** §10's original claim that generic cluster-name-based resolution
already covers the aggregate-cluster case, and its silence on the per-attempt `:path` correction, were
both found to be factually wrong once the kernel-side removal (Task 10 of the implementation plan) was
actually attempted — not gaps left open at design time, but a design premise disproven by tracing the
real Envoy/xDS mechanics. §10 has been amended in place to describe the actual mechanism (wire-field
widening + dual-emit, policy-owned `:path` correction, cluster-name-based chain-membership matching for
the suspended-primary bypass) rather than the disproven one. No further open items follow from this;
the amendment is documentation catching up to already-shipped, already-reviewed code.

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
like any other policy:

```yaml
operationPolicies:
  - name: model-failover
    version: v0
    paths: [{ path: /chat/completions, methods: [POST] }]
    params:
      targets:
        - model: gpt-4o
          fallbacks:
            - model: gpt-4o-mini              # same-provider fallback — see §6
            - model: claude-sonnet-4-5-20250929
              provider: anthropic-upstream
        - model: gpt-4o-mini
          fallbacks:
            - model: claude-3-5-haiku
              provider: anthropic-upstream
              upstreamDefinition: anthropic-eu-west   # optional — see below
      statusCodes: [500, 502, 503, 429]   # optional — see below
      suspendDuration: 5
      suspendAfterFailures: 1   # optional — consecutive qualifying failures before suspending; default 1
      maxSuspendDuration: 40    # optional — cap on the exponential backoff in §7; default 8x suspendDuration
```

A fallback may omit `provider:` entirely, naming only a different `model:` on the SAME provider (a
same-provider fallback, e.g. `gpt-4o` → `gpt-4o-mini`) — see §6 for how the upstream phase tells this
case apart from the primary despite both dialing the same physical cluster.

**Post-implementation revision:** the params shape above supersedes an earlier one where each entry
wrapped its primary slot in a nested `target: { model, provider }` object while `fallbacks[]` entries
were already flat. Two changes:

- **Flattened target entries.** `model`/`provider` now sit directly on each `targets[]` entry, at the
  same level `fallbacks[]` entries already used — the primary slot is authored exactly like a fallback
  slot (just without its own further fallbacks), removing an asymmetry that served no purpose.
- **`upstreamDefinition` decouples credential identity from dial target.** `provider` alone used to do
  double duty: it named both which credential/transformer applies (the `selected_provider` identity)
  *and*, by being looked up as a named upstream cluster, which physical backend gets dialed — this is
  exactly the overload `resolvedProvider()` (§7) exists to work around on the runtime side. An optional
  `upstreamDefinition` on any member (a named `additionalProviders[].as`/`.id`, or any hand-declared
  `upstreamDefinitions[].name` — resolution doesn't distinguish their origin) now carries the dial
  target instead, letting one provider's credentials be reused against a differently-named upstream
  (a regional/load-balanced variant, for instance) without touching `provider`. Omitted, it defaults to
  `provider` — today's behavior, unchanged.
- **`statusCodes` makes the escalation trigger configurable again**, restoring (in a stricter, integer-
  status-code form) the configurability the original `resilience.failover.retryOn` had before this
  session hardcoded it to `["5xx"]` (§5's original claim). Omitted, the default stays "any 5xx". Set,
  it REPLACES that default entirely rather than extending it — list every code that should trigger
  failover, 5xx codes included if still wanted. Drives both Envoy's own `retry_policy` (via a new
  `RetriableStatusCodes` carried on `models.RouteFailover`, mapped to Envoy's native
  `retriable_status_codes` + `retry_on: "retriable-status-codes"`) and the policy's own suspension
  trigger (`isFailureStatus`), so the two stay in agreement about what counts as "this target failed."

That's the *author-facing* shape — what an operator (or the LlmProxy transformer, if this stays
generated from higher-level provider config rather than hand-written) writes. The controller expands
it before attaching: each `targets[]` entry gets an `aggregateCluster` field injected with the exact
name the controller assigned that entry's `envoy.clusters.aggregate` cluster in §5 — the *same* params
map is used for both the `operationPolicies:` and `upstreamPolicies:` attachments, so `model-failover`
never needs to compute or guess a cluster name; it only ever reads one it was handed. This removes the
"two independently-written naming schemes must agree" risk entirely — there is exactly one source of
truth (the controller), and matching in §6 is a plain string comparison against `params`, not a
recomputation.

Implementation additionally injects, per member — the entry's own primary AND every one of its
fallbacks alike: `primaryProvider` (the entry's own resolved provider id, so a member authored with no
`provider:` — meaning "the primary" — can still be told apart from an explicit one when seeding
`selected_provider`); each member's own `basePath` and the route's `operationPath` (so the upstream
phase can reconstruct the per-attempt `:path`, see §6); and each member's own resolved Envoy
`clusterName` — every member, not fallbacks only, since the upstream phase's member resolution (§6)
matches on real dialed-cluster identity for both the aggregate-routed path and the suspended-primary
bypass, and a same-provider fallback can share its cluster with its own entry's primary, which needs
both members' identities on hand to tell them apart. All of these are
controller-computed and always overwrite any author-supplied value of the same name; none of them are
part of the author-facing shape above.

Same validation rules as before (`provider` must resolve to `additionalProviders[].as`/`.id` or the
primary; deploy-time error otherwise). The policy is also attached under `upstreamPolicies:` with the
*same expanded params* — the controller synthesizes this second attachment automatically when it sees
`model-failover` under `operationPolicies:`, the same way it already synthesizes provider-scoped
`upstreamPolicies:` attachments for credential/transform policies (§6).

**Post-implementation correction:** `upstreamPolicies:` has no author-facing schema *list* field at
all — removed from `LLMProxyConfigData`/`LLMProviderConfigData` after this session established that
nothing, model-failover included, ever needed an author to hand-write a separate list for it (every
real consumer is controller-synthesized), and that keeping it open invited exactly the kind of
downstream/upstream credential double-attachment §8 had to fix. `upstreamPolicies:` throughout this
document names the *internal* wire representation the controller populates programmatically for
model-failover's own synthesis — never a list an operator writes directly.

That said, hand-authored per-attempt execution IS a supported, deliberate mechanism — just expressed as
a boolean on an existing attachment, not a second list. `Policy.upstream` (on `globalPolicies:`
entries) and `OperationPolicy.upstream` (on `operationPolicies:` entries, added after the removal
above, for parity) both let an author mark ANY attachment to run once per upstream attempt instead of
once downstream — the exact same generic dispatch (`if policyConfig.Upstream { ... }` in
`internal/xdsclient/handler.go`) that model-failover's synthesized attachments use, with no
policy-name special-casing. This is the intended way for a policy other than model-failover to
participate in the upstream-attempt phase: mark it `upstream: true` directly, rather than needing its
own bespoke controller-side synthesis the way Step 3.6 provides for model-failover specifically (that
synthesis exists only because model-failover's upstream instance needs *computed* values — an author
marking an ordinary policy `upstream: true` needs nothing computed, so no synthesis is needed for it).

## 4. Downstream phase

`model-failover`'s `OnRequestBody` distinguishes the downstream invocation from the upstream-attempt one
via `reqCtx.Downstream == nil` (§9's dispatch signal) and, for the downstream case:

1. Parses the client-requested `model` from the JSON body.
2. Matches it against its own `params.targets[]`. No match → no-op (`UpstreamRequestModifications{}`) — the request proceeds through the proxy's normal primary-provider path unchanged.
3. Checks its own suspension state (§7) for the matched entry's primary. Not suspended → routes to the
   entry's aggregate cluster (`UpstreamRequestModifications{UpstreamName: &aggregateClusterName}`),
   reusing the existing upstream-selection mechanism `openai-to-anthropic-transformer` already uses
   (`UpstreamRequestModifications.UpstreamName`).
4. Primary suspended → walks `fallbacks` for the first non-suspended entry and routes directly to that
   fallback's own real cluster, bypassing the aggregate entirely (a known-bad primary is skipped, since
   Envoy has no native concept of "skip priority 0"). This bypass also sets `x-envoy-max-retries: 0` on
   the request: the route's `RetryPolicy` (§5) is attached at the route level, not scoped to the
   aggregate, so it would otherwise still apply here and retry this one bypassed host against itself —
   masking a genuine failure as success if the retry happens to land while the host has recovered.
   Envoy's router filter honors `x-envoy-max-retries` as a per-request override, so this suppresses
   exactly that one bypassed attempt's retry without touching the route's normal aggregate-routed
   behavior.
5. Every member suspended (primary and every fallback) → falls through to the aggregate anyway; nothing
   is left to skip to, so Envoy's own retry-exhaustion behavior applies as if nothing were suspended.

## 5. Controller: xDS generation

Unchanged from the superseded design's §4, except triggered by policy detection instead of a schema
field: when a route has a `model-failover` `PolicyInstance` attached, the controller reads its `params`
(same shape as before) and generates:

- One real cluster per distinct `{model, provider}` pair referenced in `targets`, if one doesn't
  already exist from normal provider/`additionalProviders` cluster creation.
- One `envoy.clusters.aggregate` cluster per `targets[]` entry, deterministically named
  (`failover_agg_<routeKey-hash>_<index>`), members in priority order — this same name is written back
  into that entry's `aggregateCluster` param (§3) before the policy instance is attached.
- `retry_policy: { retry_on: "5xx"` (or `"retriable-status-codes"` + `retriable_status_codes` when
  `statusCodes` is set, §3)`, retry_priority: previous_priorities }`, `auto_host_rewrite: true`,
  `include_attempt_count_in_request: true` on the route.
- The upstream ext_proc filter attached to each aggregate cluster, requesting the
  `xds.upstream_host_metadata` attribute (§6).
- Every real cluster any chain references — the primary and every fallback alike, suspended or not —
  gets its own name stamped onto its single `LbEndpoint`'s metadata, under a well-known filter-metadata
  namespace/key. This is what makes an attempt's dial-accurate identity readable back on the upstream
  side (§6): Envoy reports only the *aggregate's* own name via `xds.cluster_name` for an aggregate-routed
  attempt, never which real member it actually dialed.
- The controller also synthesizes one `upstreamPolicies:` `PolicyInstance` per provider referenced in
  `targets` for each attached credential/transform policy (see §6), each carrying an
  `executionCondition` — the same `selectedProviderExecutionCondition`-style helper
  (`gateway-controller/pkg/utils/llm_transformer.go`) already used to gate their downstream instances.

No `RouteConfig.Metadata.FailoverTargets` sync is needed anymore — `model-failover`'s own `params`
(passed to its `GetPolicy` the same way any policy's params are) *are* the failover-chain declaration;
nothing needs to be pre-resolved and pushed into kernel route metadata separately.

Suspension (§7) is enforced entirely by the policy's own in-process state — never by Envoy-native
`outlier_detection` on these clusters. Applying both would mean two independent mechanisms deciding
whether to avoid the same host, on two different timelines: `outlier_detection`'s ejection duration
scales with cumulative ejection count and never resets for the cluster's lifetime, so it can keep a host
unreachable long after the policy's own suspension window has correctly expired, fighting the policy's
suspend/resume decisions rather than reflecting them. The policy is the single source of truth for
whether a target is currently avoided.

## 6. Upstream-attempt phase

### New SDK surface: which entry, and which real host, this attempt is

Two pieces of dial identity are exposed to policies, both added to `UpstreamRequestContext` and
`UpstreamResponseContext`:

```go
type UpstreamRequestContext struct {
    Name     string // resolved real backend cluster
    URL      string
    BasePath string
    // RouteCluster is the raw xds.cluster_name Envoy reported for this
    // attempt, before any kernel-side member resolution — for an
    // aggregate-routed attempt, this is the aggregate cluster's own name,
    // stable across every attempt against it. Empty for a route with no
    // aggregate/failover involvement.
    RouteCluster string
    // MemberClusterName is the REAL cluster the dialed host itself declares as
    // its own identity, read from that host's xds.upstream_host_metadata —
    // dial-accurate for every attempt, unlike x-envoy-attempt-count, which
    // counts dials rather than priorities. Populated from the per-endpoint
    // metadata the controller stamps on every chain member's cluster (§5).
    MemberClusterName string
}
```

`RouteCluster` identifies *which `targets[]` entry* an attempt belongs to (the aggregate's name is
stable across every attempt against it, since each entry gets its own aggregate). It does not say which
member of that entry was actually dialed — Envoy reports only the aggregate's own name via
`xds.cluster_name` for an aggregate-routed attempt, never the real host — so member identity comes from
`MemberClusterName` instead, populated in `kernel.BuildUpstreamAttemptRequestContext`/
`BuildUpstreamAttemptResponseContext` from the ext_proc `xds.upstream_host_metadata` attribute (parsed
as textproto-serialized `core.Metadata`, not a structured value — Envoy delivers it as a string
containing the textproto encoding).

### `model-failover`'s per-attempt role

Attached via `upstreamPolicies:` (§3), `model-failover`'s `OnRequestHeaders` resolves which chain member
an attempt represents in two passes, scoped by dispatch shape:

- **Through the entry's aggregate cluster** (the normal path): `RouteCluster` matches
  `params.targets[].aggregateCluster` and identifies the entry unambiguously — a proxy's `targets[]`
  entries often share the same primary provider (one base provider serving several model chains), so
  matching is scoped to just this one entry's own primary and fallbacks, never across every entry.
  Within that entry, `MemberClusterName` is matched against the primary's and each fallback's own
  `clusterName` (§3).
- **Directly onto a fallback's own cluster** (the suspended-primary downstream bypass, §4):
  `RouteCluster` is the real fallback cluster's own name, not an aggregate's, so it's matched by name
  across every entry's fallbacks instead.

A same-provider fallback (`gpt-4o` → `gpt-4o-mini`, no `provider:`) dials the exact same physical cluster
as its own entry's primary, so cluster identity alone can't tell the two apart within that entry. When
more than one candidate in the entry matches the same `MemberClusterName`, `x-envoy-attempt-count` is
used as a narrow tiebreaker — matching the candidate whose 1-based chain position equals the attempt
count, defaulting to the first candidate if none matches. This is safe specifically because it's scoped
to ties within one cluster: Envoy's health-based priority-skipping can eject or avoid a host, but it can
never separate two priorities that point at the identical host (ejecting the host ejects both), so
attempt-count can't drift out of sync with chain position for this narrow case the way it could as a
general-purpose signal.

Once resolved, `OnRequestHeaders` writes `reqCtx.SharedContext.Metadata["selected_provider"]`/
`["selected_model"]` to the member's `provider`/`model` — the same metadata key/convention
`llm-header-router` already writes downstream (see
`docs/superpowers/specs/2026-09-21-upstream-policy-reuse-design.md`), just written by a different
policy, in a different phase — and corrects `:path` to that member's own base path (Envoy replays the
first attempt's `:path` verbatim on every retry; only the resolved member's base path can re-point it).
`onUpstreamAttemptRequestBody` re-resolves the same member and rewrites the replayed body's `"model"`
field when it differs — needed specifically for a same-provider fallback, which has no cross-provider
transformer policy to do this for free.

`model-failover`'s `OnResponseHeaders` re-resolves the member one more time, reads the response status,
and records the outcome (§7) — a qualifying failure or a success, either way. This mirrors where the
superseded design put suspension *recording* (upstream phase, since that's where attempt failure is
actually observed), just moved from kernel code into the policy.

## 7. Suspension tracking moves into the policy

Today's kernel-owned `map[string]time.Time` tracker (shared between the downstream pre-emption check
and the upstream-phase recording) is replaced by `model-failover` self-managing its own suspension
state — the exact same pattern `model-round-robin` already uses today for its own suspended-model
tracking, just applied to failover targets instead of round-robin candidates. This removes the
kernel's dedicated tracker entirely; `model-failover` becomes the only place suspension state lives.

State is keyed by `(model, provider)`, purely in-process, one instance per policy attachment (one chain
per instance):

- A qualifying failure (any 5xx by default, or exactly `statusCodes` when set) increments a per-key
  failure counter. Once it reaches `suspendAfterFailures` (default 1 — immediate), the target is
  suspended for a backoff-computed window and the counter resets.
- The suspend window doubles on each further *consecutive* suspend-then-immediately-refail cycle for the
  same key (`suspendDuration` → 2x → 4x → …), capped at `maxSuspendDuration` (default 8x
  `suspendDuration`). This is deliberate escalation for a target that keeps failing right after each
  suspension expires.
- A cycle only counts as "consecutive" if the new failure lands before `streakExpiresAt` — one base
  `suspendDuration` past the *previous* suspension's own expiry. A failure arriving after that deadline
  is a fresh, unrelated incident and starts the backoff over at the base window. Without this bound, two
  qualifying failures for the same key sharing nothing but that key — separate incidents hours apart, or
  just several unrelated requests — would incorrectly compound backoff as if the target had been failing
  continuously the whole time.
- A success clears both the failure counter and the backoff streak entirely — one clean response and the
  target is fully trusted again, at the base window if it ever fails again.

Both suspension state and the route's own `RetryPolicy` (§5) can independently decide to avoid the same
host, which is why the downstream bypass (§4) explicitly suppresses the route's retry for a bypassed
single-cluster dispatch — leaving it active would let Envoy retry (and potentially mask the failure of)
a host the policy just decided to route around.

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
  can set this itself in its own `OnResponseHeaders` when it detects `index > 1` for the current
  attempt, using the same header/analytics-attribution contract, no kernel involvement needed. This
  also covers the suspended-primary bypass case in §4 (dispatch straight at a fallback's own cluster,
  never through the aggregate): the upstream phase identifies which chain member an attempt belongs to
  by the two-pass, dial-accurate resolution in §6 — the aggregate cluster name plus the dialed host's own
  declared identity (`MemberClusterName`) for the normal path, or a fallback-cluster name match for the
  bypass path. Every member's real cluster carries this identity metadata, primary included (§5) — not
  fallbacks only — since a same-provider fallback can share its cluster with its own entry's primary,
  and telling those two apart requires both members' identities to be on hand.

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

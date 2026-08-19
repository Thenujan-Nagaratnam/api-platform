# Decoupled Multi-Target Retry — Design

## Status

Builds directly on `docs/superpowers/specs/2026-08-13-model-failover-design.md`
(the shipped `envoy.clusters.aggregate` + `retry_priority` mechanism) and
`docs/superpowers/specs/2026-08-12-upstream-attempt-retry-refresh-design.md`
(the `UpstreamAttemptPolicy` mechanism). Two research passes were run before
committing to this design — findings captured in
`docs/superpowers/plans/2026-08-14-upstream-retry-multi-policy-questions.md`
and `docs/superpowers/plans/2026-08-14-upstream-retry-requirements-questions.md`
— plus a live Envoy-capability + codebase-coupling investigation whose
findings are folded into this doc directly (see Verified Facts).

## Problem

Two concrete issues with the shipped model-failover implementation:

1. **Spec coupling.** Attaching `oauth2-generator` to a route works today
   only because *some* route-level retry policy exists for Envoy to actually
   retry against — but model-failover's validator hard-rejects
   `resilience.retry` being configured on the same route, forcing anyone
   combining the two to route all retry-triggering configuration through
   model-failover's own params. Adding a policy should never require editing
   an unrelated part of the spec.
2. **Gateway coupling.** `gateway-controller/pkg/xds/translator.go` contains
   ~40 lines that exist only because a policy is literally named
   `"model-failover"` (string equality check), synthesizing a bespoke
   `envoy.clusters.aggregate` cluster + `RetryPriority` for it. A future
   policy needing the same "retry across N named targets" capability would
   need its own equivalent special-casing bolted on, not something it could
   reuse.

## Verified Facts

**F1 — Envoy's upstream filter chain CAN see a failed attempt's response,
today's kernel just doesn't ask for it.** Envoy's ext_proc filter is
dual-direction (`is_upstream_` flag, `ext_proc.cc:317`) and implements
`encodeHeaders` (response direction) even when attached as a per-cluster
upstream filter — it fires per attempt, before Envoy decides whether to
retry. This repo's `ProcessingMode` for the upstream filter currently omits
`ResponseHeaderMode: SEND`; that's a config choice, not an Envoy ceiling.
Envoy's `RetryPriority` extension (`previous_priorities`, already used here)
does **not** get response content — only a host descriptor and stream info —
so it's not a channel for failure-cause info.

**F2 — Whether Envoy's own dynamic metadata survives across the fresh
per-attempt filter-chain instances is unverified.** Rather than depend on
resolving this, this design routes around it entirely (see Component 3).

**F3 — The current aggregate-cluster/retry-policy-building code is already
generic; only *discovery* is coupled to the policy's name.**
`createAggregateCluster(name, memberClusterNames)`,
`modelFailoverGroupMemberClusterNames(...)`, and
`buildModelFailoverRetryPolicy(mf)` (which reads only `StatusCodes`,
`MaxFallbackChainLength()`, and `RequestTimeout` off the params) contain zero
model-failover-specific semantics — they operate purely on an ordered
cluster-name list, a status-code set, and a timeout. Likewise
`ValidateModelFailoverAggregateMembersHaveNoBasePath` operates on a generic
`map[string]string` of base paths, and `ValidateModelFailoverPolicy` is a
two-line "a retry-owning declaration exists AND resilience.retry is also
set → reject" check. The only coupling is the ~3 call sites that *discover*
model-failover by name before handing its params to this otherwise-generic
code.

## Goals

- Attaching any policy never requires editing a different part of the spec.
- No policy-name string checks anywhere in `translator.go` or the validator.
- A future policy gets multi-target Envoy-native retry for free by
  implementing one SDK interface — zero new `translator.go` code.
- Every symbol touched has a name that describes what it actually does now,
  not what the first (single) consumer happened to be called.
- No behavior change, no YAML config change, for existing model-failover
  deployments.

## Non-Goals

- **Multiple simultaneous retry-source policies with merged semantics.**
  Envoy has exactly one `RouteAction.RetryPolicy` and one `RetryPriority`
  extension slot per route — two independent policies each wanting to own
  retry behavior have no coherent merge (whose member-priority order would
  `previous_priorities` even walk?). At most one retry-source per route,
  enforced generically (Component 4).
- **Threading Envoy's own cross-attempt dynamic metadata.** Per F2, left
  unresolved; irrelevant here since Component 3 doesn't depend on it.
- Cross-provider failover / payload-shape transformation — already deferred
  in the original design, unchanged here.

## Design Revision — `RetryTriggerPolicy` (post-review correction)

**The original version of this design was wrong in a way worth recording,
not silently fixing.** It made every policy other than the single declared
`RetrySourcePolicy` a pure *observer* — reacting to retries, never causing
one. That's the correct shape for model-failover (a genuinely different
*destination* per attempt), but wrong for `oauth2-generator`: its need is
"retry the SAME target with a fresh credential," which isn't a
destination-selection problem at all, and it should not have to depend on
an unrelated policy (model-failover) being present just to get its own core
feature (refresh-then-retry) working. Splitting "picks where a retry goes"
from "says a retry should happen" fixes this:

- **`RetrySourcePolicy`** (Component 1, unchanged) — multi-target failover
  chain, still exclusive: at most one per route, a real Envoy limitation
  (one `RetryPriority` slot), not a design choice.
- **`RetryTriggerPolicy`** (new) — declares retry *conditions*
  (status codes, minimum attempt count) without claiming destination
  ownership. Any number of these may coexist on one route; they compose by
  simple union, never conflict, because none of them decide *where* a retry
  goes.

```go
// RetryTriggerDeclaration contributes retry conditions without claiming
// ownership of retry-target selection. Any number of policies on a route
// may declare one — they compose by union, never conflict, unlike
// RetrySourceDeclaration.
type RetryTriggerDeclaration struct {
    RetriableStatusCodes []int
    MinAttempts          int // at least this many total attempts needed
}

// RetryTriggerPolicy is implemented by a policy that needs Envoy to retry
// the SAME target on specific conditions (e.g. a rejected credential),
// without needing a different destination. Composes freely with any other
// RetryTriggerPolicy declaration and with an optional RetrySourcePolicy on
// the same route — see Component 4's composition rule.
type RetryTriggerPolicy interface {
    DeclareRetryTrigger(params map[string]interface{}) (*RetryTriggerDeclaration, error)
}
```

`oauth2-generator` implements this using data it already has —
`DeclareRetryTrigger` returns `{RetriableStatusCodes: p.purgeStatusCodes,
MinAttempts: 2}`, derived from its existing `tokenPurgeStatusCodes` param.
**No new user-facing config field.** This is what makes oauth2-generator's
refresh-then-retry behavior work standalone, on any route, with or without
model-failover present.

**Composition rule (supersedes Component 4's original text below):**
1. Collect every `RetryTriggerPolicy` declaration on the route; union their
   `RetriableStatusCodes`, take the max of their `MinAttempts`.
2. If a `RetrySourcePolicy` is also present: fold that union into the
   *same* aggregate-cluster `RouteAction.RetryPolicy`'s
   `RetriableStatusCodes` — one Envoy retry policy, richer trigger
   conditions, no conflict, no second aggregate cluster.
3. If no `RetrySourcePolicy` exists but trigger declarations do: build a
   **plain, non-aggregate** `RouteAction.RetryPolicy` (no `RetryPriority`,
   no aggregate cluster — ordinary single-cluster Envoy retry) from the
   union alone. This is the path that gives oauth2-generator full
   independence.
4. Two `RetrySourcePolicy` declarations on one route is still a hard
   validation error — the one case that remains genuinely irreducible
   given Envoy's one-`RetryPriority`-slot-per-route limit.

| Route has | Result |
|---|---|
| `oauth2-generator` alone | Auto-built plain `RetryPolicy` from its own `tokenPurgeStatusCodes`. Full same-request refresh-and-retry, zero extra config. |
| `model-failover` alone | Unchanged — aggregate cluster, multi-target failover. |
| Both | One aggregate-cluster `RetryPolicy`; `RetriableStatusCodes` = model-failover's codes ∪ oauth2's `tokenPurgeStatusCodes`. A 401 now triggers a retry even when model-failover's own chain has zero fallbacks (`NumRetries` would otherwise be 0) — a correctness improvement over today's shipped behavior, not just a coupling fix. |
| Two `RetrySourcePolicy` declarations | Still a hard error (irreducible). |

## Design Revision 2 — discovery must be declarative, not Go-interface-based

**Found during implementation prep, confirmed against `go.mod`, not
inferred:** `gateway-controller` (the control-plane process that builds
Envoy xDS config) has **zero dependency** on the policy SDK's implementation
types and never instantiates a policy's actual Go code — it only ever sees
a policy as `{Name string, Params map[string]interface{}}`, straight off
the deployed YAML. Only `gateway-runtime`/`policy-engine` (a separate
process, the data plane) links in and instantiates real policy Go objects.

This means `p.(policy.RetrySourcePolicy)`-style type assertions — anywhere
in `gateway-controller` — are impossible: there is no `policy.Policy` Go
value in that process to assert against. `RetrySourcePolicy`/
`RetryTriggerPolicy` as Go interfaces a policy *implements*, discovered by
`gateway-controller` via type assertion, do not work. (This also explains
why today's `model_failover_validator.go` has its own complete
reimplementation of model-failover's params parsing, `ParseModelFailoverParams`
— a second, hand-kept-in-sync copy of the same logic that also lives inside
`dev-policies/model-failover/model_failover.go`'s own `GetPolicy`. Two
processes, two copies, no shared code path — a real, separate duplication
problem from the one this design already targets.)

**Fix — declarative capability flags in `policy-definition.yaml`, plus one
generic gateway-controller-side parser for a fixed params shape:**

```yaml
# dev-policies/model-failover/policy-definition.yaml, new top-level field:
x-wso2-retry-source:
  groupKeyField: model   # which field in each targets[] entry gateway-controller
                          # treats as the opaque RetryGroup.Key

# dev-policies/oauth2-generator/policy-definition.yaml, new top-level field:
x-wso2-retry-trigger:
  statusCodesField: tokenPurgeStatusCodes
  minAttempts: 2
```

`gateway-controller` already loads and caches `policy-definition.yaml`
content per policy (confirmed — `pkg/config/policy_validator.go`, used for
existing params-schema validation). Discovery becomes: for each policy in a
route's chain, look up its already-loaded definition metadata; if
`x-wso2-retry-source` is present, generically parse `p.Params` against a
**fixed structural shape** (`targets: [{<groupKeyField>: string,
upstreamDefinition: string, fallbacks: [{upstreamDefinition: string}]}],
statusCodes: [int]`) into a `RetrySourceDeclaration` — one parser, works for
any policy whose params happen to already match that shape (model-failover's
existing `targets`/`fallbacks`/`statusCodes` params already do, unchanged,
zero YAML migration). Same pattern for `x-wso2-retry-trigger`. No Go
type assertion, no policy Go code loaded into `gateway-controller`, ever.

**What this removes from the SDK:** `RetrySourcePolicy`/`RetryTriggerPolicy`
as Go interfaces (nothing would ever call `.DeclareRetrySource()` — remove
entirely, don't leave them as unused "documentation"). **What stays:**
`RetryTarget`, `RetryGroup`, `RetrySourceDeclaration`, `RetryTriggerDeclaration`
as plain data structs (now purely `gateway-controller`'s own generic
parser's output type), and `RetrySourceUpstreamName` (the naming formula) —
both safe for `gateway-controller` to import since they carry no policy
implementation code, just types and a pure function. A policy's *runtime*
behavior (setting `UpstreamName`, refreshing a token) still goes through
the existing, unchanged `RequestPolicy.OnRequestBody`/
`UpstreamAttemptPolicy.OnUpstreamAttemptRequest` — those remain real
Go interfaces, because `gateway-runtime` (where they're dispatched) *does*
instantiate real policy objects.

**Component 3 (`UpstreamAttemptResponseObserver`) is unaffected** — it's
dispatched entirely inside `gateway-runtime`'s kernel, which already
instantiates real policy Go objects; the type-assertion discovery there
(Task 9) remains valid as designed. Only the parts of Component 4 that
run inside `gateway-controller` — deciding whether a route needs an
aggregate cluster/response-observer filter — are affected, and gain a third
declarative flag for the same reason:

```yaml
x-wso2-upstream-response-observer: true
```
(No sub-fields needed — unlike the other two, `gateway-controller` doesn't
need to parse anything out of params for this one, only know whether the
route's ext_proc filter needs `ResponseHeaderMode: SEND`.)

## Architecture

Three SDK-level building blocks, one generic gateway-controller consumer,
one generic validator, one kernel wiring change. (`RetryTriggerPolicy`
above is a fourth SDK contract, layered on top of Component 4's discovery
loop per the composition rule — Component 4's text below describes the
pre-revision single-declaration path; the composition rule above is
authoritative where the two differ.)

### Component 1 — the retry-source declaration `gateway-controller` parses out of a policy's params

**Superseded by Design Revision 2 below in one respect:** `RetrySourcePolicy`
as a Go interface a policy *implements* does not work (`gateway-controller`
never instantiates policy Go code — see Design Revision 2). `RetryTarget`/
`RetryGroup`/`RetrySourceDeclaration` remain, as `gateway-controller`'s own
generic parser's output types; discovery and construction are declarative
(`x-wso2-retry-source` in `policy-definition.yaml`), not a Go method call.

`sdk/core/policy/v1alpha2/retry_source.go` (new file):

**Correction (found during implementation prep, not in the original
version of this section):** a route doesn't have one failover chain — it
can have N independent ones, selected at runtime by the policy's own logic
(model-failover: N target groups, chosen per-request by matching
`request.body.model`). Confirmed against the real code:
`ModelFailoverGroupClusterKey(kind, uuid, routeKey, groupModel)` is keyed
per group, and `translator.go`'s aggregate-cluster loop builds one
aggregate cluster *per target group*, not one for the whole policy. So the
declaration is a list of groups, not a single ordered list:

```go
// RetryTarget is one ordered destination within a single retry group.
// UpstreamDefinitionName must resolve to a registered upstreamDefinition
// on the resource the policy is attached to — the same primitive plain
// upstreamDefinitions already provide, not a new one.
type RetryTarget struct {
    UpstreamDefinitionName string
}

// RetryGroup is one independently-selectable failover chain within a
// route (model-failover: one per client-selectable model). Key is an
// opaque, policy-chosen discriminator used only for deterministic cluster
// naming (translator.go never interprets it) — it must be stable across
// deploys and unique within this declaration.
type RetryGroup struct {
    Key            string
    OrderedTargets []RetryTarget // index 0 tried first, index 1 on first failure, etc. Non-empty.
}

// RetrySourceDeclaration is the generic contract translator.go builds one
// aggregate cluster per Group from, regardless of which policy declared
// it. WHICH group applies to a given request is entirely the declaring
// policy's own runtime decision (e.g. OnRequestBody setting UpstreamName)
// — translator.go never needs to know why there are multiple groups, or
// how one gets selected, only that there are some.
type RetrySourceDeclaration struct {
    // Groups: must be non-empty. A policy with only one possible chain
    // (no runtime group-selection logic) still returns a single-entry
    // slice — there is no separate "one chain" shape to keep in sync.
    Groups []RetryGroup

    // RetriableStatusCodes triggers moving to the next target within
    // whichever group matched. Must be non-empty. Route-wide, not
    // per-group — Envoy has one RetryPolicy per route, shared by every
    // group's resolved cluster regardless of which one a given request
    // actually used (same constraint the original ModelFailoverParams
    // already documents).
    RetriableStatusCodes []int

    // PerAttemptTimeout bounds a single attempt; nil uses the route's
    // existing default.
    PerAttemptTimeout *time.Duration
}

// RetrySourcePolicy is implemented by a policy that wants Envoy to
// transparently retry a request against a different, named upstream target
// on failure. Implemented alongside, never instead of, a policy's normal
// RequestHeaderPolicy/RequestPolicy interfaces. Discovery is a plain type
// assertion by gateway-controller — same pattern UpstreamAttemptPolicy
// discovery already uses — never a hardcoded policy name.
//
// At most one policy per route may implement this interface; see Component
// 4 for the generic exclusivity rule (this replaces today's
// model-failover-vs-resilience.retry-specific check).
type RetrySourcePolicy interface {
    DeclareRetrySource(params map[string]interface{}) (*RetrySourceDeclaration, error)
}
```

`model-failover`'s existing Go implementation gains exactly one new method,
deriving this struct from its own already-typed `targets`/`fallbacks`/
`model` params. **No YAML change for existing deployments.**

### Component 2 — Renaming the existing per-attempt request contract

The existing hook is dispatched twice per attempt today (once for headers,
once for body — `upstream_extproc.go`'s `processRequestHeaders` and
`processRequestBody` both call the *same* method), but its name and its
action type only mention headers. Renamed for accuracy (Naming Reference
table below has the full list):

- `UpstreamAttemptPolicy.OnUpstreamAttemptRequestHeaders` → `OnUpstreamAttemptRequest`
- `UpstreamAttemptHeaderModifications` → `UpstreamAttemptRequestModifications`

Fields, semantics, and dispatch behavior (fresh call per attempt, last-write-
wins across chain on collision, fail-open on error) are unchanged — this is
a rename, not a behavior change. `dev-policies/oauth2-generator` is updated
in the same change since it's the only existing consumer.

### Component 3 — `UpstreamAttemptResponseObserver`: optional per-attempt failure-cause visibility

New, additive capability enabled by F1. A policy that wants to condition its
*next*-attempt behavior on *why* the previous attempt failed can observe it:

```go
// UpstreamAttemptResponseContext mirrors UpstreamAttemptContext's per-attempt
// (not per-client-request) scope, on the response side of that same attempt.
type UpstreamAttemptResponseContext struct {
    *SharedContext
    AttemptCount   int
    RequestID      string // x-request-id — stable across every attempt of
                           // one client request; assigned once at the edge,
                           // not per-attempt. The correlation key below.
    ResponseStatus int
}

// UpstreamAttemptResponseObserver is implemented by a policy that wants to
// know why a specific attempt failed, to inform behavior on a later attempt
// of the SAME client request. Read-only — nothing here can mutate the
// response; mutation only ever happens in
// UpstreamAttemptPolicy.OnUpstreamAttemptRequest on a subsequent attempt.
//
// Correlate via RequestID, not Envoy dynamic metadata (F2 is unresolved;
// this design doesn't depend on it). A policy's Go instance is long-lived —
// spans every request and every attempt for the route's lifetime, same as
// model-round-robin's own suspendedModels map already relies on — so a
// policy records (RequestID -> observed cause) in its own in-memory state
// here, and reads it back in OnUpstreamAttemptRequest on the next attempt.
// Entries must be cleaned up on the request's final response (downstream
// OnResponseHeaders) or a bounded TTL, to avoid unbounded growth from
// requests that error out before a later attempt ever reads the entry.
type UpstreamAttemptResponseObserver interface {
    OnUpstreamAttemptResponse(ctx context.Context, actx *UpstreamAttemptResponseContext)
}
```

Kernel wiring: the upstream ext_proc filter's `ProcessingMode` gains
`ResponseHeaderMode: SEND`, but only for clusters backing a route where some
policy in the chain implements this interface — same opt-in-capability-flag
pattern already used for `RequestBodyMode: BUFFERED` today, generalized to a
third flag rather than growing the existing one. Existing cause-agnostic
observers (`oauth2-generator`) implement nothing new and are unaffected.

### Component 4 — Generic discovery + build in `translator.go`

**Superseded by Design Revision 2:** the type-assertion loop below does not
work in `gateway-controller` (no policy Go instantiation there). Replace
`p.(policy.RetrySourcePolicy)` with a lookup against the policy's cached
`policy-definition.yaml` metadata (`x-wso2-retry-source` present?), and
`rsp.DeclareRetrySource(p.Params)` with a call to the new generic parser
(`config.ParseRetrySourceParams(p.Params, groupKeyField)`) described in
Design Revision 2. The composition rule and per-group aggregate-cluster
construction below this point are otherwise unchanged.

Replace the hardcoded loop (`translator.go:300-327`, `if p.Name ==
"model-failover"`) with a generic, declaratively-driven loop:

```go
for _, p := range chain.Policies {
    rsp, ok := p.(policy.RetrySourcePolicy)
    if !ok {
        continue
    }
    decl, err := rsp.DeclareRetrySource(p.Params)
    // One aggregate cluster PER GROUP (mirrors today's per-target-group
    // loop, keyed by Group.Key instead of a model-failover-specific
    // groupModel) — using the SAME createAggregateCluster/retry-policy-
    // building code that exists today (F3 — already generic, needs no
    // changes beyond the rename).
    for _, group := range decl.Groups {
        if len(group.OrderedTargets) < 2 {
            continue // single-target group: no failover, no aggregate needed
        }
        memberNames := retrySourceTargetClusterNames(group, apiKind, apiID, mainClusterName)
        aggName := RetrySourceAggregateClusterKey(apiKind, apiID, routeKey, group.Key)
        aggCluster, err := t.createAggregateCluster(aggName, memberNames)
        // ...
    }
}
```

Per F3, the cluster/retry-policy-building functions need no logic changes —
only renames (Naming Reference table) to stop describing them as
model-failover-specific.

**Cluster de-duplication (scalability fix, found during this session's live
testing):** today, a target referenced both as a route's default upstream
(`upstream.ref`/`upstream.url`) *and* as a named `upstreamDefinition` gets
two independent Envoy clusters pointing at the identical backend (confirmed
live: `upstream_main_sample-backend_5000` and
`upstream_LlmProvider_<id>_azure-eastus` for the same URL), because
`resolveUpstreamCluster` (host+scheme-keyed naming) and the
`UpstreamDefinitions` loop (name-keyed naming) are two independent code
paths. This design introduces one shared
`resolveOrCreateUpstreamDefinitionCluster(name string)` helper, memoized per
API resource, that both paths call — eliminating the duplicate cluster and
its doubled connection-pool/health-check overhead.

### Component 5 — Generic validator, replaces `model_failover_validator.go`

`retry_source_validator.go` (renamed):

- `ValidateModelFailoverAggregateMembersHaveNoBasePath` → `ValidateRetrySourceTargetsHaveNoBasePath` — same logic (F3 confirms it's already generic), operating on any `RetrySourceDeclaration.Groups[].OrderedTargets`.
- `ValidateModelFailoverPolicy` → `ValidateAtMostOneRetrySourcePerRoute` — generalizes "model-failover XOR resilience.retry" into "at most one of {any `RetrySourcePolicy` declaration, `resilience.retry`} per route." A second future retry-source policy is automatically covered — no new pairwise check to write.
- `ValidateModelFailoverForOperations` → `ValidateRetrySourcesForOperations` — same synchronous pre-persist wiring (`llm_deployment.go`/`api_deployment.go`) already fixed for model-failover, now walking "any `RetrySourcePolicy`" instead of one name.

Model-failover-specific structural validation (e.g. "each target needs a
non-empty `model` field") stays where it already belongs — in
`ParseModelFailoverParams`, the policy's own params-parsing — never in this
file, which only ever validates the generic contract.

### Component 6 — Kernel renames (`upstream_extproc.go`)

`processRequestBody` is ambiguous with unrelated downstream request-body
processing elsewhere in the kernel; renamed to `processUpstreamAttemptRequestBody`
to make the "this is the upstream per-attempt phase" scope explicit at the
call site, matching `processRequestHeaders` → `processUpstreamAttemptRequestHeaders`.
New `processUpstreamAttemptResponse` handler added for Component 3,
dispatched only when the route's capability flags include response
observation.

## Naming Reference (full list)

| Old | New | Why |
|---|---|---|
| `OnUpstreamAttemptRequestHeaders` | `OnUpstreamAttemptRequest` | Dispatched for body mutation too, not headers-only |
| `UpstreamAttemptHeaderModifications` | `UpstreamAttemptRequestModifications` | Carries `Body` as well as `HeadersToSet` |
| `p.Name == "model-failover"` checks | `p.(policy.RetrySourcePolicy)` type assertion | Generic capability discovery, not name matching |
| `modelFailoverGroupMemberClusterNames` | `retrySourceTargetClusterNames` | Not model-failover-specific (F3) |
| `ModelFailoverGroupClusterKey` | `RetrySourceAggregateClusterKey` | Same |
| `buildModelFailoverRetryPolicy` | `buildRetrySourceRetryPolicy` | Same |
| `isModelFailoverAggregateCluster` | `isRetrySourceAggregateCluster` | Same |
| `model_failover_validator.go` | `retry_source_validator.go` | File-level generalization |
| `ValidateModelFailoverAggregateMembersHaveNoBasePath` | `ValidateRetrySourceTargetsHaveNoBasePath` | Same |
| `ValidateModelFailoverPolicy` | `ValidateAtMostOneRetrySourcePerRoute` | Describes the actual rule, not the two names it happened to first apply to |
| `ValidateModelFailoverForOperations` | `ValidateRetrySourcesForOperations` | Same |
| `processRequestBody` (upstream kernel) | `processUpstreamAttemptRequestBody` | Disambiguates from downstream request-body processing in the same kernel package |
| `processRequestHeaders` (upstream kernel) | `processUpstreamAttemptRequestHeaders` | Same |
| *(new)* | `UpstreamAttemptResponseObserver`, `OnUpstreamAttemptResponse`, `UpstreamAttemptResponseContext` | New Component 3 capability |
| *(new)* | `RetrySourcePolicy`, `RetrySourceDeclaration`, `RetryTarget` | New Component 1 capability |
| *(new)* | `RetryTriggerPolicy`, `RetryTriggerDeclaration` | New capability (Design Revision) — composable retry conditions without destination ownership; makes `oauth2-generator` retry-independent of `RetrySourcePolicy` |

## Failure Mode

Unchanged from the original design's convention: any failure in a
`RetrySourcePolicy`'s own logic, or an `UpstreamAttemptResponseObserver`'s
own logic, fails open — no retry-target override, no cause recorded, request
proceeds as if the interface weren't implemented for that attempt. Never a
new way to fail.

## Migration / Rollout

1. SDK: add Component 1 and 3 types; apply Component 2 renames (breaking,
   but the only consumer — `oauth2-generator` — is updated in the same
   change).
2. `gateway-controller`: Component 4 (generic discovery, cluster dedup),
   Component 5 (validator rename + generalization).
3. `gateway-runtime`: Component 6 kernel renames + new response-observer
   dispatch.
4. `dev-policies/model-failover`: implement `DeclareRetrySource`; remove
   nothing from its existing params surface.
5. No end-user-visible YAML/config change for any existing deployment at any
   step — this is entirely an internal-contract refactor plus one new
   optional capability.

## Testing Strategy

- Unit tests for `retry_source_validator.go`'s generalized rules, including
  a regression test asserting a *second*, synthetic `RetrySourcePolicy`
  implementation triggers `ValidateAtMostOneRetrySourcePerRoute` against
  `resilience.retry` and against model-failover, without any new code in the
  validator (proves genericity, not just a passing test for one pairing).
- Existing model-failover IT feature files (`gateway/it/features/model-*`)
  must pass unmodified — proves zero behavior/config-surface change.
- New unit test for cluster de-duplication: a resource whose `upstream.ref`
  target is also a named `upstreamDefinition` produces exactly one Envoy
  cluster for it, not two.
- New unit test for `UpstreamAttemptResponseObserver`: a synthetic observer
  policy correctly correlates a recorded failure cause across two attempts
  of the same `RequestID`, and correctly does *not* see a cause recorded
  under a different `RequestID` (concurrent-request isolation).
- New unit tests for the `RetryTriggerPolicy` composition rule: (a)
  `oauth2-generator` alone on a route produces a plain, non-aggregate
  `RetryPolicy` with no `RetryPriority`; (b) `oauth2-generator` +
  model-failover on one route produces exactly one aggregate `RetryPolicy`
  whose `RetriableStatusCodes` is the union of both; (c) two synthetic
  `RetrySourcePolicy` declarations on one route is still rejected at
  validation time; (d) two synthetic `RetryTriggerPolicy` declarations
  (neither being oauth2-generator) compose without error, proving the rule
  is generic and not oauth2-specific.

## Open Items Carried Forward

- F2 (Envoy cross-attempt dynamic-metadata visibility) remains unresolved.
  Not required for this design; worth a standalone follow-up spike if a
  future need specifically wants Envoy-native (not policy-engine-side)
  cause propagation.
- **F4 (new, unverified this session) — `x-request-id` stability across
  attempts.** Component 3's correlation mechanism assumes `x-request-id` is
  assigned once per client request and stays constant across every retry
  attempt of it. This is standard Envoy behavior (request ID is generated
  once at the edge, and each attempt replays the original request), but
  unlike F1/F2 it was not independently confirmed against Envoy source in
  this session's research — it's carried in as a general-knowledge
  assumption, not a verified fact. Should be confirmed (a quick source check
  or a throwaway live spike logging `x-request-id` across a forced 2-attempt
  retry) before or during implementation of Component 3 specifically; the
  rest of this design (Components 1, 2, 4, 5, 6) does not depend on it.

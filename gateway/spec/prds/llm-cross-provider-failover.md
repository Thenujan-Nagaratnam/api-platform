# FR7: Cross-Provider Failover for LLM Requests

**Status:** Proposed — design only, no implementation yet. This is a pre-implementation
proposal: everything described as "existing" below has been verified directly against the
current codebase; everything described as "new" does not exist yet.

**Revision history:**
- Rev 1: proposed reusing `LlmProxy`'s existing `additionalProviders`/`transformer`/
  `selected_provider` mechanism as the failover vehicle. Verifying that mechanism's actual
  capabilities (not just its schema) showed it doesn't fit — see "Why `LlmProxy`'s existing
  transformer mechanism doesn't fit" below. Replaced with a mechanism shaped after
  `model-failover` itself, but as a new sibling policy (`provider-failover`).
- Rev 2: cross-provider failover is the same *feature* as same-template failover from an
  operator's perspective — a target just happens to sometimes be a different template.
  Splitting that into two policies an operator has to choose between doesn't make sense.
  `model-failover` itself gains the config surface (a `template` field per target), not a new
  policy. Rev 2 initially had `model-failover`'s own `OnUpstreamAttemptRequest` doing the body
  conversion inline, and (implicitly) doing auth too — reasonable for template conversion
  (see Rev 2's reasoning, still valid), but wrong for auth: different targets often need
  entirely different credentials, sometimes different *auth types* (an API key here, OAuth2
  there) — concentrating that into `model-failover`'s own code duplicates a whole existing
  policy family (`oauth2-generator`, `api-key-auth`, …) inside a policy whose job is routing.
- Rev 3: separates *config surface* from *implementation ownership*. An operator still
  configures everything through `model-failover`'s own params — one policy, as Rev 2 decided.
  But the actual auth and body-conversion *work* is done by existing/new policies, unmodified
  in their own logic, auto-injected into the chain and scoped to fire only on the specific
  Envoy attempt their target corresponds to. `model-failover`'s own Go code turns out to need
  **no changes at all** for this — see "Where the work happens" below.
- Rev 4: two corrections found by working through a maximally complicated scenario end to end
  (see "Worst-case walkthrough" below), plus one placement fix:
  - `template`/`auth` move from `model-failover`'s own fallback entries onto `UpstreamDefinition`
    itself — reasoning at the time: they're properties of the backend, and `UpstreamDefinition`
    is already the reusable-backend-descriptor construct this codebase has. Reverted in Rev 5 —
    see below.
  - Attempt-index scoping (Rev 3) is wrong whenever a request reaches a target via
    `model-failover`'s own existing skip-ahead-after-suspend redirect, which bypasses the
    aggregate cluster and always dials as `AttemptCount == 1` regardless of the target's
    position in the priority list. Fix: scope by the resolved destination (`:authority`, read
    from `UpstreamAttemptContext.Headers`) instead of by attempt number — partially verified
    (see Open Questions). Still holds in Rev 5.
  - `template: awsbedrock`/`gemini` (pathParam-located model identity) cannot work as a
    cross-provider fallback target at all with the SDK as it exists today —
    `UpstreamAttemptRequestModifications` has no field to rewrite the outgoing path per
    attempt. A real, if small, SDK change, not a policy-level workaround. Still holds in Rev 5.
- Rev 5 (current): explicit direction on two points Rev 4 got wrong, plus the genericity goal
  made concrete:
  - **`UpstreamDefinition` is untouched — no `auth`/`template` fields added to it.**
    `UpstreamDefinition` keeps its existing, single meaning: a same-provider backend that
    shares this API's whole-operation `upstream.auth`. That sharing is *expected, accepted
    behavior* for any target/fallback referencing a plain `upstreamDefinition` — not a gap this
    FR closes. A genuinely different provider needing its own credential/template is a
    *different kind of reference*, not a property bolted onto the existing one — see
    `additionalProviders` below.
  - **The mechanism generalizes to any policy, not just `model-failover`**, by extending
    machinery that was already built to be policy-agnostic: `policy.RetryTarget` (SDK) and
    `config.ParseRetrySourceParams` (gateway-controller) — the exact same generic
    `x-wso2-retry-source` plumbing every retry-source-declaring policy already goes through —
    rather than anything specific to `model-failover`'s own params. Any current or future
    policy declaring `x-wso2-retry-source` gets this for free.

## Overview

Today, failing over an LLM request from a struggling backend to a healthy one only works
*within* a single provider's wire format, using one uniform credential. `model-failover` (an
`LlmProvider`-scoped policy) can retry `gpt-4o` against a `gpt-4o-mini` fallback because both
speak the same template and the same operation-wide auth applies to both. It cannot fail over
to a backend with a different wire format, and it has no way to use a different credential —
let alone a different *kind* of credential — for a specific fallback.

This FR closes both gaps with a new, named, resource-level concept —
`LlmProvider.spec.additionalProviders[]`, each entry an independently-authenticated,
independently-templated backend — that a target/fallback can reference *instead of* a plain
`upstreamDefinition`. gateway-controller reads that reference at translation time to
auto-inject the right existing-or-new policy for that specific target, scoped to fire only
when that specific backend is actually being dialed. The extension point is generic — built
into the same `x-wso2-retry-source` machinery every retry-source-declaring policy already
uses, not specific to `model-failover` — so any future policy with an ordered target chain
gets this capability for free, not just this one.

## Problem

An operator wanting an LLM request to survive a provider outage by falling over to a
*differently-shaped, differently-credentialed* backend (say, an OpenAI primary using an API
key, and an Anthropic backup needing OAuth2) has no way to do that today. The request would
reach the Anthropic backend still shaped for OpenAI's API, with the wrong kind of credential
entirely, and fail outright — and even if it somehow succeeded, the response would come back
Anthropic-shaped to a client expecting OpenAI's contract.

## Current State (verified in code)

**`model-failover`'s own mechanism, which this design builds on:** an aggregate Envoy cluster
(primary + fallbacks, priority-ordered) built at translation time, a `RetryPolicy` with
`retry_priorities: previous_priorities`, and `OnUpstreamAttemptRequest` — a per-*attempt* hook
(not per-request) that fires once per Envoy-driven dial, including native retries, and can
rewrite that attempt's outgoing headers/body. Critically, `OnUpstreamAttemptRequest` runs in a
genuinely separate ext_proc stream from the downstream request/response phases — it never
receives `SharedContext`, so it cannot read metadata another phase set. `model-failover` works
around this by making the fallback-for-this-attempt lookup purely a function of `AttemptCount`
against its own static, already-parsed config — no metadata needed. This same trick is what
makes attempt-scoped policy composition (below) possible at all.

As of the earlier work in this same plan (the `2026-08-17-generalized-retry-conditions` retry
work), **the aggregate-cluster/`RetryPolicy` machinery is fully generic in
gateway-controller** — driven by a policy's own `x-wso2-retry-source`/`x-wso2-retry-conditions`
declarations, not hardcoded to `model-failover` by name.

**This generality goes one level deeper than the cluster-building step, and it's the reason
this FR's mechanism can be policy-agnostic too.** `config.ParseRetrySourceParams` — the
function that reads *any* retry-source-declaring policy's `targets`/`fallbacks` config into
`policy.RetrySourceDeclaration{Groups: []RetryGroup{OrderedTargets: []RetryTarget{...}}}` — is
itself generic, driven only by that policy's own declared `groupKeyField`/`targetsField`, not
`model-failover`'s params specifically (confirmed directly: `ParseRetrySourceParams`'s own doc
comment states it works "for ANY policy whose policy-definition.yaml declares
x-wso2-retry-source"). `policy.RetryTarget` — the per-target descriptor this function
produces — lives in the shared SDK (`sdk/core/policy/v1alpha2`), today just
`{UpstreamDefinitionName string}`. Extending *this one shared type*, and the one parser that
produces it, is what makes the new capability available to every retry-source-declaring
policy at once — see Design.

**Why `LlmProxy`'s existing transformer mechanism doesn't fit:**
`LlmProxy.spec.additionalProviders[].transformer`/`.auth` (real, wired, tested —
`llm_transformer.go`'s `proxyTransformerPolicy`/`proxyUpstreamAuthPolicy`) inject an ordinary
conditional policy whose `ExecutionCondition` (`request.Metadata['selected_provider'] ==
'<name>'`) is CEL evaluated only in downstream phases, against `SharedContext.Metadata` —
which doesn't exist in the upstream-attempt phase. And `UpstreamAttemptRequestModifications`
has no way to redirect the dial target at all — destination selection happens once, before
the first dial. So this mechanism can express "pick a provider once, before dispatch" but not
"retry against a different provider after a failure." Confirmed nothing in the codebase ever
writes `selected_provider`, and no translator policy has ever been implemented — never used.

**But `proxyUpstreamAuthPolicy`'s auth-*type* dispatch is directly reusable.** Its
`switch auth.Type { case apiKey: ...; case oauth2: ...; case other: ...; case none: ... }`
already resolves an `auth: {type, ...}` block to the right concrete policy — that part isn't
broken, only its *activation* (CEL/`SharedContext`) was. Reused here with a different
activation mechanism (attempt-index scoping, below).

## Goals

- One config surface — `model-failover`'s own params — for failover whether or not it happens
  to cross template or credential boundaries. An operator never chooses between policies.
- **The mechanism is generic, not `model-failover`-specific.** It's built by extending shared,
  already-policy-agnostic machinery (`policy.RetryTarget`, `config.ParseRetrySourceParams`) —
  any current or future policy declaring `x-wso2-retry-source` gets cross-provider targets for
  free, with zero additional gateway-controller work for that policy.
- **`UpstreamDefinition` is never modified.** A same-provider target/fallback referencing a
  plain `upstreamDefinition` continues to share this API's whole-operation `upstream.auth`,
  exactly as today — that's expected, accepted behavior, not a gap. Only a target explicitly
  referencing an `additionalProviders` entry gets its own credential/template.
- A fallback target's request is converted to its own template (when it references an
  `additionalProviders` entry declaring one) and authenticated with its own credential/type,
  independent of the primary's and independent of any other target's.
- The response is converted back to the primary's template regardless of which target served
  it.
- **`model-failover`'s own Go code needs zero changes.** The new capability lives entirely in
  (a) the shared SDK/parser extension, whose values `model-failover` never reads at runtime,
  and (b) new gateway-controller translation logic that resolves an `additionalProviders`
  reference to auto-inject other policies.
- Existing auth policies (`oauth2-generator`, `api-key-auth`, …) keep their own logic
  completely unmodified except one small, uniform, opt-in guard clause (see below) — no
  duplicated token-fetch/cache/credential logic anywhere.
- Adding a new provider template, or a new auth type, is additive — one adapter, or reuse of
  an existing auth policy — not an architectural change.

## Non-Goals

- Changing what a plain `upstreamDefinition` means, or adding any field to it. It stays a
  same-provider, shared-auth backend descriptor, unchanged.
- `LlmProxy`'s `additionalProviders`/`transformer`/`selected_provider` mechanism as a
  *before-dispatch* multi-provider routing tool (content-based routing, A/B testing) remains
  legitimate and untouched — this FR doesn't build failover on it, but doesn't remove it either.
  (Naming note: `LlmProvider.spec.additionalProviders`, introduced by this FR, is a distinct,
  new field on a different resource — not a change to `LlmProxy`'s existing field of the same
  name.)
- Streaming-response translation (SSE chunk-by-chunk reshaping) — materially harder than a
  single-body translation; an open question, not solved here.
- Supporting every provider template/auth type on day one — see Phasing.

## Design

### Config surface — a new, distinct reference type; `UpstreamDefinition` and `model-failover`'s own schema both unchanged in meaning

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
spec:
  upstream: {url: https://api.openai.com/v1, auth: {type: apiKey, ...}}  # unchanged
  upstreamDefinitions:                              # unchanged — same-provider, shared upstream.auth
    - name: primary
      upstreams: [{url: https://api.openai.com/v1}]
    - name: fallback-1
      upstreams: [{url: https://backup.openai.com/v1}]
  additionalProviders:                              # NEW — independently-authenticated, independently-templated backends
    - id: anthropic-backup
      upstream: {url: https://api.anthropic.com/v1}
      template: anthropic
      auth:                                          # resolved via the SAME type-dispatch switch
        type: oauth2                                 # upstream.auth already uses (LLMUpstreamAuth
        policyParams:                                 # shape) — apiKey/oauth2/other/none
          tokenEndpoint: https://idp.example.com/oauth2/token
          clientId: anthropic-client
          clientSecret: anthropic-secret

# model-failover's own config: one new field per fallback, additionalProvider, used INSTEAD
# of upstreamDefinition (mutually exclusive) when this target crosses provider boundaries:
policies:
  - name: model-failover
    version: v0
    paths:
      - path: /chat/completions
        methods: [POST]
        params:
          statusCodes: [500, 502, 503]
          targets:
            - model: gpt-4o
              upstreamDefinition: primary
              fallbacks:
                - model: gpt-4o-mini
                  upstreamDefinition: fallback-1        # same provider — shares upstream.auth, unchanged
                - model: claude-3-5-sonnet
                  additionalProvider: anthropic-backup   # NEW field — own auth/template
```

### Where the work happens — a generic SDK/parser extension, not `model-failover`-specific

Two small, shared changes, not a `model-failover`-specific one:

1. **`policy.RetryTarget`** (SDK, `sdk/core/policy/v1alpha2`) gains a second field:
   `AdditionalProviderName string`, alongside the existing `UpstreamDefinitionName string` —
   mutually exclusive, exactly mirroring how `additionalProvider`/`upstreamDefinition` are
   mutually exclusive in a target/fallback entry.
2. **`config.ParseRetrySourceParams`** (gateway-controller) — already the single, generic
   parser every retry-source-declaring policy's `targets`/`fallbacks` goes through — reads a
   fixed `additionalProvider` key at each target/fallback entry alongside the existing fixed
   `upstreamDefinition` key (both are already fixed key names in this shared structural shape,
   not per-policy-configurable — `groupKeyField`/`targetsField` are the only configurable
   parts), populating `RetryTarget.AdditionalProviderName` when present.

Because both changes live in shared code every retry-source-declaring policy already routes
through, **`model-failover`'s own `GetPolicy`/`OnRequestBody`/`OnUpstreamAttemptRequest` need
zero changes**, and any *other* policy declaring `x-wso2-retry-source` gets the identical
capability automatically — the whole point of the genericity goal.

For each `RetryTarget` whose `AdditionalProviderName` is set, gateway-controller resolves it
against `spec.additionalProviders` (by `id`) and auto-injects one or two *additional* policy
instances into the operation's chain, appended after the declaring policy so they run on its
already-corrected body, each scoped to fire only when that specific backend is being dialed:

- **`auth`** → reuses `proxyUpstreamAuthPolicy`'s existing type-dispatch switch (`apiKey` →
  `api-key-auth`, `oauth2` → `oauth2-generator`, …) to resolve which concrete policy to inject
  — this logic is already correct and doesn't change; only its activation does.
- **`template`** → resolves to that template's adapter policy (naming/resolution TBD — see
  Open Questions), injected the same way.

A target whose `UpstreamDefinitionName` is set instead (today's only case, and still the
common case) triggers none of this — no auto-injection, no new policy instances, the exact
translation-time path that exists today.

### Attempt-scoped activation — the one new, small, generic mechanism

Every auto-injected instance gets a new, optional scoping param. Each policy capable of
`OnUpstreamAttemptRequest` gets one small, uniform, additive guard:

```go
if p.scopedToAttempt && !thisIsMyAttempt(actx) {
    return policy.UpstreamAttemptRequestModifications{} // not my attempt — fail open, no-op
}
```

Unset for every *existing*, whole-operation use of these policies — completely inert, zero
behavior change for every deployment that doesn't use this feature.

**The guard has to cover every hook the policy has, not just `OnUpstreamAttemptRequest`.** An
auto-injected instance is a *plain* chain member (no CEL condition — that's exactly the
mechanism that doesn't work here), so its *downstream* hooks fire too unless also suppressed.
Concretely: `oauth2-generator`'s `OnRequestHeaders` — the hook that injects the token for the
*initial* dispatch — would otherwise fire on the very first pass, before Envoy has dialed
anything, injecting the Anthropic-scoped instance's token onto the request Envoy is about to
send to the *primary*. Every hook on an attempt-scoped instance needs the same "only ever act
from `OnUpstreamAttemptRequest`, and only for my own target" guard — not just the one hook this
design happens to care about.

**`thisIsMyAttempt(actx)` — what actually discriminates "my attempt," and why this matters more than it looks.** Two candidate implementations:

1. **`AttemptCount`-based** (Rev 3's original proposal): compare `actx.AttemptCount` against a
   number computed once, at translation time, from the target's position in the aggregate
   cluster's priority list. Simple, and correct for a request that reaches this target *via*
   Envoy's own retry walk. **Wrong** for a request that reaches it via `model-failover`'s own,
   already-shipped skip-ahead-after-suspend redirect (see Worst-case walkthrough) — that path
   dials the target directly, bypassing the aggregate cluster entirely, so Envoy's own
   `AttemptCount` for that dial is always `1`, never the position-derived number the
   auto-injected instance was configured to expect.
2. **`:authority`-based** (the fix): compare the resolved destination Envoy is actually
   dialing — read via `actx.Headers.Get(":authority")` — against this target's own known
   hostname. Correct regardless of *how* the request got routed there, because it keys off
   *where the request is actually going*, not a position-in-a-list assumption computed before
   the request ever arrived. Partially verified: confirmed the kernel doesn't filter this
   pseudo-header out (see Open Questions) — not yet confirmed against Envoy's real wire
   behavior on the upstream filter chain.

**Ordering guarantees correctness.** `model-failover`'s own model-field rewrite (already
shipped, template-aware via `requestModel`) always runs first, in the primary's own shape.
Only after that does the auto-injected adapter (if any) convert the *whole*, already-correct
body to the target's shape — so the adapter never needs to know anything about model-name
placement itself, just full-body conversion between two known templates.

**Mixed auth types across fallbacks fall out of this for free.** An OpenAI primary (API key)
and an Anthropic fallback (OAuth2) auto-inject *two different* policies
(`api-key-auth`/`oauth2-generator`), each scoped to its own target — no new dispatch logic
needed beyond what `proxyUpstreamAuthPolicy` already has.

### Response conversion — genuinely new capability, still not inside `model-failover`

Converting the target's response back to the primary's shape needs response-*body* access
(`ResponseBodyMode: BodyModeBuffer`) — a capability `model-failover` doesn't have today and,
per the design above, still doesn't need: the response-side conversion is the *same*
auto-injected adapter policy's job (it already knows both templates).

**Unlike the request side, this likely does NOT inherit Gap 2** — worth stating precisely,
not just asserting. `ProcessingMode.ResponseBodyMode` governs the *downstream* response phase
(the same one `model-failover`'s own `OnResponseHeaders` already runs in), which — unlike the
upstream-attempt phase — has full `SharedContext` access. `model-failover` already correctly
disambiguates skip-ahead vs. normal-walk responses there today, using `SharedContext.Metadata`
it stashed during `OnRequestBody` (`metaStartIndexKey`) rather than `x-envoy-attempt-count`
alone. If the auto-injected adapter's response hook runs in that same phase, it could use the
same disambiguation — **but that means reading `model-failover`'s own, currently-private
metadata keys from a different policy**, a real coupling this design hasn't addressed. Left as
an open question rather than assumed solved (see Open Questions) — resolving it might mean
`model-failover` needs to expose these keys as a documented, stable contract other policies
can rely on, not just internal bookkeeping.

### End-to-end flow (example)

1. `additionalProviders` declares `anthropic-backup` (`template: anthropic`, an `oauth2`
   `auth` block); the fallback references it via `additionalProvider: anthropic-backup`
   instead of `upstreamDefinition`. gateway-controller resolves that reference and
   auto-injects an `oauth2-generator` instance (this target's own credentials, scoped to this
   backend's own resolved `:authority`) and an `anthropic-adapter` instance (same scoping),
   both appended after `model-failover`.
2. Client's request dispatches unmodified via the aggregate cluster's priority-0 member
   (`primary`, a plain `upstreamDefinition` — shares `upstream.auth` as always) — neither
   auto-injected policy's destination matches, so both no-op.
3. Primary returns `503`. Envoy's native retry redispatches to priority-1
   (`anthropic-backup`).
4. This dial's resolved `:authority` now matches. `model-failover`'s own hook rewrites the
   model field (unchanged logic, keyed off `AttemptCount` as it already was).
   `oauth2-generator`'s scoped instance now matches — injects its own fetched token.
   `anthropic-adapter`'s scoped instance now matches — converts the already-model-corrected
   body from `openai` shape to `anthropic` shape.
5. Anthropic responds. `anthropic-adapter`'s response-phase hook (matching the same resolved
   destination) converts the response body back to `openai` shape.
6. Client sees a clean, OpenAI-shaped response — unaware failover crossed provider *or*
   credential boundaries.

### Canonical-IR adapter implementation, not pairwise ones

Unchanged from Rev 1: a translator per *ordered pair* of providers is O(n²). Each adapter
translates to/from the *primary's* template for that specific deployment (not a fixed
platform-wide format), giving O(n) adapters, not O(n²) pairs.

### Worst-case walkthrough

The design above reads clean in isolation. Both Rev 4 corrections were found only by
constructing the most complicated realistic scenario and tracing it through attempt by
attempt — worth keeping as a permanent stress test of the design, not just a derivation of
the two fixes.

**Setup.** One `LlmProvider`, template `openai`. Two independent target groups — Group A's
chain deliberately mixes a plain same-provider fallback (shares `upstream.auth`, exactly
today's behavior) with two `additionalProviders`-referenced fallbacks (each fully independent
auth), so the walkthrough exercises coexistence, not just the new capability in isolation:

```yaml
additionalProviders:
  - id: anthropic-backup
    upstream: {url: https://api.anthropic.com/v1}
    template: anthropic
    auth: {type: oauth2, policyParams: {...}}
  - id: bedrock-backup
    upstream: {url: https://bedrock-runtime.us-east-1.amazonaws.com}
    template: awsbedrock
    auth: {type: other, policyName: aws-authentication, policyParams: {...}}   # SigV4, not OAuth2/apiKey

targets:
  - model: gpt-4o                              # Group A — the complicated one
    upstreamDefinition: openai-primary          # shares this API's whole-operation upstream.auth (API key)
    fallbacks:
      - model: gpt-4o-mini
        upstreamDefinition: openai-secondary    # ALSO shares upstream.auth — plain, expected, unchanged
      - model: claude-3-5-sonnet
        additionalProvider: anthropic-backup    # own oauth2 credential + anthropic template
      - model: anthropic.claude-3-sonnet        # Bedrock's own model-id convention
        additionalProvider: bedrock-backup      # own SigV4 credential + awsbedrock (pathParam) template
  - model: claude-3-5-sonnet                    # Group B — independent, untouched by Group A
    upstreamDefinition: anthropic-primary
statusCodes: [500, 502, 503]
suspendDuration: 30s
```

Three different auth arrangements across one chain (shared operation auth, OAuth2, and an
`other`-type SigV4 policy), one `pathParam`-located template (Bedrock), and a second request
that lands inside the suspend window — deliberately stacking every mechanism this design
touches into one request sequence.

**First request, total failure of every fallback.**

1. `OnRequestBody` (unchanged): matches Group A, no suspend state yet, starts at index 0,
   routes to the aggregate cluster (`openai-primary` → `openai-secondary` → `anthropic-backup`
   → `bedrock-backup`, priority order).
2. Attempt 1 → `openai-primary`. The *existing*, unscoped, whole-operation auth policy fires
   (correct — this is the primary) and injects the OpenAI key. OpenAI returns `503`.
3. Envoy retries → `openai-secondary`. `model-failover`'s own hook rewrites `model` to
   `gpt-4o-mini`. The whole-operation auth policy fires again, correctly — `openai-secondary`
   is a plain `upstreamDefinition`, so it shares the exact same credential as the primary, no
   auto-injection involved at all. `openai-secondary` also returns `503`.
4. Envoy retries → `anthropic-backup`. `model-failover`'s own hook rewrites `model` (baseline
   body, always the original pre-attempt-1 bytes, never a previous attempt's mutation) to
   `claude-3-5-sonnet`. The whole-operation auth policy *also* fires here — wrong credential,
   harmless, about to be overwritten. The scoped `oauth2-generator` instance (resolved from
   `anthropic-backup`'s own `additionalProviders` entry) matches this destination and
   overwrites with the correct token. The scoped `anthropic-adapter` instance matches and
   converts the already-model-corrected body to Anthropic's shape. **Anthropic also returns
   `503`** — worst case, every fallback fails.
5. Envoy retries again → `bedrock-backup`. `model-failover`'s hook rewrites `model` (baseline
   is *still* the original openai-shaped body — this attempt doesn't see any earlier attempt's
   mutation either) to `anthropic.claude-3-sonnet`. The scoped `aws-authentication` instance
   (resolved from `bedrock-backup`'s `auth: {type: other, ...}`) matches and signs the
   request — a header-only operation, works identically to OAuth2 here, no special-casing for
   the different auth type needed anywhere. **The scoped `bedrock-adapter` instance needs to
   move the model identifier out of the body and into the URL path** — Bedrock's own template
   declares `requestModel.location: pathParam`, there is no body field for it at all.

   **This is where Gap 1 (SDK limitation) bites, concretely.** `UpstreamAttemptRequestModifications`
   has exactly `HeadersToSet` and `Body` — no `Path` field (confirmed directly in the SDK,
   `action.go`). Compare the *downstream*-phase actions
   (`UpstreamRequestModifications`/`UpstreamRequestHeaderModifications`), which do have
   `Path *string`/`Host *string` — but those only apply once, before the first dispatch, so
   they can't conditionally rewrite the path for "whichever attempt happens to land on
   Bedrock." **A `pathParam`-located template cannot be a cross-provider fallback target at
   all until this SDK gap is closed** — restricted to `payload`-located templates for now
   (still 5 of the 7 shipped templates).

6. Setting that gap aside to finish the trace: suppose Bedrock succeeds, `200`.
   `model-failover`'s own `OnResponseHeaders` reads the final `x-envoy-attempt-count: 4`,
   suspends indices 0, 1, and 2 (every target that failed before the one that succeeded — the
   plain `openai-secondary` fallback included, identically to how it already worked before
   this FR) for 30s — unaffected by any of the cross-template complexity, exactly as it works
   today. `bedrock-adapter`'s response hook converts the response back to `openai` shape.
   Client sees one clean `200`.

**Second request, 5 seconds later — still inside the suspend window.**

`OnRequestBody` sees indices 0, 1, and 2 all suspended, walks forward to index 3
(`bedrock-backup`), and — per `model-failover`'s **existing**, already-shipped skip-ahead
logic — sets `UpstreamName` directly to that target's own cluster, bypassing the aggregate
entirely (Envoy has no way to "start" a priority walk partway through; this is deliberate,
tested behavior, unrelated to this FR). **This single dial's own `AttemptCount` is `1`** — not
`4` — because it never went through the retry walk at all.

**This is Gap 2.** If scoping were `AttemptCount`-based (Rev 3's original proposal), neither
the auto-injected auth instance nor the adapter would fire — their guard compares against `4`,
computed once from Bedrock's position in the aggregate's priority list, and this dial's real
`AttemptCount` is `1`. The request would go out unauthenticated and unconverted, straight to
Bedrock, in OpenAI's shape. Not a rare edge case either — skip-ahead specifically exists to
route around a target already known to be down, and a cross-template fallback under sustained
load is exactly the kind of target likely to end up suspended. The fix (destination-based
scoping via `:authority`, see "Attempt-scoped activation" above) handles this correctly,
because it keys off *where the request is actually going* rather than a
position-in-a-list assumption computed before this specific request ever arrived.

## Alternatives Considered

**Do auth and conversion inside `model-failover`'s own `OnUpstreamAttemptRequest`** (Rev 2).
Rejected for auth specifically: duplicates an entire existing policy family's credential
logic inside a routing policy, and doesn't scale to mixed auth types cleanly. Conversion
alone might have stayed inline reasonably, but keeping both mechanisms identical
(attempt-scoped auto-injection) is simpler than solving the same problem two different ways
for two capabilities that have the same shape.

**A new sibling policy** (`provider-failover`, Rev 1). Rejected: two policies for what's one
feature to an operator is a worse config surface (see Rev 2).

**Build this on `LlmProxy`'s existing mechanism** (original Rev 1 proposal). Rejected — see
"Why `LlmProxy`'s existing transformer mechanism doesn't fit" above.

**`template`/`auth` fields added directly to `UpstreamDefinition`** (Rev 4). Rejected in Rev
5: conflates two genuinely different kinds of reference into one — a same-provider backend
that shares this API's `upstream.auth` (what `UpstreamDefinition` has always meant) vs. an
independently-authenticated, independently-templated one. `UpstreamDefinition` should keep its
one existing meaning rather than growing a second, optional one that only sometimes applies.
A distinct, named concept (`additionalProviders`) is clearer for an operator reading the
config, and avoids the (small but real) risk of a future generic `UpstreamDefinition` consumer
having to special-case "unless it also happens to declare auth/template."

**Pairwise translators.** Rejected in favor of canonical-IR once more than ~2 providers are in
scope (see open questions on fidelity).

## Open Questions / Risks

- **[BLOCKING] Path-located templates (`awsbedrock`, `gemini`) cannot work as cross-provider
  fallback targets with the SDK as it exists today.** `UpstreamAttemptRequestModifications`
  has no field to rewrite the outgoing path per attempt (confirmed directly against the SDK —
  see Worst-case walkthrough). Needs an actual SDK change (a `Path` field, plumbed through the
  upstream ext_proc kernel), not a policy-level workaround. Until it lands, restrict this
  feature to `payload`-located templates.
- **Destination-based (`:authority`) scoping needs one live spot-check before the design fully
  commits to it.** `AttemptCount`-based scoping (Rev 3) is confirmed wrong for `model-failover`'s
  own skip-ahead-after-suspend path (see Worst-case walkthrough). The proposed fix is
  partially verified: a new kernel test
  (`TestUpstreamExtProc_AuthorityPseudoHeaderReachesPolicy`) confirms
  `processUpstreamAttemptRequestHeaders` copies every header Envoy sends with no filtering,
  and gateway-controller's upstream ext_proc filter config sets no `forward_rules`/header
  allowlist restricting what Envoy forwards — so `:authority` *should* reach a policy in
  practice. Not yet confirmed against Envoy's actual wire behavior on the real upstream filter
  chain in a live deployment.
- **Response-side target attribution may require reading `model-failover`'s private metadata
  keys from the adapter policy** (`metaGroupModelKey`/`metaStartIndexKey`) — the downstream
  response phase has `SharedContext` access (unlike the request-side upstream-attempt phase),
  so this doesn't inherit Gap 2 directly, but it does introduce cross-policy coupling to
  currently-internal state. Needs deciding whether `model-failover` exposes these as a
  documented contract, or whether the adapter derives target attribution some other way.
- **Adapter policy resolution/naming.** How gateway-controller maps a `template` value to a
  concrete adapter policy name/version isn't decided — a naming convention
  (`<template>-adapter`), an explicit field, or a registry. Needs a decision before
  implementation.
- **The attempt-scoping guard clause needs adding to every auth policy this feature should
  support** — `oauth2-generator` first (per Phasing), others incrementally, each covering
  *every* hook the policy has (not just `OnUpstreamAttemptRequest` — see "Attempt-scoped
  activation"). Each is small and mechanical, but it's still N changes across N existing,
  shipped policies, not zero.
- **Streaming responses.** SSE-chunked responses need chunk-by-chunk reshaping, not a
  single-body conversion. Needs its own design pass.
- **Fidelity loss through a canonical IR.** Provider-specific fields (Anthropic's top-level
  `system` prompt vs. OpenAI's `system`-role message, differing sampling parameters, etc.) may
  not round-trip losslessly. Needs a concrete field-mapping table per provider before
  implementation.
- **Response status-code semantics differ per provider** — `model-failover`'s existing
  `statusCodes` config needs to account for non-HTTP-standard error shapes if any target has
  them.
- **Chain-ordering guarantee.** The design assumes gateway-controller can reliably append
  auto-injected policies *after* `model-failover` in the chain and that execution order
  matches declaration order for `OnUpstreamAttemptRequest` — needs confirming against the
  actual chain executor, not just assumed by analogy with the downstream-phase ordering
  already observed elsewhere.
- **`additionalProviders` schema validation isn't designed yet** — `id` uniqueness within the
  list, and a target's `additionalProvider: <name>` reference resolving to a real declared
  entry, need the same registration-time validation `upstreamDefinition` references
  presumably already get (unverified — needs checking what that existing validation actually
  covers before assuming the new reference type gets equivalent treatment for free).

## Phasing (indicative, not a committed plan)

**Done:** the generic SDK/parser foundation — `policy.RetryTarget.AdditionalProviderName` and
`config.ParseRetrySourceParams` reading a fixed `additionalProvider` key, mutually exclusive
with `upstreamDefinition`, shared by any `x-wso2-retry-source`-declaring policy (api-platform
`decoupled-retry-source`, commit `430b7b0ed`). `model-failover`'s own schema/Go code and the
`additionalProviders` resource-level schema are still unbuilt — this is purely the shared
parsing primitive both depend on.

1. Live spot-check the `:authority` scoping mechanism against the real upstream filter chain;
   add the `Path` field to `UpstreamAttemptRequestModifications` (SDK change) if path-located
   templates are in scope for the first cut, or explicitly defer them if not.
2. Add the attempt-scoping guard to `oauth2-generator` (the auth type this FR's own examples
   use, covering every hook it has) and confirm the chain-ordering assumption above against the
   real executor.
3. Decide adapter policy resolution/naming; build the `openai` ↔ `anthropic` adapter's
   concrete field mapping and prove the full end-to-end flow live — auth, request conversion,
   response conversion, all destination-scoped.
4. Add the attempt-scoping guard to other auth policies, and adapters for additional
   payload-located templates, incrementally — additive once the mechanism is proven.
5. Revisit streaming-response translation and path-located templates (if deferred in step 1)
   as separate, later pieces of work.

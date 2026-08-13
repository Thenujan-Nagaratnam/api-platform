# AI Gateway Model Failover — Design

## Status

Builds directly on `docs/superpowers/specs/2026-08-12-upstream-attempt-retry-refresh-design.md`
(the `UpstreamAttemptPolicy` mechanism, implemented in `dev-policies/oauth2-generator`
as `OnUpstreamAttemptRequestHeaders`). Two prerequisite facts below were verified
with throwaway spikes against a real Envoy v1.38.3 (matching this repo's pinned
`ENVOY_VERSION`) before committing to this design; both spikes are discarded,
not part of this repo.

## Scope

Same-provider failover only: fallback targets share the request/response body
*shape* (e.g. several OpenAI-compatible deployments/models). Only the body's
`model` field, plus the resolved upstream (URL/auth), differ per target.
Cross-provider failover (OpenAI → Anthropic → Bedrock, full payload-shape
transformation reusing the `openai-to-*-transformer` policies) is explicitly
deferred — noted in Non-goals.

## Problem

Old APIM's Model Failover policy: configure a primary model+endpoint and an
ordered list of fallback model+endpoint pairs. If the primary's call fails
(configured status codes), the gateway transparently retries against the next
fallback — the client only ever sees the final outcome. A model-endpoint pair
that just failed is "suspended" for a configurable duration so future requests
don't waste an attempt rediscovering the same failure.

## Why not native Envoy retry alone

`RouteAction.RetryPolicy` retries within *one* cluster — Envoy chooses the
cluster once via route matching; a retry can hit a different **endpoint**
within that cluster (via load balancing) but never a functionally different
cluster (different host, different auth). Model failover needs to hop between
genuinely independent upstreams (different `upstreamDefinitions` entries,
each with its own URL/auth) — that requires a different Envoy primitive,
not just reusing `resilience.retry`'s existing single-cluster mechanism.

## Verified fact #1 — `aggregate cluster` + `retry_priority` really does cross-cluster failover

Spiked directly: two independent `STRICT_DNS` clusters (`backend_cluster_a`,
always-500; `backend_cluster_b`, always-200), composed via
`envoy.extensions.clusters.aggregate.v3.ClusterConfig{clusters: [a, b]}`
(`lb_policy: CLUSTER_PROVIDED`), route `retry_policy` with
`retry_on: "5xx"`, `num_retries: 1`, and
`retry_priority: {name: envoy.retry_priorities.previous_priorities}`. One
client request: cluster A received attempt 1, cluster B received attempt 2,
client saw a clean `200`. This construct is unused anywhere in this codebase
today — genuinely new Envoy config surface for gateway-controller's
translator, not an extension of anything already generated.

## Verified fact #2 — the upstream ext_proc filter can rewrite the body per attempt, but only when attached to the *aggregate* cluster

Spiked directly, same setup as fact #1 plus an ext_proc filter
(`ProcessingMode{RequestHeaderMode: SEND, RequestBodyMode: BUFFERED}`) in the
upstream `http_filters` chain:

- Attaching the filter to the **member** clusters (`backend_cluster_a`/`_b`)
  individually → **zero invocations**. Silently never fires. This is the one
  real dead end worth flagging loudly during implementation.
- Attaching it to the **aggregate cluster's own**
  `TypedExtensionProtocolOptions` → fires once per attempt (a fresh
  ext_proc stream per attempt, mirroring the already-verified header-only
  behavior from the retry-refresh work), sees the *original* client body on
  every attempt (Envoy replays the original buffered bytes, not whatever was
  mutated last attempt), and a `BodyMutation` response is faithfully
  forwarded to whichever real cluster Envoy dials for that attempt — content
  confirmed to differ correctly per attempt at the real backend.
- Gotcha: mutating the body without also rewriting `Content-Length` in the
  same response makes Envoy reject it (500, before the backend is ever
  dialed) — this repo's kernel already has `setContentLengthHeader` for
  exactly this in the downstream body path; the new upstream path must call
  it too, not leave this to policy authors to remember.
- `xds.cluster_name` (an ext_proc request attribute) reports the **aggregate**
  cluster's name on every attempt, never the resolved member — cannot be
  used to determine which fallback target a given attempt landed on.
  Unnecessary anyway: since the translator builds the aggregate's member
  list in the same order as the policy's configured `models` list,
  `AttemptCount` alone is sufficient (attempt 1 → `models[0]`, attempt 2 →
  `models[1]`, ...) — the same index-aligned pattern already relied on for
  retry-refresh (no new field-threading needed, matching
  `oauth2-upstream-extproc-task5-policychain-already-index-aligned`).

## Config surface

A new dev-policy, `model-failover` (not a `resilience.*` schema field — this
is a mediation policy, same category as `model-round-robin`/
`model-weighted-round-robin`, not a transient-retry concern like
`resilience.retry`). Each target references an **existing**
`upstreamDefinitions` entry by name — reusing that already-shipped,
already-validated primitive for "named additional backend with its own
URL/auth" rather than inventing a parallel one:

```yaml
upstreamDefinitions:
  - name: primary
    url: https://primary.example.com/v1
  - name: fallback-1
    url: https://fallback.example.com/v1

policies:
  - name: model-failover
    policyParams:
      models:
        - name: gpt-4o              # injected into body.model for this attempt
          upstreamDefinition: primary
        - name: gpt-4o-mini
          upstreamDefinition: fallback-1
      statusCodes: [429, 500, 502, 503, 504]   # required, non-empty
      requestTimeout: 10s                       # per_try_timeout
      suspendDuration: 30s                       # optional; omitted = no suspend tracking
      cache:                                     # only consulted when suspendDuration is set
        strategy: memory | redis                 # same shape as oauth2-generator's cacheParams
```

`numRetries` is not a separate knob — it's `len(models) - 1`, derived, so it
can't drift out of sync with the fallback list.

## Translator changes (gateway-controller)

When a route's policy chain includes `model-failover`:

1. Resolve each `models[].upstreamDefinition` to its cluster name via the
   **existing** `upstreamDefinitions` resolution path — unchanged, no new
   per-target cluster-creation code.
2. Build one new `envoy.clusters.aggregate` cluster whose `clusters: [...]`
   list is those resolved names, in `models[]` order (member order encodes
   priority).
3. Attach the upstream ext_proc filter (extended for body, see below) to
   *this aggregate cluster's* `TypedExtensionProtocolOptions` — per verified
   fact #2, member clusters must NOT also get it; doing so is a silent no-op,
   not a harmless redundancy, so the implementation must not attach to both
   "to be safe."
4. Route this operation to the aggregate cluster instead of its normal
   single/weighted upstream, with `RouteAction.RetryPolicy{`
   `RetriableStatusCodes: statusCodes, RetryOn: "retriable-status-codes",`
   `NumRetries: len(models)-1, RetryPriority: previous_priorities,`
   `PerTryTimeout: requestTimeout}`.
5. `VirtualHost.IncludeRequestAttemptCount: true` for this vhost — reusing
   the existing per-vhost scoping fixed in `340387e4d`/`ed36f7ef0` for
   retry-refresh, not a new mechanism.

## SDK: additive `Body` support on the existing upstream-attempt phase

`sdk/core/policy/v1alpha2` — extend, don't replace, the retry-refresh types:

```go
type UpstreamAttemptContext struct {
    *SharedContext
    AttemptCount int
    Headers      *Headers
    Body         *Body // NEW — nil if the cluster's filter isn't body-buffered
}

type UpstreamAttemptHeaderModifications struct {
    HeadersToSet map[string]string
    Body         []byte // NEW — nil = no change; kernel sets Content-Length to match
}
```

`OnUpstreamAttemptRequestHeaders` keeps its name and signature (no interface
split) — a header-only consumer (oauth2-generator) is unaffected: it never
reads `actx.Body` and never sets the new `Body` field, so its existing
behavior is bit-for-bit unchanged. `model-failover` is simply a second,
independent consumer of the same interface that happens to also use the new
field.

## Kernel changes (`upstream_extproc.go`)

- The upstream ext_proc filter's `ProcessingMode` gains `RequestBodyMode:
  BUFFERED`, but **only** for clusters actually backing a `model-failover`
  route — a header-only retry-refresh route must not pay for body buffering
  it never uses. This mirrors the existing `clustersNeedingUpstreamFilter`
  gating, just with a second, distinct capability flag (needs-headers vs.
  needs-body) rather than widening the existing one.
- New `processRequestBody` handler, dispatched only when a `request_body`
  message arrives — the attempt count parsed at the `request_headers`
  message (already happens today) must be retained across the two messages
  of the same stream (per-`Process()`-call local state, exactly as the
  spike's own `attemptCount` variable did) so the body-phase call can reuse
  it without re-deriving it.
- After a policy sets a non-nil `Body`, the kernel — not the policy — sets
  `Content-Length` to match, via the same helper `setContentLengthHeader`
  already used on the downstream body path. Per verified fact #2, skipping
  this makes Envoy reject the mutated body outright.
- Discovery stays the generic type-assertion pattern already established:
  no new hardcoded policy name anywhere in gateway-controller or
  policy-engine.

## Suspend duration — resolved via the *existing* `UpstreamName` mechanism, not new plumbing

The upstream-attempt phase runs after Envoy has already committed to dialing
a specific member of the aggregate cluster for that attempt — it cannot
redirect an in-flight retry away from a target the policy knows is
suspended. Suspend therefore only ever affects a **future, separate**
client request's *first* attempt, at the normal downstream `OnRequestBody`
phase (which runs once, before anything is sent):

1. On every final response (downstream `OnResponseHeaders`, which — unlike
   the upstream-attempt phase — *does* see the final `x-envoy-attempt-count`
   header), infer which targets failed: a final count of *N* means
   `models[0..N-2]` all failed this request (each had to fail to trigger the
   next retry). No new response-side upstream-attempt hook is needed —
   Envoy's existing attempt-count header on the response the client
   eventually gets is sufficient to reconstruct this.
2. Record each failed target's index with a TTL of `suspendDuration` — same
   `cacheParams`/Redis-or-memory pattern `token_cache.go` already
   implements for oauth2-generator, reusing the gateway-wide shared Redis
   client (`redisclient.Resolve`) when `cache.strategy: redis`.
3. On the *next* request's `OnRequestBody`, walk `models[]` in order; if
   `models[0]` is currently suspended, set `UpstreamName` to the first
   non-suspended target's `upstreamDefinition` name and rewrite the body's
   `model` field to match — using the **existing, unmodified**
   `UpstreamName` → `upstreamDefinitions`-cluster resolution path
   (`resolveUpstreamRedirect`/`applyUpstreamRedirect` in
   `policy-engine/internal/kernel/translator.go`), since `upstreamDefinitions`
   already produces exactly the kind of independently-addressable cluster
   this needs. If nothing is suspended (the common case), no `UpstreamName`
   override happens and the request proceeds via the aggregate cluster as
   normal, still rewriting the body to `models[0].name`.

This means suspend "skips ahead" for the *next* request's starting point,
but a request already mid-retry still walks the full priority chain — which
is fine, since it only reaches attempt 2+ *because* attempt 1 just failed,
i.e. it's already moving off the bad target regardless.

## Failure mode

Any failure in this policy's own logic (cache unavailable, malformed config)
fails open: no `UpstreamName` override, no body rewrite beyond the
primary's `models[0].name` — the request proceeds exactly as if
model-failover weren't configured for that attempt, never a new way to
fail. Matches the existing fail-open convention from retry-refresh.

## Open risks to verify early during implementation

- `resolveUpstreamRedirect` derives its cluster name from
  `execCtx.sharedCtx.APIKind`/`APIId` plus the given name — needs
  confirming this resolves correctly when the *route* itself already
  defaults to the aggregate cluster (not a plain single upstream), i.e.
  that overriding via `UpstreamName` on a model-failover route behaves the
  same as on any other route. Should be the first thing checked once
  scaffolding exists, before building out the rest.
- `per_try_timeout` interacting with `resilience.retry`'s existing timeout
  validation (`api_validator.go`'s `validateResilience`) — model-failover's
  `requestTimeout` needs the same "outer timeout must stay tighter than
  chained inner ones" scrutiny as `go-network-service-hardening.md`
  directive 5 already requires elsewhere in this codebase.

## Non-goals (deferred, YAGNI)

- Cross-provider failover / payload-shape transformation (composing with
  `openai-to-*-transformer` policies) — same-provider only, per Scope.
- Any Envoy-visible health-state manipulation (outlier detection, admin API
  health overrides) — suspend is a policy-level, next-request routing
  decision only, never mutates Envoy's own health view of a cluster.
- Per-target auth distinct from what `upstreamDefinitions` already supports
  today.

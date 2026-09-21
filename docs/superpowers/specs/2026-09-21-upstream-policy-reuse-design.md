# Upstream-attempt reuse of plain body-phase policies — Design

**Status:** Approved for implementation
**Related:** `docs/superpowers/specs/2026-09-17-llm-model-failover-design.md` (FR7 — LLM cross-provider failover)

## Problem

The failover mechanism runs two policy chains per client request: the downstream
chain (`ExecuteRequestPolicies`/`ExecuteResponsePolicies`, once per client
request) and the upstream-attempt chain (`ExecuteUpstreamRequestPolicies`/
`ExecuteUpstreamResponsePolicies`, once per Envoy attempt — including retries to
a different `additionalProviders` backend). Only policies implementing
`UpstreamRequestPolicy`/`UpstreamResponsePolicy` (`OnUpstreamRequestBody`/
`OnUpstreamResponseBody`, taking `*policy.UpstreamAttemptContext`) can run in the
second chain. A policy that only implements the plain `RequestPolicy`/
`ResponsePolicy` interface (`OnRequestBody`/`OnResponseBody`, taking
`*policy.RequestContext`/`*policy.ResponseContext`) can never run per-attempt,
even when an operator wants it scoped to one specific `additionalProviders`
entry — it would have to be rewritten against `UpstreamAttemptContext` first,
duplicating logic that already exists and works downstream.

This gap was surfaced while investigating whether `set-headers` (today used for
downstream api-key injection) could run per-provider upstream without a
rewrite. It cannot today, and neither can any other existing plain body-phase
policy.

## Goals

- Let an *existing, unmodified* policy that implements `RequestPolicy`/
  `ResponsePolicy` run in the upstream-attempt chain, scoped to one
  `additionalProviders` entry, with zero code changes to the policy itself.
- No new fields on `RequestContext`, `ResponseContext`, or
  `UpstreamAttemptContext` — reuse what already exists on each.
- Zero behavior change for every route that doesn't use the new mechanism.

## Non-goals

- Header-phase upstream policies (no `UpstreamRequestHeaderPolicy`-equivalent
  interface exists; `set-headers` specifically stays downstream-only). Flagged
  as a future gap, not addressed here.
- Scoping a reused policy to the *primary* provider (chain index 0) — this
  design only covers `additionalProviders` entries.
- Retrofitting `openai-to-anthropic-transformer` / `oauth2-generator` /
  `llm-upstream-provider-auth` to use the new scoping mechanism instead of
  their own internal `ResolvedProvider` self-check. They are unaffected.

## Design

### 1. Context adapters (gateway-runtime, new, no SDK struct changes)

Two small functions in `internal/executor/chain.go` build a `*policy.RequestContext`
/ `*policy.ResponseContext` from a `*policy.UpstreamAttemptContext`'s existing
fields only:

```go
func upstreamAttemptToRequestContext(upCtx *policy.UpstreamAttemptContext) *policy.RequestContext {
    return &policy.RequestContext{
        SharedContext: upCtx.SharedContext,
        Headers:       upCtx.Headers,
        Body:          upCtx.Body,
        Path:          upCtx.Path,
        Method:        upCtx.Method,
        Upstream:      upCtx.UpstreamRequestContext,
        // Authority/Scheme/Vhost/UpstreamInfo/Downstream: UpstreamAttemptContext
        // has no equivalent. Left zero-value. A policy attached via this
        // mechanism must not depend on them.
    }
}

func upstreamAttemptToResponseContext(upCtx *policy.UpstreamAttemptContext) *policy.ResponseContext {
    return &policy.ResponseContext{
        SharedContext:   upCtx.SharedContext,
        RequestBody:     &policy.Body{Content: upCtx.OriginalRequestRaw, Present: len(upCtx.OriginalRequestRaw) > 0, EndOfStream: true},
        RequestPath:     upCtx.Path,  // resolved outbound path, not the client-facing one
        RequestMethod:   upCtx.Method,
        ResponseHeaders: upCtx.Headers, // mutation-accumulator at this phase, not a raw upstream snapshot
        ResponseBody:    upCtx.Body,
        ResponseStatus:  upCtx.ResponseStatusCode,
        Upstream: &policy.UpstreamResponseContext{
            Name: upCtx.Name, URL: upCtx.URL, BasePath: upCtx.BasePath,
            Response: &policy.UpstreamResponse{Headers: upCtx.Headers, StatusCode: upCtx.ResponseStatusCode},
        },
        // RequestHeaders/Downstream: no equivalent. Left nil.
    }
}
```

The returned `RequestAction`/`ResponseAction` is applied via the **existing**
`applyUpstreamRequestModifications`/`applyUpstreamResponseModifications` — no
new apply path, since `UpstreamRequestModifications`/
`DownstreamResponseModifications` already satisfy both action interfaces (the
action/return side was already unified before this design).

**Known, accepted lossiness** (documented on the adapter functions themselves):
`Authority`/`Scheme`/`Vhost`/`Downstream`/`RequestHeaders` are unavailable and
left zero-value; `RequestPath`/`RequestMethod` reflect the attempt's *resolved
outbound* line, not the client-facing one; `ResponseHeaders` reflects only
this chain's own accumulated mutations, not a real snapshot of the upstream's
response headers. A policy attached via this mechanism must not depend on
these values.

### 2. `PolicySpec.TargetUpstream` (SDK)

New field on `policy.PolicySpec` (`sdk/core/policy/v1alpha2/definition.go`):

```go
// TargetUpstream scopes this chain entry's upstream-attempt-phase execution
// to one resolved provider (see UpstreamAttemptContext.ResolvedProvider).
// Empty means unscoped — identical to today's behavior. Non-empty additionally
// enables the plain-RequestPolicy/ResponsePolicy fallback in
// ExecuteUpstreamRequestPolicies/ExecuteUpstreamResponsePolicies (see chain.go).
TargetUpstream string
```

Threaded on the wire via a new `TargetUpstream *string` field on
`policyengine.PolicyInstance` (`sdk/core/policyengine/config.go`,
`json:"targetUpstream,omitempty"`).

### 3. Executor fallback dispatch

In `ExecuteUpstreamRequestPolicies`/`ExecuteUpstreamResponsePolicies`
(`internal/executor/chain.go`), for each chain entry:

1. If `spec.TargetUpstream != "" && !strings.EqualFold(spec.TargetUpstream, upCtx.ResolvedProvider)`: skip.
2. Else if the policy implements `UpstreamRequestPolicy`/`UpstreamResponsePolicy`: call it natively (today's path, unchanged).
3. Else if `spec.TargetUpstream != ""` and the policy implements plain `RequestPolicy`/`ResponsePolicy`: adapt context, call it, apply the result via the existing apply function.
4. Else: skip (today's behavior, unchanged).

Because step 3 only fires when `TargetUpstream != ""`, no existing route's
behavior changes — a downstream-only policy never starts running per-attempt
unless explicitly attached via the new mechanism below.

### 4. Config: `additionalProviders[].policies`

New field on `LLMProxyAdditionalProvider` (`management-openapi.yaml`):

```yaml
policies:
  type: array
  description: >
    Existing policies to run in the upstream-attempt chain, scoped to this
    provider only. Each runs once per attempt against this provider's
    backend (including retries), never for any other provider's attempt.
  items:
    $ref: '#/components/schemas/LLMProxyUpstreamPolicy'

LLMProxyUpstreamPolicy:
  type: object
  required: [name, version]
  properties:
    name: { type: string, example: some-existing-policy }
    version: { type: string, pattern: '^v\d+$', example: v1 }
    params: { type: object, additionalProperties: true }
```

No `executionCondition`/`paths` — upstream-phase execution never evaluates
CEL (see `chain.go`'s doc comment on `ExecuteUpstreamRequestPolicies`), and
scoping here is per-provider, not per-path.

`gateway-controller/pkg/utils/llm_transformer.go`'s `transformProxy` injects
one chain entry per listed policy per provider — the same pattern already
used for `.transformer`/`.auth` — setting `Policy.TargetUpstream = &name`
(the provider's `as`, defaulting to `id`). `pkg/policyxds/snapshot.go`'s
`createPolicyChainResource` emits `"targetUpstream"` in the wire JSON when
set. Both `internal/kernel/xds.go`'s `ConfigLoader.buildPolicyChain` and
`internal/xdsclient/handler.go`'s `ResourceHandler.buildPolicyChain` (a
maintained duplicate of the same logic) set `spec.TargetUpstream` from the
decoded value; `pkg/engine/engine.go`'s `Engine.buildPolicyChain` and
`internal/admin/dumper.go` are updated for the same parity `ExecutionCondition`
already has across all four.

## Testing

- `chain_test.go`: fallback dispatch unit tests — native `UpstreamRequestPolicy`
  wins over the adapted plain-`RequestPolicy` path when a policy implements
  both; `TargetUpstream` mismatch skips the attempt; a policy with
  `TargetUpstream == ""` never receives the adapter fallback even if it's a
  plain `RequestPolicy`; same three cases mirrored for the response side.
- A `upstream_extproc_test.go` (or `chain_test.go`) case using a test-double
  plain `RequestPolicy` (no `UpstreamRequestPolicy` implementation at all)
  proving it runs only for its configured provider's attempt and never for a
  different provider's attempt on the same request.
- No live e2e required — this is dispatch-mechanism plumbing, not a new
  user-facing LLM feature; existing failover e2e coverage already exercises
  the unchanged native-`UpstreamRequestPolicy` path.

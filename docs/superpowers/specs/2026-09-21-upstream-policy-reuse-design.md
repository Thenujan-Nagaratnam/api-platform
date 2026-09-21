# Attachment-point selection of downstream vs. upstream policy execution — Design

**Status:** Implemented
**Related:** `docs/superpowers/specs/2026-09-17-llm-model-failover-design.md`

## Problem

The gateway runs two policy chains per client request: the downstream ext_proc
chain (once per request) and the upstream-attempt ext_proc chain (once per Envoy
attempt, including retries to another backend). Until now a policy had to
implement *different* interfaces to participate in each (`OnRequestBody` vs
`OnUpstreamRequestBody`, `OnRequestHeaders` had no upstream-attempt analog at
all), so reusing an existing policy upstream meant rewriting it, and header-only
policies (`set-headers`, `oauth2-generator`) couldn't run upstream at all.

## Goal

One policy implementation, the same interfaces it already has
(`RequestPolicy`/`ResponsePolicy`/`RequestHeaderPolicy`/`ResponseHeaderPolicy`)
for **any** policy — body-phase, header-phase, or both. **Where the policy is
attached in the LlmProvider/LlmProxy YAML decides which ext_proc runs it:**

```yaml
spec:
  operationPolicies:   # downstream ext_proc (unchanged)
    - name: set-headers
      version: v0
      paths: [{ path: /chat/completions, methods: [POST], params: {...} }]
  upstreamPolicies:    # upstream-attempt ext_proc — same policy, same shape
    - name: set-headers
      version: v0
      paths: [{ path: /chat/completions, methods: [POST], params: {...} }]
```

No new SDK interfaces, no new `ProcessingMode` fields, no new context fields.
Every route not using `upstreamPolicies` behaves exactly as before.

## Design

### Controller
- `LLMProviderConfigData` and `LLMProxyConfigData` gain `upstreamPolicies`
  (`[]OperationPolicy`, same schema as `operationPolicies`).
- `llm_transformer.go` attaches `upstreamPolicies` through the same
  path/method-matching logic as `operationPolicies` (all three attachment
  sites), marking each resulting `api.Policy` with `upstream: true` (internal
  field on the `Policy` schema).
- The flag travels `api.Policy.Upstream` → `PolicyInstance.Upstream`
  (`sdk/core/policyengine`) → `models.Policy.Upstream` → xDS JSON `"upstream"`.
- `clusterNeedsUpstreamPolicyFilter` treats `Upstream: true` as needing the
  per-cluster upstream ext_proc filter.

### Runtime
At chain-build time, all three builders (`kernel/xds.go`, `xdsclient/handler.go`,
`pkg/engine/engine.go`) route each `PolicyInstance` based on its `Upstream`
flag alone:

- `Upstream: true` → the policy instance goes **only** into a new
  `registry.PolicyChain.UpstreamPolicies`/`UpstreamPolicySpecs` list, never
  into `Policies` — so it never runs downstream.
- `Upstream: false` (the default) → goes only into `Policies`, as before.

Attaching the same policy under both `operationPolicies:` and
`upstreamPolicies:` creates two separate `PolicyInstance`s (two separate
`GetInstance()` calls), one in each list — the policy runs in both phases,
each as its own instance.

`registry.ComputeUpstreamRequirements` inspects `UpstreamPolicies` and sets
`RequiresUpstreamRequest`/`RequiresUpstreamResponse` by checking which
interfaces (`RequestPolicy`/`RequestHeaderPolicy` for request,
`ResponsePolicy`/`ResponseHeaderPolicy` for response) are actually
implemented — these gate whether `upstream_extproc.go` does any work at all
for a given attempt.

`upstream_extproc.go`'s `Process()` now dispatches all four phases against
`chain.UpstreamPolicies`, using new executor functions
(`ExecuteUpstreamAttemptRequestHeaderPolicies`/`RequestPolicies`/
`ResponseHeaderPolicies`/`ResponsePolicies` in `internal/executor/chain.go`)
that call the policy's own, unmodified interface methods — no wrapper, no
conversion:

- **Header phases** (`RequestHeaders`/`ResponseHeaders` Envoy messages) now
  actually dispatch policies for the first time — previously these cases only
  did routing/suspension bookkeeping. A policy's `OnRequestHeaders`/
  `OnResponseHeaders` runs directly against a `RequestHeaderContext`/
  `ResponseHeaderContext` built for the attempt.
- **Body phases** dispatch `OnRequestBody`/`OnResponseBody` against a
  `RequestContext`/`ResponseContext` built for the attempt, reusing the
  existing downstream mutation-accumulation helpers
  (`executor.applyRequestModifications`/`applyResponseModifications`) since
  the context types are identical to the downstream ones.
- Header-phase mutations and body-phase mutations from the *same* attempt
  share one `*policy.Headers`/`*policy.SharedContext` object
  (`upstreamAttemptState.requestHeaders`/`responseHeaders`/`sharedContext`),
  so a header-phase policy's mutation is visible to a body-phase policy
  running after it in the same attempt, mirroring how downstream threads one
  `SharedContext` across its own phases.
- Header mutations are diffed against a pre-chain snapshot
  (`diffHeaderMutation`) rather than dumped wholesale, since — unlike the
  original body-only design — `Headers` here starts seeded from Envoy's real
  request/response headers (`extractHeaderMap`), not empty.

**The one signal distinguishing an upstream-attempt invocation from a genuine
downstream one**, for a policy whose logic needs to tell them apart, is
`Downstream == nil` on `RequestContext`/`ResponseContext`/
`RequestHeaderContext`/`ResponseHeaderContext` — always populated for a real
downstream call, always nil for an attempt-phase call. `SharedContext.Metadata`
is seeded per attempt with `"selected_provider"`/`"selected_model"`
(`kernel.NewUpstreamAttemptSharedContext`), the same convention
`llm-header-router` writes downstream, so a policy that already self-gates on
that metadata key (e.g. a transformer scoped to one provider) works
unmodified per attempt too.

### Accepted lossiness
A policy attached via `upstreamPolicies` (or running upstream via a native
dual-attachment) receives zero-valued `Authority`/`Scheme`/`Vhost`/
`UpstreamInfo`; `Path`/`Method` are the attempt's resolved outbound request
line (already combined with the backend's base path, no separate
`APIContext` prefix to strip); `Headers` start seeded from Envoy's real
headers, not empty, but are otherwise this attempt's own working state, not a
guaranteed-fresh snapshot across retries. `executionCondition` is not
evaluated upstream.

## Non-goals / known gaps
- Chains built via `kernel/body_mode.go`'s `Kernel.BuildPolicyChain` (from
  plain `PolicySpec`, no wire `Upstream` flag) never populate
  `UpstreamPolicies` — this entry point has no production caller.
- `upstreamPolicies` is not yet exposed through the CLI/portal schemas.
- **No native per-instance dual-phase participation.** An earlier iteration
  of this design let a single, normally-attached `PolicyInstance` opt into
  both phases via `ProcessingMode.UpstreamRequestMode`/`UpstreamResponseMode`
  (used by `aws-authentication`, `oauth2-generator`,
  `llm-upstream-provider-auth`, `openai-to-anthropic-transformer`). That
  mechanism has been removed in favor of a single, uniform rule: upstream
  participation is decided *only* by attachment point. Those four dev-policies
  still have per-attempt-aware logic internally (gated on `Downstream == nil`),
  but nothing currently attaches them with `Upstream: true`, so that logic is
  presently unreachable — they'd need an explicit `upstreamPolicies:`
  attachment (a second `PolicyInstance`, like any other policy) to run
  per-attempt again. `gateway-controller/pkg/xds/upstream_policy_names.go`'s
  `upstreamPhasePolicyNames` allowlist is now vestigial for the same reason:
  it still attaches the upstream ext_proc filter for these four names, but
  there's nothing in `UpstreamPolicies` for the filter to find unless one of
  them is separately attached via `upstreamPolicies:`.

## Testing
- `executor/upstream_chain_test.go`: `ExecuteUpstreamAttemptRequestPolicies`/
  `ResponsePolicies` dispatch a plain `RequestPolicy`/`ResponsePolicy`
  against `chain.UpstreamPolicies` with `Downstream == nil`, accumulate
  mutations across a multi-policy chain, and short-circuit on
  `ImmediateResponse`; `ExecuteUpstreamAttemptRequestHeaderPolicies`/
  `ResponseHeaderPolicies` do the same for a header-only policy (the
  `set-headers` case) via `RequestHeaderContext`/`ResponseHeaderContext`.
- `kernel/upstream_attempt_test.go`: the context builders seed
  `SharedContext`/`Headers`/`Body` correctly, never alias the caller's
  original byte slice, and share one `*policy.Headers` across header- and
  body-phase contexts for the same attempt.
- `kernel/upstream_extproc_test.go`: `Process()` end-to-end — backend
  resolution, failover chain/suspension behavior, response-chain
  accumulation, and a policy attached via `chain.UpstreamPolicies` running
  correctly for its resolved backend.
- `utils/llm_provider_transformer_test.go`: `operationPolicies` and
  `upstreamPolicies` entries for the same policy/path yield one unflagged and
  one `upstream: true` operation policy.
- `xdsclient/handler_test.go`: a real xDS-delivered chain with an
  `Upstream`-flagged `PolicyInstance` sets `RequiresUpstreamRequest` and
  keeps the policy out of `chain.Policies`.
- `xds/upstream_policy_names_test.go`: an `Upstream`-flagged policy with a
  non-allowlisted name still gets the upstream filter.

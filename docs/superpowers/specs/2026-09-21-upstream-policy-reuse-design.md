# Attachment-point selection of downstream vs. upstream policy execution — Design

**Status:** Implemented
**Related:** `docs/superpowers/specs/2026-09-17-llm-model-failover-design.md`

## Problem

The gateway runs two policy chains per client request: the downstream ext_proc
chain (once per request) and the upstream-attempt ext_proc chain (once per Envoy
attempt, including retries to another backend). Until now a policy had to
implement *different* interfaces to participate in each (`OnRequestBody` vs
`OnUpstreamRequestBody`), so reusing an existing policy upstream meant
rewriting it.

## Goal

One policy implementation, one interface (`RequestPolicy`/`ResponsePolicy`).
**Where the policy is attached in the LlmProvider/LlmProxy YAML decides which
ext_proc runs it:**

```yaml
spec:
  operationPolicies:   # downstream ext_proc (unchanged)
    - name: some-policy
      version: v0
      paths: [{ path: /chat/completions, methods: [POST], params: {...} }]
  upstreamPolicies:    # upstream-attempt ext_proc — same policy, same shape
    - name: some-policy
      version: v0
      paths: [{ path: /chat/completions, methods: [POST], params: {...} }]
```

No new fields on `RequestContext`, `ResponseContext` or `UpstreamAttemptContext`.
Every route not using `upstreamPolicies` behaves exactly as before.

## Design

### Controller
- `LLMProviderConfigData` and `LLMProxyConfigData` gain `upstreamPolicies`
  (`[]OperationPolicy`, same schema as `operationPolicies`).
- `llm_transformer.go` attaches `upstreamPolicies` through the same
  path/method-matching logic as `operationPolicies` (all three attachment
  sites), marking each resulting `api.Policy` with `upstream: true` (new
  internal field on the `Policy` schema).
- The flag travels `api.Policy.Upstream` → `PolicyInstance.Upstream`
  (`sdk/core/policyengine`) → `models.Policy.Upstream` → xDS JSON `"upstream"`.
- `clusterNeedsUpstreamPolicyFilter` treats `Upstream: true` as needing the
  per-cluster upstream ext_proc filter (previously keyed only on a static name
  allowlist).

### Runtime
`registry.WrapUpstreamAttached` (`internal/registry/upstream_attached.go`) is
applied at chain-build time in all three builders (`kernel/xds.go`,
`xdsclient/handler.go`, `pkg/engine/engine.go`) when `PolicyInstance.Upstream`
is set. The wrapper:

- reports every downstream mode as skip and does **not** implement
  `RequestPolicy`/`ResponsePolicy`, so the downstream executors never run it;
- implements `UpstreamRequestPolicy`/`UpstreamResponsePolicy` by delegating to
  the inner policy's `OnRequestBody`/`OnResponseBody` with a `RequestContext`/
  `ResponseContext` built from the attempt's existing fields;
- returns the inner policy's action unchanged — `UpstreamRequestModifications`/
  `DownstreamResponseModifications` already satisfy both action interfaces, so
  the existing `applyUpstream*Modifications` accumulation applies as-is.

Because the wrapper presents itself through the upstream interfaces, the
existing executor, `RequiresUpstreamRequest/Response` chain flags and
`upstream_extproc.go` need no changes.

### Accepted lossiness
A policy attached via `upstreamPolicies` receives zero-valued `Authority`,
`Scheme`, `Vhost`, `UpstreamInfo`, `Downstream` and `RequestHeaders`;
`RequestPath`/`RequestMethod` are the attempt's resolved outbound request line;
`ResponseHeaders` holds only mutations accumulated in this attempt's chain, not
a snapshot of the real upstream response headers. Such a policy must not depend
on these. `executionCondition` is not evaluated upstream.

## Non-goals / known gaps
- **Header-phase policies** (e.g. `set-headers`) cannot run upstream: no
  upstream header-phase interface exists. The wrapper logs a warning at chain
  build and the policy never runs. Separate future work.
- Chains built via `kernel/body_mode.go` from `PolicySpec` (protocol-resolver
  composed chains) do not carry the flag; not used by LLM kinds.
- Existing native upstream policies (`openai-to-anthropic-transformer`,
  `oauth2-generator`, `llm-upstream-provider-auth`, `aws-authentication`) are
  untouched and keep working via the static allowlist.
- `upstreamPolicies` is not yet exposed through the CLI/portal schemas.

## Testing
- `executor/upstream_attached_test.go`: a plain policy (only
  `RequestPolicy`/`ResponsePolicy`) wrapped via `WrapUpstreamAttached` is
  invisible to the downstream interfaces/modes, runs through
  `ExecuteUpstreamRequestPolicies`/`ExecuteUpstreamResponsePolicies`, and its
  mutations accumulate; a header-only policy gets no upstream phase.
- `utils/llm_provider_transformer_test.go`: `operationPolicies` and
  `upstreamPolicies` entries for the same policy/path yield one unflagged and
  one `upstream: true` operation policy.
- `xds/upstream_policy_names_test.go`: an `Upstream`-flagged policy with a
  non-allowlisted name still gets the upstream filter.

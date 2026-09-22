# Model Failover as a Policy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the `resilience.failover` LlmProxy schema section with an ordinary `model-failover`
policy that owns target selection, per-attempt chain resolution, provider/model metadata seeding, and
suspension tracking end to end — with zero code changes to `oauth2-generator`/`openai-to-anthropic-transformer`.

**Architecture:** A new `model-failover` policy (own Go module) runs downstream (parses the client's
model, checks its own suspension state, routes via the existing `UpstreamRequestModifications.UpstreamName`
mechanism) and per upstream attempt (resolves its own chain position from a new SDK-exposed raw cluster
name + attempt count, seeds `selected_provider`/`selected_model` metadata, records suspension). The
existing Envoy-level xDS generation (aggregate clusters, retry policy, attempt-count header, upstream
ext_proc filter attachment — `gateway-controller/pkg/xds/failover_cluster.go` and
`pkg/xds/translator.go`) is reused unchanged; only what feeds it changes, from the `resilience.failover`
schema to the new policy's params. Provider-scoped credential/transform policies gate via CEL
`executionCondition` on that seeded metadata — the same mechanism already used downstream today,
now also evaluated in the upstream-attempt phase for the first time.

**Tech Stack:** Go 1.26, the WSO2 API Platform gateway-controller/gateway-runtime/policy-engine
modules, the `policyv1alpha2` policy SDK, CEL (`google/cel-go` via the existing `internal/pkg/cel`
evaluator), Envoy xDS/ext_proc.

**Spec:** `docs/superpowers/specs/2026-09-21-model-failover-policy-design.md` (this plan implements it
in full; no open items remain in that doc). Also read
`docs/superpowers/specs/2026-09-21-upstream-policy-reuse-design.md` — the attachment-point mechanism
(`chain.UpstreamPolicies`, `Downstream == nil`) this plan builds directly on top of; already merged,
not part of this plan's work.

## Global Constraints

- **No changes to `oauth2-generator` or `openai-to-anthropic-transformer` Go source, anywhere in this
  plan.** They gate correctly via the new upstream `executionCondition` mechanism (Task 3) with zero
  code changes — do not add or accept any step that touches those policies' implementation files.
- Commits end with `Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>`.
- Follow this branch's existing commit-message convention: `feat(llm-failover): ...` / `fix(llm-failover): ...` / `test(llm-failover): ...` / `refactor(llm-failover): ...` (see `git log --oneline` in the repo root for examples).
- No backward-compatibility or migration path is needed for `resilience.failover` — it has never
  shipped. Remove it outright; do not keep both config surfaces working side by side.
- `gateway/dev-policies/` is gitignored local dev wiring (`.gitignore:169`); it is not part of this
  repo's tracked history. `gateway/build.yaml` and `gateway/build-manifest.yaml` ARE tracked.
- The user runs all gateway Docker builds/restarts themselves — never run `make build`, `docker
  compose`, or similar yourself; give exact commands instead.
- `gateway/configs/keys.env` must never be read or committed.
- This session runs inside a git worktree at
  `/Users/thenujan/Desktop/Git-Repos/api-platform/.claude/worktrees/llm-model-failover` — all commands
  run from there (or a subdirectory), never `cd` to the main checkout.

---

## Shared contract: `model-failover` policy params

Every task below that touches either the policy or the controller must produce/consume this exact
JSON shape (Task 4 defines the Go struct with these tags; Task 7 builds a `map[string]interface{}`
with these exact keys). Documenting it once here, referenced by name (`ModelFailoverParams`) from every
task that needs it, per the "types match across tasks" plan requirement.

```go
// ModelFailoverParams is the parsed shape of the model-failover policy's
// params (policy.PolicyMetadata's params map, unmarshaled).
type ModelFailoverParams struct {
	Targets         []FailoverTargetEntry `json:"targets"`
	SuspendDuration int                   `json:"suspendDuration"`
}

// FailoverTargetEntry is one client-requested-model's primary + fallback chain.
type FailoverTargetEntry struct {
	Target FailoverTarget   `json:"target"`
	// Fallbacks is ordered: Fallbacks[0] is attempt index 1, Fallbacks[1] is
	// attempt index 2, etc. (Target is always attempt index 0.)
	Fallbacks []FailoverTarget `json:"fallbacks"`
	// AggregateCluster is injected by gateway-controller (Task 7) with the
	// exact envoy.clusters.aggregate cluster name it assigned this entry
	// (xds.AggregateClusterName(routeKey, index)) — the policy never computes
	// this itself, only compares against it (see design doc §3).
	AggregateCluster string `json:"aggregateCluster"`
}

// FailoverTarget identifies one chain member.
type FailoverTarget struct {
	Model    string `json:"model"`
	Provider string `json:"provider,omitempty"`
}
```

---

## Task 1: SDK — add `RouteCluster` to `UpstreamRequestContext`/`UpstreamResponseContext`

**Files:**
- Modify: `sdk/core/policy/v1alpha2/context.go` (the `UpstreamRequestContext` struct at line 55, the
  `UpstreamResponseContext` struct at line ~138 — re-read the file first, line numbers shift as the
  file has been edited repeatedly this session)
- Test: `sdk/core/policy/v1alpha2/context_test.go` (create if it doesn't exist; check first)

**Interfaces:**
- Produces: `UpstreamRequestContext.RouteCluster string` and `UpstreamResponseContext.RouteCluster
  string` — the raw `xds.cluster_name` Envoy reported for this attempt, before any kernel-side member
  resolution. Consumed by Task 2 (kernel populates it) and Task 5 (`model-failover`'s upstream logic
  reads it).

- [ ] **Step 1: Read the current file to get exact line numbers**

Run: `grep -n "type UpstreamRequestContext struct\|type UpstreamResponseContext struct" sdk/core/policy/v1alpha2/context.go`

- [ ] **Step 2: Add the field to both structs**

In `UpstreamRequestContext`, add after the `BasePath string` field:

```go
	// RouteCluster is the raw xds.cluster_name Envoy reported for this
	// attempt, before any kernel-side member-cluster resolution. For an
	// attempt routed through an envoy.clusters.aggregate cluster (e.g. a
	// model-failover chain), this is the aggregate cluster's own name,
	// stable across every attempt against it — Name above is the resolved
	// real member cluster instead. Empty for a route with no
	// aggregate/failover involvement. Only ever populated for an
	// upstream-attempt invocation (Downstream == nil on the enclosing
	// context); always empty for a genuine downstream invocation.
	RouteCluster string
```

Add the identical field (same doc comment) to `UpstreamResponseContext`.

- [ ] **Step 3: Build to confirm no compile errors**

Run: `cd sdk/core && go build ./...`
Expected: no output (success).

- [ ] **Step 4: Write a test confirming both structs carry the field and zero-value correctly**

Create `sdk/core/policy/v1alpha2/context_test.go` if it doesn't already exist (check first —
`ls sdk/core/policy/v1alpha2/*_test.go`), otherwise append to an existing suitable one:

```go
package policyv1alpha2

import "testing"

func TestUpstreamRequestContext_RouteClusterField(t *testing.T) {
	ctx := &UpstreamRequestContext{Name: "resolved-real-cluster", RouteCluster: "failover_agg_abc_0"}
	if ctx.RouteCluster != "failover_agg_abc_0" {
		t.Fatalf("RouteCluster = %q, want %q", ctx.RouteCluster, "failover_agg_abc_0")
	}
	if ctx.Name != "resolved-real-cluster" {
		t.Fatalf("Name = %q, want %q, RouteCluster must not alias Name", ctx.Name, "resolved-real-cluster")
	}
}

func TestUpstreamResponseContext_RouteClusterField(t *testing.T) {
	ctx := &UpstreamResponseContext{Name: "resolved-real-cluster", RouteCluster: "failover_agg_abc_0"}
	if ctx.RouteCluster != "failover_agg_abc_0" {
		t.Fatalf("RouteCluster = %q, want %q", ctx.RouteCluster, "failover_agg_abc_0")
	}
}
```

- [ ] **Step 5: Run the tests**

Run: `cd sdk/core && go test ./policy/v1alpha2/... -run RouteCluster -v`
Expected: both tests PASS.

- [ ] **Step 6: Run the full SDK test suite to confirm no regressions**

Run: `cd sdk/core && go build ./... && go test ./...`
Expected: all packages `ok`.

- [ ] **Step 7: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform/.claude/worktrees/llm-model-failover
git add sdk/core/policy/v1alpha2/context.go sdk/core/policy/v1alpha2/context_test.go
git commit -m "$(cat <<'EOF'
feat(llm-failover): expose the raw pre-resolution cluster name to upstream-attempt policies

RouteCluster carries Envoy's xds.cluster_name attribute as reported for
this attempt, before the kernel substitutes the resolved real member
cluster into Name — for an aggregate-routed attempt this is the aggregate
cluster's own name, the signal a policy needs to know which failover chain
it's walking.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Kernel — capture and thread the raw cluster name

**Files:**
- Modify: `gateway/gateway-runtime/policy-engine/internal/kernel/upstream_extproc.go` (the
  `upstreamAttemptState` struct, and `Process()`'s `RequestHeaders` case around line 159-160)
- Modify: `gateway/gateway-runtime/policy-engine/internal/kernel/upstream_attempt.go` (the four
  `BuildUpstreamAttempt*Context` functions)
- Test: `gateway/gateway-runtime/policy-engine/internal/kernel/upstream_attempt_test.go`
- Test: `gateway/gateway-runtime/policy-engine/internal/kernel/upstream_extproc_test.go`

**Interfaces:**
- Consumes: `policy.UpstreamRequestContext.RouteCluster`/`policy.UpstreamResponseContext.RouteCluster`
  (Task 1).
- Produces: `kernel.BuildUpstreamAttemptRequestHeaderContext`/`BuildUpstreamAttemptRequestContext`/
  `BuildUpstreamAttemptResponseHeaderContext`/`BuildUpstreamAttemptResponseContext` all gain a new
  `rawClusterName string` parameter (inserted right after the existing `shared *policy.SharedContext`
  parameter in each signature) and populate `.RouteCluster` on the `Upstream`/`UpstreamRequestContext`
  they build. Consumed by Task 5 (`model-failover`'s upstream `OnRequestHeaders`/`OnResponseHeaders`).

- [ ] **Step 1: Read the current exact lines to confirm this task's premises still hold**

```bash
grep -n "state.clusterName = extractAttribute\|state.rawClusterName\|resolved.ClusterName != \"\"" gateway/gateway-runtime/policy-engine/internal/kernel/upstream_extproc.go
```

Confirm `state.clusterName = extractAttribute(req, "xds.cluster_name")` appears once, and that a few
lines later `state.clusterName = resolved.ClusterName` (inside `if resolved.ClusterName != "" {`)
overwrites it. If either line has moved or no longer matches this shape, read the surrounding 40 lines
before proceeding — this task's insertion point depends on it.

- [ ] **Step 2: Add `rawClusterName` to `upstreamAttemptState`**

In the `upstreamAttemptState` struct definition, add:

```go
	// rawClusterName is the xds.cluster_name attribute exactly as Envoy
	// reported it for this attempt, captured before resolveBackend
	// substitutes the resolved real member cluster into clusterName. For an
	// aggregate-routed attempt this is the aggregate cluster's own name —
	// exposed to policies via RouteCluster (kernel.BuildUpstreamAttempt*Context).
	rawClusterName string
```

- [ ] **Step 3: Capture it immediately after the existing extraction, before resolveBackend runs**

Find the line `state.clusterName = extractAttribute(req, "xds.cluster_name")` inside the
`RequestHeaders` case. Immediately after it, add:

```go
			state.rawClusterName = state.clusterName
```

This must run before the `resolved := s.resolveBackend(...)` call and the subsequent
`if resolved.ClusterName != "" { state.clusterName = resolved.ClusterName }` substitution — confirm by
reading the surrounding lines that the insertion point is correct (the capture line must come before
`resolveBackend` is called, not after).

- [ ] **Step 4: Update the four context-builder signatures in `upstream_attempt.go`**

For each of `BuildUpstreamAttemptRequestHeaderContext`, `BuildUpstreamAttemptRequestContext`,
`BuildUpstreamAttemptResponseHeaderContext`, `BuildUpstreamAttemptResponseContext`: add a
`rawClusterName string` parameter right after the existing `shared *policy.SharedContext` parameter,
and set `RouteCluster: rawClusterName` on the `UpstreamRequestContext`/`UpstreamResponseContext`
literal each function builds. Example for `BuildUpstreamAttemptRequestHeaderContext` (apply the same
pattern to the other three):

```go
func BuildUpstreamAttemptRequestHeaderContext(
	shared *policy.SharedContext,
	rawClusterName string,
	headers *policy.Headers,
	backendName, backendURL, basePath, method, outboundPath string,
) *policy.RequestHeaderContext {
	return &policy.RequestHeaderContext{
		SharedContext: shared,
		Headers:       headers,
		Path:          outboundPath,
		Method:        method,
		Upstream: &policy.UpstreamRequestContext{
			Name: backendName, URL: backendURL, BasePath: basePath,
			RouteCluster: rawClusterName,
		},
	}
}
```

- [ ] **Step 5: Update all four call sites in `upstream_extproc.go`**

Each of the four builder calls (in the `RequestHeaders`, `RequestBody`, `ResponseHeaders`, `ResponseBody`
cases of `Process()`/`processRequestBody`/`processResponseBody`) must pass `state.rawClusterName` as
the new second argument. Find each call via:

```bash
grep -n "BuildUpstreamAttemptRequestHeaderContext(\|BuildUpstreamAttemptRequestContext(\|BuildUpstreamAttemptResponseHeaderContext(\|BuildUpstreamAttemptResponseContext(" gateway/gateway-runtime/policy-engine/internal/kernel/upstream_extproc.go
```

and insert `state.rawClusterName,` as the argument immediately after `state.sharedContext,` at each.

- [ ] **Step 6: Build**

Run: `cd gateway/gateway-runtime/policy-engine && go build ./...`
Expected: compile errors listing every call site you haven't updated yet (existing tests in
`upstream_attempt_test.go` also call these builders directly) — fix each until it's clean.

- [ ] **Step 7: Update existing tests in `upstream_attempt_test.go` for the new parameter**

Every call to the four builder functions in this file needs the new `rawClusterName` argument inserted.
Search and update:

```bash
grep -n "BuildUpstreamAttemptRequest\|BuildUpstreamAttemptResponse" gateway/gateway-runtime/policy-engine/internal/kernel/upstream_attempt_test.go
```

For each call, insert a literal string argument (e.g. `""` where the test doesn't care about routing,
or `"failover_agg_test_0"` for a new test asserting the field flows through — see Step 8).

- [ ] **Step 8: Add a new test asserting `RouteCluster` flows through correctly**

Add to `upstream_attempt_test.go`:

```go
func TestBuildUpstreamAttemptRequestHeaderContext_SetsRouteCluster(t *testing.T) {
	shared := NewUpstreamAttemptSharedContext("", "")
	headers := policy.NewHeaders(nil)

	hdrCtx := BuildUpstreamAttemptRequestHeaderContext(shared, "failover_agg_chat_0", headers,
		"openai-provider-cluster", "https://api.openai.com", "/v1", "POST", "/chat/completions")

	require.NotNil(t, hdrCtx.Upstream)
	assert.Equal(t, "failover_agg_chat_0", hdrCtx.Upstream.RouteCluster)
	assert.Equal(t, "openai-provider-cluster", hdrCtx.Upstream.Name, "RouteCluster must not overwrite the resolved Name")
}

func TestBuildUpstreamAttemptResponseContext_SetsRouteCluster(t *testing.T) {
	shared := NewUpstreamAttemptSharedContext("", "")
	headers := policy.NewHeaders(nil)

	respCtx := BuildUpstreamAttemptResponseContext(shared, "failover_agg_chat_0", headers,
		[]byte(`{}`), []byte(`{}`), "openai-provider-cluster", "https://api.openai.com", "/v1", "POST", "/chat/completions", 200)

	require.NotNil(t, respCtx.Upstream)
	assert.Equal(t, "failover_agg_chat_0", respCtx.Upstream.RouteCluster)
}
```

(Match the exact current parameter order of `BuildUpstreamAttemptResponseContext` — re-read its
signature from Step 4's edit before writing this call; the plan's example above assumes
`shared, rawClusterName, responseHeaders, originalRequestRaw, body, backendName, backendURL, basePath,
requestMethod, requestPath, statusCode` in that order, matching the pre-existing parameter order with
`rawClusterName` inserted second.)

- [ ] **Step 9: Run kernel tests**

Run: `cd gateway/gateway-runtime/policy-engine && go test ./internal/kernel/... -v 2>&1 | tail -80`
Expected: all PASS, including the two new tests.

- [ ] **Step 10: Run the full module build + test suite**

Run: `cd gateway/gateway-runtime/policy-engine && go build ./... && go test ./...`
Expected: all `ok`.

- [ ] **Step 11: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform/.claude/worktrees/llm-model-failover
git add gateway/gateway-runtime/policy-engine/internal/kernel/upstream_extproc.go gateway/gateway-runtime/policy-engine/internal/kernel/upstream_attempt.go gateway/gateway-runtime/policy-engine/internal/kernel/upstream_attempt_test.go
git commit -m "$(cat <<'EOF'
feat(llm-failover): thread the raw pre-resolution cluster name into upstream-attempt contexts

Captures xds.cluster_name before resolveBackend substitutes the resolved
real member cluster, and threads it through all four
BuildUpstreamAttempt*Context builders as RouteCluster — the signal an
upstream-attempt policy needs to know which aggregate/failover chain (if
any) this attempt belongs to.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Executor — CEL `executionCondition` for the upstream-attempt phase

**Files:**
- Modify: `gateway/gateway-runtime/policy-engine/internal/executor/chain.go`
- Test: `gateway/gateway-runtime/policy-engine/internal/executor/upstream_chain_test.go`

**Interfaces:**
- Consumes: `executor.CELEvaluator` interface (already defined in this file — unchanged),
  `registry.PolicyChain.HasExecutionConditions` (already exists, already covers upstream-attached
  entries per this session's own verification — no change needed to how it's computed).
- Produces: the four `ExecuteUpstreamAttempt*Policies` functions now skip a policy whose
  `spec.ExecutionCondition` evaluates false, exactly like their downstream counterparts. No signature
  changes — this is a pure behavior addition inside the existing function bodies.

- [ ] **Step 1: Locate the four functions and their downstream siblings for the exact pattern to mirror**

```bash
grep -n "func (c \*ChainExecutor) ExecuteUpstreamAttempt\|func (c \*ChainExecutor) ExecuteRequestHeaderPolicies\|func (c \*ChainExecutor) ExecuteRequestPolicies" gateway/gateway-runtime/policy-engine/internal/executor/chain.go
```

Read `ExecuteRequestHeaderPolicies`'s existing CEL block (the `if hasExecutionConditions && spec.ExecutionCondition != nil && *spec.ExecutionCondition != "" { conditionMet, err := c.celEvaluator.EvaluateRequestHeaderCondition(...) ... }` block) to copy its exact error-handling shape.

- [ ] **Step 2: Write a failing test for `ExecuteUpstreamAttemptRequestHeaderPolicies`**

Add to `upstream_chain_test.go`:

```go
func TestExecuteUpstreamAttemptRequestHeaderPolicies_SkipsWhenConditionFalse(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	fakeCEL := &fakeCELEvaluator{requestHeaderResult: false}
	exec := NewChainExecutor(nil, fakeCEL, tracer)

	ctx := context.Background()
	reqCtx := &policy.RequestHeaderContext{
		SharedContext: &policy.SharedContext{Metadata: map[string]interface{}{}},
		Headers:       policy.NewHeaders(nil),
		Upstream:      &policy.UpstreamRequestContext{Name: "test-backend"},
	}
	called := false
	pol := &upstreamHeaderMockPolicy{
		mode: policy.ProcessingMode{RequestHeaderMode: policy.HeaderModeProcess},
		onReq: func(_ *policy.RequestHeaderContext) policy.RequestHeaderAction {
			called = true
			return nil
		},
	}
	cond := "selected_provider == 'anthropic-upstream'"
	specs := []policy.PolicySpec{newPolicySpec("oauth2-generator", "v0", true, &cond)}

	_, err := exec.ExecuteUpstreamAttemptRequestHeaderPolicies(ctx, []policy.Policy{pol}, reqCtx, specs, "api", "route")

	require.NoError(t, err)
	assert.False(t, called, "policy must not run when its executionCondition evaluates false")
}

func TestExecuteUpstreamAttemptRequestHeaderPolicies_RunsWhenConditionTrue(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	fakeCEL := &fakeCELEvaluator{requestHeaderResult: true}
	exec := NewChainExecutor(nil, fakeCEL, tracer)

	ctx := context.Background()
	reqCtx := &policy.RequestHeaderContext{
		SharedContext: &policy.SharedContext{Metadata: map[string]interface{}{"selected_provider": "anthropic-upstream"}},
		Headers:       policy.NewHeaders(nil),
		Upstream:      &policy.UpstreamRequestContext{Name: "test-backend"},
	}
	called := false
	pol := &upstreamHeaderMockPolicy{
		mode: policy.ProcessingMode{RequestHeaderMode: policy.HeaderModeProcess},
		onReq: func(_ *policy.RequestHeaderContext) policy.RequestHeaderAction {
			called = true
			return nil
		},
	}
	cond := "selected_provider == 'anthropic-upstream'"
	specs := []policy.PolicySpec{newPolicySpec("oauth2-generator", "v0", true, &cond)}

	_, err := exec.ExecuteUpstreamAttemptRequestHeaderPolicies(ctx, []policy.Policy{pol}, reqCtx, specs, "api", "route")

	require.NoError(t, err)
	assert.True(t, called, "policy must run when its executionCondition evaluates true")
}
```

`upstreamHeaderMockPolicy` and `newPolicySpec` already exist in this test package (from earlier this
session's work) — reuse them, do not redefine. Check `newPolicySpec`'s exact signature first:

```bash
grep -n "func newPolicySpec" gateway/gateway-runtime/policy-engine/internal/executor/*.go
```

`newPolicySpec` today likely doesn't take an `ExecutionCondition` parameter — if so, either add an
overload/variant or construct the `policy.PolicySpec{}` literal directly in these two new tests instead
of using the helper. Prefer constructing the literal directly to avoid changing a shared helper's
signature and breaking other callers.

- [ ] **Step 3: Add the `fakeCELEvaluator` test double**

Check first whether a CEL evaluator test double already exists in this package
(`grep -rn "CELEvaluator" gateway/gateway-runtime/policy-engine/internal/executor/*_test.go`). If none
exists, add:

```go
type fakeCELEvaluator struct {
	requestHeaderResult  bool
	requestBodyResult    bool
	responseHeaderResult bool
	responseBodyResult   bool
	err                  error
}

func (f *fakeCELEvaluator) EvaluateRequestHeaderCondition(_ string, _ *policy.RequestHeaderContext) (bool, error) {
	return f.requestHeaderResult, f.err
}
func (f *fakeCELEvaluator) EvaluateRequestBodyCondition(_ string, _ *policy.RequestContext) (bool, error) {
	return f.requestBodyResult, f.err
}
func (f *fakeCELEvaluator) EvaluateResponseHeaderCondition(_ string, _ *policy.ResponseHeaderContext) (bool, error) {
	return f.responseHeaderResult, f.err
}
func (f *fakeCELEvaluator) EvaluateResponseBodyCondition(_ string, _ *policy.ResponseContext) (bool, error) {
	return f.responseBodyResult, f.err
}
func (f *fakeCELEvaluator) EvaluateStreamingRequestCondition(_ string, _ *policy.RequestStreamContext) (bool, error) {
	return true, nil
}
func (f *fakeCELEvaluator) EvaluateStreamingResponseCondition(_ string, _ *policy.ResponseStreamContext) (bool, error) {
	return true, nil
}
```

(Match this to the `CELEvaluator` interface's exact current method set in `chain.go` — add/remove
methods here if the interface has changed since this plan was written.)

- [ ] **Step 4: Run the new tests to confirm they fail**

Run: `cd gateway/gateway-runtime/policy-engine && go test ./internal/executor/... -run ExecuteUpstreamAttemptRequestHeaderPolicies -v`
Expected: FAIL (condition evaluation not implemented yet — `called` will be `true` in the first test
since nothing currently gates on the condition).

- [ ] **Step 5: Add CEL evaluation to `ExecuteUpstreamAttemptRequestHeaderPolicies`**

Inside the loop, after the existing `spec.Enabled` check and before calling `hp.OnRequestHeaders`, add:

```go
		if chain0HasExecutionConditions(specs) && spec.ExecutionCondition != nil && *spec.ExecutionCondition != "" {
			conditionMet, err := c.celEvaluator.EvaluateRequestHeaderCondition(*spec.ExecutionCondition, reqCtx)
			if err != nil {
				return finalAction, fmt.Errorf("condition evaluation failed for policy %s:%s: %w", spec.Name, spec.Version, err)
			}
			if !conditionMet {
				continue
			}
		}
```

Do not invent `chain0HasExecutionConditions` — these four functions don't currently receive a `*registry.PolicyChain`, only `policyList`/`specs`. Instead of threading a new parameter through every
call site (which would also require updating `upstream_extproc.go`'s four call sites and every
existing test), compute it cheaply inline instead: replace `chain0HasExecutionConditions(specs) &&` with
nothing — just check `spec.ExecutionCondition != nil && *spec.ExecutionCondition != ""` directly, the
same short-circuit the downstream functions effectively achieve via their own per-spec check. The
`hasExecutionConditions`/`chain.HasExecutionConditions` flag downstream is purely a chain-level fast
path to skip is a cheap `nil` map lookup shortcut across the WHOLE chain when no policy anywhere has a
condition — for a much shorter `chain.UpstreamPolicies` list, the per-spec check alone is sufficient and
avoids a signature change. Final block to add:

```go
		if spec.ExecutionCondition != nil && *spec.ExecutionCondition != "" {
			conditionMet, err := c.celEvaluator.EvaluateRequestHeaderCondition(*spec.ExecutionCondition, reqCtx)
			if err != nil {
				return finalAction, fmt.Errorf("condition evaluation failed for policy %s:%s: %w", spec.Name, spec.Version, err)
			}
			if !conditionMet {
				continue
			}
		}
```

- [ ] **Step 6: Run the two new tests again to confirm they pass**

Run: `cd gateway/gateway-runtime/policy-engine && go test ./internal/executor/... -run ExecuteUpstreamAttemptRequestHeaderPolicies -v`
Expected: both PASS.

- [ ] **Step 7: Repeat steps 2-6 for the other three functions**

`ExecuteUpstreamAttemptRequestPolicies` (using `EvaluateRequestBodyCondition`/`*policy.RequestContext`),
`ExecuteUpstreamAttemptResponseHeaderPolicies` (`EvaluateResponseHeaderCondition`/`*policy.ResponseHeaderContext`),
`ExecuteUpstreamAttemptResponsePolicies` (`EvaluateResponseBodyCondition`/`*policy.ResponseContext`).
Same test-then-implement shape, same inline condition check pattern (no signature changes).

- [ ] **Step 8: Run the full executor test suite**

Run: `cd gateway/gateway-runtime/policy-engine && go build ./... && go test ./internal/executor/... -v 2>&1 | tail -100`
Expected: all PASS, no regressions in the pre-existing tests from
`docs/superpowers/specs/2026-09-21-upstream-policy-reuse-design.md`'s work (e.g.
`TestExecuteUpstreamAttemptRequestPolicies_ExecutesForBackend` must still pass unconditioned — it
passes no `ExecutionCondition`, so the new check is a no-op for it).

- [ ] **Step 9: Run the full module test suite**

Run: `cd gateway/gateway-runtime/policy-engine && go test ./...`
Expected: all `ok`.

- [ ] **Step 10: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform/.claude/worktrees/llm-model-failover
git add gateway/gateway-runtime/policy-engine/internal/executor/chain.go gateway/gateway-runtime/policy-engine/internal/executor/upstream_chain_test.go
git commit -m "$(cat <<'EOF'
feat(llm-failover): evaluate CEL executionCondition in the upstream-attempt phase

Mirrors the existing downstream per-policy condition check in all four
ExecuteUpstreamAttempt* dispatch functions. This is what lets a
provider-scoped credential/transform policy (already gated downstream by
executionCondition on selected_provider) gate identically upstream, with
zero changes to the policy itself — only the executor learns to check the
condition it already carries.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: `model-failover` policy — scaffold, params, downstream target selection

**Files:**
- Create: `gateway/dev-policies/model-failover/go.mod`
- Create: `gateway/dev-policies/model-failover/modelfailover.go`
- Create: `gateway/dev-policies/model-failover/modelfailover_test.go`

**Interfaces:**
- Consumes: `ModelFailoverParams`/`FailoverTargetEntry`/`FailoverTarget` (this plan's shared contract,
  above) — defined in this task.
- Produces: `package modelfailover`, `func GetPolicy(metadata policy.PolicyMetadata, params
  map[string]interface{}) (policy.Policy, error)`, `type Policy struct{...}` implementing
  `policy.RequestPolicy` (`OnRequestBody`) and `policy.Mode`. Consumed by Task 5 (adds the
  upstream-attempt methods to the same `Policy` type) and Task 6 (gateway-controller attaches it by
  name `"model-failover"`).

- [ ] **Step 1: Create the module**

```bash
mkdir -p gateway/dev-policies/model-failover
cd gateway/dev-policies/model-failover
cat > go.mod <<'EOF'
module github.com/wso2/gateway-controllers/policies/model-failover

go 1.26.5

require github.com/wso2/api-platform/sdk/core v0.3.5

// Local dev-policies copy — points at this repo's own sdk/core so the
// unreleased UpstreamRequestContext.RouteCluster field this policy depends
// on is available. See gateway/dev-policies/FAILOVER_TESTING.md. Remove
// this replace once model-failover has a real release with RouteCluster
// available in its published sdk/core dependency.
replace github.com/wso2/api-platform/sdk/core => ../../../sdk/core
EOF
```

- [ ] **Step 2: Write the failing test for params parsing**

```go
package modelfailover

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseParams_ValidTargets(t *testing.T) {
	raw := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"target": map[string]interface{}{"model": "gpt-4o"},
				"fallbacks": []interface{}{
					map[string]interface{}{"model": "claude-sonnet-4-5-20250929", "provider": "anthropic-upstream"},
				},
				"aggregateCluster": "failover_agg_chat_0",
			},
		},
		"suspendDuration": float64(900),
	}

	params, err := parseParams(raw)

	require.NoError(t, err)
	require.Len(t, params.Targets, 1)
	assert.Equal(t, "gpt-4o", params.Targets[0].Target.Model)
	assert.Equal(t, "failover_agg_chat_0", params.Targets[0].AggregateCluster)
	require.Len(t, params.Targets[0].Fallbacks, 1)
	assert.Equal(t, "anthropic-upstream", params.Targets[0].Fallbacks[0].Provider)
	assert.Equal(t, 900, params.SuspendDuration)
}

func TestParseParams_MissingTargets(t *testing.T) {
	_, err := parseParams(map[string]interface{}{})
	require.Error(t, err)
}
```

- [ ] **Step 3: Run to confirm it fails**

Run: `cd gateway/dev-policies/model-failover && GOWORK=off go test ./... -run TestParseParams -v`
Expected: FAIL (`parseParams` undefined).

- [ ] **Step 4: Implement `modelfailover.go` — types, params parsing, `Mode`, `GetPolicy`, downstream `OnRequestBody`**

```go
/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

// Package modelfailover implements cross-provider LLM model failover: retry
// a request against a declared fallback chain of {model, provider} targets
// on upstream failure, with correct per-attempt credential injection and
// payload translation. See docs/superpowers/specs/2026-09-21-model-failover-policy-design.md.
package modelfailover

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

const PolicyName = "model-failover"

const (
	selectedProviderMetadataKey = "selected_provider"
	selectedModelMetadataKey    = "selected_model"
)

// ResolvedFailoverProviderHeader mirrors the header name the (now-removed)
// kernel-side mechanism used, so downstream analytics attribution keeps
// working unchanged. See kernel.ResolvedFailoverProviderHeader's own history.
const ResolvedFailoverProviderHeader = "x-wso2-resolved-failover-provider"

var (
	_ policy.Policy               = (*Policy)(nil)
	_ policy.RequestPolicy        = (*Policy)(nil)
	_ policy.RequestHeaderPolicy  = (*Policy)(nil)
	_ policy.ResponseHeaderPolicy = (*Policy)(nil)
)

// FailoverTarget identifies one chain member.
type FailoverTarget struct {
	Model    string `json:"model"`
	Provider string `json:"provider,omitempty"`
}

// FailoverTargetEntry is one client-requested-model's primary + fallback chain.
type FailoverTargetEntry struct {
	Target FailoverTarget   `json:"target"`
	Fallbacks []FailoverTarget `json:"fallbacks"`
	// AggregateCluster is injected by gateway-controller — see this plan's
	// shared params contract.
	AggregateCluster string `json:"aggregateCluster"`
}

// ModelFailoverParams is the parsed shape of this policy's params.
type ModelFailoverParams struct {
	Targets         []FailoverTargetEntry `json:"targets"`
	SuspendDuration int                   `json:"suspendDuration"`
}

// Policy implements downstream target selection and, per upstream attempt,
// chain-position resolution + provider/model metadata seeding + suspension.
type Policy struct {
	params ModelFailoverParams

	mu              sync.Mutex
	suspendedTargets map[string]time.Time
}

// GetPolicy is the v1alpha2 factory entry point.
func GetPolicy(_ policy.PolicyMetadata, rawParams map[string]interface{}) (policy.Policy, error) {
	params, err := parseParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid params: %w", PolicyName, err)
	}
	return &Policy{params: params, suspendedTargets: make(map[string]time.Time)}, nil
}

func parseParams(raw map[string]interface{}) (ModelFailoverParams, error) {
	var params ModelFailoverParams

	targetsRaw, ok := raw["targets"]
	if !ok {
		return params, fmt.Errorf("'targets' is required")
	}

	// Round-trip through JSON to reuse encoding/json's struct-tag-driven
	// decoding instead of hand-walking map[string]interface{} — the params
	// map already came from a JSON-shaped source (policy config), so this
	// is a safe, standard pattern for this kind of nested parsing.
	blob, err := json.Marshal(map[string]interface{}{"targets": targetsRaw})
	if err != nil {
		return params, fmt.Errorf("failed to marshal 'targets': %w", err)
	}
	if err := json.Unmarshal(blob, &params); err != nil {
		return params, fmt.Errorf("failed to parse 'targets': %w", err)
	}
	if len(params.Targets) == 0 {
		return params, fmt.Errorf("'targets' must have at least one entry")
	}

	if suspendRaw, ok := raw["suspendDuration"]; ok {
		switch v := suspendRaw.(type) {
		case float64:
			params.SuspendDuration = int(v)
		case int:
			params.SuspendDuration = v
		default:
			return params, fmt.Errorf("'suspendDuration' must be a number")
		}
	}

	return params, nil
}

// Mode declares participation in both the downstream body phase (target
// selection) and — when this instance is separately attached via
// upstreamPolicies: — the upstream-attempt request/response header phases
// (chain resolution, metadata seeding, suspension recording). See
// docs/superpowers/specs/2026-09-21-upstream-policy-reuse-design.md: there is
// no separate upstream mode/interface — attachment point alone selects the
// phase, and RequestHeaderMode/ResponseHeaderMode below govern whichever
// phase this specific instance actually runs in (Downstream == nil tells the
// two apart inside the handlers).
func (p *Policy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeProcess,
		ResponseBodyMode:   policy.BodyModeSkip,
	}
}

// requestedModel extracts the OpenAI-shaped "model" field from a JSON request body.
func requestedModel(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Model)
}

// findEntry returns the target entry whose Target.Model matches the
// requested model, or nil.
func (p *Policy) findEntry(model string) *FailoverTargetEntry {
	for i := range p.params.Targets {
		if strings.EqualFold(p.params.Targets[i].Target.Model, model) {
			return &p.params.Targets[i]
		}
	}
	return nil
}

// suspensionKey matches model-round-robin's own key shape
// (routeKey+model+provider is unavailable here without threading routeKey
// through GetPolicy — this policy instance is already route-scoped by
// attachment, the same way model-round-robin's own instance is, so the key
// only needs model+provider).
func suspensionKey(model, provider string) string {
	return model + "|" + provider
}

// isSuspended checks and lazily clears an expired suspension entry — same
// check-and-delete-if-expired pattern model-round-robin uses (no background
// sweep).
func (p *Policy) isSuspended(model, provider string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := suspensionKey(model, provider)
	until, ok := p.suspendedTargets[key]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(p.suspendedTargets, key)
	return false
}

func (p *Policy) suspend(model, provider string) {
	if p.params.SuspendDuration <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.suspendedTargets[suspensionKey(model, provider)] = time.Now().Add(time.Duration(p.params.SuspendDuration) * time.Second)
}

// OnRequestBody parses the client-requested model, matches it against the
// configured chain, and routes to that chain's aggregate cluster — skipping
// straight to the first non-suspended fallback's own single cluster if the
// primary target is currently suspended (bypassing the aggregate for a
// known-bad primary, per the design's §4).
func (p *Policy) OnRequestBody(_ context.Context, reqCtx *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
	if reqCtx.Body == nil || !reqCtx.Body.Present || len(reqCtx.Body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	model := requestedModel(reqCtx.Body.Content)
	if model == "" {
		return policy.UpstreamRequestModifications{}
	}

	entry := p.findEntry(model)
	if entry == nil {
		return policy.UpstreamRequestModifications{}
	}

	if !p.isSuspended(entry.Target.Model, entry.Target.Provider) {
		cluster := entry.AggregateCluster
		return policy.UpstreamRequestModifications{UpstreamName: &cluster}
	}

	for _, fallback := range entry.Fallbacks {
		if !p.isSuspended(fallback.Model, fallback.Provider) {
			// Route directly to the fallback's own upstream, bypassing the
			// aggregate entirely — a known-bad primary is skipped, at the
			// cost of no further in-request retry if this fallback also
			// fails (the aggregate was never entered). See design doc's
			// discussion of this tradeoff (inherited from the superseded
			// design's §5.3/§10).
			upstream := fallback.Provider
			return policy.UpstreamRequestModifications{UpstreamName: &upstream}
		}
	}

	// Every member suspended — fall through to the aggregate anyway (Envoy's
	// own retry exhaustion behavior applies; nothing left to skip to).
	cluster := entry.AggregateCluster
	return policy.UpstreamRequestModifications{UpstreamName: &cluster}
}
```

- [ ] **Step 5: Run the params test to confirm it passes**

Run: `cd gateway/dev-policies/model-failover && GOWORK=off go test ./... -run TestParseParams -v`
Expected: both PASS.

- [ ] **Step 6: Write a failing test for downstream target selection**

```go
func TestOnRequestBody_RoutesToMatchedTargetAggregate(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{
				{
					Target:           FailoverTarget{Model: "gpt-4o"},
					Fallbacks:        []FailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
					AggregateCluster: "failover_agg_chat_0",
				},
			},
		},
		suspendedTargets: make(map[string]time.Time),
	}
	reqCtx := &policy.RequestContext{
		Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true},
	}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "failover_agg_chat_0", *mods.UpstreamName)
}

func TestOnRequestBody_NoMatchIsNoop(t *testing.T) {
	p := &Policy{
		params:           ModelFailoverParams{Targets: []FailoverTargetEntry{{Target: FailoverTarget{Model: "gpt-4o"}}}},
		suspendedTargets: make(map[string]time.Time),
	}
	reqCtx := &policy.RequestContext{Body: &policy.Body{Content: []byte(`{"model":"some-other-model"}`), Present: true}}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	assert.Nil(t, mods.UpstreamName)
}

func TestOnRequestBody_SuspendedTargetSkipsToFallback(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{
				{
					Target:           FailoverTarget{Model: "gpt-4o"},
					Fallbacks:        []FailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
					AggregateCluster: "failover_agg_chat_0",
				},
			},
		},
		suspendedTargets: map[string]time.Time{
			suspensionKey("gpt-4o", ""): time.Now().Add(time.Hour),
		},
	}
	reqCtx := &policy.RequestContext{Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "anthropic-upstream", *mods.UpstreamName, "must bypass the aggregate and target the fallback's own upstream directly")
}
```

- [ ] **Step 7: Run — these should already pass since Step 4 implemented `OnRequestBody` alongside its test-driven design**

Run: `cd gateway/dev-policies/model-failover && GOWORK=off go test ./... -v`
Expected: all PASS. (If any fail, fix `OnRequestBody`/`isSuspended`/`suspensionKey` until green — this
is the one task in this plan where implementation and test were written together rather than strictly
red-then-green, because the downstream logic and its params contract needed to be nailed down as one
unit; still confirm green before moving on.)

- [ ] **Step 8: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform/.claude/worktrees/llm-model-failover
git add gateway/dev-policies/model-failover/
git commit -m "$(cat <<'EOF'
feat(llm-failover): scaffold model-failover policy — params, downstream target selection

New policy replacing the resilience.failover schema section: parses the
client-requested model, matches it against configured targets/fallbacks,
checks its own suspension state, and routes to the matched chain's
aggregate cluster via the existing UpstreamRequestModifications.UpstreamName
mechanism. Upstream-attempt behavior (chain resolution, metadata seeding,
suspension recording) added in a follow-up commit.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: `model-failover` policy — upstream-attempt chain resolution and suspension recording

**Files:**
- Modify: `gateway/dev-policies/model-failover/modelfailover.go`
- Modify: `gateway/dev-policies/model-failover/modelfailover_test.go`

**Interfaces:**
- Consumes: `policy.UpstreamRequestContext.RouteCluster`/`policy.UpstreamResponseContext.RouteCluster`
  (Task 1), `policy.RequestHeaderContext`/`policy.ResponseHeaderContext` with `Downstream == nil` for
  an attempt invocation (existing SDK contract from
  `docs/superpowers/specs/2026-09-21-upstream-policy-reuse-design.md`).
- Produces: `Policy.OnRequestHeaders`, `Policy.OnResponseHeaders` — both already declared as
  implemented in Task 4's compile-time assertions (`_ policy.RequestHeaderPolicy = (*Policy)(nil)`,
  `_ policy.ResponseHeaderPolicy = (*Policy)(nil)`); this task provides their bodies.

- [ ] **Step 1: Write the failing test for attempt-index resolution and metadata seeding**

```go
func TestOnRequestHeaders_ResolvesTargetAttemptAndSeedsMetadata(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{
				{
					Target:           FailoverTarget{Model: "gpt-4o", Provider: ""},
					Fallbacks:        []FailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
					AggregateCluster: "failover_agg_chat_0",
				},
			},
		},
		suspendedTargets: make(map[string]time.Time),
	}
	reqCtx := &policy.RequestHeaderContext{
		SharedContext: &policy.SharedContext{Metadata: map[string]interface{}{}},
		Headers:       policy.NewHeaders(map[string][]string{"x-envoy-attempt-count": {"1"}}),
		Upstream:      &policy.UpstreamRequestContext{RouteCluster: "failover_agg_chat_0"},
	}

	p.OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "gpt-4o", reqCtx.SharedContext.Metadata[selectedModelMetadataKey])
	// The primary target's provider is empty (the LlmProxy's own primary) —
	// selected_provider should reflect that as empty too, not "" vs unset
	// ambiguity; assert the key is present and empty, matching how
	// llm-header-router treats an unset provider (see its own doc comment).
	assert.Equal(t, "", reqCtx.SharedContext.Metadata[selectedProviderMetadataKey])
}

func TestOnRequestHeaders_ResolvesFallbackAttempt(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{
				{
					Target:           FailoverTarget{Model: "gpt-4o"},
					Fallbacks:        []FailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
					AggregateCluster: "failover_agg_chat_0",
				},
			},
		},
		suspendedTargets: make(map[string]time.Time),
	}
	reqCtx := &policy.RequestHeaderContext{
		SharedContext: &policy.SharedContext{Metadata: map[string]interface{}{}},
		Headers:       policy.NewHeaders(map[string][]string{"x-envoy-attempt-count": {"2"}}),
		Upstream:      &policy.UpstreamRequestContext{RouteCluster: "failover_agg_chat_0"},
	}

	p.OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "claude-sonnet-4-5-20250929", reqCtx.SharedContext.Metadata[selectedModelMetadataKey])
	assert.Equal(t, "anthropic-upstream", reqCtx.SharedContext.Metadata[selectedProviderMetadataKey])
}

func TestOnRequestHeaders_UnknownClusterIsNoop(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{{Target: FailoverTarget{Model: "gpt-4o"}, AggregateCluster: "failover_agg_chat_0"}},
		},
		suspendedTargets: make(map[string]time.Time),
	}
	reqCtx := &policy.RequestHeaderContext{
		SharedContext: &policy.SharedContext{Metadata: map[string]interface{}{}},
		Headers:       policy.NewHeaders(nil),
		Upstream:      &policy.UpstreamRequestContext{RouteCluster: "some-unrelated-cluster"},
	}

	p.OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Empty(t, reqCtx.SharedContext.Metadata, "an attempt on a cluster this instance doesn't own must not write any metadata")
}

func TestOnResponseHeaders_RecordsSuspensionOn5xxAndSetsResolvedProviderHeader(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{
				{
					Target:           FailoverTarget{Model: "gpt-4o"},
					Fallbacks:        []FailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
					AggregateCluster: "failover_agg_chat_0",
				},
			},
			SuspendDuration: 900,
		},
		suspendedTargets: make(map[string]time.Time),
	}
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		Headers:        policy.NewHeaders(map[string][]string{"x-envoy-attempt-count": {"1"}}),
		ResponseStatus: 500,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0"},
	}

	p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.True(t, p.isSuspended("gpt-4o", ""), "a 5xx on the primary attempt must suspend it")
}
```

(This test asserts against `policy.ResponseHeaderContext.Headers` for the attempt-count header and
`policy.ResponseHeaderContext.Upstream.RouteCluster` — confirm these field names against the actual
current `ResponseHeaderContext` struct shape in `sdk/core/policy/v1alpha2/context.go` before writing
this test; adjust field names if they differ; the shape used here matches what earlier tasks in this
session's `upstream_extproc.go` work already builds via `BuildUpstreamAttemptResponseHeaderContext`.)

- [ ] **Step 2: Run to confirm all four fail**

Run: `cd gateway/dev-policies/model-failover && GOWORK=off go test ./... -run "OnRequestHeaders|OnResponseHeaders" -v`
Expected: FAIL (`OnRequestHeaders`/`OnResponseHeaders` undefined on `*Policy`).

- [ ] **Step 3: Implement `OnRequestHeaders`**

Add to `modelfailover.go`:

```go
// findEntryByAggregateCluster returns the target entry whose AggregateCluster
// matches cluster, or nil.
func (p *Policy) findEntryByAggregateCluster(cluster string) *FailoverTargetEntry {
	if cluster == "" {
		return nil
	}
	for i := range p.params.Targets {
		if p.params.Targets[i].AggregateCluster == cluster {
			return &p.params.Targets[i]
		}
	}
	return nil
}

// attemptCount reads x-envoy-attempt-count, defaulting to 1 (the primary
// attempt) for a missing or unparseable header — the same default
// resolveBackend used to apply, now owned here instead.
func attemptCount(headers *policy.Headers) int {
	values := headers.Get("x-envoy-attempt-count")
	if len(values) == 0 {
		return 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil || n <= 0 {
		return 1
	}
	return n
}

// resolveAttempt returns the FailoverTarget for the given entry and attempt
// index (1 = the primary target, 2 = Fallbacks[0], etc.), or nil if the
// index runs past the end of the configured chain.
func resolveAttempt(entry *FailoverTargetEntry, index int) *FailoverTarget {
	if index <= 1 {
		return &entry.Target
	}
	fallbackIdx := index - 2
	if fallbackIdx < 0 || fallbackIdx >= len(entry.Fallbacks) {
		return nil
	}
	return &entry.Fallbacks[fallbackIdx]
}

// OnRequestHeaders runs only for an upstream-attempt invocation on a cluster
// this instance's own chain owns (reqCtx.Upstream.RouteCluster matches one
// of params.Targets[].AggregateCluster) — a no-op for a downstream
// invocation or an attempt on an unrelated cluster. It resolves which chain
// member this specific attempt represents and seeds selected_provider/
// selected_model metadata, the upstream-phase equivalent of what
// llm-header-router writes downstream (see
// docs/superpowers/specs/2026-09-21-upstream-policy-reuse-design.md).
func (p *Policy) OnRequestHeaders(_ context.Context, reqCtx *policy.RequestHeaderContext, _ map[string]interface{}) policy.RequestHeaderAction {
	if reqCtx.Upstream == nil {
		return nil
	}
	entry := p.findEntryByAggregateCluster(reqCtx.Upstream.RouteCluster)
	if entry == nil {
		return nil
	}

	member := resolveAttempt(entry, attemptCount(reqCtx.Headers))
	if member == nil {
		return nil
	}

	if reqCtx.SharedContext.Metadata == nil {
		reqCtx.SharedContext.Metadata = map[string]interface{}{}
	}
	reqCtx.SharedContext.Metadata[selectedModelMetadataKey] = member.Model
	reqCtx.SharedContext.Metadata[selectedProviderMetadataKey] = member.Provider

	return nil
}
```

- [ ] **Step 4: Implement `OnResponseHeaders`**

```go
// OnResponseHeaders records suspension for a failing attempt (matching the
// superseded design's suspendIfFailoverAttemptFailed: any 5xx, no
// retryOn-specific matching beyond that today) and sets
// ResolvedFailoverProviderHeader for the downstream analytics-attribution
// fix, when this attempt actually escalated past the primary (index > 1).
func (p *Policy) OnResponseHeaders(_ context.Context, respCtx *policy.ResponseHeaderContext, _ map[string]interface{}) policy.ResponseHeaderAction {
	if respCtx.Upstream == nil {
		return nil
	}
	entry := p.findEntryByAggregateCluster(respCtx.Upstream.RouteCluster)
	if entry == nil {
		return nil
	}

	index := attemptCount(respCtx.Headers)
	member := resolveAttempt(entry, index)
	if member == nil {
		return nil
	}

	if respCtx.ResponseStatus >= 500 {
		p.suspend(member.Model, member.Provider)
	}

	if index > 1 {
		return policy.DownstreamResponseHeaderModifications{
			HeadersToSet: map[string]string{ResolvedFailoverProviderHeader: member.Provider},
		}
	}

	return nil
}
```

- [ ] **Step 5: Run all four new tests**

Run: `cd gateway/dev-policies/model-failover && GOWORK=off go test ./... -run "OnRequestHeaders|OnResponseHeaders" -v`
Expected: all PASS.

- [ ] **Step 6: Run the full policy test suite**

Run: `cd gateway/dev-policies/model-failover && GOWORK=off go build ./... && GOWORK=off go test ./... -v`
Expected: all PASS.

- [ ] **Step 7: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform/.claude/worktrees/llm-model-failover
git add gateway/dev-policies/model-failover/
git commit -m "$(cat <<'EOF'
feat(llm-failover): model-failover upstream-attempt chain resolution and suspension recording

OnRequestHeaders resolves which chain member (target or fallback N) this
specific upstream attempt represents, from RouteCluster + x-envoy-attempt-count,
and seeds selected_provider/selected_model metadata for provider-scoped
credential/transform policies to gate on via executionCondition.
OnResponseHeaders records suspension on a 5xx and sets the resolved-provider
analytics-attribution header for an escalated attempt.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Build wiring — add `model-failover` to `build.yaml`

**Files:**
- Modify: `gateway/build.yaml`
- Modify: `gateway/dev-policies/FAILOVER_TESTING.md`

**Interfaces:**
- Consumes: `gateway/dev-policies/model-failover/` (Tasks 4-5).
- Produces: a `model-failover` entry gateway-runtime's build can compile, for Task 8's gateway-controller
  work to attach by name and for local end-to-end testing.

- [ ] **Step 1: Add the entry to `build.yaml`, alphabetically ordered among the other policies**

```bash
grep -n "name: model-round-robin\|name: model-weighted-round-robin" gateway/build.yaml
```

Insert immediately before `model-round-robin` (alphabetical: `model-failover` < `model-round-robin`):

```yaml
  # TEMPORARY (no released module yet — model-failover is new in this
  # session). Remove this filePath: override and add the gomodule: line
  # once a real release exists in github.com/wso2/gateway-controllers.
  - name: model-failover
    filePath: ./dev-policies/model-failover
```

- [ ] **Step 2: Update `FAILOVER_TESTING.md`'s "What's here" section**

Add a bullet noting the new local-only policy and its temporary status, following the same phrasing
this session already used for the other three (now-removed) `filePath:` overrides, so the doc stays
internally consistent with its own established caveat style:

```markdown
- `dev-policies/model-failover/` — the new model-failover policy (see
  `docs/superpowers/specs/2026-09-21-model-failover-policy-design.md`), local-only until it has a real
  release in `github.com/wso2/gateway-controllers`. `build.yaml` has a temporary `filePath:` override
  for it — remove once a `gomodule:` version exists.
```

- [ ] **Step 3: Confirm the module builds in isolation**

Run: `cd gateway/dev-policies/model-failover && GOWORK=off go build ./...`
Expected: no output (success) — already true from Task 4/5, this step just reconfirms nothing in this
task broke it.

- [ ] **Step 4: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform/.claude/worktrees/llm-model-failover
git add gateway/build.yaml gateway/dev-policies/FAILOVER_TESTING.md
git commit -m "$(cat <<'EOF'
feat(llm-failover): wire model-failover into build.yaml (temporary filePath override)

No released gomodule exists yet for this brand-new policy — same
temporary-override convention this session already used (and later
removed) for the other dev-policies forks. Remove once model-failover has
a real release.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: gateway-controller — parse `model-failover` params, drive the existing xDS generation from them

**Files:**
- Modify: `gateway/gateway-controller/pkg/transform/llm.go`
- Test: `gateway/gateway-controller/pkg/transform/llm_test.go` (check exact existing test file name
  first: `ls gateway/gateway-controller/pkg/transform/*failover*`)

**Interfaces:**
- Consumes: this plan's shared `ModelFailoverParams`/`FailoverTargetEntry`/`FailoverTarget` contract
  (mirrored here as gateway-controller's own equivalent types, since gateway-controller cannot import
  the policy's Go package — separate module/repo); `xds.AggregateClusterName(routeKey string,
  targetIndex int) string` (already exported, `gateway-controller/pkg/xds/failover_cluster.go:31`,
  unchanged by this task).
- Produces: `func modelFailoverPolicyParams(policies *[]api.OperationPolicy) (*api.Policy, bool)` (or
  equivalent — finds a `model-failover`-named entry) and `func buildRouteFailoverFromPolicy(pol
  *api.Policy, routeKey string) (*models.RouteFailover, error)` — the new source `applyFailoverToRoutes`
  reads from, producing the exact same `*models.RouteFailover` shape it already builds from the schema
  today. Consumed by Task 8 (attachment-loop wiring) and Task 9 (removal of the old schema-driven path).

- [ ] **Step 1: Read the current `applyFailoverToRoutes`/`resolveFailoverEntry` to confirm the exact target shape they build**

```bash
sed -n '1,50p;180,300p' gateway/gateway-controller/pkg/transform/llm.go | grep -n "func applyFailoverToRoutes\|func resolveFailoverEntry" 
```

Read both functions in full (they're at approximately lines 193 and 251 per this session's earlier
research — confirm current line numbers first) before writing this task's code, since this task's new
function must produce input those two already-correct functions can consume unchanged.

- [ ] **Step 2: Write the failing test for finding the policy and parsing its params**

Add to the failover-related test file (or create `gateway/gateway-controller/pkg/transform/model_failover_policy_test.go` if no clearly-matching existing file is found):

```go
package transform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
)

func TestFindModelFailoverPolicy_Found(t *testing.T) {
	params := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"target": map[string]interface{}{"model": "gpt-4o"},
			},
		},
	}
	policies := []api.OperationPolicy{
		{Name: "some-other-policy", Version: "v1"},
		{Name: "model-failover", Version: "v0", Params: &params},
	}

	found, ok := findModelFailoverPolicy(&policies)

	require.True(t, ok)
	assert.Equal(t, "model-failover", found.Name)
}

func TestFindModelFailoverPolicy_NotFound(t *testing.T) {
	policies := []api.OperationPolicy{{Name: "some-other-policy", Version: "v1"}}

	_, ok := findModelFailoverPolicy(&policies)

	assert.False(t, ok)
}

func TestParseModelFailoverParams_ValidatesProviderReference(t *testing.T) {
	params := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"target": map[string]interface{}{"model": "gpt-4o"},
				"fallbacks": []interface{}{
					map[string]interface{}{"model": "claude-sonnet-4-5-20250929", "provider": "unregistered-provider"},
				},
			},
		},
	}

	_, err := parseModelFailoverParams(params, []string{"anthropic-upstream"}, "openai-primary")

	require.Error(t, err, "a provider not matching any additionalProviders[].as/.id must be a deploy-time error")
}
```

(Adjust the `api.OperationPolicy`/`api.Policy` import alias and exact field names —
`Params *map[string]interface{}` vs `map[string]interface{}` — to match the real generated types in
`gateway-controller/pkg/api/management/generated.go`; check with
`grep -n "type OperationPolicy struct" -A 10 gateway/gateway-controller/pkg/api/management/generated.go`
before finalizing this test.)

- [ ] **Step 3: Run to confirm failure**

Run: `cd gateway/gateway-controller && go test ./pkg/transform/... -run "FindModelFailoverPolicy|ParseModelFailoverParams" -v`
Expected: FAIL (functions undefined).

- [ ] **Step 4: Implement `findModelFailoverPolicy` and `parseModelFailoverParams` in `llm.go`**

```go
const modelFailoverPolicyName = "model-failover"

// findModelFailoverPolicy scans an operation-level policy list for a
// model-failover attachment. Mirrors how other policy-name lookups already
// work in this file (e.g. apiKeyAuthValuePrefix's scan of globalPolicies).
func findModelFailoverPolicy(policies *[]api.OperationPolicy) (*api.OperationPolicy, bool) {
	if policies == nil {
		return nil, false
	}
	for i := range *policies {
		if (*policies)[i].Name == modelFailoverPolicyName {
			return &(*policies)[i], true
		}
	}
	return nil, false
}

// modelFailoverTarget/modelFailoverTargetEntry/modelFailoverParams mirror
// the policy's own ModelFailoverParams/FailoverTargetEntry/FailoverTarget
// shape (gateway/dev-policies/model-failover/modelfailover.go) — kept as a
// separate, hand-written type here since gateway-controller cannot import
// the policy's Go package (separate module). Keep field names/JSON tags in
// lockstep with that file.
type modelFailoverTarget struct {
	Model    string `json:"model"`
	Provider string `json:"provider,omitempty"`
}

type modelFailoverTargetEntry struct {
	Target           modelFailoverTarget   `json:"target"`
	Fallbacks        []modelFailoverTarget `json:"fallbacks"`
	AggregateCluster string                `json:"aggregateCluster,omitempty"`
}

type modelFailoverParams struct {
	Targets         []modelFailoverTargetEntry `json:"targets"`
	SuspendDuration int                        `json:"suspendDuration"`
}

// parseModelFailoverParams parses raw policy params and validates every
// provider reference resolves to an additionalProviders[].as/.id entry
// (availableProviders) or the primary provider — the same rule
// resolveFailoverEntry already enforces for the schema-driven path, applied
// here at parse time instead so a bad config is rejected before any xDS
// generation runs.
func parseModelFailoverParams(raw map[string]interface{}, availableProviders []string, primaryProviderID string) (*modelFailoverParams, error) {
	blob, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal model-failover params: %w", err)
	}
	var params modelFailoverParams
	if err := json.Unmarshal(blob, &params); err != nil {
		return nil, fmt.Errorf("failed to parse model-failover params: %w", err)
	}
	if len(params.Targets) == 0 {
		return nil, fmt.Errorf("model-failover: 'targets' must have at least one entry")
	}

	allowed := make(map[string]bool, len(availableProviders)+1)
	allowed[primaryProviderID] = true
	for _, p := range availableProviders {
		allowed[p] = true
	}

	validateProvider := func(t modelFailoverTarget) error {
		provider := t.Provider
		if provider == "" {
			provider = primaryProviderID
		}
		if !allowed[provider] {
			return fmt.Errorf("model-failover: provider %q does not match any additionalProviders or the primary provider", provider)
		}
		return nil
	}

	for _, entry := range params.Targets {
		if err := validateProvider(entry.Target); err != nil {
			return nil, err
		}
		for _, fb := range entry.Fallbacks {
			if err := validateProvider(fb); err != nil {
				return nil, err
			}
		}
	}

	return &params, nil
}
```

(`json`/`fmt` imports — confirm they're already imported in `llm.go`, add if not.)

- [ ] **Step 5: Run the tests again to confirm green**

Run: `cd gateway/gateway-controller && go test ./pkg/transform/... -run "FindModelFailoverPolicy|ParseModelFailoverParams" -v`
Expected: all PASS.

- [ ] **Step 6: Write the failing test for building `*models.RouteFailover` + injecting `aggregateCluster`**

```go
func TestBuildRouteFailoverFromPolicy_InjectsAggregateClusterName(t *testing.T) {
	params := &modelFailoverParams{
		Targets: []modelFailoverTargetEntry{
			{
				Target:    modelFailoverTarget{Model: "gpt-4o"},
				Fallbacks: []modelFailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
			},
		},
		SuspendDuration: 900,
	}

	rf, expandedParams, err := buildRouteFailoverFromPolicy(params, "test-route-key", /* ...whatever additional args resolveFailoverEntry needs to resolve real clusters, e.g. provider->cluster lookup — determine from Step 1's reading of resolveFailoverEntry's actual signature and pass equivalent test fixtures */)

	require.NoError(t, err)
	require.Len(t, rf.Targets, 1)
	assert.Equal(t, "failover_agg_test-route-key_0", rf.Targets[0].Target.ClusterKey) // adjust to xds.AggregateClusterName's real sanitization behavior — verify by calling it directly in this test instead of hand-computing the string
	require.Len(t, expandedParams.Targets, 1)
	assert.NotEmpty(t, expandedParams.Targets[0].AggregateCluster, "must inject the aggregate cluster name back into the params the policy instance will carry")
}
```

**Implementer note:** this step's exact signature depends on what `resolveFailoverEntry` (read in Step
1) actually needs to resolve a `{model, provider}` pair into a real `ClusterKey`/`Upstream` — reuse that
existing function's logic/signature rather than reimplementing cluster resolution. Write this test only
after Step 1's reading is complete, matching `resolveFailoverEntry`'s real signature exactly. The
function under test, `buildRouteFailoverFromPolicy`, should return both the `*models.RouteFailover`
(fed to the existing, unchanged xDS generation) and a copy of `*modelFailoverParams` with each entry's
`AggregateCluster` field populated via `xds.AggregateClusterName(routeKey, index)` (Task 7's Step 4
work) — that expanded-params copy is what Task 8 attaches back onto the policy instance.

- [ ] **Step 7: Implement `buildRouteFailoverFromPolicy`, reusing `resolveFailoverEntry`'s existing per-target resolution**

Implement using the exact shape `resolveFailoverEntry` already produces (read in Step 1) — call it (or
extract its core logic into a small shared helper if it's currently unexported and tightly coupled to
the schema type) once per `modelFailoverTargetEntry`, and additionally set:

```go
entry.AggregateCluster = xdspkg.AggregateClusterName(routeKey, index)
```

(import `gateway-controller/pkg/xds` under whatever alias the file already uses for cross-package xDS
helpers — check existing imports in `llm.go` first).

- [ ] **Step 8: Run all new tests**

Run: `cd gateway/gateway-controller && go test ./pkg/transform/... -run "ModelFailover" -v`
Expected: all PASS.

- [ ] **Step 9: Run the full transform package test suite**

Run: `cd gateway/gateway-controller && go build ./... && go test ./pkg/transform/...`
Expected: all `ok`.

- [ ] **Step 10: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform/.claude/worktrees/llm-model-failover
git add gateway/gateway-controller/pkg/transform/
git commit -m "$(cat <<'EOF'
feat(llm-failover): parse model-failover policy params into the existing RouteFailover shape

New source for the already-correct xDS generation pipeline
(pkg/xds/failover_cluster.go, pkg/xds/translator.go — both unchanged):
finds a model-failover-named policy attachment, validates its provider
references the same way the schema-driven path already did, and builds the
same *models.RouteFailover the existing generation code already consumes —
plus injects each target's assigned aggregate cluster name back into the
policy's own params, so the runtime policy instance never needs to compute
it independently.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: gateway-controller — attach `model-failover` + provider-scoped `upstreamPolicies:` instances

**Files:**
- Modify: `gateway/gateway-controller/pkg/utils/llm_transformer.go`
- Test: `gateway/gateway-controller/pkg/utils/llm_provider_transformer_test.go` (or the closest
  existing failover-attachment test file — check
  `ls gateway/gateway-controller/pkg/utils/*failover*test*` first)

**Interfaces:**
- Consumes: `parseModelFailoverParams`/`buildRouteFailoverFromPolicy` (Task 7, same package family —
  confirm whether `pkg/transform` and `pkg/utils` need a shared exported function or whether this
  logic should move to `pkg/utils` directly; resolve by checking which package `applyFailoverToRoutes`
  is actually called from relative to where `llm_transformer.go`'s attachment loops run, and keep
  Task 7's functions in whichever package avoids an import cycle — likely `pkg/utils` calling into
  `pkg/transform`, or vice versa; check current cross-package call direction with
  `grep -rn "pkg/transform\"" gateway/gateway-controller/pkg/utils/llm_transformer.go` and
  `grep -rn "pkg/utils\"" gateway/gateway-controller/pkg/transform/llm.go` before deciding).
- Consumes: `selectedProviderExecutionCondition(providerName string, includeDefault bool) string`
  (existing, `llm_transformer.go:1109`, unchanged).
- Produces: the `model-failover` policy attached (with expanded params from Task 7) under both
  `operationPolicies:` (unconditioned) and `upstreamPolicies:` (unconditioned, same expanded params),
  attached via a code block that runs *before* new provider-scoped `upstreamPolicies:` attachment loops
  for `oauth2-generator`/`openai-to-anthropic-transformer`, each of the latter carrying an
  `executionCondition` from `selectedProviderExecutionCondition`.

- [ ] **Step 1: Read the exact current attachment loop sequence to confirm insertion points**

```bash
sed -n '260,460p' gateway/gateway-controller/pkg/utils/llm_transformer.go
```

Confirm the three sequential loops this session's research identified: `transformerPolicies` (built
~274-327, appended ~431-437), `upstreamAuthPolicies` (appended ~438-444), `failoverUpstreamAuthPolicies`
(built ~339-354, appended ~445-451). Re-derive exact current line numbers — this file may have shifted
slightly from earlier edits this session.

- [ ] **Step 2: Write the failing test asserting attachment shape and order**

```go
func TestTransform_ModelFailoverPolicy_AttachedDownstreamAndUpstreamWithProviderScopedInstances(t *testing.T) {
	// Build a minimal LlmProxy fixture carrying a model-failover
	// operationPolicies: entry (targets: gpt-4o -> claude via
	// anthropic-upstream) plus an additionalProviders entry named
	// "anthropic-upstream" with an oauth2-generator credential — mirror the
	// existing fixture-construction pattern already used by this file's
	// other TestTransform_* tests (e.g. TestTransform_UpstreamPolicies_FlaggedUpstreamOnly
	// from this session's earlier work — copy its fixture-building style).

	// ... build proxy fixture ...

	rdc, err := transformer.Transform(proxy, providerConfigs)
	require.NoError(t, err)

	op := findOperation(t, rdc, "/chat/completions", "POST") // reuse whatever existing test helper this file already has for locating a synced operation's policy chain

	var sawDownstreamModelFailover, sawUpstreamModelFailover bool
	var sawConditionedOauth2Upstream bool
	for _, pol := range op.OperationPolicies {
		if pol.Name == "model-failover" {
			if pol.Upstream != nil && *pol.Upstream {
				sawUpstreamModelFailover = true
			} else {
				sawDownstreamModelFailover = true
			}
		}
		if pol.Name == "oauth2-generator" && pol.Upstream != nil && *pol.Upstream {
			require.NotNil(t, pol.ExecutionCondition)
			assert.Contains(t, *pol.ExecutionCondition, "anthropic-upstream")
			sawConditionedOauth2Upstream = true
		}
	}

	assert.True(t, sawDownstreamModelFailover, "model-failover must be attached downstream")
	assert.True(t, sawUpstreamModelFailover, "model-failover must also be attached via upstreamPolicies:")
	assert.True(t, sawConditionedOauth2Upstream, "oauth2-generator must get a conditioned upstreamPolicies: attachment per referenced provider")

	// Ordering: model-failover's upstream instance must appear before the
	// conditioned oauth2-generator upstream instance in the emitted slice.
	modelFailoverIdx, oauth2Idx := -1, -1
	for i, pol := range op.OperationPolicies {
		if pol.Name == "model-failover" && pol.Upstream != nil && *pol.Upstream {
			modelFailoverIdx = i
		}
		if pol.Name == "oauth2-generator" && pol.Upstream != nil && *pol.Upstream {
			oauth2Idx = i
		}
	}
	require.NotEqual(t, -1, modelFailoverIdx)
	require.NotEqual(t, -1, oauth2Idx)
	assert.Less(t, modelFailoverIdx, oauth2Idx, "model-failover must be ordered before the policies that read its seeded metadata")
}
```

**Implementer note:** this test's fixture-construction details (exact `LlmProxy`/`api.Policy` literal
shapes, the `findOperation` helper name) depend on this file's existing test conventions — read
`llm_provider_transformer_test.go`'s existing tests fully before writing this one, and copy its
established fixture-building and assertion-helper patterns exactly rather than inventing new ones.

- [ ] **Step 3: Run to confirm failure**

Run: `cd gateway/gateway-controller && go test ./pkg/utils/... -run TestTransform_ModelFailoverPolicy -v`
Expected: FAIL.

- [ ] **Step 4: Implement the attachment logic**

In the same function that already builds `transformerPolicies`/`upstreamAuthPolicies`/
`failoverUpstreamAuthPolicies` (per Step 1's reading), add, *before* the existing three append loops:

```go
	if mfPolicy, ok := findModelFailoverPolicy(opLevelPoliciesSource); ok { // adjust variable name to whatever Step 1 shows opLevelPolicies is actually sourced from at this point in the function
		expandedParams, routeFailover, err := buildRouteFailoverFromPolicy(mfPolicy, routeKey, availableProviders, primaryProviderID) // exact args per Task 7's real signature
		if err != nil {
			return nil, fmt.Errorf("model-failover: %w", err)
		}
		rdc.Routes[routeKey].Upstream.Failover = routeFailover // adjust to however applyFailoverToRoutes currently assigns this — reuse that exact assignment path from Task 7/Step 1's reading instead of guessing a new one

		upstreamFlag := true
		expandedParamsMap := toParamsMap(expandedParams) // marshal the expanded modelFailoverParams back into map[string]interface{} via encoding/json, matching this file's existing pattern for building api.Policy.Params (check an existing example, e.g. proxyUpstreamAuthPolicy's own params construction, for the idiom already used here)
		op.OperationPolicies = append(op.OperationPolicies, api.Policy{
			Name: modelFailoverPolicyName, Version: mfPolicy.Version, Params: &expandedParamsMap,
		})
		op.OperationPolicies = append(op.OperationPolicies, api.Policy{
			Name: modelFailoverPolicyName, Version: mfPolicy.Version, Params: &expandedParamsMap, Upstream: &upstreamFlag,
		})

		for _, providerID := range failoverReferencedProviders(routeFailoverToLLMFailoverConfigShapeIfStillNeeded, primaryProviderID) { // reuse the EXISTING failoverReferencedProviders helper (llm_transformer.go:1042) if its input shape still fits after Task 7's changes — otherwise adapt it to take modelFailoverParams directly; do not duplicate its logic
			for _, credPolicy := range []string{"oauth2-generator" /* extend to other provider-scoped credential/transform policy names actually attached for this provider, mirroring however transformerPolicies/upstreamAuthPolicies already determine which policy applies per provider */} {
				cond := selectedProviderExecutionCondition(providerID, false)
				op.OperationPolicies = append(op.OperationPolicies, api.Policy{
					Name: credPolicy, Version: /* resolve the version the same way transformerPolicies/upstreamAuthPolicies already do for this provider */ "",
					Upstream: &upstreamFlag, ExecutionCondition: &cond,
				})
			}
		}
	}
```

**Implementer note:** this step is intentionally written at a higher level of pseudocode-with-real-API-calls
than earlier tasks, because it depends on exact local variable names/control flow this plan's author
could not fully pin down without reading the entire ~1300-line `llm_transformer.go` function body
verbatim. Before writing the final version: (1) re-read the full `transformProxy` function (or
equivalent) end to end, (2) confirm exactly how `transformerPolicies`/`upstreamAuthPolicies` currently
determine "which policy name + version applies for provider X" (there is existing logic for this — do
not invent a second mechanism, extend/reuse it for the new provider-scoped `upstreamPolicies:` loop),
(3) confirm the exact `api.Policy.Upstream`/`ExecutionCondition` field types (`*bool`/`*string`) against
`generated.go`. If the existing `transformerPolicies`/`upstreamAuthPolicies` construction can be
directly reused (calling the same per-provider policy-resolution helpers they already call, just also
setting `Upstream: &upstreamFlag` and `ExecutionCondition: &cond` on the result), prefer that over new
code — this task's job is threading two new fields onto attachments whose *content* logic already
exists, not reimplementing that content logic.

- [ ] **Step 5: Run the new test**

Run: `cd gateway/gateway-controller && go test ./pkg/utils/... -run TestTransform_ModelFailoverPolicy -v`
Expected: PASS. Iterate on Step 4 until green — this task carries the most implementation risk in the
plan (see the note above); budget real investigation time here rather than forcing the test green with
an incorrect shortcut.

- [ ] **Step 6: Run the full utils package test suite**

Run: `cd gateway/gateway-controller && go build ./... && go test ./pkg/utils/...`
Expected: all `ok`, no regressions in pre-existing `TestTransform_*` tests.

- [ ] **Step 7: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform/.claude/worktrees/llm-model-failover
git add gateway/gateway-controller/pkg/utils/
git commit -m "$(cat <<'EOF'
feat(llm-failover): attach model-failover downstream+upstream, provider-scoped credential/transform via executionCondition

model-failover is attached under both operationPolicies: and
upstreamPolicies: with its controller-expanded params (aggregate cluster
names injected). Provider-scoped credential/transform policies
(oauth2-generator, and future ones) get a conditioned upstreamPolicies:
attachment per provider referenced in the failover chain, gated by the
same selectedProviderExecutionCondition mechanism already used downstream
— zero code changes to those policies. model-failover's upstream
attachment is ordered before the conditioned ones so its seeded metadata
exists when their conditions are checked.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: gateway-controller — remove the `resilience.failover` schema and its old trigger path

**Files:**
- Modify: `gateway/gateway-controller/api/management-openapi.yaml`
- Modify: `gateway/gateway-controller/pkg/api/management/generated.go` (regenerated, not hand-edited)
- Modify: `gateway/gateway-controller/pkg/transform/llm.go`
- Modify: `gateway/gateway-controller/pkg/utils/llm_transformer.go`
- Test: whatever existing tests reference `Resilience.Failover`/`failoverUpstreamAuthPolicy`/
  `LLMResilience`/`LLMFailoverConfig` (locate via grep in Step 1)

**Interfaces:**
- Consumes: Task 7/8's new policy-driven path (must be green and merged first — this task only removes
  the now-dead old path, never leaves both active simultaneously per this session's own review
  discipline).
- Produces: no schema-level `resilience.failover` field; `applyFailoverToRoutes`'s old
  schema-driven call site removed; `failoverUpstreamAuthPolicy`/`llm-upstream-provider-auth`-building
  code deleted (superseded by Task 8's conditioned `upstreamPolicies:` loop).

- [ ] **Step 1: Find every reference to the schema field and the old trigger/attachment code**

```bash
grep -rn "Resilience.Failover\|LLMFailoverConfig\|LLMFailoverTarget\|failoverUpstreamAuthPolicy\b" gateway/gateway-controller --include="*.go" --include="*.yaml" | grep -v _test.go
```

Also find every test referencing these symbols:

```bash
grep -rln "Resilience.Failover\|LLMFailoverConfig\|failoverUpstreamAuthPolicy" gateway/gateway-controller --include="*_test.go"
```

- [ ] **Step 2: Remove `LLMFailoverConfig`/`LLMFailoverTarget`/`LLMFailoverTargetEntry` and the
  `resilience.failover` field from the OpenAPI spec**

Edit `management-openapi.yaml`: remove the `failover:` property from the `LLMResilience` schema object
and delete the now-unreferenced `LLMFailoverConfig`/`LLMFailoverTarget`/`LLMFailoverTargetEntry`
component schemas.

- [ ] **Step 3: Regenerate the Go types**

Run: `cd gateway/gateway-controller && make generate-server-code`
Expected: `generated.go` updates with `LLMFailoverConfig` etc. removed, `LLMResilience` no longer
carrying `Failover`.

- [ ] **Step 4: Remove `applyFailoverToRoutes`'s old call site and the now-dead schema-driven branch**

In `pkg/transform/llm.go`, remove the `if proxy.Spec.Resilience != nil { applyFailoverToRoutes(...) }`
call site (found in Step 1) — Task 7's policy-driven path is the only remaining trigger. If
`applyFailoverToRoutes`/`resolveFailoverEntry` themselves are now called *only* from Task 7's new
`buildRouteFailoverFromPolicy`, leave them in place unchanged (they're still needed, just triggered
differently — Task 7 already established this). If Task 7 instead inlined their logic directly, remove
them here as dead code — confirm which happened by checking Task 7's actual diff before deciding.

- [ ] **Step 5: Remove `failoverUpstreamAuthPolicy` and its call site in `llm_transformer.go`**

Delete the `failoverUpstreamAuthPolicy` function and the `failoverUpstreamAuthPolicies`-building block
(the third of the three loops identified in Task 8/Step 1) — Task 8's conditioned `upstreamPolicies:`
loop fully replaces it. If `failoverReferencedProviders` is still called from Task 8's new loop, keep
it; otherwise remove it too (check Task 8's actual final code first).

- [ ] **Step 6: Delete or update every test referencing the removed symbols**

For each file found in Step 1's second `grep`, either delete the specific test function (if it tested
purely the removed schema-driven path with no remaining relevance) or update it to use the new
policy-attachment path (if it tested a behavior — like "aggregate cluster gets created" — that's still
valid and should just change its trigger fixture from `Resilience.Failover` to a `model-failover`
policy attachment).

- [ ] **Step 7: Build and run the full gateway-controller test suite**

Run: `cd gateway/gateway-controller && go build ./... && go test ./...`
Expected: all `ok`, zero references to removed symbols remain.

- [ ] **Step 8: Confirm no stray references remain anywhere in the module**

Run: `grep -rn "LLMFailoverConfig\|failoverUpstreamAuthPolicy" gateway/gateway-controller --include="*.go"`
Expected: no output.

- [ ] **Step 9: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform/.claude/worktrees/llm-model-failover
git add gateway/gateway-controller/
git commit -m "$(cat <<'EOF'
refactor(llm-failover): remove the resilience.failover schema, superseded by the model-failover policy

resilience.failover has never shipped, so no migration path is needed —
removed outright along with the schema-driven trigger path and
failoverUpstreamAuthPolicy (superseded by Task 8's conditioned
upstreamPolicies: attachment loop). The existing xDS generation
(aggregate clusters, retry_policy, etc.) is untouched — it's driven
entirely by *models.RouteFailover regardless of source, and Task 7's
policy-driven path already produces the same shape.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: gateway-runtime kernel — remove the now-superseded resolveBackend/suspension/metadata-sync code

**Files:**
- Modify: `gateway/gateway-runtime/policy-engine/internal/kernel/upstream_extproc.go`
- Modify: `gateway/gateway-runtime/policy-engine/internal/kernel/translator.go`
- Delete: `gateway/gateway-runtime/policy-engine/internal/kernel/failover_suspension.go`
- Modify: `gateway/gateway-runtime/policy-engine/internal/xdsclient/handler.go`
- Modify (if present): any `RouteConfig.Metadata.FailoverTargets`-related type definitions
- Test: `gateway/gateway-runtime/policy-engine/internal/kernel/upstream_extproc_test.go`,
  `internal/kernel/failover_suspension_test.go` (if it exists), `internal/xdsclient/handler_test.go`

**Interfaces:**
- Consumes: Task 5's `model-failover` policy (must already own suspension recording, chain resolution,
  and the `ResolvedFailoverProviderHeader` emission — this task only removes code once that coverage
  exists, matching the design's §10 explicitly).
- Produces: no kernel-owned `FailoverTargets` matching, no kernel-owned suspension tracker, no
  `RouteConfig.Metadata.FailoverTargets`/`FailoverSuspendDurationSeconds` xDS sync.

- [ ] **Step 1: Confirm Task 5-9 are all merged and green before starting removal — re-run the full policy-engine and gateway-controller test suites**

```bash
cd gateway/gateway-runtime/policy-engine && go build ./... && go test ./...
cd ../../gateway-controller && go build ./... && go test ./...
```

Expected: all green. Do not proceed with removal until both are clean — removing this code before its
replacement is proven working would leave failover broken with no rollback path other than git.

- [ ] **Step 2: Remove `resolveBackend`'s `FailoverTargets` matching branch**

In `upstream_extproc.go`, delete the `for _, target := range rc.Metadata.FailoverTargets { ... }` block
inside `resolveBackend` (confirm exact current line numbers first — this session's earlier research
found it starting around line 692, but re-verify since Tasks 1-2 already edited this file). Leave the
remaining fallback chain (route-scoped `DefaultUpstream` match, then global cluster index, then
`:authority`) intact — those are unrelated to failover and still needed for every other route.

- [ ] **Step 3: Remove `suspendIfFailoverAttemptFailed` and its call site**

Delete the function and its call in `Process()`'s `ResponseHeaders` case — `model-failover`'s own
`OnResponseHeaders` (Task 5) now owns this.

- [ ] **Step 4: Remove the kernel-owned suspension tracker**

Delete `internal/kernel/failover_suspension.go` and its test file if one exists
(`ls internal/kernel/failover_suspension_test.go`). Remove the `suspension` field from `Kernel` (or
whatever struct owns it — confirm via `grep -rn "suspension " internal/kernel/*.go`) and every
remaining reference to it.

- [ ] **Step 5: Remove the downstream suspension-check/target-selection logic in `translator.go`**

Read `internal/kernel/translator.go` around lines 259-267 (per this session's research) in full first
— this is the downstream `ExternalProcessorServer`'s own pre-emption/target-selection logic from the
superseded design's old §5, now fully replaced by `model-failover`'s downstream `OnRequestBody` (Task
4). Remove the function(s) implementing it and their call site(s) in the downstream request-processing
flow.

- [ ] **Step 6: Remove the `RouteConfig.Metadata.FailoverTargets`/`FailoverSuspendDurationSeconds` sync**

In `internal/xdsclient/handler.go`, remove `parseFailoverTargets` and wherever it's called (around
line 406-408 per this session's research) — the `RouteConfig.Metadata` fields it populates are no
longer read by anything after Steps 2-5. Also remove the corresponding emission in
`gateway-controller/pkg/policyxds/snapshot.go` (the `data["failover_targets"]`/
`data["failover_suspend_duration"]` writes, around line 466/471 per this session's research) — this is
a gateway-controller change; include it in this task since it's the paired write-side of the same dead
sync, even though it's a different module (or split into a separate small commit within this same task
if that's cleaner — either is fine, just keep both removed together so no half-dead sync code lingers).

- [ ] **Step 7: Remove or update every test referencing the removed symbols**

```bash
grep -rln "resolveBackend.*FailoverTargets\|suspendIfFailoverAttemptFailed\|suspensionTracker\|FailoverSuspendDurationSeconds\|parseFailoverTargets" gateway/gateway-runtime/policy-engine --include="*_test.go"
```

For each hit, delete the specific test if it only exercised removed behavior, or update it if it tested
something still valid under a different trigger.

- [ ] **Step 8: Build and run the full policy-engine test suite**

Run: `cd gateway/gateway-runtime/policy-engine && go build ./... && go test ./...`
Expected: all `ok`.

- [ ] **Step 9: Build and run the full gateway-controller test suite (for the paired snapshot.go removal)**

Run: `cd gateway/gateway-controller && go build ./... && go test ./...`
Expected: all `ok`.

- [ ] **Step 10: Confirm no stray references remain**

Run: `grep -rn "FailoverTargets\|suspendIfFailoverAttemptFailed\|failoverSuspension" gateway/gateway-runtime/policy-engine --include="*.go"`
Expected: no output (or only the new, unrelated `model-failover` policy's own `suspendedTargets` field
if grep is broad enough to catch its name similarity — confirm any hits are in
`gateway/dev-policies/model-failover/` only, which is expected and correct).

- [ ] **Step 11: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform/.claude/worktrees/llm-model-failover
git add gateway/gateway-runtime/policy-engine/ gateway/gateway-controller/pkg/policyxds/
git commit -m "$(cat <<'EOF'
refactor(llm-failover): remove kernel-owned failover resolution, suspension, and metadata sync

model-failover (the policy, Tasks 4-8) now owns all of this: downstream
target selection + suspension pre-emption, per-attempt chain resolution,
and suspension recording. Removes resolveBackend's FailoverTargets
matching branch, suspendIfFailoverAttemptFailed, the kernel's dedicated
suspension tracker, the downstream target-selection logic in
translator.go, and the RouteConfig.Metadata.FailoverTargets xDS sync on
both the emitting (gateway-controller) and consuming (gateway-runtime)
sides — nothing reads any of it anymore.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: End-to-end verification doc update

**Files:**
- Modify: `gateway/dev-policies/FAILOVER_TESTING.md`
- Modify: `gateway/dev-policies/postman/llm-failover-e2e.postman_collection.json` (setup requests only)

**Interfaces:**
- Consumes: everything from Tasks 1-10.
- Produces: an accurate, currently-runnable e2e verification doc reflecting the new config surface.

- [ ] **Step 1: Update the Postman collection's Setup folder**

The `LlmProxy`/`LlmProvider` registration requests in the collection's **Setup** folder currently embed
a `resilience: { failover: {...} }` block (per the design this plan implements, that field no longer
exists in the schema). Open the collection JSON, find the setup requests' bodies, and replace the
`resilience.failover` block with an `operationPolicies:` entry attaching `model-failover` with
equivalent `targets`/`suspendDuration` params, matching this plan's `ModelFailoverParams` JSON shape
exactly (§ shared contract, top of this plan).

- [ ] **Step 2: Update `FAILOVER_TESTING.md`'s caveat section**

The caveat this session added earlier ("folders 2 and 8... not expected to pass against released
modules alone") is now resolved by Tasks 3+8's `executionCondition` mechanism — remove that caveat,
since it no longer applies once this plan is fully implemented.

- [ ] **Step 3: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform/.claude/worktrees/llm-model-failover
git add gateway/dev-policies/FAILOVER_TESTING.md gateway/dev-policies/postman/
git commit -m "$(cat <<'EOF'
docs(llm-failover): update e2e verification doc/collection for the model-failover policy config surface

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Self-review notes (per writing-plans skill)

**Spec coverage:** §3 (config surface) → Tasks 4, 7. §4 (downstream phase) → Task 4. §5 (controller xDS
generation, reused unchanged) → Task 7 (source only), confirmed no changes needed to
`failover_cluster.go`/`translator.go` themselves. §6 (upstream-attempt phase, new SDK field) → Tasks 1,
2, 5. §7 (suspension in the policy) → Tasks 4, 5. §8 (zero policy changes, executionCondition gating) →
Task 3 (executor) + Task 8 (controller attachment) — confirmed no task in this plan touches
`oauth2-generator`/`openai-to-anthropic-transformer` source, per the Global Constraint. §9 (executor CEL
change) → Task 3. §10 (kernel removals) → Task 10.

**Known implementation-risk areas, flagged explicitly rather than papered over:** Task 8 (the
attachment-ordering/per-provider-policy-resolution wiring in `llm_transformer.go`) carries the most
risk in this plan — the exact current control flow of that ~1300-line function wasn't fully read
verbatim while writing this plan, only its function/line-number shape via research. Task 8's steps say
so explicitly and instruct the implementer to re-read the real function before finalizing, rather than
presenting invented pseudocode as if it were exact. This is a legitimate task-scoping decision (the
function is large enough that reading it in full belongs to the task that touches it, not to plan
authoring), not a placeholder — every other task in this plan has fully concrete code.

**Type consistency:** `ModelFailoverParams`/`FailoverTargetEntry`/`FailoverTarget` (Go, policy side,
Task 4) and `modelFailoverParams`/`modelFailoverTargetEntry`/`modelFailoverTarget` (Go, controller side,
Task 7) are deliberately two separate, structurally-identical type definitions (different Go modules,
can't share one Go type) — kept in lockstep via the shared JSON-shape contract documented once at the
top of this plan and referenced by name from both tasks, rather than restated ad hoc.

## Execution options

Plan complete and saved to `docs/superpowers/plans/2026-09-21-model-failover-policy.md`. Two execution
options:

1. **Subagent-Driven (recommended)** — dispatch a fresh subagent per task, review between tasks, fast
   iteration. Given Task 8's flagged risk, this also means a dedicated review pass catches any drift
   between what the plan assumed and what `llm_transformer.go` actually looks like, before it compounds
   into later tasks.
2. **Inline Execution** — execute tasks in this session using `executing-plans`, batch execution with
   checkpoints.

Which approach?

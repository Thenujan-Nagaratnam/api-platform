# Decoupled Multi-Target Retry Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace model-failover's hardcoded `p.Name == "model-failover"` special-casing in `gateway-controller` with a generic SDK contract any policy can implement to get Envoy-native multi-target retry, and let `oauth2-generator` (and any future auth-refresh-style policy) get same-request retry-with-mutation independently, without needing model-failover or `resilience.retry` present.

**Architecture:** `gateway-controller` never instantiates policy Go code (confirmed: zero dependency on any policy implementation, only ever sees `{Name, Params}`) — so discovery of "which policy needs multi-target retry" or "which policy needs retry conditions" must be declarative, not a Go interface a policy implements. Two new optional `policy-definition.yaml` fields — `x-wso2-retry-source` (declares one or more independently-selectable failover chains; at most one per route, a real Envoy limit) and `x-wso2-retry-trigger` (declares retry conditions without owning destination selection; any number compose by union) — plus one generic parser in `gateway-controller` for each, driven by the metadata rather than a policy method call. `translator.go`'s policy-chain walk looks up each policy's cached definition metadata, builds the aggregate cluster(s) + `RouteAction.RetryPolicy` from whichever combination is present. Separately, renames `OnUpstreamAttemptRequestHeaders`/`UpstreamAttemptHeaderModifications` (misleadingly headers-only names for a hook that already handles body too) and adds an optional `UpstreamAttemptResponseObserver` Go interface for per-attempt failure-cause visibility — this one IS a real Go interface, type-asserted inside `gateway-runtime`'s kernel, which (unlike `gateway-controller`) does instantiate real policy objects.

**Tech Stack:** Go, Envoy xDS (go-control-plane), gRPC ext_proc protocol, the existing `sdk/core/policy/v1alpha2` policy SDK, YAML policy-definition metadata.

**Spec:** `docs/superpowers/specs/2026-08-14-decoupled-retry-source-design.md` (see "Design Revision 2" section — supersedes the interface-based mechanism described earlier in that doc for the `gateway-controller`-side pieces specifically).

## Global Constraints

- Zero YAML/config-surface change for existing model-failover or oauth2-generator **deployments** — every param field this plan reads is one that already exists in each policy's params today. (`policy-definition.yaml` itself DOES gain new optional top-level metadata fields — that's policy-package-level, not per-deployment config, and has no effect on any already-deployed instance's own YAML.)
- `gateway-controller` must never import or instantiate any policy implementation package (`gateway/dev-policies/*`) — every mechanism in this plan that runs inside `gateway-controller` works from `{Name, Params}` plus cached `policy-definition.yaml` metadata only.
- At most one policy per route may declare `x-wso2-retry-source` (Envoy has one `RetryPolicy`/one `RetryPriority` slot per route) — enforced at validation time with a generic error, never inferred silently.
- Any number of policies per route may declare `x-wso2-retry-trigger` — compose by union, never a conflict, never require destination knowledge.
- Every existing model-failover IT feature (`gateway/it/features/model-*.feature`) must pass unmodified after this plan — proves zero behavior change for the shipped policy.
- No `p.Name == "<literal>"` string check may remain anywhere in `translator.go`'s retry-related code after this plan — discovery is always via cached `policy-definition.yaml` metadata (`gateway-controller` side) or type assertion (`gateway-runtime` side, where real policy objects exist).
- `gateway/dev-policies/model-failover` and `gateway/dev-policies/oauth2-generator` are NOT listed in the repo root's `go.work` `use` block (verified) — any `go build`/`go test` run inside either directory MUST be prefixed with `GOWORK=off`, or Go's workspace auto-detection (which searches upward from CWD regardless of the `use` list) fails the command with `pattern ./...: directory prefix . does not contain modules listed in go.work`. `sdk/core`, `gateway/gateway-controller`, and `gateway/gateway-runtime/policy-engine` ARE in `go.work` — no flag needed there.

---

## Task 1: SDK — retry-source data types + canonical naming formula

**Files:**
- Create: `sdk/core/policy/v1alpha2/retry_source.go`
- Test: `sdk/core/policy/v1alpha2/retry_source_test.go`

**Interfaces:**
- Produces: `RetryTarget{UpstreamDefinitionName string}`, `RetryGroup{Key string; OrderedTargets []RetryTarget}`, `RetrySourceDeclaration{Groups []RetryGroup; RetriableStatusCodes []int; PerAttemptTimeout *time.Duration}`, and `RetrySourceUpstreamName(routeKey, groupKey string) string` — the canonical local-name formula both `translator.go`'s generic parser (Task 6) and any retry-source policy's own runtime code (Task 11) call, replacing today's manually-duplicated-across-two-repos `modelFailoverGroupUpstreamName`.

**No `RetrySourcePolicy` Go interface in this task** — `gateway-controller` (the only place that would need to discover it) never instantiates policy Go code (confirmed against `go.mod`: zero dependency on any policy implementation package), so nothing could ever call an interface method on a policy. Discovery is declarative instead — see Task 5/6 (`x-wso2-retry-source` in `policy-definition.yaml` + a generic params parser). These types are plain data, safe for `gateway-controller` to import (no policy implementation code involved) — used as `gateway-controller`'s own parser's output shape.

- [ ] **Step 1: Write the failing test**

```go
// sdk/core/policy/v1alpha2/retry_source_test.go
package policyv1alpha2

import "testing"

func TestRetrySourceUpstreamName_SingleGroup(t *testing.T) {
	got := RetrySourceUpstreamName("POST|/chat/completions|main", "gpt-4o")
	want := "__retry_source_target__POST_/chat/completions_main__gpt-4o"
	if got != want {
		t.Errorf("RetrySourceUpstreamName() = %q, want %q", got, want)
	}
}

func TestRetrySourceUpstreamName_EmptyRouteKey(t *testing.T) {
	got := RetrySourceUpstreamName("", "gpt-4o")
	want := "__retry_source_target__gpt-4o"
	if got != want {
		t.Errorf("RetrySourceUpstreamName() = %q, want %q", got, want)
	}
}

func TestRetrySourceUpstreamName_PipeInRouteKeyIsSanitized(t *testing.T) {
	got := RetrySourceUpstreamName("GET|/a|b", "x")
	want := "__retry_source_target__GET_/a_b__x"
	if got != want {
		t.Errorf("RetrySourceUpstreamName() = %q, want %q", got, want)
	}
}

func TestRetrySourceDeclaration_IsUpstreamAttemptActionFree(t *testing.T) {
	// Compile-time shape check: RetrySourceDeclaration must not itself be an
	// action type — it's a registration-time declaration, never returned
	// from a runtime hook.
	var _ = RetrySourceDeclaration{
		Groups: []RetryGroup{
			{Key: "gpt-4o", OrderedTargets: []RetryTarget{{UpstreamDefinitionName: "primary"}}},
		},
		RetriableStatusCodes: []int{429, 500},
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd sdk/core && go test ./policy/v1alpha2/... -run TestRetrySource -v`
Expected: FAIL — `RetrySourceUpstreamName`/`RetrySourceDeclaration`/`RetryGroup`/`RetryTarget` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// sdk/core/policy/v1alpha2/retry_source.go
package policyv1alpha2

import (
	"strings"
	"time"
)

// RetryTarget is one ordered destination within a single RetryGroup.
// UpstreamDefinitionName must resolve to a registered upstreamDefinition on
// the resource the declaring policy is attached to — the same primitive
// plain upstreamDefinitions already provide, not a new one. An empty
// UpstreamDefinitionName means "this API's own main upstream", matching the
// existing convention plain upstreamDefinition-based routing already uses.
type RetryTarget struct {
	UpstreamDefinitionName string
}

// RetryGroup is one independently-selectable failover chain within a route
// (e.g. model-failover: one per client-selectable model, chosen at runtime
// by the declaring policy's own request-body matching logic). Key is an
// opaque, policy-chosen discriminator used only for deterministic cluster
// naming via RetrySourceUpstreamName below — translator.go never interprets
// it. Key must be stable across deploys and unique within one
// RetrySourceDeclaration's Groups.
type RetryGroup struct {
	Key string

	// OrderedTargets: index 0 is tried first, index 1 on the first failure,
	// and so on. Must have at least 2 entries for gateway-controller to
	// build an aggregate cluster for this group — a single-entry group is
	// legal (means "route this target, no failover") but produces no
	// aggregate cluster, matching today's zero-fallback model-failover
	// target-group behavior.
	OrderedTargets []RetryTarget
}

// RetrySourceDeclaration is the generic contract gateway-controller builds
// one aggregate cluster PER Group from, regardless of which policy declared
// it. Which group applies to a given request is entirely the declaring
// policy's own runtime decision (e.g. its own OnRequestBody setting
// UpstreamName via RetrySourceUpstreamName) — gateway-controller never
// needs to know why there are multiple groups, or how one gets selected,
// only that there are some.
type RetrySourceDeclaration struct {
	// Groups must be non-empty.
	Groups []RetryGroup

	// RetriableStatusCodes triggers moving to the next target within
	// whichever group matched. Must be non-empty. Route-wide, not
	// per-group — Envoy has one RetryPolicy per route, shared by every
	// group's resolved cluster regardless of which one a given request
	// actually used.
	RetriableStatusCodes []int

	// PerAttemptTimeout bounds a single attempt; nil uses the route's
	// existing default.
	PerAttemptTimeout *time.Duration
}

// retrySourceTargetPrefix marks a logical upstream name as belonging to
// this mechanism, reserved-looking so it can't collide with a real,
// operator-declared UpstreamDefinition.Name that happens to equal a group's
// own Key (e.g. an upstreamDefinition literally named "gpt-4o").
const retrySourceTargetPrefix = "__retry_source_target__"

// RetrySourceUpstreamName is the canonical formula for a RetryGroup's
// logical upstream name — the single source of truth both
// gateway-controller (building the aggregate cluster, see
// RetrySourceAggregateClusterKey) and a retry-source-capable policy's own
// runtime code (setting UpstreamName at runtime to redirect into that same
// cluster) call identically. Living in the SDK — which every policy
// already imports, including ones in separate repos from
// gateway-controller — replaces what was previously a formula manually
// duplicated across repos with a comment asking developers to keep both
// copies in sync.
//
// routeKey is "METHOD|PATH|VHOST[|DISCRIMINATOR]" and may contain "|",
// which is not valid in an Envoy cluster name component; it's replaced
// with "_" here. An empty routeKey (a policy that doesn't need per-route
// disambiguation) omits the routeKey segment entirely rather than leaving
// a stray separator.
func RetrySourceUpstreamName(routeKey, groupKey string) string {
	if routeKey == "" {
		return retrySourceTargetPrefix + groupKey
	}
	safeRouteKey := strings.ReplaceAll(routeKey, "|", "_")
	return retrySourceTargetPrefix + safeRouteKey + "__" + groupKey
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd sdk/core && go test ./policy/v1alpha2/... -run TestRetrySource -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/core/policy/v1alpha2/retry_source.go sdk/core/policy/v1alpha2/retry_source_test.go
git commit -m "feat(sdk): add RetrySourcePolicy contract for generic multi-target retry"
```

---

## Task 2: SDK — `RetryTriggerDeclaration` data type

**Files:**
- Create: `sdk/core/policy/v1alpha2/retry_trigger.go`
- Test: `sdk/core/policy/v1alpha2/retry_trigger_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 (deliberately independent — see spec's Design Revision).
- Produces: `RetryTriggerDeclaration{RetriableStatusCodes []int; MinAttempts int}` — a plain data type only. **No `RetryTriggerPolicy` Go interface**, for the same reason as Task 1: `gateway-controller` cannot call a method on a policy it never instantiates. Discovery is declarative (`x-wso2-retry-trigger` in `policy-definition.yaml`, Task 5/6).

- [ ] **Step 1: Write the failing test**

```go
// sdk/core/policy/v1alpha2/retry_trigger_test.go
package policyv1alpha2

import "testing"

func TestRetryTriggerDeclaration_Shape(t *testing.T) {
	d := RetryTriggerDeclaration{
		RetriableStatusCodes: []int{401},
		MinAttempts:          2,
	}
	if len(d.RetriableStatusCodes) != 1 || d.RetriableStatusCodes[0] != 401 {
		t.Errorf("unexpected RetriableStatusCodes: %v", d.RetriableStatusCodes)
	}
	if d.MinAttempts != 2 {
		t.Errorf("MinAttempts = %d, want 2", d.MinAttempts)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd sdk/core && go test ./policy/v1alpha2/... -run TestRetryTriggerDeclaration -v`
Expected: FAIL — `RetryTriggerDeclaration` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// sdk/core/policy/v1alpha2/retry_trigger.go
package policyv1alpha2

// RetryTriggerDeclaration contributes retry conditions without claiming
// ownership of retry-target selection. Any number of policies on a route
// may declare one — they compose by union at gateway-controller's
// discovery step (Task 6), never conflict, unlike RetrySourceDeclaration.
// A plain data type — gateway-controller's own generic parser (Task 5)
// produces this by reading a policy's params according to its
// policy-definition.yaml's x-wso2-retry-trigger metadata; no policy Go
// code is ever called to produce one.
type RetryTriggerDeclaration struct {
	// RetriableStatusCodes is unioned with every other declared
	// RetryTriggerDeclaration's (and, if present, the route's
	// RetrySourceDeclaration's) own status codes into one
	// RouteAction.RetryPolicy.
	RetriableStatusCodes []int

	// MinAttempts is the minimum total attempts this policy needs to get
	// value from retrying (e.g. 2: one to observe the failure, one to
	// retry with a corrected request). The route's final NumRetries is at
	// least max(every declared MinAttempts) - 1.
	MinAttempts int
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd sdk/core && go test ./policy/v1alpha2/... -run TestRetryTriggerDeclaration -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/core/policy/v1alpha2/retry_trigger.go sdk/core/policy/v1alpha2/retry_trigger_test.go
git commit -m "feat(sdk): add RetryTriggerDeclaration data type for composable retry conditions"
```

---

## Task 3: SDK — rename `OnUpstreamAttemptRequestHeaders`/`UpstreamAttemptHeaderModifications`

**Files:**
- Modify: `sdk/core/policy/v1alpha2/action.go:300-332`
- Modify: `sdk/core/policy/v1alpha2/action_test.go` (or wherever the existing action tests for this type live — grep `UpstreamAttemptHeaderModifications` under `sdk/core/policy/v1alpha2/*_test.go` first and update every hit)
- Test: same file, updated in place

**Interfaces:**
- Produces: `UpstreamAttemptRequestModifications` (renamed from `UpstreamAttemptHeaderModifications`), `UpstreamAttemptPolicy.OnUpstreamAttemptRequest` (renamed from `OnUpstreamAttemptRequestHeaders`). Fields (`HeadersToSet`, `Body`) and semantics (fail-open, last-write-wins across chain) are unchanged — this is a pure rename.
- Consumes: nothing new.

- [ ] **Step 1: Write the failing test**

```go
// Add to sdk/core/policy/v1alpha2/action_test.go
package policyv1alpha2

import "testing"

func TestUpstreamAttemptRequestModifications_ImplementsAction(t *testing.T) {
	var _ UpstreamAttemptAction = UpstreamAttemptRequestModifications{
		HeadersToSet: map[string]string{"Authorization": "Bearer x"},
		Body:         []byte(`{"model":"gpt-4o"}`),
	}
}

type stubAttemptPolicy struct{}

func (stubAttemptPolicy) OnUpstreamAttemptRequest(ctx interface{}, actx *UpstreamAttemptContext) UpstreamAttemptAction {
	return UpstreamAttemptRequestModifications{}
}
```

Note: `OnUpstreamAttemptRequest`'s real signature takes `context.Context`, not `interface{}` — this stub uses `interface{}` only to keep the added test self-contained without a new import; the real interface (Step 3) uses `context.Context`. Adjust the stub's import/signature to match exactly once Step 3 lands, before running.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd sdk/core && go test ./policy/v1alpha2/... -run TestUpstreamAttemptRequestModifications -v`
Expected: FAIL — `UpstreamAttemptRequestModifications` undefined, `OnUpstreamAttemptRequest` undefined.

- [ ] **Step 3: Rename in `action.go`**

In `sdk/core/policy/v1alpha2/action.go`, replace lines 300-332:

```go
// BEFORE (lines 300-332):
// UpstreamAttemptAction is the sealed oneof returned by
// UpstreamAttemptPolicy.OnUpstreamAttemptRequestHeaders.
type UpstreamAttemptAction interface {
	isUpstreamAttemptAction()
}

// UpstreamAttemptHeaderModifications sets the given headers on this specific
// upstream attempt. An empty/nil HeadersToSet is a valid, common no-op (e.g.
// AttemptCount == 1, nothing to refresh yet, or a fail-open path after an
// error).
type UpstreamAttemptHeaderModifications struct {
	HeadersToSet map[string]string
	Body []byte
}

func (UpstreamAttemptHeaderModifications) isUpstreamAttemptAction() {}

// UpstreamAttemptPolicy is implemented by any policy that wants to attach
// fresh, per-attempt state (e.g. a refreshed credential) to an Envoy-native
// retry. Discovery is a plain type assertion by the kernel — see Task 3.
// A policy implements this in addition to, not instead of, its normal
// RequestHeaderPolicy/ResponseHeaderPolicy interfaces.
type UpstreamAttemptPolicy interface {
	OnUpstreamAttemptRequestHeaders(ctx context.Context, actx *UpstreamAttemptContext) UpstreamAttemptAction
}
```

```go
// AFTER:
// UpstreamAttemptAction is the sealed oneof returned by
// UpstreamAttemptPolicy.OnUpstreamAttemptRequest.
type UpstreamAttemptAction interface {
	isUpstreamAttemptAction()
}

// UpstreamAttemptRequestModifications sets the given headers and/or body on
// this specific upstream attempt. An empty/nil HeadersToSet and nil Body is
// a valid, common no-op (e.g. AttemptCount == 1, nothing to refresh yet, or
// a fail-open path after an error). Named for the whole outgoing request
// this hook can mutate — not "Header" — because it is dispatched for both
// the request-headers AND request-body phase of the same attempt (see
// UpstreamAttemptContext.Body).
type UpstreamAttemptRequestModifications struct {
	HeadersToSet map[string]string

	// Body replaces this attempt's outgoing request body when non-nil. Only
	// meaningful when UpstreamAttemptContext.Body was non-nil (the kernel
	// buffers the body for this attempt) — setting it otherwise is a no-op,
	// not an error. The kernel — not the caller — sets Content-Length to
	// match the replacement.
	Body []byte
}

func (UpstreamAttemptRequestModifications) isUpstreamAttemptAction() {}

// UpstreamAttemptPolicy is implemented by any policy that wants to attach
// fresh, per-attempt state (e.g. a refreshed credential, a corrected model
// name) to an Envoy-native retry. Discovery is a plain type assertion by
// the kernel. A policy implements this in addition to, not instead of, its
// normal RequestHeaderPolicy/ResponseHeaderPolicy interfaces.
type UpstreamAttemptPolicy interface {
	OnUpstreamAttemptRequest(ctx context.Context, actx *UpstreamAttemptContext) UpstreamAttemptAction
}
```

- [ ] **Step 4: Find and update every other reference in the SDK module**

Run: `grep -rln "UpstreamAttemptHeaderModifications\|OnUpstreamAttemptRequestHeaders" sdk/core/`

For each hit outside `action.go` (expected: test files only, per this task's scope — `gateway/dev-policies/*` consumers are Tasks 10 and 11), replace `UpstreamAttemptHeaderModifications` → `UpstreamAttemptRequestModifications` and `OnUpstreamAttemptRequestHeaders` → `OnUpstreamAttemptRequest` verbatim.

- [ ] **Step 5: Run test to verify it passes**

Run: `cd sdk/core && go test ./policy/v1alpha2/... -v`
Expected: PASS, entire package — this rename must not break any other existing SDK test.

- [ ] **Step 6: Commit**

```bash
git add sdk/core/policy/v1alpha2/
git commit -m "refactor(sdk): rename UpstreamAttemptRequestHeaders/HeaderModifications to drop misleading 'Headers' (also dispatched for body)"
```

---

## Task 4: SDK — `UpstreamAttemptResponseObserver` contract

**Files:**
- Create: `sdk/core/policy/v1alpha2/upstream_attempt_response.go`
- Test: `sdk/core/policy/v1alpha2/upstream_attempt_response_test.go`

**Interfaces:**
- Consumes: `SharedContext` (already defined in `context.go:78`).
- Produces: `UpstreamAttemptResponseContext{*SharedContext; AttemptCount int; RequestID string; ResponseStatus int}`, `UpstreamAttemptResponseObserver` interface with `OnUpstreamAttemptResponse(ctx context.Context, actx *UpstreamAttemptResponseContext)` (no return value — read-only per the spec).

- [ ] **Step 1: Write the failing test**

```go
// sdk/core/policy/v1alpha2/upstream_attempt_response_test.go
package policyv1alpha2

import "context"

type recordingResponseObserver struct {
	seen []UpstreamAttemptResponseContext
}

func (r *recordingResponseObserver) OnUpstreamAttemptResponse(ctx context.Context, actx *UpstreamAttemptResponseContext) {
	r.seen = append(r.seen, *actx)
}

func TestUpstreamAttemptResponseObserver_Implementable(t *testing.T) {
	var obs UpstreamAttemptResponseObserver = &recordingResponseObserver{}
	obs.OnUpstreamAttemptResponse(context.Background(), &UpstreamAttemptResponseContext{
		AttemptCount:   1,
		RequestID:      "req-abc-123",
		ResponseStatus: 401,
	})
	rec := obs.(*recordingResponseObserver)
	if len(rec.seen) != 1 {
		t.Fatalf("seen = %d entries, want 1", len(rec.seen))
	}
	if rec.seen[0].RequestID != "req-abc-123" || rec.seen[0].ResponseStatus != 401 {
		t.Errorf("unexpected context: %+v", rec.seen[0])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd sdk/core && go test ./policy/v1alpha2/... -run TestUpstreamAttemptResponseObserver -v`
Expected: FAIL — `UpstreamAttemptResponseContext`/`UpstreamAttemptResponseObserver` undefined.

- [ ] **Step 3: Write minimal implementation**

```go
// sdk/core/policy/v1alpha2/upstream_attempt_response.go
package policyv1alpha2

import "context"

// UpstreamAttemptResponseContext mirrors UpstreamAttemptContext's per-attempt
// (not per-client-request) scope, but on the response side of that same
// attempt — it fires once per individual upstream dial attempt's response,
// before Envoy decides whether to retry.
type UpstreamAttemptResponseContext struct {
	*SharedContext

	// AttemptCount is Envoy's x-envoy-attempt-count for this specific dial,
	// starting at 1.
	AttemptCount int

	// RequestID is x-request-id — stable across every attempt of one
	// client request, assigned once at the edge, not per-attempt. The
	// correlation key an OnUpstreamAttemptResponse implementation uses to
	// hand information forward to a later attempt's
	// UpstreamAttemptPolicy.OnUpstreamAttemptRequest call (see that
	// interface's own doc comment) — deliberately not Envoy's own
	// cross-attempt dynamic metadata, whose visibility across the
	// per-attempt filter-chain instances Envoy creates is unverified.
	RequestID string

	// ResponseStatus is this specific attempt's response status code.
	ResponseStatus int
}

// UpstreamAttemptResponseObserver is implemented by a policy that wants to
// know why a specific attempt failed, to inform its own behavior on a
// later attempt of the SAME client request. Fires read-only — nothing
// about the response can be mutated from here; mutation only ever happens
// in UpstreamAttemptPolicy.OnUpstreamAttemptRequest, on a subsequent
// attempt.
//
// A policy's Go instance is long-lived — it spans every request and every
// attempt for this route's lifetime (the same lifetime model-round-robin's
// own suspendedModels map already relies on) — so an implementation
// typically records (RequestID -> observed cause) in its own in-memory
// state here, and reads it back in OnUpstreamAttemptRequest on the next
// attempt. Implementations MUST bound that state (a TTL, or cleanup on the
// request's final downstream response) — an unbounded map keyed by
// RequestID grows without limit for requests that error out before any
// later attempt ever reads the entry back.
type UpstreamAttemptResponseObserver interface {
	OnUpstreamAttemptResponse(ctx context.Context, actx *UpstreamAttemptResponseContext)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd sdk/core && go test ./policy/v1alpha2/... -run TestUpstreamAttemptResponseObserver -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/core/policy/v1alpha2/upstream_attempt_response.go sdk/core/policy/v1alpha2/upstream_attempt_response_test.go
git commit -m "feat(sdk): add optional UpstreamAttemptResponseObserver for per-attempt failure-cause visibility"
```

---

## Task 5: gateway-controller — declarative metadata + generic parsers + generalized validator

**Files:**
- Modify: `gateway/gateway-controller/pkg/models/policy_definition.go:22-30` (`PolicyDefinition` struct — add the three new optional metadata fields)
- Create: `gateway/gateway-controller/pkg/config/retry_source_validator.go` (generalized validator functions, PLUS the two new generic parsers)
- Delete (partial): `gateway/gateway-controller/pkg/config/model_failover_validator.go` (its model-failover-specific parsing — `ParseModelFailoverParams`, `ModelFailoverParams`, `ModelFailoverTargetGroup`, `ModelFailoverFallback`, `MaxFallbackChainLength`, `ValidateModelFailoverUpstreamReferences` — stays exactly where it is; only the two generalized validator functions below move out of this file)
- Test: `gateway/gateway-controller/pkg/config/retry_source_validator_test.go`

**Interfaces:**
- Consumes: `policy.RetrySourceDeclaration`/`RetryGroup`/`RetryTarget` (Task 1), `policy.RetryTriggerDeclaration` (Task 2).
- Produces: `ValidateRetrySourceTargetsHaveNoBasePath(decl *policy.RetrySourceDeclaration, basePathByUpstreamDef map[string]string, mainBasePath string) error`, `ValidateAtMostOneRetrySourcePerRoute(retrySourceCount int, retry *api.Retry) error`, `ParseRetrySourceParams(params map[string]interface{}, groupKeyField string) (*policy.RetrySourceDeclaration, error)`, `ParseRetryTriggerParams(params map[string]interface{}, statusCodesField string, minAttempts int) (*policy.RetryTriggerDeclaration, error)`, and the extended `models.PolicyDefinition` (new fields: `RetrySource *models.RetrySourceMetadata`, `RetryTrigger *models.RetryTriggerMetadata`, `UpstreamResponseObserver bool`).

- [ ] **Step 0: Extend `PolicyDefinition` with the three declarative capability fields**

```go
// gateway/gateway-controller/pkg/models/policy_definition.go — modify the
// existing struct (lines 22-30):

// BEFORE:
type PolicyDefinition struct {
	Name             string                  `json:"name" yaml:"name"`
	Version          string                  `json:"version" yaml:"version"`
	DisplayName      string                  `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	Description      *string                 `json:"description,omitempty" yaml:"description,omitempty"`
	Parameters       *map[string]interface{} `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	SystemParameters *map[string]interface{} `json:"systemParameters,omitempty" yaml:"systemParameters,omitempty"`
	ManagedBy        string                  `json:"managedBy" yaml:"managedBy,omitempty"`
}
```

```go
// AFTER:

// RetrySourceMetadata is a policy-definition.yaml's x-wso2-retry-source
// declaration: this policy's params describe one or more
// independently-selectable failover chains, in the fixed structural shape
// ParseRetrySourceParams understands (targets: [{<GroupKeyField>: string,
// upstreamDefinition: string, fallbacks: [{upstreamDefinition: string}]}],
// statusCodes: [int]).
type RetrySourceMetadata struct {
	// GroupKeyField names which field in each targets[] entry
	// gateway-controller treats as the group's opaque discriminator (e.g.
	// "model" for model-failover). Required when RetrySource is non-nil.
	GroupKeyField string `json:"groupKeyField" yaml:"groupKeyField"`
}

// RetryTriggerMetadata is a policy-definition.yaml's x-wso2-retry-trigger
// declaration: this policy's params contribute retry conditions without
// owning destination selection.
type RetryTriggerMetadata struct {
	// StatusCodesField names which top-level field in this policy's params
	// holds the array of retriable status codes (e.g.
	// "tokenPurgeStatusCodes" for oauth2-generator).
	StatusCodesField string `json:"statusCodesField" yaml:"statusCodesField"`

	// MinAttempts is a fixed constant (not read from params) — the minimum
	// total attempts this policy needs to get value from retrying.
	MinAttempts int `json:"minAttempts" yaml:"minAttempts"`
}

type PolicyDefinition struct {
	Name             string                  `json:"name" yaml:"name"`
	Version          string                  `json:"version" yaml:"version"`
	DisplayName      string                  `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	Description      *string                 `json:"description,omitempty" yaml:"description,omitempty"`
	Parameters       *map[string]interface{} `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	SystemParameters *map[string]interface{} `json:"systemParameters,omitempty" yaml:"systemParameters,omitempty"`
	ManagedBy        string                  `json:"managedBy" yaml:"managedBy,omitempty"`

	// RetrySource is non-nil when this policy's policy-definition.yaml
	// declares x-wso2-retry-source.
	RetrySource *RetrySourceMetadata `json:"x-wso2-retry-source,omitempty" yaml:"x-wso2-retry-source,omitempty"`

	// RetryTrigger is non-nil when this policy's policy-definition.yaml
	// declares x-wso2-retry-trigger.
	RetryTrigger *RetryTriggerMetadata `json:"x-wso2-retry-trigger,omitempty" yaml:"x-wso2-retry-trigger,omitempty"`

	// UpstreamResponseObserver is true when this policy's
	// policy-definition.yaml declares x-wso2-upstream-response-observer:
	// true — see Task 8.
	UpstreamResponseObserver bool `json:"x-wso2-upstream-response-observer,omitempty" yaml:"x-wso2-upstream-response-observer,omitempty"`
}
```

- [ ] **Step 0b: Confirm the YAML loader picks up the new fields automatically**

Run: `grep -n "yaml.Unmarshal\|yaml.NewDecoder" gateway/gateway-controller/pkg/**/*.go 2>/dev/null | grep -i "policydefinition\|policy_definition"` — this codebase's existing `PolicyDefinition` loading already unmarshals the whole YAML file into this struct (that's how `Parameters`/`SystemParameters` already work), so adding fields with `yaml` tags requires no loader changes. If the grep instead reveals a hand-written field-by-field parser (not a generic `yaml.Unmarshal` into this struct), add explicit handling for the three new fields there instead, following that parser's existing per-field pattern exactly.

- [ ] **Step 1: Write the failing tests for the two generic parsers and the two generalized validators**

- [ ] **Step 1: Write the failing tests**

```go
// gateway/gateway-controller/pkg/config/retry_source_validator_test.go
package config

import (
	"testing"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestValidateRetrySourceTargetsHaveNoBasePath_RejectsGroupWithFallbacksAndBasePath(t *testing.T) {
	decl := &policy.RetrySourceDeclaration{
		Groups: []policy.RetryGroup{
			{Key: "gpt-4o", OrderedTargets: []policy.RetryTarget{
				{UpstreamDefinitionName: "aliased-provider"},
				{UpstreamDefinitionName: "fallback"},
			}},
		},
	}
	basePathByUpstreamDef := map[string]string{"aliased-provider": "/anthropic-ctx"}
	err := ValidateRetrySourceTargetsHaveNoBasePath(decl, basePathByUpstreamDef, "")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

func TestValidateRetrySourceTargetsHaveNoBasePath_AllowsSingleTargetGroup(t *testing.T) {
	decl := &policy.RetrySourceDeclaration{
		Groups: []policy.RetryGroup{
			{Key: "gpt-4o", OrderedTargets: []policy.RetryTarget{{UpstreamDefinitionName: "aliased-provider"}}},
		},
	}
	basePathByUpstreamDef := map[string]string{"aliased-provider": "/anthropic-ctx"}
	if err := ValidateRetrySourceTargetsHaveNoBasePath(decl, basePathByUpstreamDef, ""); err != nil {
		t.Errorf("unexpected error for a single-target (no-failover) group: %v", err)
	}
}

func TestValidateAtMostOneRetrySourcePerRoute_RejectsTwoDeclarations(t *testing.T) {
	if err := ValidateAtMostOneRetrySourcePerRoute(2, nil); err == nil {
		t.Fatal("expected an error for two RetrySourcePolicy declarations on one route, got nil")
	}
}

func TestValidateAtMostOneRetrySourcePerRoute_RejectsDeclarationPlusResilienceRetry(t *testing.T) {
	retry := &api.Retry{}
	if err := ValidateAtMostOneRetrySourcePerRoute(1, retry); err == nil {
		t.Fatal("expected an error for a RetrySourcePolicy declaration combined with resilience.retry, got nil")
	}
}

func TestValidateAtMostOneRetrySourcePerRoute_AllowsOneDeclarationAlone(t *testing.T) {
	if err := ValidateAtMostOneRetrySourcePerRoute(1, nil); err != nil {
		t.Errorf("unexpected error for exactly one RetrySourcePolicy declaration: %v", err)
	}
}

func TestValidateAtMostOneRetrySourcePerRoute_AllowsNeither(t *testing.T) {
	if err := ValidateAtMostOneRetrySourcePerRoute(0, nil); err != nil {
		t.Errorf("unexpected error when nothing declares retry ownership: %v", err)
	}
}

func TestParseRetrySourceParams_BuildsGroupsFromStandardShape(t *testing.T) {
	params := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"model":              "gpt-4o",
				"upstreamDefinition": "gpt4-primary",
				"fallbacks": []interface{}{
					map[string]interface{}{"upstreamDefinition": "gpt4-fallback"},
				},
			},
			map[string]interface{}{
				"model":              "claude-3",
				"upstreamDefinition": "claude-primary",
			},
		},
		"statusCodes": []interface{}{429, 503},
	}
	decl, err := ParseRetrySourceParams(params, "model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decl.Groups) != 2 {
		t.Fatalf("Groups = %d, want 2", len(decl.Groups))
	}
	if decl.Groups[0].Key != "gpt-4o" {
		t.Errorf("Groups[0].Key = %q, want %q", decl.Groups[0].Key, "gpt-4o")
	}
	if len(decl.Groups[0].OrderedTargets) != 2 {
		t.Fatalf("Groups[0].OrderedTargets = %d, want 2", len(decl.Groups[0].OrderedTargets))
	}
	if decl.Groups[0].OrderedTargets[0].UpstreamDefinitionName != "gpt4-primary" {
		t.Errorf("OrderedTargets[0] = %q, want %q", decl.Groups[0].OrderedTargets[0].UpstreamDefinitionName, "gpt4-primary")
	}
	if decl.Groups[0].OrderedTargets[1].UpstreamDefinitionName != "gpt4-fallback" {
		t.Errorf("OrderedTargets[1] = %q, want %q", decl.Groups[0].OrderedTargets[1].UpstreamDefinitionName, "gpt4-fallback")
	}
	if len(decl.Groups[1].OrderedTargets) != 1 {
		t.Errorf("Groups[1].OrderedTargets = %d, want 1 (no fallbacks declared)", len(decl.Groups[1].OrderedTargets))
	}
	if len(decl.RetriableStatusCodes) != 2 {
		t.Errorf("RetriableStatusCodes = %v, want [429, 503]", decl.RetriableStatusCodes)
	}
}

func TestParseRetrySourceParams_RejectsMissingTargets(t *testing.T) {
	_, err := ParseRetrySourceParams(map[string]interface{}{"statusCodes": []interface{}{500}}, "model")
	if err == nil {
		t.Fatal("expected an error for missing 'targets', got nil")
	}
}

func TestParseRetryTriggerParams_ReadsNamedStatusCodesField(t *testing.T) {
	params := map[string]interface{}{
		"tokenPurgeStatusCodes": []interface{}{401},
	}
	decl, err := ParseRetryTriggerParams(params, "tokenPurgeStatusCodes", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decl.RetriableStatusCodes) != 1 || decl.RetriableStatusCodes[0] != 401 {
		t.Errorf("RetriableStatusCodes = %v, want [401]", decl.RetriableStatusCodes)
	}
	if decl.MinAttempts != 2 {
		t.Errorf("MinAttempts = %d, want 2", decl.MinAttempts)
	}
}

func TestParseRetryTriggerParams_EmptyFieldIsNotAnError(t *testing.T) {
	// tokenPurgeStatusCodes explicitly set to an empty list (purge-on-reject
	// disabled) is valid — the caller (Task 6) treats an empty
	// RetriableStatusCodes as "no trigger contribution", not a parse error.
	params := map[string]interface{}{"tokenPurgeStatusCodes": []interface{}{}}
	decl, err := ParseRetryTriggerParams(params, "tokenPurgeStatusCodes", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decl.RetriableStatusCodes) != 0 {
		t.Errorf("RetriableStatusCodes = %v, want empty", decl.RetriableStatusCodes)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd gateway/gateway-controller && go test ./pkg/config/... -run "TestValidateRetrySourceTargetsHaveNoBasePath|TestValidateAtMostOneRetrySourcePerRoute|TestParseRetrySourceParams|TestParseRetryTriggerParams" -v`
Expected: FAIL — all four functions undefined.

- [ ] **Step 3: Write the implementation**

```go
// gateway/gateway-controller/pkg/config/retry_source_validator.go
package config

import (
	"fmt"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// ValidateRetrySourceTargetsHaveNoBasePath rejects any RetryGroup with 2+
// OrderedTargets (i.e. one that will actually get an aggregate cluster
// built — see xds/translator.go) where any target resolves to a backend
// with a non-empty BasePath. An empty UpstreamDefinitionName means "the
// API's main upstream" and is checked against mainBasePath.
//
// This is a real, confirmed correctness gap, not a hypothetical: Envoy's
// aggregate-cluster + retry_priority mechanism only ever varies WHICH
// CLUSTER gets dialed on a retry — it has no hook to vary the REQUEST PATH
// per member. An upstream that relies on a base path to reach the right
// destination (most notably an LlmProxy additionalProviders-derived alias,
// or an LlmProxy's own main upstream — both always resolve to the
// identical loopback address and are distinguished ONLY by path) silently
// misroutes on retry otherwise. Generalized from
// ValidateModelFailoverAggregateMembersHaveNoBasePath — same logic,
// operating on any policy's declared RetrySourceDeclaration.Groups instead
// of one policy's own ModelFailoverParams.Targets.
func ValidateRetrySourceTargetsHaveNoBasePath(decl *policy.RetrySourceDeclaration, basePathByUpstreamDef map[string]string, mainBasePath string) error {
	if decl == nil {
		return nil
	}
	resolve := func(upstreamDef string) string {
		if upstreamDef == "" {
			return mainBasePath
		}
		return basePathByUpstreamDef[upstreamDef]
	}
	describe := func(upstreamDef string) string {
		if upstreamDef == "" {
			return "the API's main upstream"
		}
		return fmt.Sprintf("upstreamDefinition %q", upstreamDef)
	}
	for _, group := range decl.Groups {
		if len(group.OrderedTargets) < 2 {
			continue // no aggregate cluster is ever built for this group - not at risk
		}
		for _, target := range group.OrderedTargets {
			if bp := resolve(target.UpstreamDefinitionName); bp != "" && bp != "/" {
				return fmt.Errorf("retry-source group %q resolves target %s, which has a non-empty basePath (%q), and will be used as an aggregate-cluster member — Envoy's native retry cannot vary the request path per member, so a basePath-dependent upstream (e.g. an LlmProxy additionalProviders alias, or an LlmProxy's own main upstream) would silently misroute on retry. Give this target no fallbacks, or point every member of this group at a plain, no-basePath upstream", group.Key, describe(target.UpstreamDefinitionName), bp)
			}
		}
	}
	return nil
}

// ValidateAtMostOneRetrySourcePerRoute rejects a route where retry
// ownership is ambiguous: more than one policy declaring
// x-wso2-retry-source, OR exactly one such policy combined with
// resilience.retry also configured. Both are real conflicts — Envoy has
// exactly one RouteAction.RetryPolicy (and one RetryPriority extension
// slot) per route, so two independent owners can never be reconciled.
// retrySourceCount is the number of policies in this route's chain whose
// policy-definition.yaml declares x-wso2-retry-source (computed by the
// caller via the declarative metadata lookup — see
// xds/translator.go's resolveRetryDeclarations, Task 6 Step 6a — never a
// Go type assertion, since gateway-controller never instantiates policy
// Go code) — this function only enforces the counting rule, generic to
// whichever policies happen to be present. Generalized from
// ValidateModelFailoverPolicy.
func ValidateAtMostOneRetrySourcePerRoute(retrySourceCount int, retry *api.Retry) error {
	if retrySourceCount == 0 {
		return nil
	}
	if retrySourceCount > 1 {
		return fmt.Errorf("this route has %d policies each declaring retry-source ownership — at most one is allowed per route, since Envoy has a single RouteAction.RetryPolicy slot", retrySourceCount)
	}
	if retry != nil {
		return fmt.Errorf("a retry-source policy on this route cannot be combined with resilience.retry — both would drive RouteAction.RetryPolicy")
	}
	return nil
}

// ParseRetrySourceParams generically parses a policy's params into a
// RetrySourceDeclaration, for ANY policy whose policy-definition.yaml
// declares x-wso2-retry-source — driven entirely by the fixed structural
// shape (targets: [{<groupKeyField>: string, upstreamDefinition: string,
// fallbacks: [{upstreamDefinition: string}]}], statusCodes: [int]) and the
// caller-supplied groupKeyField (from that policy's own
// models.RetrySourceMetadata.GroupKeyField). gateway-controller never
// executes policy Go code to produce this — see the design's Design
// Revision 2 for why. Fields in each targets[]/fallbacks[] entry other
// than groupKeyField/upstreamDefinition (e.g. model-failover's own
// fallbacks[].model, used to rewrite the request body — a concern entirely
// internal to that policy's own runtime code) are ignored here; this
// parser only extracts what gateway-controller itself needs to build
// Envoy config.
func ParseRetrySourceParams(params map[string]interface{}, groupKeyField string) (*policy.RetrySourceDeclaration, error) {
	rawTargets, ok := params["targets"].([]interface{})
	if !ok || len(rawTargets) == 0 {
		return nil, fmt.Errorf("retry-source policy requires a non-empty 'targets' list")
	}

	groups := make([]policy.RetryGroup, 0, len(rawTargets))
	for i, raw := range rawTargets {
		t, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("retry-source policy: targets[%d] is not an object", i)
		}
		key, _ := t[groupKeyField].(string)
		if key == "" {
			return nil, fmt.Errorf("retry-source policy: targets[%d].%s is required", i, groupKeyField)
		}
		upstreamDef, _ := t["upstreamDefinition"].(string)
		orderedTargets := []policy.RetryTarget{{UpstreamDefinitionName: upstreamDef}}

		rawFallbacks, _ := t["fallbacks"].([]interface{})
		for j, rawFb := range rawFallbacks {
			fb, ok := rawFb.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("retry-source policy: targets[%d].fallbacks[%d] is not an object", i, j)
			}
			fbUpstreamDef, _ := fb["upstreamDefinition"].(string)
			orderedTargets = append(orderedTargets, policy.RetryTarget{UpstreamDefinitionName: fbUpstreamDef})
		}

		groups = append(groups, policy.RetryGroup{Key: key, OrderedTargets: orderedTargets})
	}

	rawStatusCodes, ok := params["statusCodes"].([]interface{})
	if !ok || len(rawStatusCodes) == 0 {
		return nil, fmt.Errorf("retry-source policy requires a non-empty 'statusCodes' list")
	}
	statusCodes := make([]int, 0, len(rawStatusCodes))
	for i, raw := range rawStatusCodes {
		code, ok := raw.(int)
		if !ok {
			if f, ok := raw.(float64); ok { // YAML/JSON numeric decode may hand back float64
				code = int(f)
			} else {
				return nil, fmt.Errorf("retry-source policy: statusCodes[%d] must be an integer, got %T", i, raw)
			}
		}
		statusCodes = append(statusCodes, code)
	}

	return &policy.RetrySourceDeclaration{Groups: groups, RetriableStatusCodes: statusCodes}, nil
}

// ParseRetryTriggerParams generically parses a policy's params into a
// RetryTriggerDeclaration, for ANY policy whose policy-definition.yaml
// declares x-wso2-retry-trigger. statusCodesField and minAttempts come
// from that policy's own models.RetryTriggerMetadata. An absent or empty
// named field is not an error — it means this policy contributes no
// trigger conditions for the current config (e.g. oauth2-generator's
// tokenPurgeStatusCodes explicitly set to []), which the caller (Task 6)
// treats as "nothing to add", not a failure.
func ParseRetryTriggerParams(params map[string]interface{}, statusCodesField string, minAttempts int) (*policy.RetryTriggerDeclaration, error) {
	rawStatusCodes, ok := params[statusCodesField].([]interface{})
	if !ok {
		return &policy.RetryTriggerDeclaration{}, nil
	}
	statusCodes := make([]int, 0, len(rawStatusCodes))
	for i, raw := range rawStatusCodes {
		code, ok := raw.(int)
		if !ok {
			if f, ok := raw.(float64); ok {
				code = int(f)
			} else {
				return nil, fmt.Errorf("retry-trigger policy: %s[%d] must be an integer, got %T", statusCodesField, i, raw)
			}
		}
		statusCodes = append(statusCodes, code)
	}
	if len(statusCodes) == 0 {
		return &policy.RetryTriggerDeclaration{}, nil
	}
	return &policy.RetryTriggerDeclaration{RetriableStatusCodes: statusCodes, MinAttempts: minAttempts}, nil
}
```

- [ ] **Step 4: Delete the superseded functions from `model_failover_validator.go`**

Remove `ValidateModelFailoverAggregateMembersHaveNoBasePath` (old lines 226-256) and `ValidateModelFailoverPolicy` (old lines 261-269) from `gateway/gateway-controller/pkg/config/model_failover_validator.go` — their logic now lives in `retry_source_validator.go` above. `ParseModelFailoverParams`, `ModelFailoverParams`/`ModelFailoverTargetGroup`/`ModelFailoverFallback`, `MaxFallbackChainLength`, and `ValidateModelFailoverUpstreamReferences` (upstream-reference existence checking — genuinely model-failover-specific, since it validates model-failover's own params shape, not a generic retry-source concern) stay in `model_failover_validator.go` unchanged. `ValidateModelFailoverForOperations` is rewritten in Task 6 once the generic discovery loop exists to call it from — do not remove it yet in this task, only the two functions named above.

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd gateway/gateway-controller && go test ./pkg/config/... -v`
Expected: PASS, entire package.

- [ ] **Step 6: Commit**

```bash
git add gateway/gateway-controller/pkg/config/ gateway/gateway-controller/pkg/models/
git commit -m "feat(gateway-controller): declarative retry-source/retry-trigger policy-definition metadata + generic params parsers"
```

---

## Task 6: gateway-controller `translator.go` — generic discovery + composition loop

**Files:**
- Modify: `gateway/gateway-controller/pkg/xds/translator.go:287-327` (the existing model-failover aggregate-cluster loop)
- Modify: `gateway/gateway-controller/pkg/xds/translator.go:732-745` (`modelFailoverGroupMemberClusterNames`)
- Modify: `gateway/gateway-controller/pkg/xds/translator.go:3109-3139` (`modelFailoverGroupUpstreamName`, `ModelFailoverGroupClusterKey`)
- Modify: `gateway/gateway-controller/pkg/config/model_failover_validator.go` (rewrite `ValidateModelFailoverForOperations` → move a generalized version into `retry_source_validator.go`)
- Test: `gateway/gateway-controller/pkg/xds/translator_retry_source_test.go`

**Interfaces:**
- Consumes: `policy.RetryGroup`/`RetrySourceDeclaration`/`RetryTriggerDeclaration` (Tasks 1-2), `policy.RetrySourceUpstreamName` (Task 1), `config.ParseRetrySourceParams`/`ParseRetryTriggerParams`/`ValidateAtMostOneRetrySourcePerRoute`/`ValidateRetrySourceTargetsHaveNoBasePath` (Task 5), the loaded `map[string]models.PolicyDefinition` registry (`policyDefinitions`, the same one `PolicyValidator` already holds — see Step 6a) for `RetrySource`/`RetryTrigger` metadata lookup.
- Produces: `retrySourceTargetClusterNames(group policy.RetryGroup, apiKind, apiID, mainClusterName string) []string` (renamed from `modelFailoverGroupMemberClusterNames`, now takes a `policy.RetryGroup` instead of `config.ModelFailoverTargetGroup`), `RetrySourceAggregateClusterKey(kind, uuid, routeKey, groupKey string) string` (renamed from `ModelFailoverGroupClusterKey`, delegates to `policy.RetrySourceUpstreamName`), `(t *Translator) resolveRetryDeclarations(chain *models.PolicyChain) (sourceDecl *policy.RetrySourceDeclaration, sourceCount int, triggerCodes map[int]struct{}, triggerMinAttempts int, err error)` — the declarative (metadata-lookup-driven, not type-assertion-driven) replacement for what Step 6's original design called `resolvePolicyImplementation`.

- [ ] **Step 1: Write the failing test**

```go
// gateway/gateway-controller/pkg/xds/translator_retry_source_test.go
package xds

import (
	"testing"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestRetrySourceTargetClusterNames_MainAndNamedTargets(t *testing.T) {
	group := policy.RetryGroup{
		Key: "gpt-4o",
		OrderedTargets: []policy.RetryTarget{
			{UpstreamDefinitionName: ""},       // main
			{UpstreamDefinitionName: "backup"}, // named
		},
	}
	got := retrySourceTargetClusterNames(group, "LlmProvider", "abc-123", "upstream_main_example.com_443")
	want := []string{
		"upstream_main_example.com_443",
		"upstream_LlmProvider_abc-123_backup",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRetrySourceAggregateClusterKey_MatchesSDKFormula(t *testing.T) {
	got := RetrySourceAggregateClusterKey("LlmProvider", "abc-123", "POST|/chat/completions|main", "gpt-4o")
	// Must be built from the SAME local-name formula policy.RetrySourceUpstreamName
	// produces, so a policy setting UpstreamName at runtime resolves to
	// exactly this cluster.
	wantLocal := policy.RetrySourceUpstreamName("POST|/chat/completions|main", "gpt-4o")
	want := "upstream_LlmProvider_abc-123_" + wantLocal
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd gateway/gateway-controller && go test ./pkg/xds/... -run TestRetrySource -v`
Expected: FAIL — `retrySourceTargetClusterNames`/`RetrySourceAggregateClusterKey` undefined.

- [ ] **Step 3: Rename and rewrite `modelFailoverGroupMemberClusterNames` (lines 732-745)**

```go
// BEFORE:
func modelFailoverGroupMemberClusterNames(target config.ModelFailoverTargetGroup, apiKind, apiID, mainClusterName string) []string {
	resolve := func(upstreamDef string) string {
		if upstreamDef == "" {
			return mainClusterName
		}
		return constants.UpstreamDefinitionClusterPrefix + apiKind + "_" + apiID + "_" + sanitizeUpstreamDefinitionName(upstreamDef)
	}
	names := make([]string, 0, 1+len(target.Fallbacks))
	names = append(names, resolve(target.UpstreamDefinition))
	for _, fb := range target.Fallbacks {
		names = append(names, resolve(fb.UpstreamDefinition))
	}
	return names
}
```

```go
// AFTER — takes a generic policy.RetryGroup instead of a
// model-failover-specific ModelFailoverTargetGroup:
func retrySourceTargetClusterNames(group policy.RetryGroup, apiKind, apiID, mainClusterName string) []string {
	resolve := func(upstreamDef string) string {
		if upstreamDef == "" {
			return mainClusterName
		}
		return constants.UpstreamDefinitionClusterPrefix + apiKind + "_" + apiID + "_" + sanitizeUpstreamDefinitionName(upstreamDef)
	}
	names := make([]string, 0, len(group.OrderedTargets))
	for _, target := range group.OrderedTargets {
		names = append(names, resolve(target.UpstreamDefinitionName))
	}
	return names
}
```

Add `policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"` to this file's import block if not already present (it is — `translator.go` already imports the policy SDK for other hook types).

- [ ] **Step 4: Rename `modelFailoverGroupUpstreamName`/`ModelFailoverGroupClusterKey` (lines 3109-3139)**

```go
// BEFORE:
func modelFailoverGroupUpstreamName(routeKey, groupModel string) string {
	safeRouteKey := strings.ReplaceAll(routeKey, "|", "_")
	if routeKey == "" {
		return "__modelfailover_target__" + groupModel
	}
	return "__modelfailover_target__" + safeRouteKey + "__" + groupModel
}

func ModelFailoverGroupClusterKey(kind, uuid, routeKey, groupModel string) string {
	return constants.UpstreamDefinitionClusterPrefix + kind + "_" + uuid + "_" + sanitizeUpstreamDefinitionName(modelFailoverGroupUpstreamName(routeKey, groupModel))
}
```

```go
// AFTER — the local-name formula itself moves into the SDK
// (policy.RetrySourceUpstreamName, Task 1) so it's no longer duplicated
// between this repo and any out-of-repo retry-source-capable policy's own
// runtime code. This function now only adds the kind/uuid cluster-naming
// prefix, exactly mirroring how a plain upstreamDefinition's cluster name
// is built.
func RetrySourceAggregateClusterKey(kind, uuid, routeKey, groupKey string) string {
	return constants.UpstreamDefinitionClusterPrefix + kind + "_" + uuid + "_" + sanitizeUpstreamDefinitionName(policy.RetrySourceUpstreamName(routeKey, groupKey))
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `cd gateway/gateway-controller && go test ./pkg/xds/... -run TestRetrySource -v`
Expected: PASS

- [ ] **Step 6: Replace the discovery loop (lines 287-327) with the generic version**

```go
// BEFORE (lines 287-327):
	for routeKey, chain := range rdc.PolicyChains {
		for _, p := range chain.Policies {
			if p.Name != "model-failover" {
				continue
			}
			mf, err := config.ParseModelFailoverParams(p.Params)
			if err != nil {
				return nil, nil, fmt.Errorf("route %q: %w", routeKey, err)
			}
			var mainClusterName string
			if rdcRoute, ok := rdc.Routes[routeKey]; ok {
				mainClusterName = rdcRoute.Upstream.DefaultCluster
			}
			for _, target := range mf.Targets {
				if len(target.Fallbacks) == 0 {
					continue
				}
				memberNames := modelFailoverGroupMemberClusterNames(target, rdc.Metadata.Kind, rdc.Metadata.UUID, mainClusterName)
				aggName := ModelFailoverGroupClusterKey(rdc.Metadata.Kind, rdc.Metadata.UUID, routeKey, target.Model)
				aggCluster, err := t.createAggregateCluster(aggName, memberNames)
				if err != nil {
					return nil, nil, fmt.Errorf("route %q, target %q: %w", routeKey, target.Model, err)
				}
				clusters = append(clusters, aggCluster)
			}
			break
		}
	}
```

```go
// AFTER — declarative discovery (policy-definition.yaml metadata lookup,
// NOT a Go type assertion — gateway-controller never instantiates policy
// Go code, confirmed against go.mod), RetryTrigger composition, and the
// plain-RetryPolicy fallback path (no RetrySource present, only trigger
// declarations) that makes oauth2-generator's retry independent of
// model-failover:
	for routeKey, chain := range rdc.PolicyChains {
		sourceDecl, sourceCount, triggerCodes, triggerMinAttempts, err := t.resolveRetryDeclarations(chain)
		if err != nil {
			return nil, nil, fmt.Errorf("route %q: %w", routeKey, err)
		}
		_ = sourceCount // exclusivity already enforced at registration time (Task 5/9's ValidateRetrySourcesForOperations); a route reaching translation with sourceCount > 1 is a bug in that gate, not something to re-validate mid-translation

		if sourceDecl == nil && len(triggerCodes) == 0 {
			continue // nothing on this route declares any retry need
		}

		var mainClusterName string
		if rdcRoute, ok := rdc.Routes[routeKey]; ok {
			mainClusterName = rdcRoute.Upstream.DefaultCluster
		}

		if sourceDecl != nil {
			// Fold every RetryTrigger declaration's status codes into the
			// SAME aggregate-cluster RetryPolicy this RetrySource already
			// needs — one Envoy retry policy, richer trigger conditions,
			// no second aggregate cluster.
			mergedCodes := append([]int{}, sourceDecl.RetriableStatusCodes...)
			for code := range triggerCodes {
				mergedCodes = append(mergedCodes, code)
			}
			sourceDecl.RetriableStatusCodes = mergedCodes

			for _, group := range sourceDecl.Groups {
				if len(group.OrderedTargets) < 2 {
					continue // single-target group: no failover, no aggregate cluster needed
				}
				memberNames := retrySourceTargetClusterNames(group, rdc.Metadata.Kind, rdc.Metadata.UUID, mainClusterName)
				aggName := RetrySourceAggregateClusterKey(rdc.Metadata.Kind, rdc.Metadata.UUID, routeKey, group.Key)
				aggCluster, err := t.createAggregateCluster(aggName, memberNames)
				if err != nil {
					return nil, nil, fmt.Errorf("route %q, group %q: %w", routeKey, group.Key, err)
				}
				clusters = append(clusters, aggCluster)
			}
		}
		// The no-RetrySource, trigger-only case (plain RouteAction.RetryPolicy,
		// no aggregate cluster, no RetryPriority) is wired into route construction
		// in createRouteFromRDC — see Step 7 below; this loop's job is only
		// cluster construction, route-level RetryPolicy fields are set where every
		// other route field already is.
	}
```

- [ ] **Step 6a: Implement `resolveRetryDeclarations` — the declarative replacement for a Go-interface discovery loop**

```go
// Add to gateway/gateway-controller/pkg/xds/translator.go:

// resolveRetryDeclarations walks chain.Policies and, for each one, looks up
// its ALREADY-LOADED policy-definition.yaml metadata (t.policyDefinitions —
// the same registry PolicyValidator already holds; confirm the exact field
// name on *Translator by grepping "policyDefinitions" in this package, and
// wire it through the Translator's own construction if it isn't already a
// field there — this registry is loaded once at controller startup, not
// per-translation) to decide whether that policy contributes a retry-source
// or retry-trigger declaration. This NEVER instantiates policy Go code —
// gateway-controller has no dependency on any policy implementation
// package (see the design's Design Revision 2) — it only reads cached YAML
// metadata plus the policy's raw Params map, both of which
// gateway-controller already has for every policy in a chain today.
func (t *Translator) resolveRetryDeclarations(chain *models.PolicyChain) (
	sourceDecl *policy.RetrySourceDeclaration,
	sourceCount int,
	triggerCodes map[int]struct{},
	triggerMinAttempts int,
	err error,
) {
	triggerCodes = map[int]struct{}{}

	for _, p := range chain.Policies {
		def, ok := t.policyDefinitions[p.Name+"@"+p.Version] // match this registry's real existing key format — grep an existing lookup against t.policyDefinitions/pv.policyDefinitions in this package rather than assuming "name@version"
		if !ok {
			continue
		}
		if def.RetrySource != nil {
			sourceCount++
			decl, parseErr := config.ParseRetrySourceParams(p.Params, def.RetrySource.GroupKeyField)
			if parseErr != nil {
				return nil, 0, nil, 0, fmt.Errorf("policy %q: %w", p.Name, parseErr)
			}
			sourceDecl = decl // exactly one, by construction — ValidateAtMostOneRetrySourcePerRoute already rejected >1 at registration time
		}
		if def.RetryTrigger != nil {
			decl, parseErr := config.ParseRetryTriggerParams(p.Params, def.RetryTrigger.StatusCodesField, def.RetryTrigger.MinAttempts)
			if parseErr != nil {
				return nil, 0, nil, 0, fmt.Errorf("policy %q: %w", p.Name, parseErr)
			}
			for _, code := range decl.RetriableStatusCodes {
				triggerCodes[code] = struct{}{}
			}
			if decl.MinAttempts > triggerMinAttempts {
				triggerMinAttempts = decl.MinAttempts
			}
		}
	}
	return sourceDecl, sourceCount, triggerCodes, triggerMinAttempts, nil
}
```

Before finalizing this function, run `grep -n "policyDefinitions\[" gateway/gateway-controller/pkg/xds/*.go gateway/gateway-controller/pkg/config/*.go` to confirm the real lookup-key format this codebase already uses for a `map[string]models.PolicyDefinition` (the placeholder above guesses `"name@version"` — `PolicyValidator.resolvePolicyVersion`/`ResolvePolicyVersion` in `policy_validator.go:212-283` almost certainly already establish the real convention; use it verbatim, not the guessed format). Also confirm whether `*Translator` already holds a `policyDefinitions` field (grep `type Translator struct` in `translator.go`) — if not, this task must add one, populated the same way `PolicyValidator` already is at controller startup (same source data, second consumer).

- [ ] **Step 7: Wire the trigger-only plain-RetryPolicy path into route construction**

In `createRouteFromRDC` (translator.go, builds `RouteAction.RetryPolicy` per route today from `rdcRoute.Timeout.Retry`), add: when this route has no retry-source declaration but has one or more retry-trigger declarations (the values `resolveRetryDeclarations` computed in Step 6, threaded through `rdc`/passed as an argument), build a plain `route.RetryPolicy{RetriableStatusCodes: <union>, RetryOn: "retriable-status-codes", NumRetries: wrapperspb.UInt32(uint32(triggerMinAttempts - 1))}` — no `RetryPriority` field set at all, so Envoy performs ordinary same-cluster retry. This is the path that makes `oauth2-generator` (Task 10) retry-independent with zero other policy present. Follow this codebase's existing convention for threading a route-scoped value computed in the clusters-building pass through to the routes-building pass (`rdc.Routes[routeKey]` already carries per-route fields set earlier in this same function — add the trigger-derived fields there, consistent with how `mainClusterName`/`useClusterHeader` are already threaded).

- [ ] **Step 8: Rewrite `ValidateModelFailoverForOperations` to the generic entry point**

Move a generalized version into `retry_source_validator.go` (from Task 5), calling it `ValidateRetrySourcesForOperations`:

```go
// gateway/gateway-controller/pkg/config/retry_source_validator.go — append:

// ValidateRetrySourcesForOperations runs the two generic retry-source
// checks (ValidateAtMostOneRetrySourcePerRoute,
// ValidateRetrySourceTargetsHaveNoBasePath) against every operation in
// spec, for whichever policies in that operation's chain declare
// x-wso2-retry-source in their policy-definition.yaml — generalized from
// ValidateModelFailoverForOperations, which only ever looked for a policy
// literally named "model-failover". resolveDeclarations is a
// caller-supplied function (xds package can't be imported here — this
// pkg/config package is a dependency of pkg/xds, not the reverse) that,
// given an operation's policy list, returns every RetrySourceDeclaration
// found via the SAME declarative policy-definition-metadata lookup +
// ParseRetrySourceParams call translator.go's resolveRetryDeclarations
// (Task 6, Step 6a) uses, plus the total count (for the exclusivity check)
// — this validator and that translation-time discovery MUST use identical
// logic, so this function accepts it as a parameter rather than
// duplicating it.
func ValidateRetrySourcesForOperations(
	spec *api.APIConfigData,
	resolveDeclarations func(apiPolicies, opPolicies *[]api.Policy) (decls []*policy.RetrySourceDeclaration, count int, err error),
) error {
	if spec == nil {
		return nil
	}

	basePathByUpstreamDef := make(map[string]string)
	if spec.UpstreamDefinitions != nil {
		for _, def := range *spec.UpstreamDefinitions {
			if def.BasePath != nil {
				basePathByUpstreamDef[def.Name] = *def.BasePath
			}
		}
	}
	mainBasePath := mainUpstreamBasePath(spec.Upstream.Main, spec.UpstreamDefinitions)
	apiRetry := effectiveResilienceRetry(spec.Resilience)

	for _, op := range spec.Operations {
		decls, count, err := resolveDeclarations(spec.Policies, op.Policies)
		if err != nil {
			return fmt.Errorf("operation %s %s: %w", op.EffectiveMethod(), op.EffectivePath(), err)
		}
		if count == 0 {
			continue
		}
		effectiveRetry := effectiveResilienceRetry(op.Resilience)
		if effectiveRetry == nil {
			effectiveRetry = apiRetry
		}
		if err := ValidateAtMostOneRetrySourcePerRoute(count, effectiveRetry); err != nil {
			return fmt.Errorf("operation %s %s: %w", op.EffectiveMethod(), op.EffectivePath(), err)
		}
		for _, decl := range decls {
			if err := ValidateRetrySourceTargetsHaveNoBasePath(decl, basePathByUpstreamDef, mainBasePath); err != nil {
				return fmt.Errorf("operation %s %s: %w", op.EffectiveMethod(), op.EffectivePath(), err)
			}
		}
	}
	return nil
}
```

Update every call site of `ValidateModelFailoverForOperations` (registration-time deploy paths — `llm_deployment.go`, `api_deployment.go`, per the design's F3 findings on where this was wired) to call `config.ValidateRetrySourcesForOperations` instead, passing a `resolveDeclarations` closure that wraps the SAME declarative policy-definition-metadata lookup + `config.ParseRetrySourceParams` call `resolveRetryDeclarations` (Step 6a) uses — these registration-time deploy paths and `xds.TranslateConfigs` must resolve identically, or a config could pass validation and then fail translation (or vice versa) on a divergent reading of the same policy chain.

- [ ] **Step 9: Run the full package tests**

Run: `cd gateway/gateway-controller && go test ./pkg/xds/... ./pkg/config/... -v`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add gateway/gateway-controller/pkg/xds/ gateway/gateway-controller/pkg/config/
git commit -m "refactor(gateway-controller): replace model-failover name check with declarative retry-source/retry-trigger discovery"
```

---

## Task 7: gateway-controller — cluster de-duplication

**Files:**
- Modify: `gateway/gateway-controller/pkg/xds/translator.go:1634-1698` (`resolveUpstreamCluster`)
- Modify: `gateway/gateway-controller/pkg/xds/translator.go:1577-1625` (the `UpstreamDefinitions` cluster-building loop)
- Test: `gateway/gateway-controller/pkg/xds/translator_cluster_dedup_test.go`

**Interfaces:**
- Produces: `(t *Translator) resolveOrCreateUpstreamDefinitionCluster(name string, def api.UpstreamDefinition, apiKind, apiID string, seen map[string]*cluster.Cluster) (clusterName string, err error)` — memoized per-API-resource cluster builder both call sites share.

- [ ] **Step 1: Write the failing test**

```go
// gateway/gateway-controller/pkg/xds/translator_cluster_dedup_test.go
package xds

import "testing"

func TestResolveOrCreateUpstreamDefinitionCluster_SameNameReturnsSameCluster(t *testing.T) {
	tr := &Translator{} // use whatever minimal construction this package's other tests already use for *Translator
	seen := map[string]*cluster.Cluster{}
	def := api.UpstreamDefinition{
		Name: "azure-eastus",
		Upstreams: []struct {
			Url    string `json:"url" yaml:"url"`
			Weight *int   `json:"weight,omitempty" yaml:"weight,omitempty"`
		}{{Url: "http://sample-backend:5000"}},
	}
	name1, err := tr.resolveOrCreateUpstreamDefinitionCluster("azure-eastus", def, "LlmProvider", "abc-123", seen)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	name2, err := tr.resolveOrCreateUpstreamDefinitionCluster("azure-eastus", def, "LlmProvider", "abc-123", seen)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if name1 != name2 {
		t.Errorf("expected the same cluster name on repeat resolution, got %q then %q", name1, name2)
	}
	if len(seen) != 1 {
		t.Errorf("expected exactly one cluster registered in seen, got %d", len(seen))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd gateway/gateway-controller && go test ./pkg/xds/... -run TestResolveOrCreateUpstreamDefinitionCluster -v`
Expected: FAIL — function undefined.

- [ ] **Step 3: Write the implementation**

```go
// Add to gateway/gateway-controller/pkg/xds/translator.go, near resolveUpstreamCluster:

// resolveOrCreateUpstreamDefinitionCluster returns the Envoy cluster name
// for the named upstreamDefinition def, creating and registering the
// cluster in seen on first use and reusing the SAME cluster on every
// subsequent call for the same name within one API resource's translation
// pass. seen is keyed by the final cluster name and shared across both the
// "main"/default upstream resolution (resolveUpstreamCluster) and the
// UpstreamDefinitions loop — previously two independent code paths that
// each built their own cluster for the same target when a name was
// referenced both as upstream.ref and standalone in upstreamDefinitions
// (confirmed live: a target used both ways produced two separate Envoy
// clusters pointing at the identical backend, doubling connection-pool and
// health-check overhead for no reason).
func (t *Translator) resolveOrCreateUpstreamDefinitionCluster(name string, def api.UpstreamDefinition, apiKind, apiID string, seen map[string]*cluster.Cluster) (string, error) {
	sanitizedName := sanitizeUpstreamDefinitionName(name)
	clusterName := constants.UpstreamDefinitionClusterPrefix + apiKind + "_" + apiID + "_" + sanitizedName
	if _, ok := seen[clusterName]; ok {
		return clusterName, nil
	}
	if len(def.Upstreams) == 0 || def.Upstreams[0].Url == "" {
		return "", fmt.Errorf("upstream definition '%s' has no URLs configured", name)
	}
	parsedURL, err := url.Parse(def.Upstreams[0].Url)
	if err != nil {
		return "", fmt.Errorf("invalid URL in upstream definition '%s': %w", name, err)
	}
	if parsedURL.Host == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		return "", fmt.Errorf("invalid upstream definition '%s' URL: must include host and http/https scheme", name)
	}
	if def.BasePath != nil {
		parsedURL.Path = *def.BasePath
	}
	var connectTimeout *time.Duration
	if def.Timeout != nil {
		resolved, err := resolveTimeoutFromDefinition(&def)
		if err != nil {
			return "", fmt.Errorf("invalid timeout in upstream definition '%s': %w", name, err)
		}
		if resolved != nil {
			connectTimeout = resolved.Connect
		}
	}
	seen[clusterName] = t.createCluster(clusterName, parsedURL, nil, connectTimeout)
	return clusterName, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd gateway/gateway-controller && go test ./pkg/xds/... -run TestResolveOrCreateUpstreamDefinitionCluster -v`
Expected: PASS

- [ ] **Step 5: Wire both call sites through the shared helper**

In `resolveUpstreamCluster` (translator.go:1634-1698), the `up.Ref != nil` branch currently resolves the definition and computes a HOST+SCHEME-keyed cluster name via `t.sanitizeClusterName(parsedURL.Host, parsedURL.Scheme)` (line ~1695) — separately from whatever the `UpstreamDefinitions` loop does. Replace that branch's cluster-naming with a call to `t.resolveOrCreateUpstreamDefinitionCluster(refName, *definition, apiKind, apiID, sharedSeenMap)`, and update the `UpstreamDefinitions` loop (lines 1577-1625) to call the same helper with the same `sharedSeenMap` instead of its own inline cluster construction. `sharedSeenMap` must be a `map[string]*cluster.Cluster` created once per API-resource translation (the same scope `clusters []*cluster.Cluster` is already accumulated in) and passed into both call sites — at the end of translation, merge every entry in `sharedSeenMap` into the resource's `clusters` slice exactly once.

- [ ] **Step 6: Add a regression test proving the fix at the resource level**

```go
// Add to translator_cluster_dedup_test.go
func TestTranslateConfigs_MainRefAndNamedDefinitionShareOneCluster(t *testing.T) {
	// Build a minimal API config where upstream.ref == "azure-eastus" AND
	// "azure-eastus" also appears in upstreamDefinitions (the exact live
	// scenario confirmed earlier: two clusters for one backend). Use this
	// package's existing test-fixture-construction helpers for
	// api.APIConfigData / RuntimeDeployConfig — grep existing
	// TestTranslateConfigs_* tests in this package for the established
	// pattern and follow it exactly rather than hand-building the config.
	//
	// Assert: the resulting []*cluster.Cluster contains exactly ONE
	// cluster whose name references "azure-eastus" (not two), regardless
	// of whether it's reached via the main/default path or a named
	// upstreamDefinition reference.
}
```

Fill in this test using the exact fixture-construction pattern already established by this package's existing `TestTranslateConfigs_*` tests (grep for one and mirror its setup) — do not invent a new fixture style.

- [ ] **Step 7: Run the full package tests**

Run: `cd gateway/gateway-controller && go test ./pkg/xds/... -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add gateway/gateway-controller/pkg/xds/
git commit -m "fix(gateway-controller): deduplicate Envoy clusters when an upstreamDefinition is referenced both as the main upstream and by name"
```

---

## Task 8: gateway-controller `translator.go` — response-observer capability wiring

**Files:**
- Modify: `gateway/gateway-controller/pkg/xds/translator.go` (near `collectClustersNeedingUpstreamFilter`/`collectClustersNeedingUpstreamBodyFilter`, `createUpstreamRefreshExtProcFilter`/`createUpstreamBodyExtProcFilter`, `attachUpstreamRefreshFilter`)
- Test: `gateway/gateway-controller/pkg/xds/translator_response_observer_test.go`

**Interfaces:**
- Consumes: `models.PolicyDefinition.UpstreamResponseObserver` (Task 5) — **not** `policy.UpstreamAttemptResponseObserver` directly; `gateway-controller` cannot type-assert against it for the same reason established in Task 6 (no policy Go instantiation in this process — see the design's Design Revision 2). The Go interface from Task 4 is real and used, but only inside `gateway-runtime`'s kernel (Task 9), which does instantiate policy objects.
- Produces: `collectClustersNeedingUpstreamResponseObserver(routes []*route.Route, chainForRoute func(routeKey string) *models.PolicyChain, policyDefinitions map[string]models.PolicyDefinition, dest map[string]bool)`, `(t *Translator) createUpstreamResponseObserverExtProcFilter() (*hcm.HttpFilter, error)`.

- [ ] **Step 1: Write the failing test**

```go
// gateway/gateway-controller/pkg/xds/translator_response_observer_test.go
package xds

import "testing"

func TestCreateUpstreamResponseObserverExtProcFilter_SetsResponseHeaderModeSend(t *testing.T) {
	tr := &Translator{routerConfig: /* minimal RouterConfig this package's other filter-construction tests already use */}
	filter, err := tr.createUpstreamResponseObserverExtProcFilter()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filter == nil {
		t.Fatal("expected a non-nil HttpFilter")
	}
	// Unmarshal filter.GetTypedConfig() into extproc.ExternalProcessor and
	// assert ProcessingMode.ResponseHeaderMode == extproc.ProcessingMode_SEND
	// and RequestHeaderMode == extproc.ProcessingMode_SEND (attempt-request
	// dispatch must keep working alongside the new response phase) — follow
	// this package's existing pattern for unmarshaling/asserting on a
	// constructed HttpFilter's TypedConfig (see the equivalent assertion
	// already written for createUpstreamBodyExtProcFilter's own test, if
	// one exists, or createUpstreamRefreshExtProcFilter's).
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd gateway/gateway-controller && go test ./pkg/xds/... -run TestCreateUpstreamResponseObserverExtProcFilter -v`
Expected: FAIL — function undefined.

- [ ] **Step 3: Write the implementation, mirroring `createUpstreamBodyExtProcFilter`**

```go
// Add to gateway/gateway-controller/pkg/xds/translator.go, near createUpstreamBodyExtProcFilter:

// createUpstreamResponseObserverExtProcFilter is the response-observation
// counterpart to createUpstreamRefreshExtProcFilter/createUpstreamBodyExtProcFilter:
// it additionally requests the response-headers phase (ResponseHeaderMode:
// SEND) so a policy implementing UpstreamAttemptResponseObserver can see
// why a specific attempt failed, before Envoy decides whether to retry.
// Targets the same internal ext_proc cluster/server as the other two
// upstream filters — no new internal cluster is needed.
func (t *Translator) createUpstreamResponseObserverExtProcFilter() (*hcm.HttpFilter, error) {
	policyEngine := t.routerConfig.PolicyEngine
	extProcConfig := &extproc.ExternalProcessor{
		GrpcService: &core.GrpcService{
			TargetSpecifier: &core.GrpcService_EnvoyGrpc_{
				EnvoyGrpc: &core.GrpcService_EnvoyGrpc{ClusterName: constants.UpstreamRefreshPolicyEngineClusterName},
			},
			Timeout: durationpb.New(time.Duration(policyEngine.TimeoutMs) * time.Millisecond),
		},
		FailureModeAllow: true,
		ProcessingMode: &extproc.ProcessingMode{
			RequestHeaderMode:  extproc.ProcessingMode_SEND,
			ResponseHeaderMode: extproc.ProcessingMode_SEND,
		},
		MessageTimeout:    durationpb.New(time.Duration(policyEngine.MessageTimeoutMs) * time.Millisecond),
		RequestAttributes: []string{constants.ExtProcRequestAttributeRouteName},
	}
	extProcAny, err := anypb.New(extProcConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal upstream response-observer ext_proc config: %w", err)
	}
	return &hcm.HttpFilter{
		Name:       constants.UpstreamRefreshExtProcFilterName, // reuse the existing filter name constant — same internal server, different ProcessingMode
		ConfigType: &hcm.HttpFilter_TypedConfig{TypedConfig: extProcAny},
	}, nil
}

// collectClustersNeedingUpstreamResponseObserver marks every cluster
// backing a route whose policy chain includes at least one policy whose
// policy-definition.yaml declares x-wso2-upstream-response-observer: true.
// Declarative, not a Go type assertion — gateway-controller never
// instantiates policy Go code (see Task 6's resolveRetryDeclarations and
// the design's Design Revision 2 for why). A SEPARATE set from
// collectClustersNeedingUpstreamFilter/collectClustersNeedingUpstreamBodyFilter
// (a cluster must never receive two upstream ext_proc filters) — a route
// with no such policy pays zero cost, matching the existing capability-flag
// convention this codebase already uses for headers-only vs. body-buffering
// need. No policy in this plan (Tasks 10-11) actually sets this flag — it's
// infrastructure ready for a future consumer, consistent with Component 3
// of the design being optional/additive.
func collectClustersNeedingUpstreamResponseObserver(routes []*route.Route, chainForRoute func(routeKey string) *models.PolicyChain, policyDefinitions map[string]models.PolicyDefinition, dest map[string]bool) {
	for _, r := range routes {
		ra := r.GetRoute()
		if ra == nil {
			continue
		}
		routeKey := r.GetName() // or whichever field this codebase's routes already use to correlate back to a policy chain — match createRouteFromRDC's own convention for setting route.Name/deriving routeKey
		chain := chainForRoute(routeKey)
		if chain == nil {
			continue
		}
		needsObserver := false
		for _, p := range chain.Policies {
			def, ok := policyDefinitions[p.Name+"@"+p.Version] // match the SAME real key format resolveRetryDeclarations (Task 6, Step 6a) uses — confirm and keep both in sync
			if !ok {
				continue
			}
			if def.UpstreamResponseObserver {
				needsObserver = true
				break
			}
		}
		if needsObserver {
			if clusterName := ra.GetCluster(); clusterName != "" {
				dest[clusterName] = true
			}
		}
	}
}
```

Note on `chainForRoute`/`policyDefinitions`: `chainForRoute` must be the exact same lookup mechanism already used elsewhere in this file for however `collectClustersNeedingUpstreamFilter`'s caller already maps a route back to its policy chain — do not introduce a second, parallel way to do this. `policyDefinitions` must be the exact same registry field `resolveRetryDeclarations` (Task 6, Step 6a) reads from `*Translator` — both this function and that one must use the identical lookup-key format, confirmed once (Task 6, Step 6a) and reused here, not re-derived.

- [ ] **Step 4: Wire the new filter into cluster attachment, alongside the existing two**

Find the call site that invokes `attachUpstreamRefreshFilter` (headers-only) and its body-filter counterpart (search `attachUpstreamRefreshFilter(` and the equivalent call using `createUpstreamBodyExtProcFilter`). Add a third, parallel call: build `clustersNeedingUpstreamResponseObserver` via `collectClustersNeedingUpstreamResponseObserver` (Step 3) and attach `createUpstreamResponseObserverExtProcFilter`'s output to exactly those clusters, using the same `TypedExtensionProtocolOptions` attachment pattern `attachUpstreamRefreshFilter` already uses (Step 4 in that existing function) — do not attach to a cluster already marked in either of the other two sets; a cluster gets exactly one upstream ext_proc filter, whichever single `ProcessingMode` its route's actual capability needs require (if a route needs BOTH body-buffering and response-observation, that combination needs its own single filter construction with both fields set — flag this as a follow-up if Task 8's scope doesn't already require it based on real usage; do not silently attach two filters to one cluster).

- [ ] **Step 5: Run the full package tests**

Run: `cd gateway/gateway-controller && go test ./pkg/xds/... -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add gateway/gateway-controller/pkg/xds/
git commit -m "feat(gateway-controller): wire UpstreamAttemptResponseObserver capability into per-cluster ext_proc filter attachment"
```

---

## Task 9: gateway-runtime kernel — renames + response-phase dispatch

**Files:**
- Modify: `gateway/gateway-runtime/policy-engine/internal/kernel/upstream_extproc.go` (entire file — renames throughout, plus new dispatch)
- Test: `gateway/gateway-runtime/policy-engine/internal/kernel/upstream_extproc_test.go` (extend existing tests, add new ones)

**Interfaces:**
- Consumes: `policy.UpstreamAttemptResponseObserver`/`UpstreamAttemptResponseContext` (Task 4), renamed `policy.UpstreamAttemptRequestModifications`/`OnUpstreamAttemptRequest` (Task 3).
- Produces: `processUpstreamAttemptRequestHeaders` (renamed from `processRequestHeaders`), `processUpstreamAttemptRequestBody` (renamed from `processRequestBody`), new `processUpstreamAttemptResponse(ctx context.Context, resp *extprocv3.HttpResponse, routeKey string, attemptCount int) (*extprocv3.ProcessingResponse, error)`.

- [ ] **Step 1: Write the failing test for the new response dispatch**

```go
// Add to gateway/gateway-runtime/policy-engine/internal/kernel/upstream_extproc_test.go
package kernel

import (
	"context"
	"testing"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

type recordingResponseObserverPolicy struct {
	lastAttemptCount   int
	lastResponseStatus int
	lastRequestID      string
}

func (r *recordingResponseObserverPolicy) OnUpstreamAttemptResponse(ctx context.Context, actx *policy.UpstreamAttemptResponseContext) {
	r.lastAttemptCount = actx.AttemptCount
	r.lastResponseStatus = actx.ResponseStatus
	r.lastRequestID = actx.RequestID
}

func TestProcessUpstreamAttemptResponse_DispatchesToObserverPolicy(t *testing.T) {
	observer := &recordingResponseObserverPolicy{}
	k := newTestKernelWithPolicyChain(t, "test-route", []policy.Policy{ /* wrap observer to satisfy whatever base policy.Policy interface this test harness already requires — follow this file's existing test-kernel-construction helper exactly */ })
	s := NewUpstreamExternalProcessorServer(k)

	respHeaders := &extprocv3.HttpResponse{
		Response: &extprocv3.HttpResponse_ResponseHeaders{
			ResponseHeaders: &extprocv3.HeadersResponse{}, // populate :status=401 and x-request-id per this package's existing header-construction test helper
		},
	}
	_, err := s.processUpstreamAttemptResponse(context.Background(), respHeaders, "test-route", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if observer.lastAttemptCount != 1 {
		t.Errorf("lastAttemptCount = %d, want 1", observer.lastAttemptCount)
	}
	if observer.lastResponseStatus != 401 {
		t.Errorf("lastResponseStatus = %d, want 401", observer.lastResponseStatus)
	}
}
```

Note: fill in the `:status`/`x-request-id` header construction and the `newTestKernelWithPolicyChain` call using this file's *existing* test helpers for constructing a minimal kernel + policy chain — grep `upstream_extproc_test.go`'s current tests (the ones covering `processRequestHeaders`/`processRequestBody` today) for the established pattern and mirror it exactly rather than inventing a new harness.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd gateway/gateway-runtime/policy-engine && go test ./internal/kernel/... -run TestProcessUpstreamAttemptResponse -v`
Expected: FAIL — `processUpstreamAttemptResponse` undefined.

- [ ] **Step 3: Rename `processRequestHeaders` → `processUpstreamAttemptRequestHeaders`, `processRequestBody` → `processUpstreamAttemptRequestBody`**

In `upstream_extproc.go`, rename both functions (lines 134 and 199 per the current file) and update `UpstreamAttemptHeaderModifications`/`OnUpstreamAttemptRequestHeaders` references inside them to the Task 3 names:

```go
// processUpstreamAttemptRequestHeaders (renamed from processRequestHeaders) —
// resolves the route's policy chain and dispatches to every policy
// implementing UpstreamAttemptPolicy, in chain order.
func (s *UpstreamExternalProcessorServer) processUpstreamAttemptRequestHeaders(ctx context.Context, req *extprocv3.ProcessingRequest, routeKey string) (*extprocv3.ProcessingResponse, int, error) {
	// ... body unchanged from today's processRequestHeaders, except:
	// action := attemptPolicy.OnUpstreamAttemptRequest(ctx, actx)
	// mods, ok := action.(policy.UpstreamAttemptRequestModifications)
}
```

```go
// processUpstreamAttemptRequestBody (renamed from processRequestBody) —
// dispatches the SAME OnUpstreamAttemptRequest hook again, this time with
// Body populated.
func (s *UpstreamExternalProcessorServer) processUpstreamAttemptRequestBody(ctx context.Context, body *extprocv3.HttpBody, routeKey string, attemptCount int) (*extprocv3.ProcessingResponse, error) {
	// ... body unchanged from today's processRequestBody, except:
	// action := attemptPolicy.OnUpstreamAttemptRequest(ctx, actx)
	// mods, ok := action.(policy.UpstreamAttemptRequestModifications)
}
```

Update the two call sites in `Process()` (lines 83 and 89) to the new function names.

- [ ] **Step 4: Add `processUpstreamAttemptResponse`**

```go
// Add to upstream_extproc.go:

// emptyContinueResponseHeadersResponse is the fail-open / no-op response
// for the response-observation phase: no mutation is ever possible here
// (UpstreamAttemptResponseObserver is read-only), so this is the only
// response this handler ever returns — it exists for symmetry with the
// other two phases' own empty-continue helpers and to keep Process()'s
// switch uniform.
func emptyContinueResponseHeadersResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{},
			},
		},
	}
}

// processUpstreamAttemptResponse dispatches to every policy in the route's
// chain implementing UpstreamAttemptResponseObserver, in chain order.
// Read-only: unlike the request-phase handlers, there is no action to
// apply back to Envoy — every policy just observes. attemptCount is the
// value already parsed from this same attempt's request-headers message
// (see Process()'s own attemptCount variable), reused here since Envoy
// does not repeat x-envoy-attempt-count on the response.
func (s *UpstreamExternalProcessorServer) processUpstreamAttemptResponse(ctx context.Context, resp *extprocv3.HttpResponse, routeKey string, attemptCount int) (*extprocv3.ProcessingResponse, error) {
	chain := s.kernel.GetPolicyChain(routeKey)
	if chain == nil {
		return emptyContinueResponseHeadersResponse(), nil
	}

	headers := resp.GetResponseHeaders().GetHeaders()
	status := 0
	requestID := ""
	if headers != nil {
		for _, h := range headers.GetHeaders() {
			switch h.Key {
			case ":status":
				if n, err := strconv.Atoi(string(h.RawValue)); err == nil {
					status = n
				}
			case "x-request-id":
				requestID = string(h.RawValue)
			}
		}
	}

	actx := &policy.UpstreamAttemptResponseContext{
		AttemptCount:   attemptCount,
		RequestID:      requestID,
		ResponseStatus: status,
	}

	for _, p := range chain.Policies {
		observer, ok := p.(policy.UpstreamAttemptResponseObserver)
		if !ok {
			continue
		}
		observer.OnUpstreamAttemptResponse(ctx, actx)
	}

	return emptyContinueResponseHeadersResponse(), nil
}
```

- [ ] **Step 5: Add the new message case to `Process()`**

```go
// In Process()'s switch statement, add a third case alongside the existing
// RequestHeaders/RequestBody cases:
case *extprocv3.ProcessingRequest_ResponseHeaders:
	resp, err = s.processUpstreamAttemptResponse(ctx, &extprocv3.HttpResponse{Response: &extprocv3.HttpResponse_ResponseHeaders{ResponseHeaders: v.ResponseHeaders}}, routeKey, attemptCount)
	if err != nil {
		slog.ErrorContext(ctx, "upstream ext_proc: failed to process response headers, failing open", "error", err)
		resp = emptyContinueResponseHeadersResponse()
	}
```

Verify the exact `ProcessingRequest` oneof field name Envoy's ext_proc proto uses for a response-headers message (`extprocv3.ProcessingRequest_ResponseHeaders`, wrapping `*extprocv3.HttpHeaders`, not `*extprocv3.HttpResponse` — check `envoy/service/ext_proc/v3/external_processor.pb.go`'s actual `ProcessingRequest` oneof before writing this case; the request-headers case at line 81 uses `*extprocv3.ProcessingRequest_RequestHeaders` wrapping `*extprocv3.HttpHeaders` for the *request* direction — the *response* direction's oneof variant and payload type must be confirmed from the same generated file rather than assumed symmetric.) Adjust `processUpstreamAttemptResponse`'s signature/field access in Step 4 to match whatever the real generated type is.

- [ ] **Step 6: Update `Process()`'s doc comment**

The comment at lines 33-44 says "Two message types are possible now" — update to reflect the third (response-headers) case now handled, and that `ResponseHeaderMode: SEND` is what causes Envoy to emit it, opt-in per cluster via Task 8's `collectClustersNeedingUpstreamResponseObserver`.

- [ ] **Step 7: Run test to verify it passes**

Run: `cd gateway/gateway-runtime/policy-engine && go test ./internal/kernel/... -v`
Expected: PASS, entire package (including every existing test for the renamed functions, updated to call the new names).

- [ ] **Step 8: Commit**

```bash
git add gateway/gateway-runtime/policy-engine/internal/kernel/
git commit -m "refactor(gateway-runtime): rename upstream ext_proc request handlers, add response-phase dispatch for UpstreamAttemptResponseObserver"
```

---

## Task 10: `gateway/dev-policies/oauth2-generator` — rename + declarative retry-trigger metadata

**Files:**
- Modify: `gateway/dev-policies/oauth2-generator/oauth2_generator.go:958-987` (rename `OnUpstreamAttemptRequestHeaders`)
- Modify: `gateway/dev-policies/oauth2-generator/policy-definition.yaml` (add `x-wso2-retry-trigger` top-level metadata — **not** a Go method; `gateway-controller` reads this declaratively, see Task 5/6)
- Test: `gateway/dev-policies/oauth2-generator/oauth2_generator_test.go` (extend, for the rename only)

**Interfaces:**
- Produces: `x-wso2-retry-trigger: {statusCodesField: tokenPurgeStatusCodes, minAttempts: 2}` in `policy-definition.yaml`. **No `DeclareRetryTrigger` Go method** — `gateway-controller` never instantiates this policy's Go code (Design Revision 2), so nothing would ever call it; the declaration is entirely the YAML metadata plus `gateway-controller`'s own generic `ParseRetryTriggerParams` (Task 5) reading this policy's existing `tokenPurgeStatusCodes` param directly.

- [ ] **Step 1: Confirm `tokenPurgeStatusCodes`'s default is materialized into `params` before `gateway-controller` reads it — a real risk specific to reading raw params generically**

`ParseRetryTriggerParams` (Task 5) reads `params[statusCodesField]` straight off the AS-DEPLOYED params map — not off `p.purgeStatusCodes` (the already-defaulted struct field `GetPolicy` produces today). If a user's YAML omits `tokenPurgeStatusCodes` entirely (relying on the schema's `default: [401]`), the generic parser only sees the schema default correctly if `gateway-controller`'s existing schema-coercion step (`pkg/config/policy_validator.go`'s `coerceParamsBySchema`/`coerceSinglePolicyParams`, confirmed to exist in that file) actually writes defaults INTO the params map at registration/coercion time, before `ParseRetryTriggerParams` ever runs.

Run: `grep -n "coerceScalarByType\|default" gateway/gateway-controller/pkg/config/policy_validator.go | head -20` and read `coerceParamsBySchema` (`policy_validator.go:375-414`) directly to confirm it populates a schema-declared `default` value into an omitted param key. If it does — this task needs no extra handling, defaults flow through automatically, matching today's behavior with zero extra code. If it does NOT — `ParseRetryTriggerParams` (Task 5) must be revisited to accept the policy's schema defaults too (read `def.Parameters`'s own `properties.<statusCodesField>.default`, not just `params[statusCodesField]`) before this task can be considered correct; flag this explicitly rather than silently shipping a behavior change (oauth2-generator's default purge-on-401 silently stopping from contributing to route-level retry for anyone relying on the omitted-field default).

- [ ] **Step 2: Add the declarative metadata**

```yaml
# gateway/dev-policies/oauth2-generator/policy-definition.yaml — add as a new
# top-level field, sibling to `name`/`version`/`parameters`:
x-wso2-retry-trigger:
  statusCodesField: tokenPurgeStatusCodes
  minAttempts: 2
```

No Go code change accompanies this step — it's pure YAML, read by `gateway-controller`'s `resolveRetryDeclarations` (Task 6, Step 6a) the same way it already loads every other policy-definition.yaml field.

- [ ] **Step 3: Rename `OnUpstreamAttemptRequestHeaders` (lines 958-987)**

```go
// BEFORE:
func (p *Policy) OnUpstreamAttemptRequestHeaders(ctx context.Context, actx *policy.UpstreamAttemptContext) policy.UpstreamAttemptAction {
	if actx.AttemptCount > 1 {
		p.tokenSource.Purge()
	}
	tok, err := p.retrieveToken()
	if err != nil {
		slog.WarnContext(ctx, "OAuth2Generator: failed to fetch token for upstream attempt, failing open (no header mutation)",
			"attempt", actx.AttemptCount, "grantType", p.grantType, "clientId", p.clientID, "error", err)
		return policy.UpstreamAttemptHeaderModifications{}
	}
	return policy.UpstreamAttemptHeaderModifications{
		HeadersToSet: map[string]string{
			p.headerName: buildHeaderValue(p.valuePrefix, tok.AccessToken),
		},
	}
}
```

```go
// AFTER — same logic, renamed method + renamed return type per Task 3:
func (p *Policy) OnUpstreamAttemptRequest(ctx context.Context, actx *policy.UpstreamAttemptContext) policy.UpstreamAttemptAction {
	if actx.AttemptCount > 1 {
		p.tokenSource.Purge()
	}
	tok, err := p.retrieveToken()
	if err != nil {
		slog.WarnContext(ctx, "OAuth2Generator: failed to fetch token for upstream attempt, failing open (no header mutation)",
			"attempt", actx.AttemptCount, "grantType", p.grantType, "clientId", p.clientID, "error", err)
		return policy.UpstreamAttemptRequestModifications{}
	}
	return policy.UpstreamAttemptRequestModifications{
		HeadersToSet: map[string]string{
			p.headerName: buildHeaderValue(p.valuePrefix, tok.AccessToken),
		},
	}
}
```

- [ ] **Step 4: Update every other reference to the renamed hook in this file**

Run: `grep -n "OnUpstreamAttemptRequestHeaders\|UpstreamAttemptHeaderModifications" gateway/dev-policies/oauth2-generator/oauth2_generator.go`

Update the doc comment immediately above the method (lines 958-969, which currently says "OnUpstreamAttemptRequestHeaders implements policy.UpstreamAttemptPolicy") to reference the new method name.

- [ ] **Step 5: Run the full package tests**

Run: `cd gateway/dev-policies/oauth2-generator && GOWORK=off go test ./... -v`
Expected: PASS, entire package — the rename must not break any existing test, and no new test is needed for the YAML-only Step 2 change (there's no Go code path to unit test; Task 12's end-to-end verification exercises it against a real `gateway-controller`).

- [ ] **Step 6: Commit**

```bash
git add gateway/dev-policies/oauth2-generator/
git commit -m "feat(oauth2-generator): declare x-wso2-retry-trigger so refresh-then-retry works without a retry-source policy present"
```

---

## Task 11: `gateway/dev-policies/model-failover` — declarative `x-wso2-retry-source`, canonical naming, rename

**Files:**
- Modify: `gateway/dev-policies/model-failover/policy-definition.yaml` (add `x-wso2-retry-source` top-level metadata — **not** a Go method; see Task 5/6)
- Modify: `gateway/dev-policies/model-failover/model_failover.go:211-227` (`modelFailoverGroupUpstreamName` — remove, replaced by SDK's `policy.RetrySourceUpstreamName`)
- Modify: `gateway/dev-policies/model-failover/model_failover.go:391` and `:442` (call sites using the removed local function / the renamed hook)
- Test: `gateway/dev-policies/model-failover/model_failover_test.go` (extend, for the naming-formula/rename changes only)

**Interfaces:**
- Consumes: `policy.RetrySourceUpstreamName` (Task 1), renamed `policy.UpstreamAttemptRequestModifications`/`OnUpstreamAttemptRequest` (Task 3).
- Produces: `x-wso2-retry-source: {groupKeyField: model}` in `policy-definition.yaml`. **No `DeclareRetrySource` Go method** — same reasoning as Task 10: `gateway-controller` never instantiates this policy's Go code, so nothing would call it. `gateway-controller`'s generic `ParseRetrySourceParams` (Task 5) reads this policy's existing `targets`/`fallbacks`/`statusCodes` params directly — all three are already `required` fields in this policy's own `policy-definition.yaml` schema (confirmed: no schema-default-materialization risk here, unlike Task 10's optional `tokenPurgeStatusCodes` — nothing to verify before this task is safe to ship).

- [ ] **Step 1: Add the declarative metadata**

```yaml
# gateway/dev-policies/model-failover/policy-definition.yaml — add as a new
# top-level field, sibling to `name`/`version`/`parameters`:
x-wso2-retry-source:
  groupKeyField: model
```

`groupKeyField: model` tells `gateway-controller`'s generic parser to read each `targets[]` entry's `model` field as the group's discriminator — matching exactly what this policy's own runtime code (`groupByModel`, `OnRequestBody`) already keys on. No Go code accompanies this step.

- [ ] **Step 2: Remove `modelFailoverGroupUpstreamName` (lines 211-227), replace call sites with `policy.RetrySourceUpstreamName`**

```go
// DELETE lines 211-227 (modelFailoverGroupUpstreamName) entirely — its
// formula now lives in policy.RetrySourceUpstreamName (Task 1), imported
// from the SDK this file already depends on.
```

At line 391 (inside `OnRequestBody`):
```go
// BEFORE:
name := modelFailoverGroupUpstreamName(p.routeName, group.model)
mods.UpstreamName = &name
```
```go
// AFTER:
name := policy.RetrySourceUpstreamName(p.routeName, group.model)
mods.UpstreamName = &name
```

- [ ] **Step 3: Rename `OnUpstreamAttemptRequestHeaders` (line 442) and its return-type references**

Apply the same rename as Task 10 Step 3: method name `OnUpstreamAttemptRequestHeaders` → `OnUpstreamAttemptRequest`, every `policy.UpstreamAttemptHeaderModifications{...}` return value in this method (lines 444, 450, 458, 462, 469, 472 per the read source) → `policy.UpstreamAttemptRequestModifications{...}`. Update the method's doc comment (lines 417-441) to reference the new name.

**No `DeclareRetrySource` Go method is added in this task** — Step 1 (the `x-wso2-retry-source` YAML metadata) plus `gateway-controller`'s own generic `ParseRetrySourceParams` (Task 5, reading this policy's existing `targets`/`fallbacks`/`statusCodes` params directly) is the complete mechanism. There is no Go-level equivalent to write or test here.

- [ ] **Step 4: Run the full package tests**

Run: `cd gateway/dev-policies/model-failover && GOWORK=off go test ./... -v`
Expected: PASS, entire package — including every existing test exercising `OnUpstreamAttemptRequestHeaders`/`modelFailoverGroupUpstreamName`, updated to the new names in this task (Steps 2-3).

- [ ] **Step 5: Commit**

```bash
git add gateway/dev-policies/model-failover/
git commit -m "feat(model-failover): declare x-wso2-retry-source; adopt SDK's canonical RetrySourceUpstreamName instead of a duplicated local formula"
```

---

## Task 12: End-to-end verification

**Files:** none modified — this task only runs existing test suites.

- [ ] **Step 1: Run every existing model-failover IT feature unmodified**

Run: `cd gateway/it && IT_FEATURE_PATHS=features/model-*.feature make test`
Expected: PASS — every scenario, with zero changes to the `.feature` files themselves. This is the proof that Tasks 5-9 and 11 produced no behavior change for the shipped policy.

- [ ] **Step 2: Manually verify the oauth2-generator-alone case from the design's motivating example**

Deploy an `LlmProvider` with `oauth2-generator` attached and NO `model-failover`, NO `resilience.retry` (mirroring the YAML example already discussed in this conversation). Force a `401` from the backend (or point `tokenEndpoint` at credentials that will be rejected once, then succeed). Confirm via the gateway-runtime `[rtr]` access log that `x-envoy-attempt-count` reaches 2 for that request, and the client sees a final `200` — proving `oauth2-generator`'s declared `x-wso2-retry-trigger` (Task 10) plus `gateway-controller`'s generic parser (Task 5/6) produced a real, working, plain `RouteAction.RetryPolicy` with no retry-source declaration present at all.

- [ ] **Step 3: Manually verify the combined case**

Deploy the same provider with BOTH `oauth2-generator` and `model-failover` attached (mirroring the earlier combined YAML example). Confirm via Envoy's `:9901/config_dump` that exactly ONE `RouteAction.RetryPolicy` exists for that route, with `RetriableStatusCodes` containing both model-failover's configured codes AND oauth2-generator's `tokenPurgeStatusCodes` — proving the composition rule (Task 6, Step 6) actually merges rather than picking one side.

- [ ] **Step 4: Confirm no `p.Name == "model-failover"` string checks, and no dead `RetrySourcePolicy`/`RetryTriggerPolicy` interface references, remain**

Run: `grep -rn '"model-failover"' gateway/gateway-controller/pkg/xds/translator.go gateway/gateway-controller/pkg/config/retry_source_validator.go`
Expected: no output (or only in comments/doc references, not in an executable `if`/`switch` condition) — confirms Global Constraint "no policy-name string check may remain."

Run: `grep -rn "RetrySourcePolicy\|RetryTriggerPolicy\b" sdk/core/ gateway/gateway-controller/`
Expected: no output — these Go interfaces were deliberately removed (Design Revision 2); their presence anywhere would mean a stray, uncallable artifact from an earlier version of this plan slipped through.

- [ ] **Step 5: Final commit (if any fixes were needed during verification)**

```bash
git add -A
git commit -m "test: verify decoupled retry-source design end-to-end"
```

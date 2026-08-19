# Generalized Retry Conditions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the narrow `x-wso2-retry-trigger`/`x-wso2-retry-source` status-code-only declaration shape with one shared `RetryConditions` vocabulary — mirroring Envoy's real `RouteAction.RetryPolicy` fields — usable by both a policy's declared metadata and the operator-facing `resilience.retry`, with explicit field-level composition rules across every contributor on a route.

**Architecture:** A new SDK type (`RetryConditions`) is the shared vocabulary. gateway-controller gains one generic resolver (`ParseRetryConditions`, literal-or-`{fromParam}`) and one generic merger (`MergeRetryConditions`, field-by-field union/floor/single-owner rules) that both a retry-source policy's declaration and the operator's `resilience.retry` funnel through — replacing four separate, duplicated `build*RetryPolicy` functions with one (`buildRoutePolicyFromConditions`). Nothing is deployed yet, so this is a clean, direct cutover — no backward-compatibility shims, no staged coexistence with the old path. The old `x-wso2-retry-trigger` shape and its supporting code are deleted in the same wave that builds the replacement, and `model-failover` (the only real consumer) migrates in the same pass.

**Tech Stack:** Go, `sdk/core/policy/v1alpha2`, `gateway-controller/pkg/config`, `gateway-controller/pkg/xds`, `envoyproxy/go-control-plane` (vendored `route.RetryPolicy` proto), OpenAPI (`oapi-codegen`).

**Spec:** `docs/superpowers/specs/2026-08-17-generalized-retry-conditions-design.md`

## Global Constraints

- gateway-controller must never execute policy Go code to discover a declaration — only structural YAML/param parsing (existing invariant, unchanged).
- Every validation failure fails closed (reject at registration/translation time), never silently picks a value on conflict.
- `PerTryTimeout` composition: tightest (minimum) wins across contributors — never widened.
- `MinAttempts`/floor-style fields: only ever raised (max), never lowered.
- `NumRetries` (exact) and `BackOff`: at most one contributor may set each; two *different* explicit values is a hard registration-time error (identical values are fine for `NumRetries`, but `BackOff` rejects even identical values — ownership ambiguity, not the value, is the problem).
- Canonical policy source of truth is `gateway-controllers/policies/<name>/` (sibling repo, absolute path `/Users/thenujan/Desktop/Git-Repos/gateway-controllers`) — edit there first, build/vet/test, then mirror byte-for-byte into `api-platform/gateway/dev-policies/<name>/` (gitignored — never `git add` inside `api-platform`).
- `sdk/core` changes land in `api-platform/sdk/core` — the canonical `gateway-controllers` repo's `go.mod` has a local `replace` directive to that checkout already; do not add a new one.
- No backward-compatibility shims anywhere in this plan — nothing is deployed yet. Old code is deleted the moment its replacement exists and compiles, not kept "for later."

---

## Task 1: `RetryConditions` SDK types, replacing `RetryTriggerDeclaration`

**Files:**
- Create: `sdk/core/policy/v1alpha2/retry_conditions.go`
- Modify: `sdk/core/policy/v1alpha2/retry_source.go` (`RetrySourceDeclaration`)
- Delete: `sdk/core/policy/v1alpha2/retry_trigger.go` (and its content — `RetryTriggerDeclaration` is fully superseded, not kept alongside)

**Interfaces:**
- Produces: `policy.RetryConditions`, `policy.RetriableHeader`, `policy.RetryBackOff` — the shared vocabulary every later task builds on.

Pure type definitions with no behavior to assert — no red/green cycle; verified by compilation only (this mirrors how `RetryTriggerDeclaration`/`RetrySourceDeclaration` today have no dedicated unit test file — exercised indirectly through their consumers, exactly what Tasks 2–4 do here).

- [ ] **Step 1: Write the types**

```go
// sdk/core/policy/v1alpha2/retry_conditions.go
/*
 *  Copyright (c) 2026, WSO2 LLC. (http://www.wso2.com) All Rights Reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 */

package policyv1alpha2

import "time"

// RetryConditions is the shared vocabulary a contributor — an operator's
// resilience.retry or a retry-source policy — uses to describe what it
// wants from Envoy's RouteAction.RetryPolicy. Multiple contributors on one
// route compose into a single RetryConditions via field-by-field merge
// rules (gateway-controller's MergeRetryConditions) — see
// docs/superpowers/specs/2026-08-17-generalized-retry-conditions-design.md.
type RetryConditions struct {
	// On lists Envoy RetryOn conditions this contributor wants active:
	// "5xx", "gateway-error", "reset", "connect-failure", "refused-stream",
	// "envoy-ratelimited", "retriable-4xx", "retriable-status-codes",
	// "retriable-headers". Composes by union across contributors.
	On []string

	// StatusCodes are retriable response status codes, used when On
	// includes "retriable-status-codes" (implied automatically if
	// StatusCodes is non-empty and On doesn't already list it). Composes
	// by union.
	StatusCodes []int

	// Headers are retriable response header matchers, used when On
	// includes "retriable-headers" (implied automatically if Headers is
	// non-empty). Composes by union.
	Headers []RetriableHeader

	// NumRetries is an exact retry-count request. At most one DISTINCT
	// value may be set across all contributors on a route — a second
	// contributor setting a different explicit value is a
	// registration-time conflict, never silently resolved. Identical
	// values from multiple contributors are fine.
	NumRetries *int

	// MinAttempts is "I need at least N total attempts to get value from
	// retrying" — distinct from NumRetries. Composes as a floor: only ever
	// raised (max across contributors), never lowered.
	MinAttempts *int

	// PerTryTimeout bounds a single attempt. Composes as a ceiling: only
	// ever tightened (min across contributors), never widened.
	PerTryTimeout *time.Duration

	// BackOff configures retry pacing. At most one contributor on a route
	// may set this — a second contributor also setting it is a
	// registration-time conflict, even with identical values.
	BackOff *RetryBackOff

	// AvoidPreviousHosts maps to Envoy's "previous_hosts" RetryHostPredicate.
	// Composes by OR: any contributor wanting the safer behavior turns it
	// on for every contributor on the route.
	AvoidPreviousHosts bool
}

// RetriableHeader is one retriable-response-header matcher. Exact-match
// only for now — extend to regex/presence-only matching if a real consumer
// needs it; not speculatively built ahead of a need.
type RetriableHeader struct {
	Name  string
	Exact string
}

// RetryBackOff configures Envoy's retry pacing between attempts.
type RetryBackOff struct {
	BaseInterval time.Duration
	MaxInterval  *time.Duration
}
```

- [ ] **Step 2: Embed `RetryConditions` into `RetrySourceDeclaration`, replacing the bare status-code list**

In `sdk/core/policy/v1alpha2/retry_source.go`, change:

```go
type RetrySourceDeclaration struct {
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
```

to:

```go
type RetrySourceDeclaration struct {
	Groups []RetryGroup

	// PerAttemptTimeout bounds a single attempt; nil uses the route's
	// existing default. A retry-source policy's own status-code/On/etc.
	// contribution is NOT a field here — it flows through the exact same
	// x-wso2-retry-conditions declaration path every other policy uses
	// (see gateway-controller's resolveRetryDeclarations, which parses a
	// chain member's retry-source metadata and its retry-conditions
	// metadata independently, in the same loop pass). A dedicated
	// Conditions field on this type would just be a second, redundant path
	// to the same merged result.
	PerAttemptTimeout *time.Duration
}
```

- [ ] **Step 3: Delete `retry_trigger.go`**

```bash
rm /Users/thenujan/Desktop/Git-Repos/api-platform/sdk/core/policy/v1alpha2/retry_trigger.go
```

- [ ] **Step 4: Verify compilation**

Run: `cd sdk/core && go build ./...`
Expected: succeeds. (This will NOT yet compile the whole monorepo — `gateway-controller`'s references to `RetrySourceDeclaration.RetriableStatusCodes` and `RetryTriggerDeclaration` will now fail. Expected and fixed in Task 6; this task only needs `sdk/core` itself to build.)

- [ ] **Step 5: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform
git add sdk/core/policy/v1alpha2/retry_conditions.go sdk/core/policy/v1alpha2/retry_source.go
git rm sdk/core/policy/v1alpha2/retry_trigger.go
git commit -m "sdk: replace RetryTriggerDeclaration with the shared RetryConditions vocabulary"
```

---

## Task 2: `MergeRetryConditions` — union and floor composition rules

**Files:**
- Create: `gateway-controller/pkg/config/retry_conditions_merge.go`
- Test: `gateway-controller/pkg/config/retry_conditions_merge_test.go`

**Interfaces:**
- Consumes: `policy.RetryConditions` (Task 1).
- Produces: `MergeRetryConditions(contributions []policy.RetryConditions) (policy.RetryConditions, error)` — exported from the start (nothing is staged, so no reason to build it unexported first); Task 3 extends this same function with the reject-on-conflict rules; Task 6 calls it from `pkg/xds`.

- [ ] **Step 1: Write the failing test for union fields**

```go
// gateway-controller/pkg/config/retry_conditions_merge_test.go
package config

import (
	"testing"
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestMergeRetryConditions_UnionFields(t *testing.T) {
	contributions := []policy.RetryConditions{
		{On: []string{"retriable-status-codes"}, StatusCodes: []int{401}},
		{On: []string{"reset"}, StatusCodes: []int{403, 401}}, // 401 duplicated on purpose
		{Headers: []policy.RetriableHeader{{Name: "x-retry-me", Exact: "true"}}},
	}

	merged, err := MergeRetryConditions(contributions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantOn := map[string]bool{"retriable-status-codes": true, "reset": true, "retriable-headers": true}
	if len(merged.On) != len(wantOn) {
		t.Fatalf("expected On %v, got %v", wantOn, merged.On)
	}
	for _, on := range merged.On {
		if !wantOn[on] {
			t.Errorf("unexpected On condition %q", on)
		}
	}

	wantCodes := map[int]bool{401: true, 403: true}
	if len(merged.StatusCodes) != len(wantCodes) {
		t.Fatalf("expected deduplicated StatusCodes %v, got %v", wantCodes, merged.StatusCodes)
	}
	for _, code := range merged.StatusCodes {
		if !wantCodes[code] {
			t.Errorf("unexpected status code %d", code)
		}
	}

	if len(merged.Headers) != 1 || merged.Headers[0].Name != "x-retry-me" {
		t.Fatalf("expected the one declared header to survive, got %v", merged.Headers)
	}
}

func TestMergeRetryConditions_StatusCodesImpliesRetriableStatusCodesOn(t *testing.T) {
	merged, err := MergeRetryConditions([]policy.RetryConditions{{StatusCodes: []int{401}}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(merged.On) != 1 || merged.On[0] != "retriable-status-codes" {
		t.Errorf("expected StatusCodes to imply On=[retriable-status-codes], got %v", merged.On)
	}
}

func TestMergeRetryConditions_HeadersImpliesRetriableHeadersOn(t *testing.T) {
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{Headers: []policy.RetriableHeader{{Name: "x-retry-me", Exact: "true"}}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(merged.On) != 1 || merged.On[0] != "retriable-headers" {
		t.Errorf("expected Headers to imply On=[retriable-headers], got %v", merged.On)
	}
}

func TestMergeRetryConditions_AvoidPreviousHosts_OR(t *testing.T) {
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{AvoidPreviousHosts: false},
		{AvoidPreviousHosts: true},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !merged.AvoidPreviousHosts {
		t.Error("expected AvoidPreviousHosts to be true when any contributor sets it")
	}
}

func TestMergeRetryConditions_MinAttempts_MaxFloor(t *testing.T) {
	two, three := 2, 3
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{MinAttempts: &two},
		{MinAttempts: &three},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.MinAttempts == nil || *merged.MinAttempts != 3 {
		t.Errorf("expected MinAttempts to be raised to the max (3), got %v", merged.MinAttempts)
	}
}

func TestMergeRetryConditions_PerTryTimeout_MinCeiling(t *testing.T) {
	fiveSec, tenSec := 5*time.Second, 10*time.Second
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{PerTryTimeout: &tenSec},
		{PerTryTimeout: &fiveSec},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.PerTryTimeout == nil || *merged.PerTryTimeout != fiveSec {
		t.Errorf("expected PerTryTimeout tightened to the min (5s), got %v", merged.PerTryTimeout)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd gateway-controller && go test ./pkg/config/... -run TestMergeRetryConditions -v`
Expected: FAIL — `MergeRetryConditions` undefined.

- [ ] **Step 3: Write the implementation**

```go
// gateway-controller/pkg/config/retry_conditions_merge.go
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

package config

import (
	"fmt"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// MergeRetryConditions combines every contributor's RetryConditions on one
// route into a single merged value, per the composition table in
// docs/superpowers/specs/2026-08-17-generalized-retry-conditions-design.md
// Component 2. Field-by-field: On/StatusCodes/Headers/AvoidPreviousHosts
// union safely (see this function); NumRetries/BackOff reject on conflict
// (see mergeSingleOwnerFields in the next task).
func MergeRetryConditions(contributions []policy.RetryConditions) (policy.RetryConditions, error) {
	var merged policy.RetryConditions

	onSet := map[string]struct{}{}
	codeSet := map[int]struct{}{}
	headerSet := map[string]policy.RetriableHeader{}

	for _, c := range contributions {
		for _, on := range c.On {
			onSet[on] = struct{}{}
		}
		for _, code := range c.StatusCodes {
			codeSet[code] = struct{}{}
		}
		for _, h := range c.Headers {
			headerSet[h.Name+"\x00"+h.Exact] = h
		}
		if c.AvoidPreviousHosts {
			merged.AvoidPreviousHosts = true
		}
		if c.MinAttempts != nil && (merged.MinAttempts == nil || *c.MinAttempts > *merged.MinAttempts) {
			v := *c.MinAttempts
			merged.MinAttempts = &v
		}
		if c.PerTryTimeout != nil && (merged.PerTryTimeout == nil || *c.PerTryTimeout < *merged.PerTryTimeout) {
			v := *c.PerTryTimeout
			merged.PerTryTimeout = &v
		}
	}

	for code := range codeSet {
		merged.StatusCodes = append(merged.StatusCodes, code)
	}
	for _, h := range headerSet {
		merged.Headers = append(merged.Headers, h)
	}
	if len(merged.StatusCodes) > 0 {
		onSet["retriable-status-codes"] = struct{}{}
	}
	if len(merged.Headers) > 0 {
		onSet["retriable-headers"] = struct{}{}
	}
	for on := range onSet {
		merged.On = append(merged.On, on)
	}

	numRetries, backOff, err := mergeSingleOwnerFields(contributions)
	if err != nil {
		return policy.RetryConditions{}, err
	}
	merged.NumRetries = numRetries
	merged.BackOff = backOff
	if merged.NumRetries == nil && merged.MinAttempts != nil {
		derived := *merged.MinAttempts - 1
		merged.NumRetries = &derived
	}

	return merged, nil
}

// mergeSingleOwnerFields is implemented in the next task (Task 3). Declared
// here so this file compiles standalone during Task 2's red/green cycle —
// Task 3 replaces this stub with the real conflict-detecting implementation.
func mergeSingleOwnerFields(contributions []policy.RetryConditions) (*int, *policy.RetryBackOff, error) {
	var numRetries *int
	var backOff *policy.RetryBackOff
	for _, c := range contributions {
		if c.NumRetries != nil {
			numRetries = c.NumRetries
		}
		if c.BackOff != nil {
			backOff = c.BackOff
		}
	}
	return numRetries, backOff, nil
}

var _ = fmt.Sprintf // placeholder import use, removed once Task 3 adds real fmt.Errorf usage
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd gateway-controller && go test ./pkg/config/... -run TestMergeRetryConditions -v`
Expected: PASS for all five tests.

- [ ] **Step 5: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform
git add gateway-controller/pkg/config/retry_conditions_merge.go gateway-controller/pkg/config/retry_conditions_merge_test.go
git commit -m "gateway-controller: add MergeRetryConditions union/floor composition rules"
```

---

## Task 3: `MergeRetryConditions` — single-owner conflict rules and `NumRetries` derivation

**Files:**
- Modify: `gateway-controller/pkg/config/retry_conditions_merge.go` (replace the Task 2 stub)
- Modify: `gateway-controller/pkg/config/retry_conditions_merge_test.go`

**Interfaces:**
- Consumes: `policy.RetryConditions` (Task 1); the `MergeRetryConditions` scaffold from Task 2.
- Produces: `MergeRetryConditions` fully implemented — no new exported names beyond what Task 2 already produced.

- [ ] **Step 1: Write the failing tests**

```go
// append to gateway-controller/pkg/config/retry_conditions_merge_test.go

func TestMergeRetryConditions_NumRetries_SingleContributorWins(t *testing.T) {
	five := 5
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{StatusCodes: []int{401}},
		{NumRetries: &five},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.NumRetries == nil || *merged.NumRetries != 5 {
		t.Errorf("expected the one explicit NumRetries (5) to win, got %v", merged.NumRetries)
	}
}

func TestMergeRetryConditions_NumRetries_ConflictingValuesRejected(t *testing.T) {
	two, five := 2, 5
	_, err := MergeRetryConditions([]policy.RetryConditions{
		{NumRetries: &two},
		{NumRetries: &five},
	})
	if err == nil {
		t.Fatal("expected an error for two contributors declaring different exact NumRetries")
	}
	if !strings.Contains(err.Error(), "NumRetries") {
		t.Errorf("expected error to mention NumRetries, got: %v", err)
	}
}

func TestMergeRetryConditions_NumRetries_IdenticalValuesStillAllowed(t *testing.T) {
	// Two contributors independently asking for the SAME exact count is not
	// an ownership conflict — nothing is ambiguous about what the route
	// should do. Unlike BackOff (see below), value equality here is a
	// legitimate way to avoid a spurious rejection.
	three := 3
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{NumRetries: &three},
		{NumRetries: &three},
	})
	if err != nil {
		t.Fatalf("unexpected error for two contributors agreeing on NumRetries: %v", err)
	}
	if merged.NumRetries == nil || *merged.NumRetries != 3 {
		t.Errorf("expected NumRetries 3, got %v", merged.NumRetries)
	}
}

func TestMergeRetryConditions_BackOff_TwoContributorsAlwaysRejected(t *testing.T) {
	// Unlike NumRetries, BackOff conflicts are rejected even when both
	// contributors set identical values — ownership ambiguity is the
	// problem, not the value.
	bo := policy.RetryBackOff{BaseInterval: 100 * time.Millisecond}
	_, err := MergeRetryConditions([]policy.RetryConditions{
		{BackOff: &bo},
		{BackOff: &bo},
	})
	if err == nil {
		t.Fatal("expected an error when two contributors both set BackOff, even identically")
	}
	if !strings.Contains(err.Error(), "BackOff") {
		t.Errorf("expected error to mention BackOff, got: %v", err)
	}
}

func TestMergeRetryConditions_NumRetries_DerivedFromMinAttemptsWhenUnset(t *testing.T) {
	four := 4
	merged, err := MergeRetryConditions([]policy.RetryConditions{
		{MinAttempts: &four},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged.NumRetries == nil || *merged.NumRetries != 3 {
		t.Errorf("expected NumRetries derived as MinAttempts-1 (3), got %v", merged.NumRetries)
	}
}
```

Add `"strings"` to the test file's imports.

- [ ] **Step 2: Run tests to verify the conflict tests fail**

Run: `cd gateway-controller && go test ./pkg/config/... -run TestMergeRetryConditions -v`
Expected: `TestMergeRetryConditions_NumRetries_ConflictingValuesRejected` and `TestMergeRetryConditions_BackOff_TwoContributorsAlwaysRejected` FAIL (no error returned by the Task 2 stub); the rest continue to pass.

- [ ] **Step 3: Replace the stub with the real implementation**

In `gateway-controller/pkg/config/retry_conditions_merge.go`, replace the `mergeSingleOwnerFields` stub and remove the `var _ = fmt.Sprintf` placeholder line with:

```go
// mergeSingleOwnerFields enforces the two single-owner composition rules:
// NumRetries allows multiple contributors only if they all agree on the
// same value; BackOff allows at most one contributor, full stop, even with
// identical values (ownership ambiguity is the problem, not the value).
func mergeSingleOwnerFields(contributions []policy.RetryConditions) (*int, *policy.RetryBackOff, error) {
	var numRetries *int
	var backOffOwners int
	var backOff *policy.RetryBackOff

	for _, c := range contributions {
		if c.NumRetries != nil {
			if numRetries != nil && *numRetries != *c.NumRetries {
				return nil, nil, fmt.Errorf(
					"conflicting NumRetries: %d and %d declared by different contributors on the same route",
					*numRetries, *c.NumRetries)
			}
			numRetries = c.NumRetries
		}
		if c.BackOff != nil {
			backOffOwners++
			backOff = c.BackOff
		}
	}
	if backOffOwners > 1 {
		return nil, nil, fmt.Errorf(
			"BackOff declared by %d contributors on the same route — at most one is allowed, even with identical values",
			backOffOwners)
	}

	return numRetries, backOff, nil
}
```

- [ ] **Step 4: Run tests to verify they all pass**

Run: `cd gateway-controller && go test ./pkg/config/... -run TestMergeRetryConditions -v`
Expected: PASS for all ten tests (five from Task 2, five from this task).

- [ ] **Step 5: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform
git add gateway-controller/pkg/config/retry_conditions_merge.go gateway-controller/pkg/config/retry_conditions_merge_test.go
git commit -m "gateway-controller: add NumRetries/BackOff single-owner conflict rules to MergeRetryConditions"
```

---

## Task 4: `resolveConditionField` + `ParseRetryConditions`

**Files:**
- Create: `gateway-controller/pkg/config/retry_conditions_parse.go`
- Test: `gateway-controller/pkg/config/retry_conditions_parse_test.go`

**Interfaces:**
- Consumes: `policy.RetryConditions`/`RetriableHeader`/`RetryBackOff` (Task 1).
- Produces: `ParseRetryConditions(raw map[string]interface{}, params map[string]interface{}, schema *map[string]interface{}) (*policy.RetryConditions, error)` — called by Task 6 for every retry-source policy's `x-wso2-retry-conditions` block, and for the operator's `resilience.retry` (Task 7).

- [ ] **Step 1: Write the failing tests**

```go
// gateway-controller/pkg/config/retry_conditions_parse_test.go
package config

import (
	"testing"
	"time"
)

func TestResolveConditionField_Literal(t *testing.T) {
	got, err := resolveConditionField(2, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2 {
		t.Errorf("expected literal passthrough, got %v", got)
	}
}

func TestResolveConditionField_FromParamPresent(t *testing.T) {
	raw := map[string]interface{}{"fromParam": "statusCodes"}
	params := map[string]interface{}{"statusCodes": []interface{}{401}}
	got, err := resolveConditionField(raw, params, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	codes, ok := got.([]interface{})
	if !ok || len(codes) != 1 || codes[0] != 401 {
		t.Errorf("expected params value to be read through, got %v", got)
	}
}

func TestResolveConditionField_FromParamAbsent_FallsBackToSchemaDefault(t *testing.T) {
	raw := map[string]interface{}{"fromParam": "statusCodes"}
	schema := &map[string]interface{}{
		"properties": map[string]interface{}{
			"statusCodes": map[string]interface{}{"default": []interface{}{401}},
		},
	}
	got, err := resolveConditionField(raw, map[string]interface{}{}, schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	codes, ok := got.([]interface{})
	if !ok || len(codes) != 1 || codes[0] != 401 {
		t.Errorf("expected schema default fallback, got %v", got)
	}
}

func TestParseRetryConditions_FullShape(t *testing.T) {
	raw := map[string]interface{}{
		"statusCodes":   map[string]interface{}{"fromParam": "statusCodes"},
		"minAttempts":   2,
		"perTryTimeout": "5s",
	}
	params := map[string]interface{}{"statusCodes": []interface{}{401}}

	rc, err := ParseRetryConditions(raw, params, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.StatusCodes) != 1 || rc.StatusCodes[0] != 401 {
		t.Errorf("unexpected StatusCodes: %v", rc.StatusCodes)
	}
	if rc.MinAttempts == nil || *rc.MinAttempts != 2 {
		t.Errorf("unexpected MinAttempts: %v", rc.MinAttempts)
	}
	if rc.PerTryTimeout == nil || *rc.PerTryTimeout != 5*time.Second {
		t.Errorf("unexpected PerTryTimeout: %v", rc.PerTryTimeout)
	}
}

func TestParseRetryConditions_EmptyRawYieldsZeroValue(t *testing.T) {
	rc, err := ParseRetryConditions(nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.StatusCodes) != 0 || rc.MinAttempts != nil {
		t.Errorf("expected zero-value RetryConditions for nil raw block, got %+v", rc)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd gateway-controller && go test ./pkg/config/... -run 'TestResolveConditionField|TestParseRetryConditions' -v`
Expected: FAIL — `resolveConditionField`/`ParseRetryConditions` undefined.

- [ ] **Step 3: Write the implementation**

```go
// gateway-controller/pkg/config/retry_conditions_parse.go
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

package config

import (
	"fmt"
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// resolveConditionField reads one RetryConditions field's raw YAML value —
// either a literal (returned as-is) or a {fromParam: "<name>"} pointer
// (resolved against the as-deployed params map, falling back to the
// policy's own JSON-schema-declared default when the param key is entirely
// absent).
func resolveConditionField(raw interface{}, params map[string]interface{}, schema *map[string]interface{}) (interface{}, error) {
	ptr, isPointer := raw.(map[string]interface{})
	if !isPointer {
		return raw, nil
	}
	paramName, ok := ptr["fromParam"].(string)
	if !ok || paramName == "" {
		return nil, fmt.Errorf("retry-conditions field: {fromParam: ...} must name a non-empty param field")
	}
	if val, present := params[paramName]; present {
		return val, nil
	}
	return schemaFieldDefault(schema, paramName), nil
}

// schemaFieldDefault reads properties.<field>.default from a policy's
// JSON-schema parameters, returning nil if schema is nil or the path is
// absent/malformed at any level.
func schemaFieldDefault(schema *map[string]interface{}, field string) interface{} {
	if schema == nil {
		return nil
	}
	properties, ok := (*schema)["properties"].(map[string]interface{})
	if !ok {
		return nil
	}
	propSchema, ok := properties[field].(map[string]interface{})
	if !ok {
		return nil
	}
	return propSchema["default"]
}

// ParseRetryConditions parses one contributor's x-wso2-retry-conditions raw
// YAML block (or an equivalently-shaped block from resilience.retry — see
// Task 7) into a policy.RetryConditions, resolving every field through
// resolveConditionField. raw may be nil (a policy/operator contributing
// nothing), which yields the zero-value RetryConditions, not an error.
func ParseRetryConditions(raw map[string]interface{}, params map[string]interface{}, schema *map[string]interface{}) (*policy.RetryConditions, error) {
	rc := &policy.RetryConditions{}
	if raw == nil {
		return rc, nil
	}

	if v, ok := raw["on"]; ok {
		resolved, err := resolveConditionField(v, params, schema)
		if err != nil {
			return nil, fmt.Errorf("on: %w", err)
		}
		rc.On = toStringSlice(resolved)
	}
	if v, ok := raw["statusCodes"]; ok {
		resolved, err := resolveConditionField(v, params, schema)
		if err != nil {
			return nil, fmt.Errorf("statusCodes: %w", err)
		}
		codes, err := toIntSlice(resolved)
		if err != nil {
			return nil, fmt.Errorf("statusCodes: %w", err)
		}
		rc.StatusCodes = codes
	}
	if v, ok := raw["minAttempts"]; ok {
		resolved, err := resolveConditionField(v, params, schema)
		if err != nil {
			return nil, fmt.Errorf("minAttempts: %w", err)
		}
		n, err := toInt(resolved)
		if err != nil {
			return nil, fmt.Errorf("minAttempts: %w", err)
		}
		rc.MinAttempts = &n
	}
	if v, ok := raw["numRetries"]; ok {
		resolved, err := resolveConditionField(v, params, schema)
		if err != nil {
			return nil, fmt.Errorf("numRetries: %w", err)
		}
		n, err := toInt(resolved)
		if err != nil {
			return nil, fmt.Errorf("numRetries: %w", err)
		}
		rc.NumRetries = &n
	}
	if v, ok := raw["perTryTimeout"]; ok {
		resolved, err := resolveConditionField(v, params, schema)
		if err != nil {
			return nil, fmt.Errorf("perTryTimeout: %w", err)
		}
		d, err := toDuration(resolved)
		if err != nil {
			return nil, fmt.Errorf("perTryTimeout: %w", err)
		}
		rc.PerTryTimeout = &d
	}
	if v, ok := raw["avoidPreviousHosts"]; ok {
		resolved, err := resolveConditionField(v, params, schema)
		if err != nil {
			return nil, fmt.Errorf("avoidPreviousHosts: %w", err)
		}
		if b, ok := resolved.(bool); ok {
			rc.AvoidPreviousHosts = b
		}
	}
	if v, ok := raw["backOff"]; ok {
		boRaw, ok := v.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("backOff: expected an object")
		}
		base, err := toDuration(boRaw["baseInterval"])
		if err != nil {
			return nil, fmt.Errorf("backOff.baseInterval: %w", err)
		}
		bo := &policy.RetryBackOff{BaseInterval: base}
		if maxRaw, ok := boRaw["maxInterval"]; ok {
			maxD, err := toDuration(maxRaw)
			if err != nil {
				return nil, fmt.Errorf("backOff.maxInterval: %w", err)
			}
			bo.MaxInterval = &maxD
		}
		rc.BackOff = bo
	}

	return rc, nil
}

func toStringSlice(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toIntSlice(v interface{}) ([]int, error) {
	arr, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected an array, got %T", v)
	}
	out := make([]int, 0, len(arr))
	for _, e := range arr {
		n, err := toInt(e)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func toInt(v interface{}) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	default:
		return 0, fmt.Errorf("expected an integer, got %T", v)
	}
}

func toDuration(v interface{}) (time.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("expected a duration string, got %T", v)
	}
	return time.ParseDuration(s)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd gateway-controller && go test ./pkg/config/... -run 'TestResolveConditionField|TestParseRetryConditions' -v`
Expected: PASS for all five tests.

- [ ] **Step 5: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform
git add gateway-controller/pkg/config/retry_conditions_parse.go gateway-controller/pkg/config/retry_conditions_parse_test.go
git commit -m "gateway-controller: add ParseRetryConditions generic literal-or-fromParam resolver"
```

---

## Task 5: `PolicyDefinition`/`RetrySourceMetadata` schema — replace trigger metadata with the conditions block

**Files:**
- Modify: `gateway-controller/pkg/models/policy_definition.go`
- Modify: `gateway-controller/pkg/config/retry_source_validator.go` (`LookupRetryMetadata`, and its one call site in `NewRetrySourceResolver`)
- Test: `gateway-controller/pkg/config/retry_source_validator_test.go` (extend existing)

**Interfaces:**
- Produces: `models.PolicyDefinition.RetryConditions *map[string]interface{}` (new); `models.RetrySourceMetadata.TargetsField string` (new); `LookupRetryMetadata` now returns `(*models.RetrySourceMetadata, *map[string]interface{}, *map[string]interface{})` — source metadata, the policy's parameter schema, and the raw retry-conditions block. `RetryTriggerMetadata`/`PolicyDefinition.RetryTrigger` are deleted in this same task, not kept around.

- [ ] **Step 1: Replace the model fields**

In `gateway-controller/pkg/models/policy_definition.go`, delete the `RetryTriggerMetadata` type entirely, and change:

```go
type RetrySourceMetadata struct {
	// GroupKeyField names which field in each targets[] entry
	// gateway-controller treats as the group's opaque discriminator (e.g.
	// "model" for model-failover). Required when RetrySource is non-nil.
	GroupKeyField string `json:"groupKeyField" yaml:"groupKeyField"`
}
```

to:

```go
type RetrySourceMetadata struct {
	// GroupKeyField names which field in each targets[] entry
	// gateway-controller treats as the group's opaque discriminator (e.g.
	// "model" for model-failover). Required when RetrySource is non-nil.
	GroupKeyField string `json:"groupKeyField" yaml:"groupKeyField"`

	// TargetsField names which top-level field in this policy's params
	// holds the ordered target list. Defaults to "targets" when empty —
	// see ParseRetrySourceParams.
	TargetsField string `json:"targetsField,omitempty" yaml:"targetsField,omitempty"`
}
```

And in `PolicyDefinition`, delete the `RetryTrigger *RetryTriggerMetadata` field and add:

```go
	// RetryConditions is this policy's x-wso2-retry-conditions declaration
	// (see docs/superpowers/specs/2026-08-17-generalized-retry-conditions-design.md) —
	// a raw map because its values are heterogeneous: each key is either a
	// literal or a {fromParam: "<name>"} pointer, resolved generically by
	// config.ParseRetryConditions, never unmarshaled into a typed struct.
	RetryConditions *map[string]interface{} `json:"x-wso2-retry-conditions,omitempty" yaml:"x-wso2-retry-conditions,omitempty"`
```

- [ ] **Step 2: Update `LookupRetryMetadata`**

In `gateway-controller/pkg/config/retry_source_validator.go`:

```go
func LookupRetryMetadata(
	definitions map[string]models.PolicyDefinition,
	latestVersions map[string]string,
	name, version string,
) (*models.RetrySourceMetadata, *map[string]interface{}, *map[string]interface{}) {
	if len(definitions) == 0 {
		return nil, nil, nil
	}
	resolved, err := ResolvePolicyVersion(definitions, latestVersions, name, version)
	if err != nil {
		return nil, nil, nil
	}
	def, ok := definitions[name+"|"+resolved]
	if !ok {
		return nil, nil, nil
	}
	return def.RetrySource, def.Parameters, def.RetryConditions
}
```

- [ ] **Step 3: Verify the one existing call site still compiles as-is**

`NewRetrySourceResolver`'s `visit` closure (same file) calls `source, _, _ := LookupRetryMetadata(definitions, latestVersions, p.Name, p.Version)` — three return values, discarding the second and third. `LookupRetryMetadata`'s arity is still three after Step 2 (only the meaning of the 2nd/3rd values changed, from `trigger, schema` to `schema, conditionsRaw`), so this call site needs no edit. Read the current line to confirm it matches this shape before moving on — if it doesn't, the codebase has drifted from what this task assumed and that's worth a ledger note, not a silent guess.

- [ ] **Step 4: Write a test locking in the new field's behavior**

```go
// append to gateway-controller/pkg/config/retry_source_validator_test.go

func TestLookupRetryMetadata_ReturnsRetryConditionsBlock(t *testing.T) {
	conditions := map[string]interface{}{"statusCodes": []interface{}{401}}
	definitions := map[string]models.PolicyDefinition{
		"test-policy|v1.0.0": {
			Name:            "test-policy",
			Version:         "v1.0.0",
			RetryConditions: &conditions,
		},
	}
	latestVersions := map[string]string{"test-policy": "v1.0.0"}

	_, _, gotConditions := LookupRetryMetadata(definitions, latestVersions, "test-policy", "v1")
	if gotConditions == nil {
		t.Fatal("expected the retry-conditions block to be returned")
	}
	if len(*gotConditions) != 1 {
		t.Errorf("expected one key in the retry-conditions block, got %v", *gotConditions)
	}
}
```

Check the top of `retry_source_validator_test.go` for its existing `models` import alias/path and match it.

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd gateway-controller && go build ./pkg/models/... ./pkg/config/... && go test ./pkg/config/... -run TestLookupRetryMetadata -v`
Expected: `./pkg/models/...` and `./pkg/config/...` build clean (`./pkg/xds/...` is deliberately left broken — Task 6 fixes it — confirm the build failure is scoped there by running these two packages explicitly, not `./...`); new test PASSes.

- [ ] **Step 6: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform
git add gateway-controller/pkg/models/policy_definition.go gateway-controller/pkg/config/retry_source_validator.go gateway-controller/pkg/config/retry_source_validator_test.go
git commit -m "gateway-controller: replace RetryTriggerMetadata with x-wso2-retry-conditions schema field"
```

---

## Task 6: Wire the new mechanism into the translator, delete the old one

**Files:**
- Modify: `gateway-controller/pkg/xds/translator.go` (`resolveRetryDeclarations`, `createRouteFromRDC`; delete `buildRetryPolicy`, `buildRetrySourceRetryPolicy`, `buildRetryTriggerRetryPolicy`, `composeRetryTriggerPolicy`, `mergeRetriableStatusCodes`)
- Modify: `gateway-controller/pkg/config/retry_source_validator.go` (`ParseRetrySourceParams`: add `targetsField` param, drop `statusCodes` reading entirely; delete `ParseRetryTriggerParams`; `ValidateAtMostOneRetrySourcePerRoute`: drop the `resilience.retry` exclusion)
- Test: `gateway-controller/pkg/xds/translator_retry_source_test.go` (extend existing; delete tests naming deleted functions)

**Interfaces:**
- Consumes: `MergeRetryConditions` (Task 2/3), `ParseRetryConditions` (Task 4), `LookupRetryMetadata` (Task 5).
- Produces: `buildRoutePolicyFromConditions(merged policy.RetryConditions, source *policy.RetrySourceDeclaration) *route.RetryPolicy` — the single assembler, and `operatorRetryToRawConditions` (fully built here, not stubbed — Task 7 only needs to extend the OpenAPI schema and regenerate, not touch this function's shape again).

This is the largest task. Work through the sub-steps in order; don't commit until the end — an intermediate state here doesn't compile.

- [ ] **Step 1: Generalize `ParseRetrySourceParams`, drop its `statusCodes` reading entirely**

In `gateway-controller/pkg/config/retry_source_validator.go`, change `ParseRetrySourceParams`'s signature:

```go
func ParseRetrySourceParams(params map[string]interface{}, groupKeyField, targetsField string) (*policy.RetrySourceDeclaration, error) {
	if targetsField == "" {
		targetsField = "targets"
	}
	rawTargets, ok := params[targetsField].([]interface{})
	if !ok || len(rawTargets) == 0 {
		return nil, fmt.Errorf("retry-source policy requires a non-empty '%s' list", targetsField)
	}
```

Keep the rest of the target/group-parsing body unchanged. Delete the `rawStatusCodes`/`statusCodes` block entirely — retriable status codes now come exclusively from `x-wso2-retry-conditions` (Task 4/8), never from this function. The final construction becomes:

```go
	return &policy.RetrySourceDeclaration{Groups: groups}, nil
```

(no `Conditions`/`RetriableStatusCodes` set here at all — `resolveRetryDeclarations`, Step 5 below, merges the source's own `x-wso2-retry-conditions` block in separately, the same way every other contributor's does).

- [ ] **Step 2: Fix `ParseRetrySourceParams`'s one call site**

In `NewRetrySourceResolver`'s `visit` closure (same file):

```go
decl, err := ParseRetrySourceParams(params, source.GroupKeyField, source.TargetsField)
```

- [ ] **Step 3: Delete `ParseRetryTriggerParams`**

Delete the whole function from `retry_source_validator.go` — nothing calls it after this task.

- [ ] **Step 4: Write the failing test for `buildRoutePolicyFromConditions`**

```go
// append to gateway-controller/pkg/xds/translator_retry_source_test.go
// (check the existing file's imports/package declaration and match them —
// do not duplicate an import already present)

func TestBuildRoutePolicyFromConditions_NoRetrySource(t *testing.T) {
	two := 2
	merged := policy.RetryConditions{
		On:          []string{"retriable-status-codes"},
		StatusCodes: []int{401},
		MinAttempts: &two,
	}

	rp := buildRoutePolicyFromConditions(merged, nil)

	if rp == nil {
		t.Fatal("expected a non-nil RetryPolicy")
	}
	if rp.GetNumRetries().GetValue() != 1 {
		t.Errorf("expected NumRetries derived from MinAttempts-1 (1), got %d", rp.GetNumRetries().GetValue())
	}
	if len(rp.RetriableStatusCodes) != 1 || rp.RetriableStatusCodes[0] != 401 {
		t.Errorf("unexpected RetriableStatusCodes: %v", rp.RetriableStatusCodes)
	}
	if rp.GetRetryPriority() != nil {
		t.Error("expected no RetryPriority when no retry source is present")
	}
}

func TestBuildRoutePolicyFromConditions_WithRetrySource_SetsRetryPriority(t *testing.T) {
	merged := policy.RetryConditions{StatusCodes: []int{500}}
	source := &policy.RetrySourceDeclaration{
		Groups: []policy.RetryGroup{
			{Key: "gpt-4o", OrderedTargets: []policy.RetryTarget{
				{UpstreamDefinitionName: "gpt-4o"},
				{UpstreamDefinitionName: "gpt-4o-mini"},
			}},
		},
	}

	rp := buildRoutePolicyFromConditions(merged, source)

	if rp.GetRetryPriority() == nil {
		t.Fatal("expected RetryPriority to be set when a retry source is present")
	}
	if rp.GetRetryPriority().GetName() != "envoy.retry_priorities.previous_priorities" {
		t.Errorf("unexpected RetryPriority name: %q", rp.GetRetryPriority().GetName())
	}
	if rp.GetNumRetries().GetValue() != 1 {
		t.Errorf("expected NumRetries derived from the one-fallback chain length (1), got %d", rp.GetNumRetries().GetValue())
	}
}

func TestBuildRoutePolicyFromConditions_NumRetries_NeverLowerThanChainLength(t *testing.T) {
	one := 1 // a MinAttempts=1 contributor must never SHRINK the retry-source-derived count
	merged := policy.RetryConditions{StatusCodes: []int{500}, MinAttempts: &one}
	source := &policy.RetrySourceDeclaration{
		Groups: []policy.RetryGroup{
			{Key: "g", OrderedTargets: []policy.RetryTarget{
				{UpstreamDefinitionName: "a"}, {UpstreamDefinitionName: "b"}, {UpstreamDefinitionName: "c"},
			}},
		},
	}

	rp := buildRoutePolicyFromConditions(merged, source)

	if rp.GetNumRetries().GetValue() != 2 {
		t.Errorf("expected NumRetries to stay at the chain length (2), not be lowered by a smaller MinAttempts, got %d", rp.GetNumRetries().GetValue())
	}
}
```

- [ ] **Step 5: Run tests to verify they fail**

Run: `cd gateway-controller && go test ./pkg/xds/... -run TestBuildRoutePolicyFromConditions -v`
Expected: FAIL — `buildRoutePolicyFromConditions` undefined.

- [ ] **Step 6: Write `buildRoutePolicyFromConditions` and `operatorRetryToRawConditions`**

Add to `gateway-controller/pkg/xds/translator.go`, replacing (not alongside) `buildRetryPolicy`/`buildRetrySourceRetryPolicy`/`buildRetryTriggerRetryPolicy`/`composeRetryTriggerPolicy`/`mergeRetriableStatusCodes` — delete all five now:

```go
// buildRoutePolicyFromConditions is the single assembler translating this
// codebase's merged RetryConditions vocabulary into a real Envoy
// route.RetryPolicy. source is nil for a route with no retry-source policy
// (plain operator/trigger-style retry); when non-nil, NumRetries is floored
// at the longest group chain length regardless of what merged.NumRetries
// derived to, and RetryPriority/PerTryTimeout (from source.PerAttemptTimeout)
// are set.
func buildRoutePolicyFromConditions(merged policy.RetryConditions, source *policy.RetrySourceDeclaration) *route.RetryPolicy {
	statusCodes := make([]uint32, len(merged.StatusCodes))
	for i, code := range merged.StatusCodes {
		statusCodes[i] = uint32(code)
	}

	numRetries := 0
	if merged.NumRetries != nil {
		numRetries = *merged.NumRetries
	}

	rp := &route.RetryPolicy{
		RetryOn:              strings.Join(retryOnOrDefault(merged.On), ","),
		RetriableStatusCodes: statusCodes,
	}

	if source != nil {
		maxChain := 0
		for _, group := range source.Groups {
			if n := len(group.OrderedTargets) - 1; n > maxChain {
				maxChain = n
			}
		}
		if maxChain > numRetries {
			numRetries = maxChain
		}

		priorityCfgAny, err := anypb.New(&previous_prioritiesv3.PreviousPrioritiesConfig{UpdateFrequency: 1})
		if err == nil { // anypb.New cannot fail for this well-formed message — see createAggregateCluster's identical guard
			rp.RetryPriority = &route.RetryPolicy_RetryPriority{
				Name:       "envoy.retry_priorities.previous_priorities",
				ConfigType: &route.RetryPolicy_RetryPriority_TypedConfig{TypedConfig: priorityCfgAny},
			}
		}
		if source.PerAttemptTimeout != nil {
			rp.PerTryTimeout = durationpb.New(*source.PerAttemptTimeout)
		} else if merged.PerTryTimeout != nil {
			rp.PerTryTimeout = durationpb.New(*merged.PerTryTimeout)
		}
	} else if merged.PerTryTimeout != nil {
		rp.PerTryTimeout = durationpb.New(*merged.PerTryTimeout)
	}

	rp.NumRetries = wrapperspb.UInt32(uint32(numRetries))

	if merged.BackOff != nil {
		bo := &route.RetryPolicy_RetryBackOff{
			BaseInterval: durationpb.New(merged.BackOff.BaseInterval),
		}
		if merged.BackOff.MaxInterval != nil {
			bo.MaxInterval = durationpb.New(*merged.BackOff.MaxInterval)
		}
		rp.RetryBackOff = bo
	}

	if merged.AvoidPreviousHosts {
		rp.RetryHostPredicate = append(rp.RetryHostPredicate, &route.RetryPolicy_RetryHostPredicate{
			Name: "envoy.retry_host_predicates.previous_hosts",
		})
	}

	for _, h := range merged.Headers {
		rp.RetriableHeaders = append(rp.RetriableHeaders, &route.HeaderMatcher{
			Name: h.Name,
			HeaderMatchSpecifier: &route.HeaderMatcher_StringMatch{
				StringMatch: &matcherv3.StringMatcher{
					MatchPattern: &matcherv3.StringMatcher_Exact{Exact: h.Exact},
				},
			},
		})
	}

	return rp
}

// retryOnOrDefault preserves the always-at-least-"retriable-status-codes"
// behavior of the old builders when a merge produced no On conditions at
// all — e.g. a bare NumRetries-only resilience.retry with no explicit
// conditions.
func retryOnOrDefault(on []string) []string {
	if len(on) == 0 {
		return []string{"retriable-status-codes"}
	}
	return on
}

// operatorRetryToRawConditions adapts the operator-facing api.Retry into
// the same raw-block shape ParseRetryConditions expects, so an operator's
// resilience.retry funnels through the identical parse+merge path as a
// policy's own x-wso2-retry-conditions declaration. Extended in Task 7 once
// api.Retry gains the richer fields (On, PerTryTimeout, BackOff,
// AvoidPreviousHosts) — this version wires NumRetries/StatusCodes only.
func operatorRetryToRawConditions(r *api.Retry) map[string]interface{} {
	raw := map[string]interface{}{"statusCodes": toInterfaceSlice(r.StatusCodes)}
	if r.NumRetries != nil {
		raw["numRetries"] = *r.NumRetries
	}
	return raw
}

func toInterfaceSlice(codes []int) []interface{} {
	out := make([]interface{}, len(codes))
	for i, c := range codes {
		out[i] = c
	}
	return out
}
```

Add `matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"` to `translator.go`'s imports if not already present (check the existing import block first — `strings` is almost certainly already imported elsewhere in this large file; `matcherv3` likely is not). Also add `config "github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"` if `translator.go` doesn't already import it under that name (it very likely already does, since `resolveRetryDeclarations` already calls `config.LookupRetryMetadata`/`config.ParseRetrySourceParams` today — confirm rather than add a duplicate).

- [ ] **Step 7: Run tests to verify they pass**

Run: `cd gateway-controller && go test ./pkg/xds/... -run TestBuildRoutePolicyFromConditions -v`
Expected: PASS for all three tests.

- [ ] **Step 8: Rewrite `resolveRetryDeclarations`**

In `translator.go`, replace the whole function:

```go
func (t *Translator) resolveRetryDeclarations(chain *models.PolicyChain) (
	sourceDecl *policy.RetrySourceDeclaration,
	merged policy.RetryConditions,
	err error,
) {
	if chain == nil {
		return nil, policy.RetryConditions{}, nil
	}

	if len(chain.Policies) > 0 && t.policyDefinitions == nil {
		t.missingPolicyDefinitionsWarnOnce.Do(func() {
			t.logger.Warn("translator has no policy definitions loaded; retry-source and " +
				"retry-conditions metadata cannot be resolved for any policy chain — the " +
				"controller binary embedding this translator is missing a " +
				"SetPolicyDefinitions call")
		})
	}

	var contributions []policy.RetryConditions
	for _, p := range chain.Policies {
		source, schema, conditionsRaw := config.LookupRetryMetadata(t.policyDefinitions, t.latestVersions, p.Name, p.Version)
		if source != nil {
			decl, parseErr := config.ParseRetrySourceParams(p.Params, source.GroupKeyField, source.TargetsField)
			if parseErr != nil {
				return nil, policy.RetryConditions{}, fmt.Errorf("policy %q: %w", p.Name, parseErr)
			}
			sourceDecl = decl
		}
		if conditionsRaw != nil {
			rc, parseErr := config.ParseRetryConditions(*conditionsRaw, p.Params, schema)
			if parseErr != nil {
				return nil, policy.RetryConditions{}, fmt.Errorf("policy %q: %w", p.Name, parseErr)
			}
			contributions = append(contributions, *rc)
		}
	}

	merged, err = config.MergeRetryConditions(contributions)
	if err != nil {
		return nil, policy.RetryConditions{}, err
	}
	return sourceDecl, merged, nil
}
```

- [ ] **Step 9: Rewrite `createRouteFromRDC`'s retry-assembly block**

Locate the block that currently reads (roughly):

```go
	if routeResilienceRetry != nil {
		routeAction.Route.RetryPolicy = buildRetryPolicy(routeResilienceRetry)
	}
```

and, further down, the block that currently reads:

```go
	if sourceDecl, _, triggerCodes, triggerMinAttempts, err := t.resolveRetryDeclarations(rdc.PolicyChains[routeKey]); err == nil {
		switch {
		case sourceDecl != nil:
			sourceDecl.RetriableStatusCodes = mergeRetriableStatusCodes(sourceDecl.RetriableStatusCodes, triggerCodes)
			routeAction.Route.RetryPolicy = buildRetrySourceRetryPolicy(sourceDecl, triggerMinAttempts)
		case len(triggerCodes) > 0:
			routeAction.Route.RetryPolicy = composeRetryTriggerPolicy(routeAction.Route.RetryPolicy, triggerCodes, triggerMinAttempts)
		}
	}
```

Delete the first block entirely (the bare `buildRetryPolicy(routeResilienceRetry)` assignment) — `routeResilienceRetry` is still read into the local variable a few lines above (unchanged), just no longer used to set `RetryPolicy` directly. Replace the second block with:

```go
	if sourceDecl, merged, err := t.resolveRetryDeclarations(rdc.PolicyChains[routeKey]); err == nil {
		if routeResilienceRetry != nil {
			opRC, parseErr := config.ParseRetryConditions(operatorRetryToRawConditions(routeResilienceRetry), nil, nil)
			if parseErr == nil {
				if combined, mergeErr := config.MergeRetryConditions([]policy.RetryConditions{*opRC, merged}); mergeErr == nil {
					merged = combined
				}
			}
		}
		if sourceDecl != nil || len(merged.StatusCodes) > 0 || len(merged.On) > 0 || merged.NumRetries != nil {
			routeAction.Route.RetryPolicy = buildRoutePolicyFromConditions(merged, sourceDecl)
		}
	}
```

(No separate merge step for `sourceDecl`'s own conditions — a retry-source policy's status codes/etc. already arrived in `merged` via its own `x-wso2-retry-conditions` block, parsed in the same `resolveRetryDeclarations` pass as any other chain member's. `RetrySourceDeclaration` carries no `Conditions` field to merge — see Task 1.)

- [ ] **Step 10: Relax `ValidateAtMostOneRetrySourcePerRoute`**

In `gateway-controller/pkg/config/retry_source_validator.go`, replace:

```go
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
```

with:

```go
// ValidateAtMostOneRetrySourcePerRoute rejects only the genuine Envoy hard
// limit: more than one policy declaring x-wso2-retry-source on the same
// route (RetryPriority is a singular field on the real Envoy RetryPolicy
// proto — confirmed against the vendored go-control-plane proto during the
// 2026-08-17 audit — so two retry-source policies can never coexist).
// A retry-source policy combined with an operator's resilience.retry is no
// longer rejected here: field-level composition (config.MergeRetryConditions)
// reconciles them safely, only rejecting an actual NumRetries/BackOff
// ownership conflict, not the mere presence of both.
func ValidateAtMostOneRetrySourcePerRoute(retrySourceCount int) error {
	if retrySourceCount > 1 {
		return fmt.Errorf("this route has %d policies each declaring retry-source ownership — at most one is allowed per route, since Envoy has a single RouteAction.RetryPolicy slot", retrySourceCount)
	}
	return nil
}
```

Update its one call site in `ValidateRetrySourcesForOperations` (same file):

```go
		if err := ValidateAtMostOneRetrySourcePerRoute(count); err != nil {
			return fmt.Errorf("operation %s %s: %w", op.EffectiveMethod(), op.EffectivePath(), err)
		}
```

Read `ValidateRetrySourcesForOperations`'s body before this edit: the `effectiveRetry`/`apiRetry` local variables (and the `effectiveResilienceRetry` calls that compute them) were only ever used to feed the now-removed `retry *api.Retry` parameter. If nothing else in the function reads them after this change, delete them and `effectiveResilienceRetry` too — an unused local is a Go compile error, not a lint warning, so this isn't optional cleanup.

- [ ] **Step 11: Run the full `pkg/config` and `pkg/xds` suites**

Run: `cd gateway-controller && go build ./... && go vet ./... && go test ./pkg/config/... ./pkg/xds/... -v 2>&1 | tail -150`

Fix any pre-existing test in `translator_retry_source_test.go`/`translator_test.go`/`retry_source_validator_test.go` that:
- Calls `ValidateAtMostOneRetrySourcePerRoute` with the old two-argument signature — update to one argument.
- Calls `ParseRetrySourceParams` with the old two-argument signature — update to three, passing `""` for `targetsField` where the test doesn't care (defaults to `"targets"`).
- Asserts on `RetrySourceDeclaration.RetriableStatusCodes` directly — that field no longer exists, and `RetrySourceDeclaration` has no replacement `Conditions` field either (see Task 1) — a retry-source policy's status codes now only ever appear in the merged `RetryConditions` `resolveRetryDeclarations` returns, or in the resulting `route.RetryPolicy` proto. Rewrite the assertion against whichever of those the test is actually about.
- Names a deleted function (`TestBuildRetryPolicy`, `TestBuildRetrySourceRetryPolicy`, `TestBuildRetryTriggerRetryPolicy`, `TestComposeRetryTriggerPolicy`, `TestMergeRetriableStatusCodes`, `TestParseRetryTriggerParams`, any `TestLookupRetryMetadata_*` asserting a trigger-metadata return value that no longer exists) — delete the test, don't try to adapt it to cover a function that's gone.

Expected: builds clean; every remaining test passes. The resulting `route.RetryPolicy` proto shape for the single-retry-source, no-operator-retry case must be unchanged from before this task (verify by checking any existing translator test asserting on that exact scenario still passes without modification to its expected values — only its *call path* into `resolveRetryDeclarations`/`createRouteFromRDC` changed, not the observable Envoy config it produces).

- [ ] **Step 12: Commit**

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform
git add gateway-controller/pkg/xds/translator.go gateway-controller/pkg/xds/translator_retry_source_test.go gateway-controller/pkg/xds/translator_test.go gateway-controller/pkg/config/retry_source_validator.go gateway-controller/pkg/config/retry_source_validator_test.go
git commit -m "gateway-controller: cut over route retry-policy assembly to RetryConditions, delete the old status-code-only path"
```

---

## Task 7: Extend operator-facing `resilience.retry`

**Files:**
- Modify: `gateway-controller/api/management-openapi.yaml` (`Retry` schema)
- Run: `make generate-server-code` (regenerates `gateway-controller/pkg/api/management/generated.go` and API docs — do not hand-edit the generated file)
- Modify: `gateway-controller/pkg/xds/translator.go` (`operatorRetryToRawConditions`, extend Task 6's NumRetries/StatusCodes-only version)
- Modify: `gateway-controller/pkg/config/retry_conditions_parse.go` (add `backOff` parsing if not already present from Task 4 — it is, per Task 4 Step 3; this task only needs the operator-side wiring)

**Interfaces:**
- Consumes: `api.Retry` (regenerated struct).
- Produces: `operatorRetryToRawConditions` fully translating every new field.

- [ ] **Step 1: Extend the `Retry` schema**

In `gateway-controller/api/management-openapi.yaml`, the current `Retry` schema has `statusCodes` (required) and `numRetries`. Add, alongside `numRetries`:

```yaml
        on:
          type: array
          items:
            type: string
            enum: [5xx, gateway-error, reset, connect-failure, refused-stream, envoy-ratelimited, retriable-4xx, retriable-status-codes, retriable-headers]
          description: >
            Envoy retry conditions. Defaults to [retriable-status-codes]
            when omitted and statusCodes is set.
        perTryTimeout:
          type: string
          description: Go-duration-formatted bound on a single retry attempt (e.g. "5s").
        backOff:
          type: object
          properties:
            baseInterval:
              type: string
              description: Go-duration-formatted base retry backoff interval (e.g. "100ms").
            maxInterval:
              type: string
              description: Go-duration-formatted max retry backoff interval.
          required: [baseInterval]
        avoidPreviousHosts:
          type: boolean
          default: false
          description: Avoid retrying against the same host a previous attempt already used.
```

- [ ] **Step 2: Regenerate**

Run: `cd gateway-controller && make generate-server-code`
Expected: `pkg/api/management/generated.go`'s `Retry` struct gains `On *[]RetryOn` (or similar — oapi-codegen names an enum array field's element type after the parent+property, e.g. `RetryOn`; read the actual regenerated file rather than assuming the exact name before Step 3), `PerTryTimeout *string`, `BackOff *RetryBackOff` (a new generated nested type), `AvoidPreviousHosts *bool`.

- [ ] **Step 3: Extend `operatorRetryToRawConditions`**

In `gateway-controller/pkg/xds/translator.go`, extend the version Task 6 built (adjust field/type names to match what Step 2 actually generated — read `generated.go` first):

```go
func operatorRetryToRawConditions(r *api.Retry) map[string]interface{} {
	raw := map[string]interface{}{"statusCodes": toInterfaceSlice(r.StatusCodes)}
	if r.NumRetries != nil {
		raw["numRetries"] = *r.NumRetries
	}
	if r.On != nil {
		on := make([]interface{}, len(*r.On))
		for i, o := range *r.On {
			on[i] = string(o)
		}
		raw["on"] = on
	}
	if r.PerTryTimeout != nil {
		raw["perTryTimeout"] = *r.PerTryTimeout
	}
	if r.AvoidPreviousHosts != nil {
		raw["avoidPreviousHosts"] = *r.AvoidPreviousHosts
	}
	if r.BackOff != nil {
		bo := map[string]interface{}{"baseInterval": r.BackOff.BaseInterval}
		if r.BackOff.MaxInterval != nil {
			bo["maxInterval"] = *r.BackOff.MaxInterval
		}
		raw["backOff"] = bo
	}
	return raw
}
```

- [ ] **Step 4: Write a test for the full operator translation**

```go
// append to gateway-controller/pkg/xds/translator_retry_source_test.go

func TestOperatorRetryToRawConditions_FullShape(t *testing.T) {
	five := 5
	baseInterval := "100ms"
	avoidHosts := true
	on := []api.RetryOn{"5xx", "connect-failure"}
	r := &api.Retry{
		StatusCodes:        []int{500},
		NumRetries:         &five,
		On:                 &on,
		PerTryTimeout:      strPtr("5s"),
		AvoidPreviousHosts: &avoidHosts,
		BackOff:            &api.RetryBackOff{BaseInterval: baseInterval},
	}

	raw := operatorRetryToRawConditions(r)
	rc, err := config.ParseRetryConditions(raw, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rc.NumRetries == nil || *rc.NumRetries != 5 {
		t.Errorf("unexpected NumRetries: %v", rc.NumRetries)
	}
	if rc.BackOff == nil || rc.BackOff.BaseInterval != 100*time.Millisecond {
		t.Errorf("unexpected BackOff: %v", rc.BackOff)
	}
	if !rc.AvoidPreviousHosts {
		t.Error("expected AvoidPreviousHosts to survive the round trip")
	}
}

func strPtr(s string) *string { return &s }
```

Adjust `api.RetryOn`/`api.RetryBackOff` field/type names to whatever Step 2 actually generated — do not guess ahead of running it.

- [ ] **Step 5: Run tests**

Run: `cd gateway-controller && go test ./pkg/xds/... -run TestOperatorRetryToRawConditions -v`
Expected: PASS.

- [ ] **Step 6: Regenerate API docs and commit**

Run whatever this repo's `Makefile` `generate` target uses for docs specifically (`make generate-apidocs`, per the `generate: generate-server-code generate-apidocs` target already visible in `gateway-controller/Makefile`) and include the output in this commit — this repo's own convention is generated docs travel with a schema change.

```bash
cd /Users/thenujan/Desktop/Git-Repos/api-platform
git add gateway-controller/api/management-openapi.yaml gateway-controller/pkg/api/management/generated.go gateway-controller/pkg/xds/translator.go gateway-controller/pkg/xds/translator_retry_source_test.go
git commit -m "gateway-controller: extend resilience.retry with on/perTryTimeout/backOff/avoidPreviousHosts"
```

---

## Task 8: Migrate `model-failover` to the new declaration shape

**Files:**
- Modify (canonical repo): `/Users/thenujan/Desktop/Git-Repos/gateway-controllers/policies/model-failover/policy-definition.yaml`
- Mirror: `/Users/thenujan/Desktop/Git-Repos/api-platform/gateway/dev-policies/model-failover/policy-definition.yaml`

`model-failover`'s current `policy-definition.yaml` declares only:

```yaml
x-wso2-retry-source:
  groupKeyField: model
```

Its retriable status codes today come from `ParseRetrySourceParams` reading `params["statusCodes"]` directly — a reading Task 6 deleted. `model-failover`'s own Go code (`model_failover.go`) needs no changes at all — it already reads `params["targets"]`/`params["statusCodes"]` directly by hardcoded key for its own runtime purposes (group selection, suspend tracking), which is unrelated to how gateway-controller's generic parser locates those same params for building the route's `RetryPolicy`. Only the declarative metadata changes.

- [ ] **Step 1: Update the canonical `policy-definition.yaml`**

Replace the `x-wso2-retry-source` block with:

```yaml
x-wso2-retry-source:
  groupKeyField: model
  targetsField: targets
x-wso2-retry-conditions:
  statusCodes: {fromParam: statusCodes}
```

- [ ] **Step 2: Build and test the canonical repo**

Run: `cd /Users/thenujan/Desktop/Git-Repos/gateway-controllers/policies/model-failover && go build ./... && go vet ./... && go test ./... 2>&1 | tail -40`
Expected: PASS — no Go source changed, this only verifies the YAML is syntactically valid and nothing in the Go test suite depends on the old block shape.

- [ ] **Step 3: Mirror to `dev-policies` and verify sync**

```bash
cp /Users/thenujan/Desktop/Git-Repos/gateway-controllers/policies/model-failover/policy-definition.yaml /Users/thenujan/Desktop/Git-Repos/api-platform/gateway/dev-policies/model-failover/policy-definition.yaml
diff -q /Users/thenujan/Desktop/Git-Repos/gateway-controllers/policies/model-failover/policy-definition.yaml /Users/thenujan/Desktop/Git-Repos/api-platform/gateway/dev-policies/model-failover/policy-definition.yaml
```

Expected: no diff output.

- [ ] **Step 4: Commit (canonical repo only — `dev-policies` is gitignored, never committed inside `api-platform`)**

```bash
cd /Users/thenujan/Desktop/Git-Repos/gateway-controllers
git add policies/model-failover/policy-definition.yaml
git commit -m "model-failover: migrate to x-wso2-retry-conditions, generalize targetsField"
```

---

## Task 9: Update model-failover's e2e docs/collection for the new declaration shape

**Files:**
- Modify (canonical repo): model-failover's e2e docs/Postman collection, wherever they live — check the directory first
- Mirror both into `api-platform/gateway/dev-policies/model-failover/e2e/`

Follow the same pattern already used this session for `oauth2-generator`'s e2e suite when its mechanism changed: back up the originals first, update descriptions/comments referencing the old flat `x-wso2-retry-source` + bare `statusCodes` shape to the new `groupKeyField`/`targetsField` + `x-wso2-retry-conditions` shape (Task 8's actual YAML), validate any Postman JSON stays well-formed, mirror, diff-confirm sync, do not commit the gitignored mirror.

- [ ] **Step 1: Locate and back up**

```bash
cd /Users/thenujan/Desktop/Git-Repos/gateway-controllers/policies/model-failover/e2e
mkdir -p backup-pre-retry-conditions
find . -maxdepth 2 -iname "*.md" -o -iname "*postman*" | grep -v backup-pre-retry-conditions
```

Copy whatever this finds into `backup-pre-retry-conditions/` before editing.

- [ ] **Step 2: Update references**

```bash
grep -rln "x-wso2-retry-source\|statusCodesField\|x-wso2-retry-trigger" . --exclude-dir=backup-pre-retry-conditions
```

For each hit, update the prose/JSON description to describe the new shape. If any Postman test script asserts on gateway-controller internals tied to the deleted `RetriableStatusCodes` field specifically (unlikely — Postman tests assert on HTTP behavior, not Go internals), fix those; leave assertions on client-visible behavior (status codes, retry counts, suspend timing) untouched, since that behavior is unchanged by this whole plan.

- [ ] **Step 3: Validate and mirror**

```bash
# if a Postman collection exists:
jq . <path-to-collection>.json > /dev/null && echo "JSON OK"

cp -r /Users/thenujan/Desktop/Git-Repos/gateway-controllers/policies/model-failover/e2e/*.md /Users/thenujan/Desktop/Git-Repos/api-platform/gateway/dev-policies/model-failover/e2e/
# (adjust the exact file list/paths to whatever Step 1 actually found)
diff -rq /Users/thenujan/Desktop/Git-Repos/gateway-controllers/policies/model-failover/e2e /Users/thenujan/Desktop/Git-Repos/api-platform/gateway/dev-policies/model-failover/e2e --exclude=backup-pre-retry-conditions
```

Expected: JSON valid (if present); no diff besides the excluded backup directory.

- [ ] **Step 4: Commit (canonical repo only)**

```bash
cd /Users/thenujan/Desktop/Git-Repos/gateway-controllers
git add policies/model-failover/e2e
git commit -m "model-failover e2e: update docs/collection for x-wso2-retry-conditions"
```

---

## Post-Plan Verification

Once all nine tasks land:

1. Rebuild both Docker images (`make build-gateway-runtime && make build-controller` from `api-platform/gateway/`) and re-run model-failover's live e2e suite (`./run-e2e.sh --model-failover`) against a freshly recreated stack (`docker compose up -d --force-recreate gateway-controller gateway-runtime`).
2. Confirm the one scenario this whole redesign was built to unlock actually works end-to-end: an operator's `resilience.retry` (with `backOff`/`perTryTimeout` set) composing with model-failover's retry-source on the same route — something the old `ValidateAtMostOneRetrySourcePerRoute` blanket rule made impossible to even register before this plan.

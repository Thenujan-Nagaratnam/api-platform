# Tasks: Upstream Retry Contribution

**Input**: Design documents from `/specs/001-upstream-retry-contribution/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/policy-definition-schema.md, quickstart.md — all present and read.

**Tests**: Included. Not explicitly requested in the spec, but every existing `pkg/xds`/`pkg/transform`/`pkg/utils` file in this module ships `_test.go` coverage with `testify` (confirmed throughout this codebase), and quickstart.md already enumerates the exact cases expected — matching that established convention rather than skipping tests.

**Organization**: Grouped by user story (spec.md P1/P2/P3) on top of one shared Foundational phase, since every story needs the full resolve→populate→merge pipeline for even a single contribution.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: US1 / US2 / US3, per spec.md's prioritized user stories

## Path Conventions

Single existing Go module: `gateway/gateway-controller`. All paths below are relative to repo root.

---

## Phase 1: Setup

**Purpose**: Confirm the starting point before touching anything.

- [X] T001 Run `go build ./... && go test ./...` from `gateway/gateway-controller` and confirm a clean baseline (no new dependency, no `go.mod` change needed — this feature uses only stdlib + already-imported `go-control-plane` types). **Environment note**: this machine's clang/macOS-SDK linker is broken (pre-existing, unrelated to this feature) — `go build`/`go test` must run with `CGO_ENABLED=0` to link at all; that in turn makes go-sqlite3-backed tests fail with a stub-driver error in `pkg/transform`/`pkg/utils` (pre-existing, confirmed before any change in this feature). `go vet ./pkg/...` and `CGO_ENABLED=0 go test ./pkg/xds/... ./pkg/models/...` are both clean baselines.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The schema, lookup, resolution, and merge pipeline every user story depends on — `retry`'s full field set (including `enabledByParam` and `maxAttemptsFromLongestList`) is treated as one mechanism, not split across stories, since data-model.md defines it that way.

**⚠️ CRITICAL**: No user story task can start until this phase is complete.

- [X] T002 [P] Add `Retry` and `LongestListAttempts` structs to `gateway/gateway-controller/pkg/models/policy_definition.go`, per data-model.md's Go representation exactly:
  ```go
  type LongestListAttempts struct {
      ParamPath string `json:"paramPath" yaml:"paramPath"`
      Plus      int    `json:"plus,omitempty" yaml:"plus,omitempty"`
  }

  type Retry struct {
      EnabledByParam string `json:"enabledByParam,omitempty" yaml:"enabledByParam,omitempty"`

      StatusCodes          []int  `json:"statusCodes,omitempty" yaml:"statusCodes,omitempty"`
      StatusCodesFromParam string `json:"statusCodesFromParam,omitempty" yaml:"statusCodesFromParam,omitempty"`

      MaxAttempts                int                  `json:"maxAttempts,omitempty" yaml:"maxAttempts,omitempty"`
      MaxAttemptsFromParam       string               `json:"maxAttemptsFromParam,omitempty" yaml:"maxAttemptsFromParam,omitempty"`
      MaxAttemptsFromLongestList *LongestListAttempts `json:"maxAttemptsFromLongestList,omitempty" yaml:"maxAttemptsFromLongestList,omitempty"`

      PerAttemptTimeoutMs          int    `json:"perAttemptTimeoutMs,omitempty" yaml:"perAttemptTimeoutMs,omitempty"`
      PerAttemptTimeoutMsFromParam string `json:"perAttemptTimeoutMsFromParam,omitempty" yaml:"perAttemptTimeoutMsFromParam,omitempty"`

      AlsoRetryOnConnectionFailure          bool   `json:"alsoRetryOnConnectionFailure,omitempty" yaml:"alsoRetryOnConnectionFailure,omitempty"`
      AlsoRetryOnConnectionFailureFromParam string `json:"alsoRetryOnConnectionFailureFromParam,omitempty" yaml:"alsoRetryOnConnectionFailureFromParam,omitempty"`
  }
  ```
  and add `Retry *Retry` to `PolicyDefinition` (alongside the existing `Parameters *map[string]interface{}` field at line 27).
- [X] T003 [P] Add `UpstreamRetryContribution` struct (`PolicyName string`, `StatusCodes []int`, `MaxAttempts int`, `PerAttemptTimeoutMs int`, `AlsoRetryOnConnectionFailure bool` — the already-resolved flat form, per data-model.md) to `gateway/gateway-controller/pkg/models/runtime_deploy_config.go`, and add `RetryContributions []UpstreamRetryContribution` to `RouteUpstream` (line 141, alongside the existing `Failover *RouteFailover` field at line 155).
- [X] T004 Extend the `PolicyVersionResolver` interface in `gateway/gateway-controller/pkg/utils/policy_version_resolver.go` with `Definition(name, version string) (*models.PolicyDefinition, bool)`, and implement it on `StaticPolicyVersionResolver` by looking up its already-held `policyDefinitions` map (the same map `NewLoadedPolicyVersionResolver` already stores for `Resolve`) — keyed the same way `Resolve` already keys it; confirm the exact key shape (name alone vs. `name+"|"+version`) by reading `NewLoadedPolicyVersionResolver`'s existing map construction before writing the lookup. [depends on T002 for the `models.PolicyDefinition` return type]
- [X] T005 Add `retry` policy-load-time validation to `validatePolicy` in `gateway/gateway-controller/pkg/utils/policy_loader.go` (point of the existing definition-shape checks, ~line 226), implementing every rule in data-model.md's "Validation Rules" section verbatim:
  1. Reject if more than one mode is set for the same condition (`statusCodes`+`statusCodesFromParam`; more than one of `maxAttempts`/`maxAttemptsFromParam`/`maxAttemptsFromLongestList`; `alsoRetryOnConnectionFailure`+`...FromParam`).
  2. Reject if any `...FromParam` value, or `maxAttemptsFromLongestList.paramPath`, or `enabledByParam`, names a parameter absent from that policy's own `Parameters` JSON schema.
  3. Reject on a schema-type mismatch between a `...FromParam` reference and the field it feeds (array-of-integer / integer / boolean per data-model.md Rule 3).
  4. Reject if `retry` is present but neither `statusCodes` nor `statusCodesFromParam` is set (status codes are never optional), or neither of the three `maxAttempts` modes is set.
  5. Reject a *fixed* `maxAttempts < 1`, a *fixed* `perAttemptTimeoutMs < 0`, or a *fixed* `maxAttemptsFromLongestList.plus < 0`.
  Return a `models.PolicyDefinitionError`-shaped error (match the existing error style `validatePolicy` already uses for its other checks) naming the offending field.
- [X] T006 [Test] [P] Unit test every rule from T005 (one rejection case per numbered rule above, plus one "valid, fixed mode," one "valid, derived mode," and one "valid, `maxAttemptsFromLongestList`" acceptance case). **Deviation from the original plan**: written in a new `gateway/gateway-controller/pkg/utils/retry_validation_test.go` calling `validateRetry` directly with in-memory `models.PolicyDefinition` fixtures, rather than `policy_loader_test.go`'s YAML-fixture-directory pattern — these are pure structural-validation cases with no file I/O to exercise, so a direct call is simpler and faster without losing coverage. [depends on T005]
- [X] T007 Implement the per-attachment resolution function in new file `gateway/gateway-controller/pkg/transform/upstream_retry_contribution.go`: `func resolveUpstreamRetryContribution(def *models.Retry, policyName string, instanceParams map[string]interface{}, paramSchema map[string]interface{}) *models.UpstreamRetryContribution` (returns `nil` if `def` is `nil`). Resolve `enabledByParam` first (per data-model.md's three-step order: instance param → schema default → `Retry`'s own default of "always on"); if it resolves `false`, return `nil` immediately — no other field is resolved. Otherwise resolve every remaining field using the same three-step order, including `maxAttemptsFromLongestList` (read the list at `paramPath` from `instanceParams`, take its length, add `plus`; if the param is absent or not list-shaped, fall through to step 3 of the resolution order — the field's own default — rather than erroring).
- [X] T008 [Test] [P] Unit test T007 directly: one case per resolution-order step for a plain `...FromParam` field (instance value present; instance absent + schema default present; both absent → `Retry`'s own default); the `enabledByParam` short-circuit (resolves `false` → function returns `nil`, no partial contribution); `maxAttemptsFromLongestList` resolving correctly from a 3-element and a 1-element list (quickstart.md case 9); and the quickstart.md case-7 "two routes, two different resolved values from the same policy definition" case (same `def`, two different `instanceParams`, assert two different resulting `StatusCodes`) — in `gateway/gateway-controller/pkg/transform/upstream_retry_contribution_test.go`. [depends on T007]
- [X] T009 Hook T007's resolver into `RestAPITransformer`'s per-route policy-chain construction in `gateway/gateway-controller/pkg/transform/restapi.go` (after `buildPolicyChain`/`sdkChainToModel` at lines 232-235). **Implementation note — two things diverged from the original task text, both verified in code, not guessed**: (1) no `PolicyVersionResolver`/T004's `Definition()` was needed here — `RestAPITransformer` already holds the raw `policyDefinitions map[string]models.PolicyDefinition` directly as a struct field; (2) a real bug caught by the integration test below: `PolicyInstance.Version` on each injected instance is **major-only** (`collectAPIPolicies`/`buildPolicyChain` deliberately store `versionutil.MajorVersion(resolved)`, not the full semver), so a naive `t.policyDefinitions[inst.Name+"|"+inst.Version]` lookup always missed — fixed by re-resolving through the same `config.ResolvePolicyVersion(t.policyDefinitions, t.latestVersions, inst.Name, inst.Version)` helper `collectAPIPolicies` itself already uses, before the lookup. Covered by 3 new tests in `pkg/transform/upstream_retry_restapi_test.go`.
- [X] T010 **Discovered to require zero new code, same pattern as T022** — traced `pkg/transform/llm.go`'s `LLMTransformer.Transform` function in full: Step 3 converts the LLM/MCP source config into a genuine `api.RestAPI` and calls `t.restTransformer.Transform(tempCfg)` — the exact same `RestAPITransformer.Transform` method T009 modified, with no separate policy-chain-building logic of its own for `llm.go` to duplicate. `TestRestAPITransformer_RetryContributionPopulatedFromAttachedPolicy` (T009) therefore already proves LLM/MCP route coverage by construction; `applyModelFailoverPolicyToRoutes` (Step 5) runs afterward and only adds `RouteFailover`, never touching `RetryContributions`. research.md's "no shared hook point" correction note was wrong and has been superseded by this finding.
- [X] T011 Implement the generic merge function in new file `gateway/gateway-controller/pkg/xds/upstream_retry.go`: `func mergeUpstreamRetryPolicy(contributions []models.UpstreamRetryContribution, failover *models.RouteFailover) *route.RetryPolicy` (or equivalent go-control-plane type already imported in `translator.go`), implementing data-model.md's "Merged Route Retry Policy" rules exactly:
  - `RetryOn` = `"5xx"`/`"retriable-status-codes"` plus `connect-failure,reset,reset-before-request` if any contributor's `AlsoRetryOnConnectionFailure` is true or `failover` is present.
  - `RetriableStatusCodes` = union of every contributor's `StatusCodes` plus `failover`'s own retriable codes when present.
  - Envoy `NumRetries` = `max(every contributor's MaxAttempts, failover's derived total attempts) − 1`. **This `−1` translation happens exactly once, here** — nowhere upstream of this function works in Envoy's "retries excluding the first send" convention; every contributor's own `MaxAttempts` is total sends.
  - Envoy `PerTryTimeout` = `min` across every contributor's resolved `PerAttemptTimeoutMs` (skip contributors that left it unset) and `failover`'s own per-try timeout when present — the **tightest**, not loosest, value (research.md Decision 7). If no contributor and no `failover` set a timeout, leave Envoy's `PerTryTimeout` unset.
  - Return `nil` if both inputs are empty/nil. [depends on T003]
- [X] T012 [Test] [P] Unit test T011 directly (not through the full translator) covering: empty inputs → nil; one contribution alone (verify the `MaxAttempts − 1` translation, e.g. `MaxAttempts: 2` → `NumRetries: 1`); `failover` alone (regression — must match what `translator.go` builds today for a failover-only route); contribution + failover together (union, not override); two contributions with different `MaxAttempts` (union of codes, `NumRetries` from the max, minus one); two contributions with different `PerAttemptTimeoutMs` (merged value is the **minimum**, per quickstart.md case 5) — in `gateway/gateway-controller/pkg/xds/upstream_retry_test.go`. [depends on T011]
- [X] T013 Broaden the `RetryPolicy` construction gate in `gateway/gateway-controller/pkg/xds/translator.go` (~line 401) from `if rdcRoute.Upstream.Failover != nil` to `if rdcRoute.Upstream.Failover != nil || len(rdcRoute.Upstream.RetryContributions) > 0`, and replace the existing inline `RetryPolicy` construction with a call to T011's `mergeUpstreamRetryPolicy(rdcRoute.Upstream.RetryContributions, rdcRoute.Upstream.Failover)`. [depends on T011]

**Checkpoint**: Foundation ready — every user story below is now a test-and-fixture exercise against an already-complete pipeline.

---

## Phase 3: User Story 1 - Zero-extra-config retry for a single contributing policy (Priority: P1) 🎯 MVP

**Goal**: A route with exactly one policy declaring `retry` and no failover chain gets a working `RetryPolicy` generated purely from that policy's own definition.

**Independent Test**: Attach a policy declaring a retry condition to a route with nothing else retry-related configured; trigger the condition; confirm an automatic retry with no additional config.

### Tests for User Story 1

- [X] T014 [P] [US1] Unit test — written in new file `gateway/gateway-controller/pkg/xds/upstream_retry_translator_test.go` (not `translator_test.go`, to avoid merge risk editing that large existing file) as `TestTranslateRuntimeConfig_NoRetrySources_NoRetryPolicy`: route with `Upstream.Failover == nil` and `Upstream.RetryContributions` empty/nil → translated `RouteConfig`'s `RetryPolicy` is `nil` (quickstart.md case 1).
- [X] T015 [P] [US1] Unit test — same file, `TestTranslateRuntimeConfig_SingleContribution_NoFailover_GeneratesRetryPolicy`: route with one `UpstreamRetryContribution{StatusCodes: [401], MaxAttempts: 2}`, no `Failover` → `RetryPolicy.RetryOn` includes `retriable-status-codes`, `RetriableStatusCodes == [401]`, Envoy `NumRetries == 1` (quickstart.md case 2).

### Implementation for User Story 1

- [X] T016 [US1] Added to `oauth2-generator`'s `policy-definition.yaml` at `/Users/thenujan/Desktop/Git-Repos/gateway-controllers/policies/oauth2-generator/policy-definition.yaml` (separate repo, branch `fix/3413-guardrail-findings`):
  ```yaml
  retry:
    statusCodes: [401]
    maxAttempts: 2
  ```
  **Correction**: the policy's actual purge-on-401 param is named `tokenPurgeStatusCodes`, not `purgeStatusCodes` as earlier design discussion assumed — doesn't affect this task since fixed mode doesn't reference it, but noted for accuracy. YAML validated (`python3 -c "import yaml; ..."` parses cleanly). **Left uncommitted** — this is a separate, actively-developed repo (not this worktree); committing/pushing there is the user's call, not done automatically here.
- [ ] T017 [US1] Live validation per quickstart.md's "Live, end-to-end validation" steps 1-4 — **not run in this session**: requires the docker-compose gateway stack up with a mock backend, and per standing session preference the user runs all gateway docker builds/restarts personally. Exact steps are in quickstart.md; `gateway/dev-policies/FAILOVER_TESTING.md` has the mock-backend pattern to reuse, scoped to a single target instead of a chain.

**Checkpoint**: User Story 1 fully functional and independently testable — this alone is the MVP (spec SC-001).

---

## Phase 4: User Story 2 - Coexists safely with the existing failover mechanism (Priority: P2)

**Goal**: A route combining a failover chain and a separately-attached contributing policy gets the union of both, neither silently dropped; a failover-only route is byte-for-byte unaffected.

**Independent Test**: Configure both mechanisms on one route; trigger each condition independently; confirm both still retry.

### Tests for User Story 2

- [X] T018 [P] [US2] Unit test — `upstream_retry_translator_test.go`, `TestTranslateRuntimeConfig_ContributionAndFailover_BothCoverageKept`: route with both a `Failover` chain and one `UpstreamRetryContribution` carrying a different status code → merged `RetriableStatusCodes` contains both, transport conditions preserved (quickstart.md case 3; spec FR-007/Acceptance Scenario 1).
- [X] T019 [P] [US2] Unit test — `upstream_retry_translator_test.go`, `TestTranslateRuntimeConfig_FailoverOnlyRoute_Unaffected`: route with only a `Failover` chain, zero `RetryContributions` → translated `RetryPolicy` matches pre-feature output exactly (spec Acceptance Scenario 2).

### Implementation for User Story 2

- [ ] T020 [US2] Re-run the existing `gateway/dev-policies/run-failover-e2e.sh` suite — **not run in this session** (same docker-stack dependency as T017; user runs gateway docker builds/restarts personally). Strong unit-level evidence already in place in its absence: T019's full-translator test and the complete `pkg/xds` suite (37 tests, all green, zero regressions from this feature's baseline) cover the same failover-route RetryPolicy shape this e2e suite exercises live — T020 is the live confirmation layer on top of that, not the only evidence.

**Checkpoint**: User Stories 1 and 2 both independently functional; no regression on existing model-failover routes.

---

## Phase 5: User Story 3 - Multiple policies on one route each contribute (Priority: P3)

**Goal**: Two independently-attached policies, each declaring a different retry condition, combine into one coherent route retry behavior.

**Independent Test**: Attach two policies with different declared conditions to one route; trigger each independently; confirm both retry.

### Tests for User Story 3

- [X] T021 [P] [US3] Unit test — `upstream_retry_translator_test.go`, `TestTranslateRuntimeConfig_TwoContributions_UnionHonored`: route with two `UpstreamRetryContribution`s from different policies, different `StatusCodes`/`MaxAttempts` → merged policy is the union/max of both (quickstart.md case 4; spec Acceptance Scenarios US3.1/US3.2).

### Implementation for User Story 3

- [X] T022 [US3] Confirmed: no new production code was needed — T009/T010/T011 already handle an arbitrary number of contributions generically (no per-count special-casing exists anywhere in the merge loop). T021 passed on first run with zero fixes.

**Checkpoint**: All three user stories independently functional.

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: Final validation sweep, not new behavior.

- [X] T023 [P] `go vet ./...` — clean, zero findings. `gofmt -l` — all 13 new/modified files clean (pre-existing unrelated files elsewhere in the module show gofmt drift, untouched by this feature, left as-is). `golangci-lint` could not run in this environment (installed binary built with Go 1.25, this module targets Go 1.26.5 — a pre-existing local toolchain mismatch, not fixed here per the same reasoning as the T001 CGO/clang note).
- [X] T024 Walked quickstart.md top to bottom. All 9 unit-level cases confirmed covered by passing tests: cases 1-4 → `upstream_retry_translator_test.go` + `upstream_retry_test.go`; case 5 → `TestMergeUpstreamRetryPolicy_PerAttemptTimeout_*`; case 6 → all 13 tests in `retry_validation_test.go`; case 7 → `TestResolveUpstreamRetryContribution_SameDefinitionTwoAttachments_ResolvesIndependently`; case 8 → `TestResolveUpstreamRetryContribution_EnabledByParam_ResolvesFalse_ReturnsNil` + the `restapi.go`-level equivalent; case 9 → `TestResolveUpstreamRetryContribution_MaxAttemptsFromLongestList`. Live scenarios 1-5 not run this session (T017/T020). Every file path cited in this tasks.md matches what was actually implemented, with two documented corrections (T009's note, T010's note).
- [X] T025 [P] Added a `retry:` pointer to `gateway/dev-policies/README.md` (in this worktree) alongside its existing `policy-definition.yaml` mention, linking to `contracts/policy-definition-schema.md`. Left `gateway-controllers/README.md` untouched — that's a separate, actively-developed repo and T016's edit there was already the minimum needed change; adding more there wasn't requested and increases surface in a repo this session doesn't own.

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies.
- **Foundational (Phase 2)**: Depends on Setup. BLOCKS every user story — T002/T003 (types) block T004/T007/T009/T010/T011; T004 and T007 block T009/T010; T011 blocks T013. This phase is unusually load-bearing for this feature: even US1 (a single contribution) needs the complete resolve→populate→merge→gate chain, not a subset of it.
- **User Stories (Phase 3-5)**: All depend on Foundational completion. US1/US2/US3 are independent of each other (no story's tasks modify a file another story's tasks also modify) and can proceed in any order or in parallel once Phase 2 is done — though P1→P2→P3 is the natural sequence since the spec itself orders them by motivating priority.
- **Polish (Phase 6)**: Depends on all three user stories.

### Within Foundational

T002, T003 → T004, T007 (type dependency) → T005 (independent of T004/T007, only needs T002) → T006 (needs T005) → T008 (needs T007) → T009 (needs T002/T003/T004/T007) → T010 (needs T009's established pattern) → T011 (needs T003) → T012 (needs T011) → T013 (needs T011).

### Parallel Opportunities

- T002 and T003 (different files: `policy_definition.go` vs. `runtime_deploy_config.go`).
- T006, T008, T012 (each a pure test file for an already-independent unit, once its implementation task lands).
- T014/T015, T018/T019, T021 within their own story phases (same file, but non-overlapping test functions — verify no shared fixture mutation before running literally in parallel).
- T023 and T025 in Polish.

---

## Parallel Example: Foundational Phase

```bash
# After T002/T003 land, these can proceed together:
Task: "Extend PolicyVersionResolver with Definition() in pkg/utils/policy_version_resolver.go"      # T004
Task: "Implement resolveUpstreamRetryContribution in pkg/transform/upstream_retry_contribution.go"   # T007
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Phase 1: Setup.
2. Phase 2: Foundational — this is the bulk of the real work; US1 adds almost no new production code beyond T016's one-line `policy-definition.yaml` change.
3. Phase 3: User Story 1.
4. **STOP and VALIDATE**: run T017's live scenario. This alone delivers spec SC-001.

### Incremental Delivery

1. Setup + Foundational → pipeline ready, provably correct via T006/T008/T012.
2. US1 → oauth2-generator gets working retry-on-401 → demo-able (MVP).
3. US2 → confirm the one existing production feature (model-failover) that could regress, doesn't.
4. US3 → confirm the merge generalizes past exactly one contributor (expected to require zero new code — T022 is a verification task, not a build task).

---

## Notes

- Foundational is front-loaded by design: the spec's own three user stories differ only in *how many contributions exist on a route*, not in the mechanism that resolves or merges them — so the mechanism itself can't be partially built story-by-story without rework. `enabledByParam` and `maxAttemptsFromLongestList` live entirely in Foundational (T002, T007, T008) rather than a story phase, since no user story's acceptance scenario specifically exercises them — they're schema capabilities proven correct once, at the mechanism level, per quickstart.md cases 8-9.
- T010's exact hook line in `llm.go` is marked "confirm at implementation time" rather than guessed — this session traced `llm.go`'s model-failover call site precisely (line 115) but did not pin the equivalent generic policy-chain-walk line; do not fabricate a line number, verify it during T010 itself.
- The `MaxAttempts − 1` → Envoy `NumRetries` translation (research.md Decision 5) happens in exactly one place: T011's merge function. No task upstream of T011 should ever compute or store an Envoy-style "retries excluding the first" number — every `UpstreamRetryContribution.MaxAttempts` is total sends throughout T002-T010.
- Commit after each task per this repo's established `feat(upstream-retry): ...`/`test(upstream-retry): ...` style (matching the branch's existing `feat(llm-failover): ...` convention visible in `git log`).

---

description: "Task list for safe PII masking and restore for agent traffic"
---

# Tasks: Safe PII Masking and Restore for Agent Traffic

**Input**: Design documents from `specs/001-pii-masking-agent-traffic/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: Included. The plan's quickstart requires unit tests, a regression guard and integration scenarios for every item.

**Organization**: Grouped by user story. US1 to US5 map to the stories in spec.md. Items 1 to 9 of the issue map to stories as: US1 = items 1, 2, 8; US2 = items 3, 5, 6; US3 = item 4; US4 = item 7; US5 = item 9.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependency on an unfinished task)
- **[Story]**: User story the task belongs to

## Path Conventions

The code is in the **gateway-controllers** repository. `POLICY/` below means `gateway-controllers/policies/pii-masking-regex/`. `SDK/` means `api-platform/sdk/core/`. Spec artifacts stay in `api-platform/specs/001-pii-masking-agent-traffic/`.

Do not run gateway docker builds or restarts as part of these tasks. Where one is needed, the task says to hand the commands to the owner.

---

## Phase 1: Setup

**Purpose**: Branch, baseline and fixtures

- [X] T001 In the gateway-controllers repo, create a branch off the tag `policies/pii-masking-regex/v1.1.0` named `pii-masking-agent-traffic-v1.2.0`. Confirm `git ls-tree` shows `POLICY/piimaskingregex.go` with no `jsonscan.go`, `maskengine.go`, `scope.go` or `streaming.go`.
- [X] T002 Run `go test ./...` in `POLICY/` on the untouched branch and record the pass count as the baseline.
- [X] T003 [P] Create regression fixtures in `POLICY/testdata/regression/`: at least 15 request bodies that are valid JSON on v1.1.0 and contain emails, phones and SSNs in user text, tool results, system prompt and tool descriptions, plus a `expected_masked_values.json` listing the original values v1.1.0 masks for each. Generate the expected list by running the v1.1.0 code, not by hand.
- [X] T004 [P] Create Anthropic Messages fixtures in `POLICY/testdata/messages/`: a request with a history of user text, `tool_use`, `tool_result` and thinking blocks; a tool with `"maximum": 9007199254740991`; and SSE streams with `text_delta`, `input_json_delta`, `thinking_delta` and two text blocks in one flush.

---

## Phase 2: Foundational (blocks all user stories)

**Purpose**: Shared mask engine, placeholder format and JSON-aware scanner that every story uses

- [X] T005 Create `POLICY/mask.go` with the placeholder builder. Placeholder text MUST match `^\[[A-Z_]+_[0-9a-f]{8}\]$` and be `[` + entity name + `_` + the first 32 bits of SHA-256 over entity name, a NUL byte and the original value, as 8 lowercase hex digits + `]`. No counter and no map iteration may influence it.
- [X] T006 In `POLICY/mask.go`, move entity matching out of `maskPIIFromContent` so matches are collected per entity, de-duplicated by original value, and applied longest-first as in v1.1.0. Keep skipping matches that already have the placeholder shape.
- [X] T007 Update the placeholder shape constants in `POLICY/piimaskingregex.go`: the request-side check and `placeholderTailPattern` MUST accept exactly 8 hex digits (`[0-9a-f]{0,8}` for a partial placeholder). Keep `maxPlaceholderTailBytes` at 64.
- [X] T008 [P] Create `POLICY/jsonmask.go` with a byte-preserving scanner: given a valid-JSON body and a callback, find string tokens, skip tokens that are object keys (a string followed after optional whitespace by `:`), decode each value token, call the callback, and re-encode only tokens whose text changed. All other bytes MUST be copied unchanged. Expose the current normalized path (`$['a'][0]['b']`) to the callback so selection can be added later.
- [X] T009 [P] Add `POLICY/jsonmask_test.go` for the scanner: keys never masked; numbers such as `9007199254740991` byte-identical; whitespace and key order identical; escapes `\n \t \r < \\ \"` before a value; surrogate pairs; nested arrays and objects; empty strings; a body with no changes returns the input slice unchanged.
- [X] T010 [P] Add `POLICY/mask_test.go`: placeholder shape, same value gives same text across 100 calls, different entities give different text for the same value, 8-digit shape accepted by the tail pattern, old 4-digit shape no longer treated as a placeholder.
- [X] T011 Update existing tests in `POLICY/piimaskingregex_test.go` and `POLICY/piimaskingregex_stream_test.go` that assert `[EMAIL_0000]` style placeholders so they build placeholders through the new function. Do not weaken any assertion. (only one existing assertion needed the new 8-digit shape; old 4-digit placeholders in stream fixtures still restore)

**Checkpoint**: `go test ./...` passes in `POLICY/` with the new placeholder format.

---

## Phase 3: User Story 1 - Valid masked requests, with optional user/tool scoping (Priority: P1) 🎯 MVP

**Goal**: Every masked request reaches the provider as valid JSON, only string text changes, everything masked today stays masked, and operators can scope masking to user and tool content.

**Independent Test**: With `jsonPath: "$"`, send bodies with an email after `\n`, a tool schema with a large number, and an email in system and tool text. Output is valid, numbers untouched, all three emails still masked. With the scoped path, only user and tool emails are masked.

### Tests for User Story 1

- [X] T012 [P] [US1] Add scenarios to `POLICY/piimaskingregex_test.go`: email after `\n`, `\t`, `\r`, `<` produce valid JSON with the escape intact (item 1); phone on with `"maximum": 9007199254740991` leaves the number byte-identical (item 2); system prompt and tool description emails are still masked under `"$"` (FR-003).
- [X] T013 [P] [US1] Add `POLICY/regression_test.go` that replays every fixture in `POLICY/testdata/regression/` and asserts each value listed in `expected_masked_values.json` is still masked (SC-009), except matches that sit inside a longer digit run.
- [X] T014 [P] [US1] Add a `.feature` scenario per item 1, 2 and 8 to `POLICY/it/features/pii_masking_regex.feature` against a mock Anthropic upstream that records the body it receives. (written; NOT run: needs the Docker gateway, see results.md)

### Implementation for User Story 1

- [X] T015 [US1] In `POLICY/piimaskingregex.go` `processRequestBody`, when `jsonPath` is empty or `"$"` and `json.Valid` passes, mask through the `jsonmask.go` scanner so every string value is masked and nothing else is changed. When the body is not valid JSON, keep the existing plain-text path.
- [X] T016 [US1] After masking in `POLICY/piimaskingregex.go`, run `json.Valid` on a JSON body. If it fails, return `buildErrorResponse` with a message that names the failure and do not forward the body (FR-005). (guard in place; no input found that triggers it, covered by review only)
- [X] T017 [P] [US1] In `api-platform` `SDK/utils/jsonpath.go`, add an RFC 9535 evaluator `SelectNormalizedPaths(payload []byte, expr string) ([]string, error)` that supports child, wildcard `[*]`, index, slice, union, filter `[?()]` with `==`, `!=`, `&&`, `||`, and descendant `..`. It MUST be read-only and MUST return normalized paths like `$['messages'][0]['content'][1]['text']`. Add unit tests in `SDK/utils/jsonpath_test.go`. This is wso2/api-platform#3571. (done in `api-platform/sdk/core/utils/jsonpath_rfc9535.go` on this branch, untracked; it must land on the SDK release line)
- [ ] T018 [US1] Release the SDK change as a new `sdk/core` tag by handing the owner the exact tag command. Do not push. Pin the policy `go.mod` to it in T019.
- [ ] T019 [US1] In `POLICY/go.mod`, require the new `sdk/core` version from T018. Verify the build with `go build ./...`. (Note: with `go.work` present, also confirm a build with `GOWORK=off` so a missing replace is not masked.)
- [X] T020 [US1] In `POLICY/piimaskingregex.go` `parseParams`, accept any RFC 9535 `jsonPath` and keep the default `$.messages[-1].content`. Single-node paths MUST behave exactly as in v1.1.0 (same extraction, same error messages).
- [X] T021 [US1] In `POLICY/piimaskingregex.go` `processRequestBody`, for a many-match path call `SelectNormalizedPaths` once per request, build the selection set (data-model.md "Selection set"), and give it to the scanner so a string token is masked only if its normalized path is in the set. Matched non-string nodes are skipped. If the path matches nothing, pass the request through unmasked as today.
- [X] T022 [US1] Add scoped-mode tests to `POLICY/piimaskingregex_test.go` and `POLICY/jsonmask_test.go`: user text and tool-result emails masked; `system`, `tools` and tool description emails untouched and byte-identical; numbers in the body byte-identical (item 8, SC-002).

**Checkpoint**: US1 is demonstrable on its own: valid, scoped, regression-safe request masking.

---

## Phase 4: User Story 2 - Streamed answers fully restored (Priority: P1)

**Goal**: Tool arguments, thinking and each text block are restored in streamed and buffered responses.

**Independent Test**: Feed `POLICY/testdata/messages` streams to the policy and compare to the original values.

### Tests for User Story 2

- [X] T023 [P] [US2] Add `POLICY/restore_test.go`: `input_json_delta` restores the original (item 3) including an original that contains a quote or backslash; `thinking_delta` restores (item 5); two text blocks in one flush each keep their own text and the second is not empty (item 6); a placeholder split over 5 events is restored; a stream ending mid-placeholder leaks no fragment; `signature_delta` untouched.
- [X] T024 [P] [US2] Add a restore parity test: the same content streamed and non-streamed yields identical restored text, tool arguments and thinking (FR-010).
- [ ] T025 [P] [US2] Add `.feature` scenarios for items 3, 5 and 6 to `POLICY/it/features/pii_masking_regex.feature` using a mock upstream that streams the fixtures.

### Implementation for User Story 2

- [X] T026 [US2] Create `POLICY/restore.go` and move `restoreSSEChunk`, `extractSSEDeltaContent` and the replace helpers there without behavior change, so later edits are small. Existing OpenAI-shape stream tests MUST still pass. (restore logic moved to `restore.go`; the shared helpers stayed in `piimaskingregex.go`)
- [X] T027 [US2] In `POLICY/restore.go`, extend `DefaultStreamingJSONPaths` handling with two kinds: `$.delta.partial_json` (tool arguments) and `$.delta.thinking`, and record the delta kind and the event's `index` with each extracted fragment.
- [X] T028 [US2] In `POLICY/restore.go`, replace the "first content event gets everything" redistribution with grouping by (`index`, delta kind): join fragments inside a group, restore, write into that group's first event, drop only that group's other text events. Shapes without an `index` form a single group, preserving today's behavior.
- [X] T029 [US2] In `POLICY/restore.go`, restore `partial_json` fragments with the original JSON-string-escaped (use `json.Marshal` and strip the outer quotes) before the outer event encoding, per research R5.
- [X] T030 [US2] Update `NeedsMoreResponseData` and `extractSSEDeltaContent` in `POLICY/restore.go` so the hold check looks at the joined text of every group (text, tool arguments, thinking), not only text.
- [X] T031 [US2] Confirm the buffered path (`processResponseBody`) restores tool arguments and thinking in non-streamed Messages bodies and add a test if it is missing.

**Checkpoint**: US2 passes on its own, and US1 tests still pass.

---

## Phase 5: User Story 3 - Stable placeholders (Priority: P1)

**Goal**: Same value, same placeholder, across requests, turns and entity mixes.

**Independent Test**: 20 identical requests with email and phone give one distinct upstream body.

### Tests for User Story 3

- [X] T032 [P] [US3] Add to `POLICY/mask_test.go`: 20 identical requests with `email` and `phone` on produce 1 distinct body (SC-004); a 10-turn growing history keeps each earlier turn's masked bytes identical (SC-005); repeated value gets one placeholder; two different values of one type get different placeholders.
- [X] T033 [P] [US3] Add a collision test in `POLICY/mask_test.go` with a hash function injected to force a collision: both values are masked, placeholders differ, and the assignment is the same regardless of input order.
- [ ] T034 [P] [US3] Add a `.feature` scenario for item 4 that sends the same body 20 times and asserts one distinct upstream body.

### Implementation for User Story 3

- [X] T035 [US3] In `POLICY/mask.go`, add deterministic collision resolution: when two different values in one request map to the same placeholder, the value that sorts later by byte order takes the next 32-bit suffix by re-hashing with a counter byte, repeating until unique. Results MUST NOT depend on map iteration order or on request position.
- [X] T036 [US3] Remove the counter and any map-order dependence left in `maskPIIFromContent` in `POLICY/piimaskingregex.go`, then confirm with `go test -count=50 ./...` that results do not flake.

**Checkpoint**: US3 passes on its own.

---

## Phase 6: User Story 4 - Several instances on one route (Priority: P2)

**Goal**: Instances do not overwrite each other's mapping.

**Independent Test**: Two instances (email, phone), a request with both values, streamed and not.

### Tests for User Story 4

- [X] T037 [P] [US4] Add to `POLICY/piimaskingregex_test.go`: two policy objects sharing one metadata map, each masking its own entity; a response restore through either object returns both original values, in buffered and streamed paths (item 7, SC-006).
- [ ] T038 [P] [US4] Add a `.feature` scenario for item 7 with two policy instances on one route.

### Implementation for User Story 4

- [X] T039 [US4] In `POLICY/piimaskingregex.go`, change the metadata write at `MetadataKeyPIIEntities` to merge into an existing `map[string]string` (keep an entry already present for the same original) instead of assigning. A redacting instance and a request with no matches MUST write nothing. Keep the key name `piimaskingregex:pii_entities` and the value type unchanged.
- [X] T040 [US4] Confirm single-instance behavior is unchanged by running T013 (regression) and the full unit suite.

---

## Phase 7: User Story 5 - Default patterns stop cutting long numbers (Priority: P3)

**Goal**: No match inside a longer digit run. Standalone phone-shaped numbers stay masked.

**Independent Test**: A tool result with a 13-digit timestamp, a standalone `4155550199`, and a formatted phone.

### Tests for User Story 5

- [X] T041 [P] [US5] Add to `POLICY/mask_test.go`: `1795104230191` is untouched (item 9); `4155550199` and `2147483647` standing alone are still masked (FR-017); `(415) 555-0199` and `+1 415-555-0199` still masked; an SSN-shaped run inside a longer digit run is untouched; a custom pattern is not affected by the digit check (FR-019).
- [X] T042 [P] [US5] Add a `.feature` scenario for item 9. (written; NOT run)

### Implementation for User Story 5

- [X] T043 [US5] In `POLICY/mask.go`, add a `digitBoundary` flag to the built-in `PHONE` and `SSN` entities and, after `FindAllStringIndex`, discard any match whose preceding byte is an ASCII digit (data-model.md "Entity detector"). Do not apply it to custom entities.

---

## Phase 8: Polish and Cross-Cutting

- [X] T044 Update `POLICY/policy-definition.yaml`: `version: v1.2.0` and the `jsonPath` description to cover `"$"` behavior and many-match RFC 9535 paths. Keep every parameter name, type and default unchanged.
- [X] T045 [P] Update the policy documentation in the gateway-controllers docs (the pii-masking-regex page) per `contracts/policy-parameters.md`: the `"$"` behavior, the user-and-tool-content example, that standalone phone-shaped numbers including bare 10-digit ones are masked and how a custom pattern narrows this, and the placeholder format change (FR-021).
- [X] T046 [P] Write release notes for v1.2.0 listing the deliberate changes: placeholder format, no matches inside longer digit runs, invalid masking now returns an error (FR-020). (written as the "What changed in v1.2" section of the v1.2 policy doc)
- [ ] T047 Run the full quickstart in `specs/001-pii-masking-agent-traffic/quickstart.md` steps 1 to 4 and record results in `specs/001-pii-masking-agent-traffic/checklists/results.md`.
- [X] T048 Run `go vet ./...`, `gofmt -l .` and `go test -race -count=3 ./...` in `POLICY/`.
- [ ] T049 Hand the owner the commands to tag `policies/pii-masking-regex/v1.2.0` and to bump the module reference in the gateway patch branch and rebuild images (quickstart step 5). Do not run builds, restarts or pushes.

---

## Dependencies and Execution Order

- **Setup (1)** first. T003 and T004 are parallel.
- **Foundational (2)** blocks every story. T008, T009 and T010 are parallel once T005 and T006 exist.
- **US1** starts after Phase 2. The SDK tasks T017 and T018 can start any time after Phase 1 and are the only cross-repo dependency. Without them, T015 and T016 (the `"$"` half of US1) can ship, and T020 to T022 (scoping) wait for T018.
- **US2** starts after Phase 2 and is independent of US1.
- **US3** needs T005 and T006 from Phase 2 and is otherwise independent.
- **US4** needs nothing beyond Phase 2.
- **US5** needs `mask.go` from Phase 2.
- **Polish (8)** after the stories you ship.

Story order by priority: US1, US2, US3 (all P1), then US4 (P2), US5 (P3).

## Parallel Examples

- Phase 1: T003 and T004.
- Phase 2: T008, T009, T010 together.
- US1 tests T012, T013, T014 together, and T017 (SDK) in parallel with T015 and T016.
- US2 tests T023, T024, T025 together.
- After Phase 2, one person per story can work on US2, US3, US4 and US5 at the same time because they touch `restore.go`, `mask.go` and `piimaskingregex.go` in different places. Expect small merge conflicts in `piimaskingregex.go`.

## Implementation Strategy

- **MVP**: Phases 1, 2 and US1 without scoping (T015, T016 plus tests). That removes the two request-breaking failures (items 1 and 2) while keeping all current masking.
- **Increment 2**: US2 and US3 (the rest of the P1 items), which together make Messages agent traffic usable.
- **Increment 3**: US1 scoping (T017 to T022) once the SDK tag exists, then US4 and US5.
- Ship v1.2.0 only after the whole set passes, since the cloud service needs all nine items.

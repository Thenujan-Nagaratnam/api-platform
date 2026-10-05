---
description: "Task list for the openai-error-format fault policy"
---

# Tasks: OpenAI-Compatible Error Responses for LLM APIs

**Input**: Design documents from `specs/001-llm-openai-compatible-errors/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md

**Tests**: Included. plan.md (Technical Context → Testing) requires table-driven unit tests and a gateway IT feature, and every spec story defines an Independent Test.

**Organization**: Tasks are grouped by user story so that each story can be implemented and verified on its own.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: the spec user story (US1–US5)

## Path conventions

| Alias | Absolute path |
|---|---|
| `GC/` | `/Users/thenujan/Desktop/Git-Repos/gateway-controllers-worktrees/openai-error-format/` (new worktree; **source of truth**) |
| `POL/` | `GC/policies/openai-error-format/` |
| `AP/` | `/Users/thenujan/Desktop/Git-Repos/api-platform/` (branch `001-llm-openai-compatible-errors`) |
| `MIR/` | `AP/gateway/dev-policies/openai-error-format/` (gitignored mirror; diff-checked, never committed) |

**Ground rules for every task**:
- No edits under `AP/gateway/gateway-controller/`, `AP/gateway/gateway-runtime/` or `AP/sdk/` (spec FR-001).
- Don't run docker image builds or restarts. Give the user the exact command instead.
- Don't commit or push unless the user asks. Commit messages never get a Co-Authored-By trailer.
- No `// TODO` or `FIXME` deferrals in code.

---

## Phase 1: Setup (shared infrastructure)

**Purpose**: an isolated workspace and an empty, buildable module.

- [X] T001 Create a new worktree `GC/` on a new branch `openai-error-format` from `upstream`/`wso2` `main` of gateway-controllers. Do not use the main checkout at `/Users/thenujan/Desktop/Git-Repos/gateway-controllers`: it has 44 uncommitted changes on `fix/3413-guardrail-findings` (research R2). Run `git fetch` for the wso2 remote first; add it if it's missing.
- [X] T002 Create `POL/go.mod`:
  - `module github.com/wso2/gateway-controllers/policies/openai-error-format`, `go 1.26.2`.
  - `require github.com/wso2/api-platform/sdk/core` at the current pseudo-version.
  - `replace github.com/wso2/api-platform/sdk/core => ../../../api-platform/sdk/core`, which resolves from the worktree's `policies/openai-error-format` to `Git-Repos/api-platform/sdk/core`. If the worktree's depth differs, fix the relative path so it does. Add a comment on the replace line saying it tracks the unreleased PR #3622 fault contract (research R3).
  - Then run `go mod tidy`.
- [X] T003 [P] Create `POL/policy-definition.yaml` exactly as in `contracts/policy-definition.md`:
  - `name: openai-error-format`, `version: v0.1.0`.
  - Top-level `parameters` with `additionalProperties: false` and an optional `fault` object (`additionalProperties: false`).
  - Under `fault`, a `maxInspectBytes` integer with `minimum: 0`, `default: 65536` and `x-wso2-policy-advanced-param: true`.
  - Empty `systemParameters`.

---

## Phase 2: Foundational (blocking prerequisites)

**Purpose**: the policy skeleton that every story extends. It must load, report all-SKIP modes, and be panic-safe.

**⚠️ No user-story work starts until this phase is complete.**

- [X] T004 Create `POL/openaierrorformat.go` (license header as in sibling policies, package `openaierrorformat`):
  - `type Policy struct{ maxInspectBytes int }`.
  - `GetPolicy(metadata policy.PolicyMetadata, params map[string]interface{}) (policy.Policy, error)`. It reads `params["fault"]["maxInspectBytes"]`, defaulting to `65536`, and rejects negative values with an error.
  - `Mode()` returns `policy.ProcessingMode` with all four fields explicitly set to `HeaderModeSkip`/`BodyModeSkip` (research R10).
  - Mirror the GetPolicy and param-parsing style of `gateway-controllers/policies/word-count-guardrail/wordcountguardrail.go`.
- [X] T005 In `POL/openaierrorformat.go`, add `OnFault(ctx, faultCtx *policy.FaultContext, params map[string]interface{}) (resp *policy.FaultResponse)`:
  - A deferred `recover()` that logs with `slog.Error` (policy name and route, no body content) and sets `resp = nil` (research R8, FR-016).
  - A nil-`faultCtx` guard that returns nil.
  - A call to an unexported `p.format(faultCtx) *policy.FaultResponse` that returns nil for now.
  - Add a compile-time assertion `var _ policy.FaultPolicy = (*Policy)(nil)`.
- [X] T006 [P] Create `POL/openaierrorformat_test.go` with:
  - a helper that builds a `*policy.FaultContext` (kind, source, policy name, status, headers, body, `*FaultDetails`, committed flag);
  - tests that `GetPolicy` applies the 65536 default, accepts `0`, and rejects `-1`;
  - a test that `Mode()` is all SKIP;
  - a test that `OnFault` with a nil context returns nil.
- [X] T007 Run `go build ./... && go vet ./... && go test ./...` in `POL/`. All of them must pass.

**Checkpoint**: the policy compiles against the PR's SDK, is a valid `FaultPolicy`, and changes nothing yet.

---

## Phase 3: User Story 1 - New LLM APIs opt in on an upgraded gateway (Priority: P1) 🎯 MVP

**Goal**: on an `LlmProvider`/`LlmProxy` with the policy attached, a policy rejection returns `{"error":{message,type,param:null,code}}` with the status unchanged.

**Independent test**: two providers, one with the policy and one without, get the same guardrail rejection. The first returns the envelope; the second is unchanged.

### Tests for User Story 1

- [X] T008 [P] [US1] Create `POL/envelope_test.go`:
  - A table test for the `error.type` mapping, exactly per `contracts/openai-error-envelope.md`:
    - `authentication`→`authentication_error`, `authorization`→`permission_error`, `throttling`→`rate_limit_error`;
    - `guardrail`/`validation`/`requestSize`→`invalid_request_error`, `routing`→`not_found_error`, `upstream`/`internal`→`server_error`;
    - `mediation`, `configuration`, `""` and an unknown value → status-derived (401→`authentication_error`, 403→`permission_error`, 404→`not_found_error`, 429→`rate_limit_error`, other 4xx→`invalid_request_error`, 5xx/other→`server_error`).
  - Assert that the marshalled envelope has exactly one top-level key, `error`, and that `param` is JSON `null`.
  - Assert that `code` is `null` when `Fault.Code` is empty.
  - Assert that `guardrail` is present inside `error` only when `Fault.Guardrail` is non-nil.
  - Assert that `Fault.Description` text never appears in the output.
- [X] T009 [P] [US1] Add `TestOnFault_US1_*` cases to `POL/openaierrorformat_test.go`: a `LlmProvider` and a `LlmProxy` with `Source: "gateway"`, `Policy: "word-count-guardrail"`, status 422, and a `Fault{Type:"guardrail", Code:"906201", Message:"…"}`. Assert:
  - the body is the envelope;
  - `HeadersToSet["content-type"] == "application/json"`;
  - `StatusCode == nil` and `Final == false`.
  Add equivalent cases for 401/`authentication` and 429/`throttling`.

### Implementation for User Story 1

- [X] T010 [US1] Create `POL/envelope.go`:
  - `type openAIError struct{ Message string; Type string; Param *string; Code *string; Guardrail *policy.GuardrailDetails }`, with JSON tags `message`, `type`, `param` (always emitted as `null`), `code`, and `guardrail,omitempty`.
  - A wrapper struct `{ Error openAIError "json:\"error\"" }`.
  - `errorType(faultType string, status int) string`, implementing the mapping from T008.
  - `buildEnvelope(f *policy.FaultDetails, status int, message string) ([]byte, error)`, using `encoding/json` only and never string concatenation.
  - `Fault.Description` must never be read.
- [X] T011 [US1] In `POL/openaierrorformat.go`, implement `format` for the base path (data-model §3, rows 1 and 6):
  - Return nil unless `faultCtx.APIKind` is `policy.APIKindLlmProvider` or `policy.APIKindLlmProxy`.
  - Message = `Fault.Message` if non-empty, else `http.StatusText(faultCtx.ResponseStatus)`. US3 adds body extraction.
  - Return `&policy.FaultResponse{Body: env, HeadersToSet: {"content-type":"application/json"}}`. If `ResponseHeaders` has `content-encoding`, also set `HeadersToRemove: []string{"content-encoding"}` (research R9).
  - Leave `StatusCode` nil and `Final` false.
- [X] T012 [US1] Run `go test ./...` in `POL/`. T008 and T009 must pass.
- [X] T013 [US1] Mirror into the monorepo:
  - Copy `POL/` to `MIR/` (whole directory).
  - Change only the `replace` line in `MIR/go.mod` to `=> ../../../sdk/core`.
  - Run `GOWORK=off go build ./... && GOWORK=off go test ./...` in `MIR/`.
  - Run `diff -r POL MIR`. The only allowed difference is the replace line.
- [X] T014 [US1] Add a `- name: openai-error-format` entry with `filePath: ./dev-policies/openai-error-format` to the `policies:` list of `AP/gateway/build.yaml`, in alphabetical position. Confirm with `git check-ignore -v AP/gateway/dev-policies/openai-error-format/go.mod` that the mirror is ignored, and don't stage it.
- [X] T015 [US1] Create `AP/gateway/it/features/openai-error-format-policy.feature`:
  - Apache license header and tags `@llm @fault-policies @openai-error-format`.
  - Use only existing steps from `AP/gateway/it/steps_*.go`.
  - A Scenario Outline over at least the `openai`, `anthropic` and `gemini` templates. Deploy an `LlmProvider` with a `word-count-guardrail` (`request: {min: 1, max: 5, jsonPath: ""}`) and `globalFaultPolicies: [{name: openai-error-format, version: v0}]`, upstream `http://echo-backend:80`. POST a 10-word body and assert:
    - status 422 and `Content-Type` contains `application/json`;
    - `error.type` = `invalid_request_error`, and `error.message` exists;
    - pattern `\Wparam\W\s*:\s*null`;
    - no top-level `message`.
  - A control scenario: an identical provider without the policy returns its pre-existing body, with `error.type` absent.
  - An `LlmProxy` scenario that does the same.
  - Delete the APIs at the end.
- [X] T016 [US1] Give the user the exact image-build and IT commands (for example `make -C AP/gateway build-coverage` then `cd AP/gateway/it && go test -run 'TestFeatures' -godog.tags=@openai-error-format`, adjusted to what `AP/gateway/it/Makefile` really supports). Don't run the builds yourself. Record the result once the user has run them.

**Checkpoint**: MVP. An LLM API opts in with one fault-policy entry and its rejections become SDK-readable (SC-001, SC-002 for rejections, SC-006).

---

## Phase 4: User Story 2 - Nothing changes unless an API opts in (Priority: P1)

**Goal**: an API without the policy, or a non-LLM API with it attached, sees byte-identical responses.

**Independent test**: the full existing IT suite passes unchanged, and a RestApi with the policy attached is unchanged.

### Tests for User Story 2

- [X] T017 [P] [US2] Add table cases to `POL/openaierrorformat_test.go`: for every `APIKind` other than `LlmProvider`/`LlmProxy` (`RestApi`, `Mcp`, `WebSubApi`, `GraphQLApi`, `Agent`, `""`), `OnFault` returns nil. Also, for an LLM kind with `ResponseCommitted: true`, `OnFault` returns nil (data-model §3, rows 1–2).

### Implementation for User Story 2

- [X] T018 [US2] In `POL/openaierrorformat.go` `format`, add the `ResponseCommitted` gate (return nil) directly after the kind gate. Make sure the kind gate returns before any body or header is read. Re-run `go test ./...`, then re-sync `MIR/` and re-diff as in T013.
- [X] T019 [US2] Add scenarios to `AP/gateway/it/features/openai-error-format-policy.feature`:
  - A `RestApi` with `faultPolicies: [{name: openai-error-format, version: v0}]` and a `word-count-guardrail` rejection returns the same body as without the policy: no `error.type`, and the original shape is kept.
  - Deploying two LLM providers, one with the policy, leaves the other's error body unchanged.
- [ ] T020 [US2] Ask the user to run the full IT suite (`cd AP/gateway/it && make test`) with the policy present in `build.yaml`. Every pre-existing feature must pass unchanged (SC-005). Record any failure with its output, and don't work around it in gateway code.

**Checkpoint**: backward compatibility is proven.

---

## Phase 5: User Story 3 - Rejection reasons survive formatting (Priority: P2)

**Goal**: `error.message` carries the rejecting policy's own text, recovered from its legacy body, within bounds.

**Independent test**: rejections with `{"message":…}`, `{"error_description":…}`, `{"detail":…}`, `{"error":"…"}`, `{"error":{"message":…}}` and plain text each produce that text in `error.message`.

### Tests for User Story 3

- [X] T021 [P] [US3] Create `POL/message_test.go` with a table test for `extractMessage(body []byte, headers, maxInspectBytes int) string`:
  - The JSON lookup order is `error.message` → `message` → `error_description` → `detail` → `error` (string). The first non-empty value wins, for example `{"message":"a","detail":"b"}` → `a`.
  - Single-line plain text of ≤ 1024 characters is returned trimmed.
  - Plain text that is multi-line or longer than 1024 characters → `""`.
  - A body longer than `maxInspectBytes` → `""`.
  - `maxInspectBytes == 0` → `""`.
  - A present `content-encoding` header → `""`.
  - An empty or nil body → `""`.
  - Non-string values (for example `{"message":123}`) are skipped.
- [X] T022 [P] [US3] Add `TestOnFault_US3_*` to `POL/openaierrorformat_test.go`:
  - With no `Fault` and body `{"message":"Too many words"}` at status 422, the envelope message is `"Too many words"` and the type is `invalid_request_error`.
  - With `Fault.Message` set and a different body message, `Fault.Message` wins.
  - With no message anywhere at status 503, the message is `"Service Unavailable"`.

### Implementation for User Story 3

- [X] T023 [US3] Create `POL/message.go` implementing `extractMessage` exactly as T021 specifies (research R6). Decode with `encoding/json` into `map[string]any` only after the size check, and never log body content.
- [X] T024 [US3] In `POL/openaierrorformat.go` `format`, set the message priority to `Fault.Message` → `extractMessage(ResponseBody.Content, ResponseHeaders, p.maxInspectBytes)`, only when `ResponseBody != nil && ResponseBody.Present` → `http.StatusText(status)`. Re-run `go test ./...`, then re-sync `MIR/` and re-diff as in T013.
- [X] T025 [US3] Add an IT scenario to `AP/gateway/it/features/openai-error-format-policy.feature`: a provider with the policy plus a rejecting policy that writes a legacy JSON body without a fault class. Pick a shipped policy whose rejection body has a `message` field; check its current body in `gateway-controllers` before choosing. Assert that `error.message` equals that policy's message text.

**Checkpoint**: rejection reasons are preserved (SC-007).

---

## Phase 6: User Story 4 - Router failures, when the gateway sends them to fault policies (Priority: P2)

**Goal**: with `handle_upstream_faults` on, router failures are formatted and backend errors pass through. With it off, nothing changes. Unknown provenance is handled conservatively.

**Independent test**: the PR's IT config already has `handle_upstream_faults = true` (`AP/gateway/it/test-config.toml`), so an unreachable upstream returns the envelope while a backend 400 passes through.

### Tests for User Story 4

- [X] T026 [P] [US4] Add Source table cases to `POL/openaierrorformat_test.go` (data-model §3, rows 3–4, research R4):
  - `gateway` → formatted;
  - `router` with a 503 and `Fault{Type:"upstream", Code:"303001"}` → formatted, type `server_error`, code `303001`;
  - `backend` → nil;
  - `noRoute` → nil;
  - `unknown` with `Policy:""` → nil;
  - `unknown` with `Policy:"api-key-auth"` → formatted.

### Implementation for User Story 4

- [X] T027 [US4] In `POL/openaierrorformat.go` `format`, add the Source gate after the committed gate, using the `policy.FaultSource*` constants: return nil for `FaultSourceBackend` and `FaultSourceNoRoute`, and for `FaultSourceUnknown` when `faultCtx.Policy == ""`. Re-run `go test ./...`, then re-sync `MIR/` and re-diff.
- [X] T028 [US4] Add scenarios to `AP/gateway/it/features/openai-error-format-policy.feature`:
  - **Router failure**: a provider with the policy and upstream `http://unreachable-host-for-it:9` (or whatever unreachable-upstream pattern `AP/gateway/it/features/fault-policies.feature` already uses). Assert status 503 (or whatever that feature asserts), `error.type` = `server_error`, and that the body is valid JSON with no plain-text `no healthy upstream`.
  - **Backend passthrough**: a provider with the policy whose upstream returns its own 4xx JSON (reuse the mock-api or echo-backend mechanism already used for backend errors in `fault-policies.feature`). Assert that the status and body equal the backend's.
- [X] T029 [US4] Add an empty-body case (research R9): a rejecting policy or upstream path that produces an error with no body, with the policy attached. Assert that the full envelope arrives and the request doesn't time out, which shows `Content-Length` was recomputed. If it hangs, stop and report it as a PR #3622 engine bug with the IT output. Don't patch the engine.

**Checkpoint**: router failures are covered under the documented condition (SC-003), and backend errors are untouched.

---

## Phase 7: User Story 5 - Composing with other fault policies (Priority: P3)

**Goal**: predictable declaration-order behaviour, and idempotency (a body that is already an OpenAI envelope is never rewritten).

**Independent test**: attached twice, the policy gives one un-nested envelope. A later body-writing fault policy has the last word.

### Tests for User Story 5

- [X] T030 [P] [US5] Add `isOpenAIEnvelope` cases to `POL/envelope_test.go`. True only for a JSON object whose single top-level key is `error`, holding an object with a string `message` and a string `type` (extra keys inside `error` are allowed). False for:
  - `{"error":"x"}`;
  - `{"error":{"message":"x"}}` (no type);
  - `{"error":{...},"extra":1}`;
  - arrays, invalid JSON and an empty body.
- [X] T031 [P] [US5] Add an idempotency test to `POL/openaierrorformat_test.go`: feed the policy's own output back in as `ResponseBody` and assert that `OnFault` returns nil.

### Implementation for User Story 5

- [X] T032 [US5] In `POL/envelope.go`, implement `isOpenAIEnvelope(body []byte) bool` as T030 specifies. Check the size first (≤ `maxInspectBytes`, else false) and skip it for encoded bodies. In `POL/openaierrorformat.go` `format`, return nil when it is true (data-model §3, row 5), placed after the Source gate. Re-run `go test ./...`, then re-sync `MIR/` and re-diff.
- [X] T033 [US5] Add scenarios to `AP/gateway/it/features/openai-error-format-policy.feature`:
  - The policy at both `globalFaultPolicies` and `operationFaultPolicies` for the same path returns a single valid envelope with no nested `error.error`.
  - The policy declared **before** a `set-headers`-style or body-writing fault policy: the later entry's effect is visible. Use a shipped policy that implements `OnFault`; if none ships besides this one, assert only the double-attachment case and note the gap in the feature-file comment and the PR description.

**Checkpoint**: all five stories are complete.

---

## Phase 8: Polish & cross-cutting concerns

- [X] T034 [P] Create `GC/docs/openai-error-format/v0.1/metadata.json`, following `docs/word-count-guardrail/v1.0/metadata.json`: `name` `openai-error-format`, `displayName` "OpenAI Error Format", `version` "0.1", `provider` "WSO2", categories `["AI","Error Handling"]` (check that the categories match ones already used in the catalogue), and a one-paragraph description.
- [X] T035 [P] Create `GC/docs/openai-error-format/v0.1/docs/openai-error-format.md`, adapted from `contracts/policy-definition.md` and `contracts/openai-error-envelope.md`. Cover:
  - attaching it at API and operation level for `LlmProvider` and `LlmProxy`;
  - the upgrade path (existing APIs unchanged, new APIs opt in);
  - the envelope and type-mapping table;
  - message sources;
  - the status being unchanged;
  - declaration-order guidance;
  - the router-failure limitation, including what `handle_upstream_faults = true` does to response policies gateway-wide;
  - not covered: unmatched routes and streamed responses after headers are sent.
- [X] T036 [P] In `AP/docs/gateway/fault-policies.md`, add a short subsection under "Shaping an error body yourself": OpenAI SDK clients on `LlmProvider`/`LlmProxy` should attach `openai-error-format`. Include a YAML example and a link to the policy doc. Don't change any other section.
- [X] T037 Run the catalogue tooling in `GC/` (for example `make` targets or `scripts/generate-policy-catalog.py`, per `GC/Makefile` and `GC/.github/workflows/validate-policy-catalog.yml`) so the generated `docs/README.md` includes the new policy, and make sure the validation passes.
- [X] T038 Run `govulncheck ./...` in `POL/` and confirm there are no new third-party modules in `go.mod` (dependency-management rule).
- [X] T039 Final consistency pass:
  - `go vet` and `go test ./...` in `POL/`, and `GOWORK=off` in `MIR/`;
  - `diff -r POL MIR` shows only the replace line;
  - `git -C AP status` shows changes only to `gateway/build.yaml`, the new IT feature, `docs/gateway/fault-policies.md` and `specs/…`, with nothing under `gateway/gateway-controller`, `gateway/gateway-runtime` or `sdk/` (SC-004);
  - grep the new code for `TODO`/`FIXME` and `Description`; there should be none.
- [ ] T040 Walk through every scenario in `specs/001-llm-openai-compatible-errors/quickstart.md`. Ask the user to run the image build and the stack, then run the OpenAI SDK interop snippet (§4) against `oai-on` and record the outcome.
- [X] T041 Draft (don't post) PR #3622 review notes for the user:
  - no `recover()` on the engine fault path, despite the SDK `FaultPolicy` doc (research R8);
  - IT references `fault-notifier`/`error-formatter`/`fault-declarer` policies that don't exist in any repo;
  - any `Content-Length` finding from T029.

---

## Dependencies & execution order

### Phase dependencies

- **Setup (T001–T003)** → **Foundational (T004–T007)** → user stories → **Polish (T034–T041)**.
- **US1 (T008–T016)** must come first among the stories. It creates `envelope.go`, the `format` function, the mirror, the `build.yaml` entry and the IT feature file that the later stories extend.
- **US2, US3, US4 and US5** each depend only on US1, so they can go in any order or in parallel. Their Go changes all touch `format` in `POL/openaierrorformat.go`, and their IT scenarios all go in the one `.feature` file. Either serialize those edits or merge with care.
- Gate order inside `format` must match data-model §3 whatever order the stories land in: kind → committed → source → already-envelope → format.

### Within each story

- Write the unit tests first and confirm they fail, then implement, then `go test`, then re-mirror and diff, then add the IT scenarios, then ask the user to run IT.

### Parallel opportunities

- T003 can run alongside T002.
- T006 can run alongside T004/T005 once the signatures are agreed.
- T008 and T009 (US1 tests) touch different files.
- T017, T021, T022, T026, T030 and T031 are test files or cases. Cases in the same `_test.go` file must be serialized; tests in different files can run in parallel.
- T034, T035 and T036 (docs) are three different files in two repos.

### Parallel example: User Story 3

```text
T021 [P] [US3] message_test.go            ┐ in parallel (different files)
T022 [P] [US3] openaierrorformat_test.go  ┘
→ T023 message.go → T024 wire into format → T025 IT scenario
```

---

## Implementation strategy

### MVP first (User Story 1)

1. Phases 1–2: worktree, module and safe skeleton.
2. Phase 3 (US1): rejections on LLM kinds become OpenAI envelopes. Mirror it, register it in `build.yaml`, and run the IT feature.
3. **Stop and validate.** At this point the feature already delivers its main value for new APIs.

### Incremental delivery

1. US2: prove nothing changes for APIs that don't opt in. This must happen before anyone deploys the policy.
2. US3: recover the rejecting policy's message.
3. US4: router failures and backend passthrough under `handle_upstream_faults`.
4. US5: idempotency and composition.
5. Polish: docs, catalogue generation, govulncheck, quickstart, PR #3622 notes.

### Release follow-up (outside this task list)

After PR #3622 merges and `sdk/core` is tagged:
1. Drop the `replace` lines.
2. Bump `sdk/core` in `POL/go.mod`.
3. Tag `policies/openai-error-format/v0.1.0`.
4. Switch `AP/gateway/build.yaml` to `gomodule: github.com/wso2/gateway-controllers/policies/openai-error-format@v0`.

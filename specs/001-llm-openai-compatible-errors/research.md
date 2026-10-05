# Research: OpenAI-Compatible Error Responses for LLM APIs

All findings were checked against branch `001-llm-openai-compatible-errors`, which is PR #3622 head `31b828af4`.

## R1. Can a plain policy do this with no gateway change?

**Decision**: Yes. The feature is a Go policy that implements only `policy.FaultPolicy`.

**Rationale**: The PR's SDK (`sdk/core/policy/v1alpha2`) already gives a fault policy everything it needs.
- `FaultContext`:
  - `SharedContext.APIKind`, to tell LLM kinds from other kinds.
  - `ResponseStatus`, `ResponseHeaders` and `ResponseBody`, the error being sent.
  - `Fault *FaultDetails`, which carries `Code`, `Type`, `Message` and `Guardrail`.
  - `Source`: `gateway`, `backend`, `router`, `noRoute` or `unknown`.
  - `ResponseCommitted` and `Policy`.
- `FaultResponse` can replace `Body`, set or remove headers, and leaves `StatusCode` nil so the status is kept.
- The engine discovers the capability by type assertion when it builds the chain (`executor/fault_chain.go`), so no registration change is needed.

**Alternatives considered**:
- **Engine-side renderer gated by a gateway setting.** Rejected in the spec: it couples the gateway to the format.
- **System-injected policy.** Rejected: needs controller changes.
- **Per-policy "router failure" declaration in the engine.** Rejected: needs engine changes.

## R2. Where the policy lives and how it gets into the gateway

**Decision**:
- **Source of truth**: `gateway-controllers/policies/openai-error-format/`, module `github.com/wso2/gateway-controllers/policies/openai-error-format`, starting at version `v0.1.0`.
- **Monorepo copy**: mirrored to `api-platform/gateway/dev-policies/openai-error-format/` and listed in `gateway/build.yaml` as `filePath: ./dev-policies/openai-error-format` until a tagged release exists. After the release, it switches to `gomodule: ...@v0`.

**Rationale**:
- This is how every catalogue policy is added. `build.yaml` is the distribution list, not gateway code, so FR-001 is met.
- The `dev-policies/` copy is the one the gateway-runtime image compiles.
- `gateway/dev-policies/` is gitignored in `api-platform`, so the mirror is never committed there. Only the `build.yaml` entry is committed.

**Alternatives considered**: putting it in `gateway/system-policies/`. Rejected: those are gateway-internal (`wso2_apip_sys_` prefix) and can't be attached by API authors.

**Working-tree caution**: the local `gateway-controllers` checkout is on `fix/3413-guardrail-findings` with 44 uncommitted changes. Implementation must use a separate worktree created from `wso2/gateway-controllers` `main`.

## R3. SDK dependency

**Decision**:
- The policy's `go.mod` requires the `sdk/core` version that contains PR #3622's SDK commits.
- No tag has those commits yet: the latest is `sdk/core/v0.4.1`, which predates `FaultPolicy`, `FaultContext` and `FaultResponse`.
- Until the PR's SDK is tagged, both copies use a `replace github.com/wso2/api-platform/sdk/core => <relative path>` line. The path is `../../../api-platform/sdk/core` from gateway-controllers and `../../../sdk/core` from dev-policies; this is the one expected difference between the two copies.
- Work inside dev-policies with `GOWORK=off`.

**Rationale**: this is the only build path while the PR is unreleased. The `replace` is a tracked dependency on PR #3622 merging, not a fork pin, so it complies with `dependency-management.md`. The PR description must state it.

**Dependencies**: no new third-party modules, only the standard library (`encoding/json`, `net/http`, `strings`, `log/slog`) plus `sdk/core`.

## R4. Telling gateway errors from backend errors

**Decision**: use `FaultContext.Source`.

| `Source` | Action |
|---|---|
| `gateway` | Format |
| `router` | Format. This only reaches the chain when `handle_upstream_faults` is on |
| `backend` | Pass through (return nil) |
| `noRoute` | Pass through. Can't occur, because an unmatched request has no API and therefore no fault chain |
| `unknown` | Format only if `FaultContext.Policy != ""` (a policy rejection is always gateway-produced). Otherwise pass through, because an old router without provenance may be sending a backend error |

**Rationale**: `Source` is the PR's closed provenance set (`fault_codes.go`). Handling `unknown` conservatively keeps FR-007 true even on routers that don't report provenance.

## R5. Error-type mapping for classes the spec doesn't list

**Decision**: map `FaultDetails.Type` as in FR-012. The other PR classes, `mediation` and `configuration`, plus any unknown future class, fall back to the status-derived type:

| Status | Type |
|---|---|
| 401 | `authentication_error` |
| 403 | `permission_error` |
| 404 | `not_found_error` |
| 429 | `rate_limit_error` |
| other 4xx | `invalid_request_error` |
| 5xx | `server_error` |

**Rationale**: the PR's `FaultType` set is open ("no vocabulary can enumerate ahead of the policies"), so an unknown class needs a deterministic fallback, and the status is always there.

## R6. Message extraction and bounds

**Decision**:
- **Order**:
  1. `Fault.Message`.
  2. A message parsed from the current body.
  3. `http.StatusText(status)`.
- **Parsing the body**:
  - Only if it's present, not `Content-Encoding`-compressed, and at most `fault.maxInspectBytes` bytes (default 65536). The limit is a policy parameter, per `file-access.md` directive 5.
  - JSON lookup order: `error.message`, `message`, `error_description`, `detail`, and `error` when it is a string.
  - A non-JSON body is used as plain text: trimmed, and only if non-empty and a single line of at most 1024 characters.
- **Never used**: `Fault.Description`. The PR guarantees it never reaches the client, and `error-handling.md` directive 1 forbids exposing internal detail.

**Implementation finding**: every shipped guardrail writes `{"type":"<NAME>","message":{"action","interveningGuardrail","actionReason",…}}`, so `message` is an object. Without a `message.actionReason` lookup, every guardrail rejection would fall back to the status text. The lookup was added, and the same object is carried over as `error.guardrail` when the fault has no `Guardrail`.

**Rationale**: keeps the rejecting policy's reason (SC-007) without unbounded parsing of an arbitrary body. Plain text longer than one line is most likely a stack trace or an HTML page, so the status text is used instead.

## R7. Idempotency and composing with other fault policies

**Decision**: if the current body already parses as an OpenAI envelope, return nil. The test: a JSON object whose only top-level key is `error`, holding an object with a string `message` and a string `type`. Otherwise reshape whatever body is there, following declaration order (spec FR-011).

The check has its own fixed 64 KiB bound, independent of `maxInspectBytes`, so setting `maxInspectBytes: 0` never makes the policy rewrite an envelope that's already there.

**Rationale**: attaching the policy at both API and operation level gives one effective formatting (the second run is a no-op). A fault policy that already wrote an OpenAI envelope is preserved.

## R8. Panic safety

**Finding**: the SDK's `FaultPolicy` comment says a panic "leaves the original error intact". There is **no `recover()`** on the fault path in the engine (`executor/fault_chain.go`, `kernel/fault_*.go`). The only `recover()` is in `internal/analytics`.

**Decision**:
- `OnFault` wraps its work in `defer func(){ if r := recover(); r != nil { slog.Error(...); resp = nil } }()` to meet FR-016 without a gateway change.
- Raise the missing engine-side recovery as a review comment on PR #3622. It's a PR gap, not part of this feature.

## R9. Response headers

**Decision**: when replacing the body, set `Content-Type: application/json`. Remove `Content-Encoding` when the original body was encoded, since the new body is plain JSON. Leave every other header alone, including `WWW-Authenticate` on 401s and `Retry-After` on 429s. The PR applies headers as operations, so these survive.

**To verify during implementation**: that the engine recomputes `Content-Length` for a `FaultResponse.Body` on both the buffered and the bodyless (response-header phase) paths. Cover it with an IT scenario that has an empty-body rejection and a router failure.

## R10. Processing cost

**Decision**: `Mode()` returns all four modes as explicit `SKIP` (`HeaderModeSkip`/`BodyModeSkip`). The policy runs only through `OnFault`.

**Rationale**: the policy adds no work on the success path. The one unavoidable cost, that any non-empty fault chain enables response-body processing for the route, is PR behaviour, and only APIs that opt in pay it (spec assumption).

## R11. Testing approach

**Decision**:
- **Unit**: table-driven tests in the policy package, building `FaultContext` values directly for every Source × kind × body-shape × fault-class combination, plus a panic-recovery test.
- **IT**: a new feature, `gateway/it/features/openai-error-format-policy.feature`, using real rejecting policies (`word-count-guardrail`, `api-key-auth`, `basic-ratelimit`) on `LlmProvider` and `LlmProxy`.
  - The PR's IT config already sets `handle_upstream_faults = true`, so router-failure and backend-passthrough scenarios run in the default suite.
  - The flag-off case means the response is unchanged. Unit tests and the "not attached" scenarios cover it.
- **Interop**: the quickstart includes an OpenAI-SDK parse check.

**Alternatives considered**: a separate IT config with the flag off. Rejected because the suite would need another target, and nothing in the policy depends on the flag.

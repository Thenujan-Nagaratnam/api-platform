# Implementation Plan: OpenAI-Compatible Error Responses for LLM APIs

**Branch**: `001-llm-openai-compatible-errors` | **Date**: 2026-10-04 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/001-llm-openai-compatible-errors/spec.md`

## Summary

Add one new catalogue policy, `openai-error-format`, that implements only PR #3622's `FaultPolicy` contract. API authors attach it to an `LlmProvider` or `LlmProxy` through `globalFaultPolicies`/`operationFaultPolicies`.

In `OnFault` it does the following:
1. It passes through anything that isn't a gateway-produced error on an LLM API: non-LLM kinds, backend errors, streamed responses that have already started, and bodies already in OpenAI format.
2. For everything else, it replaces the body with `{"error":{message,type,param:null,code}}`, built from the structured `Fault` and the rejecting policy's own message, and leaves the status unchanged.

There are **zero changes** to gateway, controller or policy-engine code. The only `api-platform` change outside tests and docs is the `gateway/build.yaml` distribution entry. Router failures are covered when the operator has `handle_upstream_faults` on (a documented limitation).

## Technical Context

**Language/Version**: Go 1.26.x, matching `sdk/core` (`go 1.26.2`) and the catalogue policies.

**Primary Dependencies**: `github.com/wso2/api-platform/sdk/core` (`policy/v1alpha2`), the version containing PR #3622's fault contract. It is unreleased, so it is wired through a `replace` until tagged ([research R3](research.md)). Standard library only otherwise.

**Storage**: N/A.

**Testing**: `go test` table-driven unit tests in the policy package, plus a gateway IT feature (godog), `gateway/it/features/openai-error-format-policy.feature`.

**Target Platform**: gateway-runtime policy engine (Linux container). The policy is compiled into the engine by gateway-builder.

**Project Type**: a gateway policy plugin (a Go module in the policy catalogue).

**Performance Goals**: no work on the success path (`Mode()` is all `SKIP`). On the fault path, at most one bounded JSON parse (≤ `maxInspectBytes`, default 64 KiB) and one small marshal per failed request.

**Constraints**:
- No gateway, controller or engine change, and no new gateway setting (FR-001).
- Must not panic into the engine, because there is no engine-side recover ([R8](research.md)).
- Must never emit `Fault.Description`.
- Status is unchanged.

**Scale/Scope**: one policy, about 300 lines of Go plus tests; one IT feature; docs for the policy page in gateway-controllers and a cross-reference in `docs/gateway/fault-policies.md`.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

`.specify/memory/constitution.md` is still the unfilled template, so there are no ratified principles to gate on. The project's checked-in rules (`.claude/rules/`) are applied as the gates instead:

| Gate (rule) | Applies | Status |
|---|---|---|
| `error-handling.md` §1, no internal exposure | The envelope must not carry `Fault.Description`, stack traces or internal names | ✅ Description is never read into output (R6). Plain-text bodies are used only if they are a single short line |
| `error-handling.md` §4, unified auth failures | The policy keeps whatever message the auth policy chose and does not add detail | ✅ No branching added. Message comes from the fault or the body |
| `file-access.md` §5, bounded reads with configurable limits | Body inspection | ✅ `fault.maxInspectBytes` parameter, default 64 KiB. Compressed bodies are not inspected |
| `dependency-management.md` | `sdk/core` bump, `replace` | ✅ No new third-party modules. `replace` is documented as tracking PR #3622, stated in the PR description, removed at release |
| `authentication_authorization.md` GO-AUTH-016, no `os.Exit` | Policy code | ✅ Errors and panics return nil and log |
| "No deferring behind a TODO" (all rules) | Whole change | ✅ The known limitation is a spec item and documented, not a code comment |
| `go-network-service-hardening`, `ssrf-prevention`, `xxe`, `pqc`, `cors`, `db-schema` | — | N/A: no servers, outbound calls, XML, crypto, CORS or schema |

**Result: PASS.** No complexity tracking is needed.

*Post-design re-check (after Phase 1)*: the contracts add one optional integer parameter and no new surfaces. **PASS.**

## Project Structure

### Documentation (this feature)

```text
specs/001-llm-openai-compatible-errors/
├── spec.md
├── plan.md                 # this file
├── research.md             # Phase 0
├── data-model.md           # Phase 1
├── quickstart.md           # Phase 1
├── contracts/
│   ├── openai-error-envelope.md
│   └── policy-definition.md
├── checklists/requirements.md
└── tasks.md                # /speckit-tasks (not created here)
```

### Source Code

```text
gateway-controllers/                       # source of truth; new worktree off wso2/main
├── policies/openai-error-format/
│   ├── go.mod / go.sum                    # sdk/core + temporary replace (R3)
│   ├── policy-definition.yaml             # contracts/policy-definition.md
│   ├── openaierrorformat.go               # GetPolicy, Mode (all SKIP), OnFault + recover
│   ├── envelope.go                        # type mapping, message extraction, envelope predicate
│   └── openaierrorformat_test.go          # decision table × kinds × sources × body shapes, panic test
└── docs/openai-error-format/v0.1/
    ├── metadata.json
    └── docs/openai-error-format.md

api-platform/
├── gateway/build.yaml                     # + filePath: ./dev-policies/openai-error-format
├── gateway/dev-policies/openai-error-format/   # gitignored mirror (diff-checked, not committed)
├── gateway/it/features/openai-error-format-policy.feature
└── docs/gateway/fault-policies.md         # cross-reference under "Shaping an error body yourself"
```

**Structure decision**: catalogue policy pattern, with `gateway-controllers` as the source of truth, mirrored into `dev-policies` and registered with `filePath` until it is tagged. Nothing under `gateway/gateway-controller`, `gateway/gateway-runtime` or `sdk/` changes.

## Implementation notes

- **Gate order** (data-model §3): kind → committed → source → unknown-source-without-policy → already-envelope → format.
- **Headers**: `HeadersToSet{"content-type":"application/json"}`. Add `HeadersToRemove{"content-encoding"}` only when it is present. `StatusCode` is nil and `Final` is false, so later entries still run (declaration order, FR-011).
- **Output encoding**: `encoding/json`, never string concatenation, so a message containing quotes or control characters can't break the envelope.
- **IT coverage**: rejection (guardrail, auth, rate limit) × {`LlmProvider`, `LlmProxy`} × several templates; not-attached control; backend passthrough; router failure (the PR's IT config already has `handle_upstream_faults = true`); RestApi no-op; double attachment; empty-body rejection (checks `Content-Length` handling, R9).

## Risks and open items

| Item | Mitigation |
|---|---|
| `sdk/core` with the fault contract is unreleased | `replace` in both copies. Switch `build.yaml` to `gomodule` and drop the `replace` after PR #3622 merges and `sdk/core` is tagged |
| Engine may not recompute `Content-Length` for a `FaultResponse` body on the bodyless path | Verify with an IT scenario. If it fails, it's a PR #3622 bug: report it there and don't work around it in the gateway |
| No panic recovery on the engine's fault path | The policy recovers itself. Flag it on PR #3622 |
| The PR's IT references fault policies (`fault-notifier`, `error-formatter`, `fault-declarer`) not present in any repo | Our IT uses only shipped rejecting policies. Mention the gap to the PR author |
| Local `gateway-controllers` checkout has 44 uncommitted changes on another branch | Use a fresh worktree off `wso2/gateway-controllers` `main` |
| Router failures are uncovered with `handle_upstream_faults` off | Accepted limitation (spec US4), documented in the policy page and `fault-policies.md` |

## Complexity Tracking

None. The Constitution Check passed with no violations.

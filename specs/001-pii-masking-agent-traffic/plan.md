# Implementation Plan: Safe PII Masking and Restore for Agent Traffic

**Branch**: `001-pii-masking-agent-traffic` | **Date**: 2026-11-25 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/001-pii-masking-agent-traffic/spec.md`, plus the requester's rule: *keep the existing working behavior; anything masked today stays masked; only drop the invalid maskings.*

## Summary

Patch the `pii-masking-regex` policy (released v1.1.0) to **v1.2.0** so Anthropic Messages agent traffic is masked safely and restored fully. The design replaces text-level masking of the raw body with a JSON-aware pass that rewrites only the contents of string values, derives placeholders from the value instead of a counter, restores streamed tool arguments and thinking per content block, merges per-instance mappings instead of overwriting them, and stops numeric patterns from matching inside longer digit runs. An opt-in selection path (RFC 9535) lets operators mask only user and tool content. This design was derived from the spec and the released v1.1.0 source only, not from any earlier fix attempt.

## Technical Context

**Language/Version**: Go 1.26.2 (as in the released module's `go.mod`)

**Primary Dependencies**: `github.com/wso2/api-platform/sdk/core` (policy API `v1alpha2`, `utils` JSONPath helpers), Go standard library (`encoding/json`, `regexp`, `crypto/sha256`)

**Storage**: N/A. Per-request mapping lives in request metadata, shared by all instances on the route. Nothing is persisted.

**Testing**: `go test` unit tests (existing `piimaskingregex_test.go`, `_stream_test.go`, `_log_test.go`), plus the Gherkin integration suite under `policies/pii-masking-regex/it/`

**Target Platform**: WSO2 AI Gateway policy engine (gateway-runtime), Linux containers

**Project Type**: Single Go module (a gateway policy)

**Performance Goals**: Masking adds no more than one extra linear scan of the body. No per-request regex recompilation. Streaming restore holds a chunk only while a placeholder may still be completing.

**Constraints**: Request bodies must stay valid JSON; unmasked bytes (keys, numbers, whitespace, key order) must be byte-identical; the existing policy parameters stay backward compatible; no new required parameters.

**Scale/Scope**: One policy module, about 5 new or changed source files, one SDK dependency for item 8.

## Constitution Check

`.specify/memory/constitution.md` is still the unfilled template, so there are no project principles to gate on. Result: no gates apply. Re-check once a constitution exists.

## Project Structure

### Documentation (this feature)

```text
specs/001-pii-masking-agent-traffic/
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   ├── policy-parameters.md
│   └── placeholder-and-metadata.md
├── checklists/requirements.md
└── tasks.md             # created later by /speckit-tasks
```

### Source Code

The code lives in the **gateway-controllers** repository, not in this repository. This repository holds only the spec artifacts and the later gateway version bump.

```text
gateway-controllers/policies/pii-masking-regex/
├── piimaskingregex.go            # params, request/response entry points (changed)
├── mask.go                       # NEW: value-derived placeholders, entity matching, boundary checks
├── jsonmask.go                   # NEW: JSON-aware, byte-preserving masking of string values
├── restore.go                    # NEW: non-stream + SSE restore, per content block
├── policy-definition.yaml        # version v1.2.0, docs for jsonPath (changed)
├── piimaskingregex_test.go       # existing tests kept, new cases added
├── piimaskingregex_stream_test.go
├── mask_test.go                  # NEW
├── jsonmask_test.go              # NEW
├── restore_test.go               # NEW
└── it/features/pii_masking_regex.feature   # new scenarios per item

gateway-controllers/policies/pii-masking-regex/docs/ or the policy docs page   # parameter + limitation docs

api-platform/sdk/core/utils/jsonpath.go   # dependency for item 8 (wso2/api-platform#3571): RFC 9535 evaluator returning the normalized path of every match (read-only, no write helper)
api-platform/gateway/...                  # later: bump pii-masking-regex module version for the 1.2.x gateway patch
```

**Structure Decision**: Keep a single module. Split the current 1,052-line `piimaskingregex.go` into focused files only where new code goes (`mask.go`, `jsonmask.go`, `restore.go`) so the diff to the entry points stays small and the existing tests keep compiling.

## Design by item

| # | Cause (from reading v1.1.0) | Planned change |
|---|---|---|
| 1, 2 | With `"$"` the whole body is one string; regexes run on raw JSON text and the result is written back unchecked. | When the target is the whole payload and the body is valid JSON, scan string-value tokens only, decode each, mask, and re-encode only the tokens that changed. Keys, numbers, whitespace and key order stay byte-identical. Non-JSON payloads keep today's plain-text path. A final `json.Valid` check guards the result; on failure return a clear policy error instead of forwarding invalid JSON. |
| 3, 5 | Streaming restore reads text from a fixed path list that lacks `partial_json` and `thinking`. | Add `$.delta.partial_json` and `$.delta.thinking` as streamed text sources, in both the restore and the "may a placeholder still be arriving" check. For `partial_json`, the original is escaped as JSON text first, because the fragment is itself JSON inside a JSON string. |
| 6 | All text deltas of a flush are joined into the first event. | Group deltas by content-block `index` and delta kind, restore each group, and write each group back into its own first event. Never merge across groups. |
| 4 | Placeholder number is a counter incremented while ranging over a Go map. | Placeholder = `[ENTITY_` + 8 hex digits of a keyed-free hash of (entity name, value) + `]`. No counter, no map-order dependence. A deterministic probe resolves the rare collision by value order. |
| 7 | Every instance writes the same metadata key and replaces it. | Merge into the shared mapping instead of replacing. Value-derived placeholders make merges idempotent. Redact-only instances write nothing. |
| 8 | `"$"` masks everything by design; no selector exists for user and tool content. | Keep `"$"` meaning all string values (today's behavior). Add support for RFC 9535 paths that select many nodes, for example user and tool content only. The SDK evaluator (#3571) returns the normalized path of every match; it never writes. The byte-preserving scanner in `jsonmask.go` tracks its current path while walking the body and masks a string token only when that token's path is in the selected set. The body is never unmarshalled and re-marshalled, so numbers and key order survive in scoped mode too. Content outside the selection is never touched. |
| 9 | Phone and SSN patterns have no left boundary. | After matching, reject any match whose previous byte is a digit, so nothing inside a longer run is masked. Standalone phone-shaped numbers, including bare 10-digit ones, stay masked as today. |

## Keeping today's behavior

- Default `jsonPath` (`$.messages[-1].content`) and its plain-string handling are unchanged.
- `redactPII` still writes `*****`, still one-way, still no restore.
- Custom entities and the `email`, `phone`, `ssn` toggles keep their schemas and validation errors.
- Every value masked today inside a string stays masked. The only removals are matches that cut into longer digit runs and matches that corrupted the message.
- A regression suite replays requests that were valid before the patch and asserts the same set of values is masked.

## Risks and open points

- **SDK dependency (item 8)**: `sdk/core/utils` on `main` supports single-node paths only. Item 8 is blocked on an RFC 9535 evaluator that returns normalized paths of all matches, tracked in wso2/api-platform#3571. The SDK's existing set helper must not be used for masking, because it re-marshals the body and rewrites numbers. A pre-release SDK tag for it already exists in the repository, which this plan deliberately does not rely on. It needs to be checked before the SDK work is scheduled.
- **Placeholder format change** (4 to 8 hex digits) is visible to anyone matching the old format downstream. It goes in the release notes.
- **Which gateway patch carries it**: the 1.2.0 gateway release tree has no policy source, so the later gateway bump needs the exact patch branch confirmed.

## Complexity Tracking

No constitution violations to justify.

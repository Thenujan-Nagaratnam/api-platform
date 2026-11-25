# Validation Results: Safe PII Masking and Restore for Agent Traffic

**Date**: 2026-11-25
**Code**: `gateway-controllers` worktree `../gateway-controllers-worktrees/pii-agent-traffic`, branch `pii-masking-agent-traffic-v1.2.0` (off tag `policies/pii-masking-regex/v1.1.0`). Nothing committed.

## Quickstart status

| Step | Result |
|---|---|
| 1. Unit tests | Pass. `go test ./...` in `policies/pii-masking-regex`: 147 test runs (baseline v1.1.0: 76). `go test -race -count=3` and `-count=50` pass. `go vet` and `gofmt -l` clean. |
| 2. Item checks | Pass at unit level for all nine items (see table below). |
| 3. Regression guard | Pass. 16 requests that v1.1.0 masked validly; every value v1.1.0 masked is still masked (`TestRegression_EverythingMaskedBeforeStaysMasked`). |
| 4. Integration suite | **Not run.** Four scenarios written for items 1, 2, 8 and 9. They need the Docker gateway. Scenarios for streaming (items 3, 5, 6), stability (item 4) and two instances (item 7) are not written because the suite's capture upstream does not stream; those items are covered by unit tests only. The `it` module also asks for `go mod tidy` before it builds. |
| 5. Gateway | **Not run** (image builds and restarts are yours). |

## Items

| # | Where proven |
|---|---|
| 1 | `TestRequest_EmailAfterEscapeStaysValidJSON`, `TestScanJSONStrings_EscapesAreDecodedBeforeMasking` |
| 2 | `TestRequest_NumbersAreNeverRewritten` |
| 3 | `TestStream_ToolCallArgumentsAreRestored`, `..EscapeTheOriginalForTheInnerJSON` |
| 4 | `TestRequest_SameBodyGivesOneUpstreamBody`, `TestRequest_GrowingConversationKeepsItsMaskedPrefix`, `TestMasker_CollisionIsResolvedDeterministically` |
| 5 | `TestStream_ThinkingIsRestored` |
| 6 | `TestStream_TwoTextBlocksInOneFlushKeepTheirOwnText`, `TestStream_EachBlockIsRestoredIndependently` |
| 7 | `TestRequest_TwoInstancesMergeTheirMappings` |
| 8 | `TestRequest_ScopedPathMasksOnlyUserAndToolContent`, `TestRequest_WholePayloadStillMasksSystemPromptAndToolDescriptions` |
| 9 | `TestMasker_DigitBoundary`, `TestMasker_StandalonePhoneShapedNumbersStayMasked` |

The restore and request tests were mutation-checked: breaking the block grouping or the inner-JSON escaping makes them fail.

## Open before release

1. **SDK**: the policy uses `utils.SelectNormalizedPaths`, which exists only in `api-platform/sdk/core/utils/jsonpath_rfc9535.go` (untracked on branch `001-pii-masking-agent-traffic`, with tests). That checkout's `sdk/core` lacks the fault API the policy needs, so the file must be added to whichever branch `sdk/core` tags are cut from, then tagged.
2. **`go.mod`**: the policy's `go.mod` currently has a local `replace` for `sdk/core` pointing at a scratch copy of v0.4.2 plus the evaluator. Remove it and require the new tag before committing. Restore `go.sum` with `go mod tidy`.
3. **Integration run** of the new feature scenarios.

## Commands for you (not run)

```bash
# 1. SDK: after adding the two evaluator files to the SDK release branch
git tag sdk/core/vX.Y.Z && git push origin sdk/core/vX.Y.Z

# 2. Policy: in the gateway-controllers worktree, drop the replace and pin the tag
cd policies/pii-masking-regex
go mod edit -dropreplace github.com/wso2/api-platform/sdk/core
go get github.com/wso2/api-platform/sdk/core@vX.Y.Z && go mod tidy && go test ./...

# 3. Policy release
git tag policies/pii-masking-regex/v1.2.0 && git push origin policies/pii-masking-regex/v1.2.0

# 4. Gateway patch: bump the module reference in gateway/build.yaml to the new policy
#    version, then build gateway-runtime before gateway-controller and recreate.
```

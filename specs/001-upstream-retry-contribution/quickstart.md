# Quickstart: Validating Upstream Retry Contribution

## Prerequisites

- `gateway/gateway-controller` builds and its existing test suite passes (baseline, before this feature's changes).
- Familiarity with how `model-failover`'s own live verification works (`gateway/dev-policies/FAILOVER_TESTING.md`, `run-failover-e2e.sh`) — this feature's live check reuses the same mock-backend pattern, scoped down to a single target instead of a chain.

## Unit-level validation (`pkg/xds`, `pkg/transform`)

Follow the existing test conventions in `translator_test.go` / `model_failover_policy_test.go` — no new test infrastructure needed.

1. **No contribution, no failover chain → unchanged behavior** (guards spec FR-004/SC-002). Build a `RuntimeDeployConfig` route with `Upstream.Failover == nil` and `Upstream.RetryContributions` empty/nil. Assert the translated `RouteConfig`'s `RetryPolicy` is `nil`, exactly as today.
2. **One contribution, no failover chain → retry policy generated from it alone** (spec User Story 1). Route with one `UpstreamRetryContribution{StatusCodes: [401], MaxAttempts: 2}`, no `Failover`. Assert `RetryPolicy.RetryOn` includes `retriable-status-codes`, `RetriableStatusCodes == [401]`, and Envoy's `NumRetries == 1` (the `MaxAttempts − 1` translation — two total sends is one retry).
3. **Contribution + existing failover chain → union, neither dropped** (spec User Story 2 / FR-007). Route with both a `Failover` chain (its own retry fields) and one `UpstreamRetryContribution` carrying a different status code. Assert the merged `RetriableStatusCodes` contains both the chain's and the contribution's codes, and `NumRetries` reflects the max of the two `MaxAttempts` values (minus one).
4. **Two contributing policies on one route → both honored** (spec User Story 3). Two `UpstreamRetryContribution`s with different `StatusCodes`/`MaxAttempts`. Assert the merged policy is the union/max of both, per data-model.md's merge rule.
5. **`PerAttemptTimeoutMs` merges by taking the tightest value, not the loosest** (research.md Decision 7). Two contributions, `PerAttemptTimeoutMs: 2000` and `PerAttemptTimeoutMs: 500`. Assert the merged Envoy `PerTryTimeout` is `500ms` (the minimum), not `2000ms`. A third case: one contributor sets no timeout at all — assert it doesn't pull the merged result toward "unset" (an absent value never participates in the `min`).
6. **Policy-definition load-time validation** (data-model.md Validation Rules). A `policy-definition.yaml` fixture with `retry: {}` (no `statusCodes`/`statusCodesFromParam` and no `maxAttempts` mode set) is rejected by `PolicyLoader.LoadPoliciesFromDirectory`/`validatePolicy`, same error-reporting path other definition-shape errors already use. Also cover: both `statusCodes` and `statusCodesFromParam` set on one definition (rejected); more than one of `maxAttempts`/`maxAttemptsFromParam`/`maxAttemptsFromLongestList` set (rejected); and a `...FromParam` (or `maxAttemptsFromLongestList.paramPath`) naming a param absent from that policy's own `Parameters` schema (rejected).
7. **Derived-from-param contribution resolves per attachment** (data-model.md "Resolving a `...FromParam` field"). A fixture policy declares `retry: {statusCodesFromParam: rateLimitStatusCode, maxAttempts: 2}`. Two routes attach it with `rateLimitStatusCode: 429` and `rateLimitStatusCode: 503` respectively. Assert each route's resolved `UpstreamRetryContribution.StatusCodes` reflects *that instance's own* configured value, not the other route's or the schema's bare default.
8. **`enabledByParam` suppresses the contribution entirely when it resolves false** (data-model.md "Resolving a `...FromParam` field, and `enabledByParam`"). A fixture policy declares `retry: {enabledByParam: autoRetry, statusCodes: [429], maxAttempts: 2}`. One route attaches it with `autoRetry: false`. Assert that instance produces **no** `UpstreamRetryContribution` at all (not one with empty fields) — a disabled contributor must be indistinguishable from a policy with no `retry` block at all, for that attachment.
9. **`maxAttemptsFromLongestList` derives from a list param's length** (contracts/policy-definition-schema.md's chain-shaped worked example). A fixture policy declares `retry: {statusCodes: [503], maxAttemptsFromLongestList: {paramPath: fallbackTargets, plus: 1}}`. An attachment with `fallbackTargets: [a, b, c]` resolves to `MaxAttempts == 4`. A second attachment with a 1-element list resolves to `MaxAttempts == 2`.

## Live, end-to-end validation

Mirrors `gateway/dev-policies/FAILOVER_TESTING.md`'s existing mock-backend pattern, scoped to one target:

1. Attach `oauth2-generator` (with its updated `policy-definition.yaml` carrying `retry: {statusCodes: [401], maxAttempts: 2}`) to a plain test route with `upstream: true`, **no** `model-failover` chain.
2. Start a mock backend that returns `401` on its first call for a given test token, and `200` on every call after.
3. Send one request through the gateway. Expected: the client receives `200` — the first (401) attempt is invisible to the client, oauth2-generator's cached token was purged after the first attempt and refetched fresh before the retry, and the mock backend's call history shows two calls (one 401, one 200) for what the client experienced as a single successful request.
4. Repeat with the mock backend returning `401` on every call (never recovering). Expected: the client receives `401` — exactly one retry happened (not more, matching `maxAttempts: 2` → Envoy `NumRetries: 1`), and the client sees the final attempt's response, consistent with `model-failover`'s own established exhaustion-response convention (spec Edge Cases).
5. Re-run the *existing* `run-failover-e2e.sh` suite unmodified, to confirm a route carrying a `model-failover` chain is byte-for-byte unaffected by this feature (spec SC-002/FR-004, and the no-regression guarantee in contracts/policy-definition-schema.md).

## Expected outcome

All nine unit-level cases and all three live scenarios pass without any configuration beyond: `oauth2-generator`'s own `policy-definition.yaml` change, and `upstream: true` on its attachment. No operator-facing config was added anywhere to make this work, per spec SC-001.

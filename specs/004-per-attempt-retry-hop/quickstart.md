# Quickstart: Validating the Per-Attempt Retry Hop

Prerequisites: the gateway IT stack (see the model-failover IT notes: Rancher `DOCKER_HOST`, locally built sample-service and mock images, `SDKROOT`), with coverage images built from this branch.

| # | Setup | Action | Expected |
|---|---|---|---|
| 1 | Test policy `retry-once-on-418` (definition: `runs: onEveryAttempt`, `canRetry.maxAttempts: 2`) on a RestApi; mock returns 418 then 200 | request | 200; backend saw 2 requests; gateway source unchanged for this policy |
| 2 | Same, its `enabledByParam` param set to false | request | 418; operation's generated config identical to one without the policy |
| 3 | Test policy asks to retry 3 times with `canRetry.maxAttempts: 2` | request | 2 attempts only; a warning logged |
| 4 | Signing test policy (`runs: onEveryAttempt`, no `canRetry`) + retry test policy | request needing a retry | both attempts carry different signatures |
| 5 | Existing model-failover suites (godog + Postman) | run | all pass |
| 6 | `grep` gateway-controller for model-failover-specific logic | inspect | none |
| 7 | OAuth2 upstream auth, `retryOnUnauthorized: true`; mock backend rejects token #1, accepts #2 | request | 200; IdP issued 2 tokens total |
| 8 | Same, 50 concurrent requests with token #1 cached | burst | all 200; IdP issued exactly 1 new token |
| 9 | Same; backend rejects every token | request | fixed `502 upstream_auth_failed`; 2 attempts |
| 10 | Same; backend returns 403 | request | 403; 1 attempt |
| 11 | model-failover + OAuth2 refresh on one LLM op; target 1 rejects token #1 then 503 | request | refresh on target 1, then target 2 serves; target 1 health counts one failure |
| 12 | Combined allowance over the cap | register | 400 naming the cap |
| 13 | Client sends `x-wso2-attempt-retry` / `-scope` | request | stripped; no effect |
| 14 | Git diff for the OAuth2 increment | inspect | no files under `gateway/gateway-controller` or `gateway/gateway-runtime` |

# Contract: What a Policy Author Provides

A policy that retries, or must run per attempt, changes **only** its own `policy-definition.yaml` and code.

## Declarations (`retryBehavior` in `policy-definition.yaml`)

| Key | Required | Meaning |
|---|---|---|
| `runs` | no (default `onClientRequest`) | When this policy runs if the gateway may send the request more than once: `onClientRequest`, `onEveryAttempt`, or `onBoth` |
| `canRetry` | no | Present only if the policy can ask for the request to be sent again |
| `canRetry.enabledByParam` | no | The boolean param that turns retrying on; absent means always on |
| `canRetry.maxAttempts` / `maxAttemptsFromParam` / `maxAttemptsFromLongestList` | exactly one | The most sends this policy can cause, the first included: a number, a numeric param, or the longest list at a param path plus a number |
| `canRetry.perAttemptTimeout` / `perAttemptTimeoutFromParam` | at most one | How long one attempt may wait for the backend to start answering; the tightest value on an operation wins |
| `canRetry.seesConnectionFailures` | no (default `false`) | Also report connection failures, resets and timeouts to this policy |
| `sameOperation[]` | no | `{policy, allowed: never}` or `{policy, allowed: onlyIf, onlyIf: {runsBeforeThisPolicy, paramNotSet}}` |
| `x-wso2-refers-to: attached-provider` (in a param's schema) | no | The value must name a provider attached to the API or proxy |

### Examples

```yaml
# oauth2-generator: refreshes its token and retries once on a 401
retryBehavior:
  runs: onEveryAttempt
  canRetry:
    enabledByParam: retryOnUnauthorized
    maxAttempts: 2

# model-failover: picks a chain per request, tries one target per attempt
retryBehavior:
  runs: onBoth
  canRetry:
    maxAttemptsFromLongestList: { path: chains[].fallbacks, plus: 1 }
    perAttemptTimeoutFromParam: perAttemptTimeout
    seesConnectionFailures: true
  sameOperation:
    - { policy: llm-header-router, allowed: never }
    - policy: model-round-robin
      allowed: onlyIf
      onlyIf: { runsBeforeThisPolicy: true, paramNotSet: "models[].provider" }

# set-headers (api-key credentials): never retries, but must run again on every attempt
retryBehavior:
  runs: onEveryAttempt
```

## Runtime behaviour the gateway guarantees

| Policy returns (response phase) | Gateway does |
|---|---|
| plain modifications | Passes the response on; no retry caused by this policy |
| `RetryAttempt{Reason}` | Resends the original request, if the operation's budget and this policy's max attempts allow; the next attempt's `Attempt()` shows the requester and reason |
| `StopAttempts{Response}` | No more attempts; the client gets `Response` (or the current response) with every internal tag removed |

| Also guaranteed | |
|---|---|
| Per-request policies run once per client request | Per-attempt policies run once per attempt |
| No retry after a response starts streaming | Internal `x-wso2-attempt-*` headers never reach a client or a backend, and are stripped when a client sends them |
| A body over `router.attempts.max_request_body_bytes` is sent once | `Attempt()` is nil on unsplit operations |

## Registration errors

| Condition | Result |
|---|---|
| A `maxAttempts*` source is missing, not a number, or below 1 (for example `maxAttemptsFromParam` names an unset param) | `400` naming the policy and key |
| Operation's combined retries exceed `router.attempts.max_retries` | `400` naming the cap |
| An `x-wso2-refers-to: attached-provider` value is not attached | `400` with the field path |
| A `sameOperation` rule is broken | `400` naming both policies and the rule |

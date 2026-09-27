# Data Model: A Per-Attempt Retry Hop Any Policy Can Use

## Policy definition additions (`policy-definition.yaml`)

One optional block, `retryBehavior`. Every key reads as a statement about the policy, and none of it needs expressions.

```yaml
retryBehavior:
  # Where this policy runs when the gateway may send a request more than once.
  #   onClientRequest - once per client request (default): guardrails, quotas, analytics
  #   onEveryAttempt  - once per attempt: credentials, request signing, transformers
  #   onBoth          - one part per client request and one part per attempt (model-failover)
  runs: onEveryAttempt

  # Only for a policy that can itself ask the gateway to send the request again.
  canRetry:
    # The boolean param of this policy that turns retrying on. Omit it and retrying is always on.
    enabledByParam: retryOnUnauthorized

    # The most times this policy can cause the request to be sent, the first send included.
    # Give exactly one of:
    maxAttempts: 2                                  # a fixed number
    # maxAttemptsFromParam: maxAttempts             # the value of a numeric param
    # maxAttemptsFromLongestList:                   # the longest list at a path in the params, plus a number
    #   path: chains[].fallbacks
    #   plus: 1

    # How long one attempt may wait for the backend to start answering.
    # Give at most one; omit both for the gateway default.
    perAttemptTimeoutFromParam: perAttemptTimeout   # the value of a duration param
    # perAttemptTimeout: 30s                        # a fixed duration

    # Also tell this policy when an attempt fails with a connection failure, reset or timeout.
    seesConnectionFailures: false

  # Rules for other policies attached to the same operation, checked when the API is registered.
  sameOperation:
    - policy: llm-header-router
      allowed: never
    - policy: model-round-robin
      allowed: onlyIf
      onlyIf:
        runsBeforeThisPolicy: true                  # it must appear earlier in the policy list
        paramNotSet: models[].provider              # none of its entries may name a provider
```

A parameter schema may mark a string field with `x-wso2-refers-to: attached-provider`. The gateway then checks, at registration, that its value names a provider attached to the API or proxy.

`models.PolicyDefinition` gains `RetryBehavior *RetryBehavior`, with `Runs`, `CanRetry` and `SameOperation`.

### Before and after

| Earlier draft | Now | Why |
|---|---|---|
| `execution: request \| attempt \| both` | `runs: onClientRequest \| onEveryAttempt \| onBoth` | Says when the policy runs, not an internal mode |
| `attempts:` | `canRetry:` | Present only when the policy can ask for a resend |
| `enabled: "params.x == true"` (CEL) | `enabledByParam: x` | A param name, no expression |
| `maxAttempts: "<CEL>"` | `maxAttempts` / `maxAttemptsFromParam` / `maxAttemptsFromLongestList` | Three named sources cover every current need |
| `perAttemptTimeout: "<CEL>"` | `perAttemptTimeout` / `perAttemptTimeoutFromParam` | Same |
| `transportFailures: true` | `seesConnectionFailures: true` | Plain words |
| `conflictsWith: [{policy, unless: "<CEL>"}]` | `sameOperation: [{policy, allowed: never \| onlyIf, onlyIf: {...}}]` | A fixed set of named conditions |
| `x-wso2-ref: provider` | `x-wso2-refers-to: attached-provider` | Says what the value must match |

## Controller-derived route settings (RDC)

| Field | Source |
|---|---|
| `Split` | any attached policy with `canRetry` whose `enabledByParam` is true (or absent) |
| `NumRetries` | min(Σ(max attempts − 1), `router.attempts.max_retries`), from each policy's resolved `maxAttempts*` |
| `PerTryTimeout` | min(resolved `perAttemptTimeout*`), else `router.attempts.default_per_attempt_timeout` |
| `RouteTimeout` | (NumRetries+1) × PerTryTimeout + margin, raised by an explicit operation timeout |
| `RetryOn` | `retriable-headers`, plus `connect-failure`/`reset` if any policy has `seesConnectionFailures: true` |

## Internal params injected into `onEveryAttempt` and `onBoth` instances

| Key | Meaning |
|---|---|
| `_runningAs` | `clientRequest` or `attempt`: which part of an `onBoth` policy this instance is |
| `_attemptScope` | the operation's scope token (today's chain token) |
| `_hopSecret` | the per-boot secret for transport-failure labels |

## Policy engine: attempt-scope store

| Field | Meaning |
|---|---|
| scope id | minted by `attempt-coordinator` on the front, sent as `x-wso2-attempt-scope` |
| operation token | must match the per-attempt route's `_attemptScope` (forgery check) |
| attempt number | incremented per per-attempt arrival |
| last requester, reason | set from the previous attempt's `RetryAttempt` |
| per-policy state | `map[policyName]map[string]any`, opaque to the engine |
| retries used per policy | enforces each policy's resolved max attempts |
| expiry | (NumRetries+1) × PerTryTimeout + margin; swept |

## SDK surface (v1alpha2 additions)

```go
type AttemptContext struct {
    Number           int               // 1 for the first attempt
    RequestedBy      string            // policy that asked for this attempt ("" for the first)
    Reason           string
    TransportFailure string            // response phase only, when declared
    State            map[string]any    // this policy's state across attempts
}
func (c *RequestHeaderContext) Attempt() *AttemptContext   // nil when the operation isn't split
func (c *ResponseHeaderContext) Attempt() *AttemptContext

type RetryAttempt struct{ Reason string; Mods DownstreamResponseHeaderModifications }
type StopAttempts struct{ Response *ImmediateResponse } // nil = pass the current response
```

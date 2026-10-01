# Contract: `retry` in `policy-definition.yaml`

This is the one new surface this feature exposes to anyone outside `gateway-controller` itself — a policy author adding `retry` to their own `policy-definition.yaml`. It is **not** a surface exposed to an operator attaching a policy to a route (see spec FR-006/FR-009 — no per-attachment override exists in this version).

## Shape

| Key | Required | Meaning |
|---|---|---|
| `retry` | no | Present only if this policy can make the gateway retry a request against the same backend target. |
| `retry.enabledByParam` | no | Name of a boolean param on this same policy that turns the whole contribution on or off for a given attachment. Absent means always on. |
| `retry.statusCodes` / `statusCodesFromParam` | exactly one | Which backend response status codes justify a retry: a fixed list, or the name of a `[]int` param on this same policy. |
| `retry.maxAttempts` / `maxAttemptsFromParam` / `maxAttemptsFromLongestList` | exactly one | The most times the gateway will send this request in total (first send included): a fixed count, an integer param's name, or one more than the length of a list-shaped param. |
| `retry.perAttemptTimeoutMs` / `perAttemptTimeoutMsFromParam` | at most one | How long a single attempt may run before the gateway gives up on it. Across every contributing policy on a route, the **tightest** resolved value wins (see research.md Decision 7). |
| `retry.alsoRetryOnConnectionFailure` / `...FromParam` | no (default `false`) | Also retry when the connection to the backend itself fails, not only on a response status. |

Each condition is either a **fixed** literal or **derived from one of this policy's own params** — never both. Use fixed for a true domain constant (oauth2-generator's 401 never varies); use derived when the right condition genuinely depends on how an operator configured that instance of the policy.

```yaml
name: <policy-name>
version: <version>
...
retry:
  enabledByParam: <paramName>              # optional

  statusCodes: [<int>, ...]                # fixed — OR —
  statusCodesFromParam: <paramName>        # derived: must name an []int param in this policy's own schema

  maxAttempts: <int>                       # fixed — OR —
  maxAttemptsFromParam: <paramName>        # derived: must name an int param — OR —
  maxAttemptsFromLongestList:              # derived from a list's length
    paramPath: <paramName>
    plus: <int>

  perAttemptTimeoutMs: <int>               # optional, fixed — OR —
  perAttemptTimeoutMsFromParam: <paramName>     # derived: must name an int param

  alsoRetryOnConnectionFailure: <bool>     # optional, fixed, default false — OR —
  alsoRetryOnConnectionFailureFromParam: <paramName>  # derived: must name a bool param
```

## Contract guarantees

1. **Omitting the field entirely is always safe.** A `policy-definition.yaml` with no `retry` behaves identically before and after this feature ships — this is the contract spec FR-004/SC-002 depend on holding for every existing policy that does nothing.
2. **No new operator-facing retry config exists.** There is no route, `operationPolicies:`/`globalPolicies:` field that sets or overrides a retry condition. The *derived* and `enabledByParam` forms don't change this: an operator who sets the referenced param is configuring that param for whatever reason the policy's own documentation already gives it — they are never filling in a field named "retry."
3. **Validity is independent of any specific deployment.** Every malformed-shape case in data-model.md's Validation Rules (both modes of a condition set, a `...FromParam` naming a param that doesn't exist in the policy's own schema, `statusCodes`/`maxAttempts` left entirely unset) is rejected at policy-load time, the same point other definition-level shape errors are already caught — never surfaced as a per-deploy error later.
4. **Multiple policies on one route each declaring `retry` is always safe and additive** — see data-model.md's merge rule (broadest wins for everything except per-attempt timeout, which takes the tightest). No combination of valid individual declarations can produce an invalid merged route retry policy.

## Worked example: `oauth2-generator` (fixed mode)

```yaml
name: oauth2-generator
version: v1
...
retry:
  statusCodes: [401]
  maxAttempts: 2
```

A bearer-token rejection is always `401` — a true domain constant, independent of how any instance is configured — so this uses the fixed form. `maxAttempts: 2` means "the original send plus one retry" (compare to Envoy's own `NumRetries: 1`, which this value translates to once, at merge time — see data-model.md). Paired with this policy's own existing `purgeStatusCodes` param (default `[401]`, unchanged by this feature) and its existing, already-correct `OnResponseHeaders` (purges the cached token on 401) and `OnRequestHeaders` (fetches fresh) — attaching `oauth2-generator` to any route with `upstream: true` now gets a correct retry-with-fresh-token behavior with zero additional configuration, per spec User Story 1 / SC-001.

## Worked example: a hypothetical backend-aware policy (derived mode + `enabledByParam`)

A policy that already exposes its own `rateLimitStatusCode` param (because different backends it talks to signal rate-limiting differently) and an `autoRetryOnRateLimit` toggle (because some operators would rather see the 429 directly than have the gateway retry silently) can express both without any new operator-facing config:

```yaml
name: some-backend-policy
version: v1
parameters:
  type: object
  properties:
    rateLimitStatusCode:
      type: integer
      default: 429
    autoRetryOnRateLimit:
      type: boolean
      default: true
...
retry:
  enabledByParam: autoRetryOnRateLimit
  statusCodesFromParam: rateLimitStatusCode
  maxAttempts: 2
```

An operator attaching this with `rateLimitStatusCode: 503, autoRetryOnRateLimit: false` for one backend gets no retry contribution from this instance at all; another attachment with the defaults gets a contribution of `{statusCodes: [429], maxAttempts: 2}` — both resolved per-instance, from params the operator already had a reason to set.

## Worked example: a chain-shaped policy (`maxAttemptsFromLongestList`)

A policy whose retry budget is naturally "one attempt per entry in my own fallback list" (the shape `model-failover` itself has, noted in spec.md as a deferred future consumer of this same contract, not built by this feature):

```yaml
name: some-chain-policy
version: v1
parameters:
  type: object
  properties:
    fallbackTargets:
      type: array
      items: { type: string }
...
retry:
  statusCodes: [503]
  maxAttemptsFromLongestList:
    paramPath: fallbackTargets
    plus: 1
```

An attachment with three entries in `fallbackTargets` resolves to `maxAttempts: 4` (three fallbacks plus the primary) — tracking the list automatically as an operator adds or removes fallback targets, with no second number to keep in sync.

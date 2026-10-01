# Phase 1 Data Model: Upstream Retry Contribution

## Retry

Declared once, statically, inside a policy's own `policy-definition.yaml`, under the key `retry`. Never present in an author-written `operationPolicies:`/`globalPolicies:` attachment as its own retry-specific block — an *operator* cannot add a new field to turn retrying on or off. What an operator *can* do is set whatever param `enabledByParam`/`...FromParam` fields point at — but that's always a param the policy already has its own reason to expose, never a field that exists solely for this feature.

| Key | Required | Meaning |
|---|---|---|
| `retry` | no | Present only if this policy can make the gateway retry a request against the same backend target. Absent (the common case) means this policy contributes nothing. |
| `retry.enabledByParam` | no | Name of a boolean param on this same policy that turns the whole contribution on or off for a given attachment. Absent means always on. |
| `retry.statusCodes` / `statusCodesFromParam` | exactly one | Which backend response status codes justify a retry: a fixed list, or the name of a `[]int` param on this same policy. |
| `retry.maxAttempts` / `maxAttemptsFromParam` / `maxAttemptsFromLongestList` | exactly one | The most times the gateway will send this request in total (the first send plus every retry): a fixed count, the name of an integer param, or one more than the length of a list-shaped param — see "maxAttemptsFromLongestList" below. |
| `retry.perAttemptTimeoutMs` / `perAttemptTimeoutMsFromParam` | at most one | How long a single attempt may run before the gateway gives up on it and (if attempts remain) tries again. Across every contributing policy on a route, the **tightest** resolved value wins — see Decision 7 in research.md for why. |
| `retry.alsoRetryOnConnectionFailure` / `...FromParam` | no (default `false`) | Also retry when the connection to the backend itself fails (refused, reset, timed out) — not only on a response status. Maps to Envoy's `connect-failure`/`reset`/`reset-before-request` retry conditions. |

Every condition except `enabledByParam` follows the same two-mode pattern, chosen per field by whoever authors that policy's definition:

- **Fixed** — a literal value, for a condition that's a true domain constant regardless of how any instance of the policy is configured (oauth2-generator's `401` is the canonical case).
- **`...FromParam`** — reads the named parameter off *this specific attachment's own configured params* at resolution time, for a condition that legitimately varies by how an operator configured that instance (e.g., a policy fronting multiple backend types, where "what status means rate-limited" depends on which backend an operator pointed it at).

Setting both forms of the same condition is a policy-definition error (see Validation Rules).

### `maxAttemptsFromLongestList`

A third mode, specific to `maxAttempts`, for a policy whose natural retry budget is driven by the size of one of its own list-shaped params rather than a plain number — the canonical future case being `model-failover` itself, whose retry count is really "one attempt per member of my own fallback chain," not a number chosen independently of that list:

```yaml
retry:
  maxAttemptsFromLongestList:
    paramPath: fallbackTargets   # dot-separated path into this policy's own Parameters
    plus: 1                      # added to that list's resolved length
```

Resolved as `len(list at paramPath) + plus`. If the referenced param isn't present on a given attachment, or doesn't resolve to a list, this field resolves to its default the same way any other unresolved `...FromParam` does (see "Resolving a `...FromParam` field" below) — it does not fail the deploy.

Go representation:

```go
type LongestListAttempts struct {
    ParamPath string `json:"paramPath" yaml:"paramPath"`
    Plus      int    `json:"plus,omitempty" yaml:"plus,omitempty"`
}

type Retry struct {
    EnabledByParam string `json:"enabledByParam,omitempty" yaml:"enabledByParam,omitempty"`

    StatusCodes           []int  `json:"statusCodes,omitempty" yaml:"statusCodes,omitempty"`
    StatusCodesFromParam  string `json:"statusCodesFromParam,omitempty" yaml:"statusCodesFromParam,omitempty"`

    MaxAttempts                 int                   `json:"maxAttempts,omitempty" yaml:"maxAttempts,omitempty"`
    MaxAttemptsFromParam        string                `json:"maxAttemptsFromParam,omitempty" yaml:"maxAttemptsFromParam,omitempty"`
    MaxAttemptsFromLongestList  *LongestListAttempts  `json:"maxAttemptsFromLongestList,omitempty" yaml:"maxAttemptsFromLongestList,omitempty"`

    PerAttemptTimeoutMs          int    `json:"perAttemptTimeoutMs,omitempty" yaml:"perAttemptTimeoutMs,omitempty"`
    PerAttemptTimeoutMsFromParam string `json:"perAttemptTimeoutMsFromParam,omitempty" yaml:"perAttemptTimeoutMsFromParam,omitempty"`

    AlsoRetryOnConnectionFailure          bool   `json:"alsoRetryOnConnectionFailure,omitempty" yaml:"alsoRetryOnConnectionFailure,omitempty"`
    AlsoRetryOnConnectionFailureFromParam string `json:"alsoRetryOnConnectionFailureFromParam,omitempty" yaml:"alsoRetryOnConnectionFailureFromParam,omitempty"`
}
```

Added to `PolicyDefinition` (`pkg/models/policy_definition.go`) as `Retry *Retry` (nil = this policy contributes nothing, the common case for most policies).

### Resolving a `...FromParam` field, and `enabledByParam`, for one attachment

At transform time, for a policy instance attached to a route, each `...FromParam` field (and `enabledByParam`) is resolved by reading the named key out of *that instance's own configured `Params`* — not the policy's declared schema, the actual value this specific attachment set (or didn't). Resolution order per field:

1. The named param is present in this instance's `Params` → use its value.
2. Not present, but the policy's own `Parameters` JSON schema declares a `default` for that param name → use the schema default (the same default the policy's own Go code would see).
3. Neither present → this one field resolves to its own field-level default for *this instance only* — it does not disqualify the instance's other fields, and does not fail the deploy. That default is empty/`[]` for status codes, `false` for `alsoRetryOnConnectionFailure`, unset for `perAttemptTimeoutMs`, and **`1`** for `maxAttempts`/`maxAttemptsFromLongestList` specifically — "no extra attempts contributed by this field," not an error, consistent with this contribution's other fields still taking effect normally.

`enabledByParam` follows the same order but resolves to a single bool; if it resolves to `false`, this instance contributes nothing to the route's retry policy at all (every other field is skipped — there's no meaning in resolving a status-code list for a disabled contribution).

A `...FromParam`/`enabledByParam` value naming a param that does not exist anywhere in that policy's own `Parameters` schema is a **policy-definition** error (Validation Rule 2) — a shipped-policy bug, not a deployment problem, caught once at policy-load time rather than silently resolving to nothing on every deploy that uses it.

## UpstreamRetryContribution

The resolved, per-route-per-attached-instance form of the above, produced during the transform phase once a policy's definition (and therefore its `Retry`, if any) is known for a specific attachment, `enabledByParam` has already been evaluated (a `false` resolution means no `UpstreamRetryContribution` is produced for that instance at all), and every `...FromParam`/`...FromLongestList` field has been resolved to a concrete value.

```go
// pkg/models/runtime_deploy_config.go
type UpstreamRetryContribution struct {
    PolicyName                   string // provenance only — for observability/debugging, never read by the merge itself
    StatusCodes                  []int
    MaxAttempts                  int  // total sends, first attempt included — see translation note below
    PerAttemptTimeoutMs          int
    AlsoRetryOnConnectionFailure bool
}
```

Collected onto the existing `Route` model:

```go
// RouteUpstream struct (pkg/models/runtime_deploy_config.go:141), alongside the existing Failover *RouteFailover field
RetryContributions []UpstreamRetryContribution
```

Empty/nil slice is the default and common case — a route with no contributing policy attached (or where every contributor's `enabledByParam` resolved to `false`) sees no change to this field, and therefore (per the broadened translator gate in research.md Decision 1) no change to its compiled `RetryPolicy` either.

## Merged Route Retry Policy (output, not a new stored type)

Not a persisted entity — the output of folding every `UpstreamRetryContribution` on a route (plus, when present, `RouteFailover`'s own retry fields, treated as exactly one more contribution) into the single `route.RetryPolicy` Envoy object `translator.go` already knows how to emit:

- `RetryOn`: `"5xx"` (or `"retriable-status-codes"` if any contributor supplies explicit codes) plus `connect-failure,reset,reset-before-request` if any contributor's `AlsoRetryOnConnectionFailure` is `true` or a failover chain is present (matching the existing unconditional-transport-conditions behavior `model-failover` already established).
- `RetriableStatusCodes`: union of every contributor's `StatusCodes`, plus `RouteFailover`'s own retriable codes when present.
- **`NumRetries` (Envoy's field) = `max(every contributor's MaxAttempts, failover-derived total attempts) − 1`.** Envoy's `RetryPolicy.NumRetries` counts retries *after* the first send; `UpstreamRetryContribution.MaxAttempts` counts total sends, first included (deliberately, so a policy author declares "how many times should this go out" without needing to remember Envoy's off-by-one convention). The merge takes the **max** of every contributor's `MaxAttempts` — a shortfall here, unlike the timeout field below, directly removes a contributor's own declared retry chance entirely, so the broadest requested attempt count must win. The `− 1` translation happens exactly once, at the point this merged value is written into Envoy's `RetryPolicy`, never inside any per-contributor resolution step.
- **`PerTryTimeout` (Envoy's field) = `min` across every contributor's resolved `PerAttemptTimeoutMs` and, when present, `RouteFailover`'s own per-try timeout** — the **tightest**, not the loosest, value wins. See research.md Decision 7 for why this is `min` rather than `max`: a long per-attempt timeout from one contributor can otherwise silently consume a route's entire overall request budget before any other contributor's retry even gets a chance to run, so the tightest declared need takes priority over the loosest. A contributor with no opinion (field unset) does not participate in the `min`.

## Validation Rules

All of the following are **policy-definition-level** checks, run once at policy-load time (`validatePolicy` in `pkg/utils/policy_loader.go`, the same point other definition-shape errors are already caught) — none of them depend on any specific deployment, so none of them belong as a per-deploy error:

1. Setting both forms of the same condition (e.g., both `statusCodes` and `statusCodesFromParam`, or more than one of `maxAttempts`/`maxAttemptsFromParam`/`maxAttemptsFromLongestList`) is rejected — ambiguous by construction, not resolvable by priority rules.
2. A `...FromParam`/`enabledByParam` value, and `maxAttemptsFromLongestList.paramPath`, MUST name a parameter that exists in that same policy's own `Parameters` JSON schema. A typo'd or nonexistent reference is a defect in the shipped policy definition, caught once here rather than silently resolving to nothing on every future deploy that uses it.
3. A `...FromParam` value's target parameter SHOULD be schema-typed consistent with the field it feeds (`statusCodesFromParam`/`maxAttemptsFromParam`/`perAttemptTimeoutMsFromParam` → array-of-integer or integer respectively, `enabledByParam`/`alsoRetryOnConnectionFailureFromParam` → boolean, `maxAttemptsFromLongestList.paramPath` → an array of any type). A mismatch is rejected the same way.
4. `retry` with neither mode set for `statusCodes` is rejected — status codes are never optional, since Envoy's retry policy has nothing else to key a response-driven retry decision on. `maxAttempts` (exactly one of its three modes) is likewise always required once `retry` is present at all.
5. A *fixed* `maxAttempts < 1` or `perAttemptTimeoutMs < 0` is rejected (a fixed `LongestListAttempts.plus` below `0` likewise); a *derived* value resolving to an invalid number at transform time is treated as resolution failure for that one field on that one instance — logged, not a deploy-time hard error, consistent with the graceful-degradation stance in "Resolving a `...FromParam` field" above.

No per-route validation is introduced beyond what `RouteFailover` already enforces today — merging already-resolved contributions is pure union/max/min, which cannot itself produce an invalid state the way conflicting `suspendDuration`s on a shared cluster can (research.md Decision 4).

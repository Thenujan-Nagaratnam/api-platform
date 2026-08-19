# Generalized Retry Conditions — Design

## Status

Draft. Supersedes the retry-trigger/retry-source declaration shape introduced in
`2026-08-14-decoupled-retry-source-design.md` and consumed by
`2026-08-13-model-failover-design.md`. Both prior docs' architecture (two SDK
declaration types, generic gateway-controller discovery/validation, the
upstream ext_proc split, aggregate clusters for target-switching) stays intact
— this doc only replaces what a policy or operator can *express* within that
architecture, and how multiple contributors on one route *compose*.

`oauth2-generator` no longer participates in this mechanism as of the
`decoupled-retry-source` branch's later self-retry work (2026-08-17,
same-session) — it purges and calls the backend directly from
`OnResponseHeaders` instead of asking Envoy to redial. It is not a target
consumer of this redesign. `model-failover` is the only current consumer and
the primary migration target.

## Problem

An audit (2026-08-17) compared this codebase's retry-trigger/retry-source
abstraction against Envoy's real `RouteAction.RetryPolicy`, grounded in the
vendored proto (`go-control-plane/envoy@v1.37.0/config/route/v3/route_components.pb.go`).
Findings, most-constraining first:

1. `RetryOn` is hardcoded to `"retriable-status-codes"` in every builder
   (`buildRetryPolicy`, `buildRetrySourceRetryPolicy`,
   `buildRetryTriggerRetryPolicy` — `gateway-controller/pkg/xds/translator.go`).
   A backend that resets the connection, times out, or never responds at all
   can never trigger this mechanism — not a richness gap, a whole failure
   class this system can't see.
2. `RetryTriggerDeclaration` has no `PerAttemptTimeout` — only
   `RetrySourceDeclaration` does. A trigger-only policy can never bound one
   attempt's duration.
3. Only ever one flat status-code list. No path to Envoy's
   `RetriableHeaders`/`RetriableRequestHeaders`.
4. `NumRetries` can only ever be pushed up (`buildRetrySourceRetryPolicy`,
   `composeRetryTriggerPolicy` both union-toward-larger) — no contributor can
   ask for an exact, independent count.
5. `minAttempts` is a fixed YAML constant at schema-authoring time, not a
   param — not runtime-adjustable, not per-operation-overridable.
6. `RetryPriority` hardcoded to exactly `envoy.retry_priorities.previous_priorities`,
   no pluggability.
7. `RetryBackOff`/`RateLimitedRetryBackOff` never set anywhere, by anything —
   confirmed the operator-facing `api.Retry` type itself
   (`gateway-controller/pkg/api/management/generated.go:1437`) only exposes
   `NumRetries`/`StatusCodes`. Even a plain operator with zero policies
   attached has no path to backoff or per-try timeout today.
8. `RetryHostPredicate`/`HostSelectionRetryMaxAttempts` absent — a plain
   trigger-only retry can resend to the exact same failed host.
9. `HedgePolicy` (Envoy's separate speculative-fanout mechanism) has no
   equivalent anywhere in this system.

Two further findings are explicitly **not** addressed by this doc (see
Non-Goals):

10. Aggregate clusters can't nest — `RetryTarget{UpstreamDefinitionName string}`
    has no way to reference another `RetryGroup`. Self-imposed by this
    codebase's Go type, not an Envoy limitation.
11. "One retry-source per route" is a genuine Envoy limit — `RetryPriority` is
    a **singular** field on the real proto (`route_components.pb.go:2665`),
    confirmed not repeated. Not fixable by generalizing the abstraction.

## Goals

- One shared, Envoy-mirrored vocabulary (`RetryConditions`) that both the
  operator-facing `resilience.retry` and a policy's declared
  trigger/source metadata resolve into — closing gaps 1–8 above.
- Field-level composition rules so multiple contributors (operator +
  N trigger policies + at most one source policy) can coexist on one route,
  replacing today's blunt "retry-source excludes resilience.retry entirely"
  rule with real per-field conflict detection.
- One generic declaration mechanism (literal-or-`{fromParam}`) usable by any
  policy, so adding a new expressible field is a `RetryConditions` struct
  change, not a new bespoke parser function per field.
- Preserve every existing invariant this codebase already relies on:
  gateway-controller never executes policy Go code to discover a
  declaration; validation stays structural and fail-closed; the upstream
  ext_proc split, aggregate-cluster construction, and `UpstreamAttemptPolicy`/
  `UpstreamAttemptResponseObserver` runtime contracts are unchanged.

## Non-Goals

- Aggregate cluster nesting (finding 10) — a real but separable feature;
  would need its own `RetryTarget` redesign (e.g. a target referencing a
  group key instead of only an upstreamDefinition) and its own validation
  story for cycles. Not attempted here.
- Lifting the one-retry-source-per-route limit (finding 11) — genuinely
  impossible without a different Envoy mechanism entirely (e.g. a custom
  `RetryPriority` extension multiplexing two behaviors, which is out of
  scope for a gateway-controller-side change).
- `HedgePolicy` (finding 9) — a materially different mechanism (concurrent
  speculative requests, not sequential retry) with its own resource and
  cost implications; deserves its own design, not a field bolted onto
  `RetryConditions`.
- Migrating `oauth2-generator` — already moved off this mechanism entirely
  (see Status).

## Architecture

### Component 1 — `RetryConditions`, the shared vocabulary

New SDK type, `sdk/core/policy/v1alpha2/retry_conditions.go`, replacing
`RetryTriggerDeclaration` and the bare `RetriableStatusCodes []int` field
currently embedded in `RetrySourceDeclaration`:

```go
type RetryConditions struct {
    On                 []string          // "5xx" | "gateway-error" | "reset" | "connect-failure" |
                                          // "refused-stream" | "envoy-ratelimited" | "retriable-4xx" |
                                          // "retriable-status-codes" | "retriable-headers"
    StatusCodes        []int             // used when On includes "retriable-status-codes"
    Headers            []RetriableHeader // used when On includes "retriable-headers"
    NumRetries         *int              // exact count this contributor wants; nil = no opinion
    MinAttempts        *int              // "at least N total attempts" — see composition rules
    PerTryTimeout      *time.Duration
    BackOff            *RetryBackOff
    AvoidPreviousHosts bool
}

type RetriableHeader struct {
    Name  string
    Exact string // start narrow; extend to Regex/Present only if a real consumer needs it
}

type RetryBackOff struct {
    BaseInterval time.Duration
    MaxInterval  *time.Duration
}
```

`RetrySourceDeclaration` keeps `Groups`/`OrderedTargets`/`PerAttemptTimeout`
(unrelated to this redesign) but drops its own `RetriableStatusCodes []int`
outright, with no replacement field on the type. A retry-source policy's own
status-code/On/etc. contribution flows through the exact same
`x-wso2-retry-conditions` declaration path every other policy uses —
gateway-controller's discovery loop parses a chain member's retry-source
metadata and its retry-conditions metadata independently in the same pass,
so a dedicated `Conditions` field on `RetrySourceDeclaration` would just be
a second, redundant path to the same merged result.

`api.Retry` (operator-facing OpenAPI type,
`gateway-controller/pkg/api/management/generated.go`) gains the same fields
`RetryConditions` has — `On`, `Headers`, `PerTryTimeout`, `BackOff`,
`AvoidPreviousHosts` — alongside its existing `NumRetries`/`StatusCodes`, and
resolves into a `RetryConditions` the same way a policy's declaration does.

### Component 2 — Composition rules

gateway-controller collects one `RetryConditions` per contributor on a route
— the operator's `resilience.retry` (if configured), one per retry-trigger
policy in the chain, one from the retry-source policy if present — and
merges field-by-field via `mergeRetryConditions(contributions []RetryConditions) (RetryConditions, error)`:

| Field | Rule | Why |
|---|---|---|
| `On`, `StatusCodes`, `Headers` | Union (dedup) | Envoy's `RetryOn` is itself OR-composed; safe to combine. |
| `AvoidPreviousHosts` | OR | More defensive behavior never harms a contributor that didn't ask for it. |
| `MinAttempts` | Max (floor only, never lowered) | Matches today's `composeRetryTriggerPolicy` semantics exactly; retry-source's chain-length requirement folds into this same mechanism instead of a separate `maxChain` computation. |
| `PerTryTimeout` | Min (tightest ceiling wins) | A contributor can only tighten another's budget, never widen it — same spirit as `go-network-service-hardening.md` directive 5. |
| `NumRetries` (exact) | At most one contributor may set it; **reject** on conflicting explicit values | No sane union for "I want exactly N" vs. "I want exactly M". If unset by all, derive as `max(MinAttempts) - 1`, same as today. |
| `BackOff` | At most one contributor may set it; **reject** if 2+ do, even with identical values | Ownership ambiguity is the problem, not the value. Expected to almost always be the operator. |
| `RetryPriority` + aggregate cluster targets | Unchanged — sole property of the one allowed retry-source policy | Confirmed genuinely singular in the proto (finding 11); not a `RetryConditions` field at all. |

**Rule change from today:** `ValidateAtMostOneRetrySourcePerRoute`'s blanket
"retry-source excludes `resilience.retry` entirely" is replaced by only the
two conflict checks above (`NumRetries`, `BackOff`). An operator's backoff/
timeout preference can now coexist with a retry-source policy's target
switching — they compose on independent axes of the same `RouteAction.RetryPolicy`,
which today's flat-struct-replacement approach had no way to express safely.

**Default convenience, preserved from today:** if a contributor sets
`StatusCodes` without `"retriable-status-codes"` in `On`, it's implied — same
for `Headers` implying `"retriable-headers"`. Keeps the common case
boilerplate-free.

### Component 3 — `policy-definition.yaml` declaration shape

Replaces `x-wso2-retry-trigger`'s single-purpose `statusCodesField` pointer.
Renamed `x-wso2-retry-conditions` to match what it now declares — every
`RetryConditions` field may be a **literal** (fixed at policy-authoring time)
or `{fromParam: <name>}` (resolved from the as-deployed instance's own
params, same fallback-to-schema-default behavior
`decoupled-retry-source-task10-schema-default-not-materialized.md` already
established for the one field that existed before):

```yaml
# oauth2-generator-style, trigger-only (illustrative — not a live consumer, see Status)
x-wso2-retry-conditions:
  statusCodes: {fromParam: tokenPurgeStatusCodes}
  minAttempts: 2
```

```yaml
# model-failover, source-capable
x-wso2-retry-source:
  groupKeyField: model     # kept a plain literal — see Open Questions
  targetsField: targets    # generalized from today's hardcoded params["targets"] read,
                            # for the same reason statusCodesField was already a pointer
x-wso2-retry-conditions:
  statusCodes: {fromParam: statusCodes}
  # minAttempts intentionally absent — still derived from the longest target
  # chain (Component 2), not re-declared here
```

One resolution primitive, used for every field of every contributor — this
is the piece that actually removes the "add a field, write a new parser"
tax:

```go
func resolveConditionField(raw interface{}, params map[string]interface{}, schema *map[string]interface{}) (interface{}, error) {
    ptr, isPointer := raw.(map[string]interface{})
    if !isPointer {
        return raw, nil // literal — identical for every deployment of this policy
    }
    paramName, _ := ptr["fromParam"].(string)
    if val, present := params[paramName]; present {
        return val, nil
    }
    return schemaDefault(schema, paramName), nil
}
```

`ParseRetryTriggerParams`/`ParseRetrySourceParams` (`gateway-controller/pkg/config/retry_source_validator.go`)
are replaced by one `ParseRetryConditions(raw map[string]interface{}, params map[string]interface{}, schema *map[string]interface{}) (*policy.RetryConditions, error)`
that walks the `RetryConditions` struct fields once, calling
`resolveConditionField` per field, with a per-field type coercer (int,
`[]int`, `[]string`, duration-string, bool) keyed off the field's static Go
type — replacing the one-off coercion each field currently gets scattered
across `getIntParam`/`getPurgeStatusCodesParam`-style helpers.

### Component 4 — RetryPolicy assembly

`buildRetryPolicy`, `buildRetrySourceRetryPolicy`, `buildRetryTriggerRetryPolicy`,
`composeRetryTriggerPolicy` (four functions today, duplicated merge logic)
collapse into one: `buildRoutePolicyFromConditions(merged policy.RetryConditions, source *policy.RetrySourceDeclaration) *route.RetryPolicy`,
translating the merged `RetryConditions` plus (if present) the retry-source's
`RetryPriority`/aggregate-cluster wiring into the real Envoy proto. Single
source of truth for the translation from this codebase's vocabulary to
Envoy's.

## Migration / Rollout

1. Add `RetryConditions`/`RetriableHeader`/`RetryBackOff` to
   `sdk/core/policy/v1alpha2`, alongside (not replacing) the existing
   `RetryTriggerDeclaration`/`RetrySourceDeclaration` types initially, to
   allow a staged cutover.
2. Extend `api.Retry` with the new fields; wire `mergeRetryConditions` +
   `buildRoutePolicyFromConditions` in gateway-controller behind the existing
   discovery path.
3. Migrate `model-failover`'s `policy-definition.yaml` and Go code
   (`groupKeyField`/`targetsField` pointers, `x-wso2-retry-conditions` block)
   to the new shape in the canonical `gateway-controllers` repo first, per
   the existing dual-repo convention, then mirror to `dev-policies`.
4. Delete `ParseRetryTriggerParams`/`ParseRetrySourceParams` and the four old
   `build*RetryPolicy` functions once model-failover is the only consumer and
   is confirmed migrated.
5. Update `x-wso2-retry-trigger` references in this policy's own e2e
   docs/Postman collection (same treatment this session already gave
   oauth2-generator's e2e suite when its mechanism changed).

## Testing Strategy

- Unit tests per `mergeRetryConditions` rule in the table above — one test
  per row, including the reject-on-conflict paths for `NumRetries`/`BackOff`.
- Unit tests for `resolveConditionField`: literal passthrough, `fromParam`
  present, `fromParam` absent falling back to schema default (locks in the
  Task 10 gotcha for every field, not just status codes).
- `ParseRetryConditions` table-driven tests mirroring today's
  `TestGetPurgeStatusCodesParam`-style coverage, generalized across field
  types.
- Live e2e: rerun model-failover's existing E.35-style suite once migrated,
  plus a new scenario exercising an operator's `resilience.retry` (backoff +
  timeout) composing with a retry-source policy on the same route — the
  exact case today's blanket exclusion rule forbids and this redesign
  enables.

## Open Questions

- **`groupKeyField` literal vs. `{fromParam}` pointer.** Sketched as a plain
  literal in Component 3 (matches today's behavior). Generalizing it to a
  pointer was considered during design and set aside as likely over-reach —
  no concrete use case surfaced for making it operator-tunable per
  deployment; revisit if one does.
- **`RetriableHeader` matcher richness.** Scoped to exact-match only for now.
  Extend to regex/presence-only matching if a real consumer needs it —
  deliberately not speculative-built ahead of a need.

# Research: A Per-Attempt Retry Hop Any Policy Can Use

## Context found in the code

| Topic | Finding | Where |
|---|---|---|
| Split trigger | The front/dispatch split is built only when a policy named `model-failover` is found | `GC/pkg/utils/llm_failover.go` (`findFailoverPolicy`) |
| Retry sizing | The controller parses model-failover's `chains`, `perAttemptTimeout`, `failoverOn.timeout` | `GC/pkg/failover`, `GC/pkg/transform/llm_failover.go` |
| Placement | Which policies move to the dispatch route is hard-coded (transformers, upstream credentials, loopback marker) | `buildFailover`, `buildProviderFailover`, `transformProvider` |
| Per-target wiring | One loopback upstream plus a transformer/credential copy per chain target | `buildFailover` |
| Envoy pieces | The internal listener `failover_dispatch`, the hop Lua filter and the local-reply mapper are generic in behaviour, with failover names | `GC/pkg/xds/failover_listener.go` |
| Policy definitions | `models.PolicyDefinition` has no extension fields. Definitions are loaded by `PolicyLoader.LoadPoliciesFromDirectory` from the runtime's compiled output | `GC/pkg/models/policy_definition.go`, `GC/pkg/utils/policy_loader.go` |
| Cross-attempt state | model-failover keeps its own process-global plan registry keyed by a nonce header, because each attempt is a separate ext_proc stream with its own `SharedContext` | `POL/plan.go`; `sdk/core/policy/v1alpha2/context.go` |
| Response actions | `DownstreamResponseHeaderModifications` has headers and metadata only, with no retry notion | `sdk/core/policy/v1alpha2/action.go` |
| Provider selection | `selected_provider` metadata plus conditional transformer/credential policies already route a proxy to any attached provider (used by model-round-robin) | `GC/pkg/utils/llm_transformer.go` (`selectedProviderExecutionCondition`) |
| System policies | The controller already injects gateway-owned policies into chains | `GC/pkg/utils/system_policies.go` |
| Live-run lessons (003) | Route `response_headers_to_remove` runs before ext_proc and hid the retry tag. A later transformer may not honour a param the dispatch set, so a policy must write what it changes into the request | 003 tasks.md |

## Decisions

### D1: Declarations live in the policy definition, in plain words

- **Decision**: Add one optional block, `retryBehavior`, to policy definitions, read into `models.PolicyDefinition`:
  - `runs: onClientRequest | onEveryAttempt | onBoth`: when the policy runs if the gateway may send a request more than once (default `onClientRequest`);
  - `canRetry`: present only if the policy can ask for a resend;
  - `sameOperation`: rules for other policies on the same operation (D10).

  Full shape and comments in data-model.md. Every key reads as a statement about the policy: `enabledByParam`, `maxAttempts`, `seesConnectionFailures`.
- **Rationale**: The controller already loads every definition, so declarations reach it with no new channel, and policy authors change only their own files. Plain keys let a reviewer read a definition without knowing the gateway's internals. An earlier draft used `execution: attempt` and an `attempts` block with CEL strings; it was rejected as not self-explanatory.
- **Alternatives rejected**:
  - a registry of retrying policy names in the controller (the current situation, generalized, but still a gateway change per policy);
  - asking the policy engine at deploy time (the controller would depend on the runtime being up).

### D2: Budgets come from named sources, not expressions

- **Decision**: A policy's maximum attempts is exactly one of:
  - `maxAttempts: <number>`;
  - `maxAttemptsFromParam: <numeric param>`;
  - `maxAttemptsFromLongestList: {path, plus}` (the longest list found at a param path, plus a number).

  The per-attempt timeout is `perAttemptTimeout: <duration>` or `perAttemptTimeoutFromParam: <duration param>`. Turning retries on is `enabledByParam: <boolean param>`.
- **Rationale**: These cover every current need:
  - OAuth2: a fixed 2, switched by a param;
  - model-failover: the longest `chains[].fallbacks` plus its primary, and a timeout param;
  - future key-pool or backoff policies: a param or a list length.

  Each is readable without learning an expression language, and the controller resolves them with plain code, so no CEL dependency is needed.
- **Alternatives rejected**:
  - CEL expressions over params: powerful but opaque in a definition file, and a new controller dependency;
  - a fixed ceiling per policy: the route timeout grows to ceiling × per-try.
- **Growth**: A new source kind (for example "sum of list lengths") is a one-time gateway addition that every policy can then use.

### D3: The gateway sizes the retries only from declarations, as an upper bound

- **Decision**:
  - `num_retries` = Σ(max attempts − 1) over enabled retrying policies on the operation, capped by `router.attempts.max_retries` (default 10); registration fails above the cap.
  - `per_try_timeout` = the smallest declared per-attempt timeout, or the gateway default.
  - The route timeout = (num_retries + 1) × per_try_timeout + margin.
  - Policies stop early by not tagging.
- **Rationale**: This is the same upper-bound-plus-early-stop model 001–003 already proved live.

### D4: Placement by declaration

- **Decision**: On a split operation:
  - `runs: onClientRequest` policies go to the front route;
  - `onEveryAttempt` policies go to the per-attempt route;
  - `onBoth` policies go to both, with `_runningAs` injected.

  Upstream credential policies (`set-headers` for api-key, `oauth2-generator`) and transformers declare `runs: onEveryAttempt`. API-level policies stay front, as today. System policies keep their current placement.
- **Rationale**: It removes the hard-coded lists in `buildFailover`/`buildProviderFailover`/`transformProvider`.

### D5: A generic attempt coordinator replaces the failover front duties

- **Decision**: A gateway-owned system policy, `attempt-coordinator`, is injected on the front route of every split operation. It:
  - strips client-supplied `x-wso2-attempt-*` headers;
  - mints the attempt scope id (the current plan nonce, generalized);
  - on the final response, strips all attempt tags;
  - maps a response tagged `stop` to the final answer the policy stored.

  Route-level header removal is used only for headers no policy reads (the 003 lesson).
- **Rationale**: A policy that only runs per attempt (OAuth2) needs no front role. model-failover keeps its front role only for chain selection.

### D6: Cross-attempt state moves into the policy engine

- **Decision**:
  - The policy engine keeps an attempt-scope store keyed by the scope id header: TTL-bounded, per process, swept, with the same bounds as model-failover's plan registry.
  - The SDK exposes it as `reqCtx.Attempt()`: attempt number, which policy requested it and why, and a per-policy `State` map.
  - model-failover's plan registry becomes a user of this store.
- **Rationale**: Every retrying policy needs "remember across attempts" (which token was used, the plan cursor). Building it once removes each policy's private registry and nonce handling.
- **Alternative rejected**: Each policy keeping a process-global map keyed by a header it mints. That's today's model-failover; it duplicates hop-secret and nonce security per policy.

### D7: Retry signals through the SDK, not headers

- **Decision**: Add response actions:
  - `RetryAttempt{Reason}` (request another attempt);
  - `StopAttempts{Response}` (no more attempts; optional final answer);
  - plain modifications meaning "pass".

  The kernel maps them onto `x-wso2-attempt-retry` / `x-wso2-attempt-stop`. When two policies answer, `StopAttempts` beats `RetryAttempt`, which beats pass. The reason and the requesting policy go into the scope store for the next attempt.
- **Rationale**: Policies never write internal header names. The kernel enforces declared allowances: a `RetryAttempt` beyond a policy's `maxAttempts` is ignored and logged.

### D8: Transport failures reach policies uniformly

- **Decision**:
  - The hop filter plus metadata-matched local-reply mapper from 001–003 becomes the general mechanism, renamed `wso2.attempt.hop`.
  - The kernel exposes the parsed failure as `respCtx.Attempt().TransportFailure` (`connect_failure | reset | timeout`), only to policies that declared `seesConnectionFailures: true`.
  - Per-attempt timeouts are inferred by the store when the next attempt arrives, as model-failover does now.

### D9: Targets reuse `selected_provider` routing

- **Decision**:
  - The per-target loopback upstreams and per-target transformer/credential copies are removed.
  - On a split proxy operation, each attached provider gets one per-attempt upstream (its provider route), with its transformer and credential gated on `selected_provider`, which is how multi-provider proxies already work.
  - A per-attempt policy selects a provider with the existing `UpstreamName` action and writes the model into the body (the 003 lesson).
- **Rationale**: This removes all failover-specific wiring. A "target" is just an attached provider plus a model.
- **Risk**: A transformer's `model` param would no longer be set per target. This is covered because dispatch writes the model into the body (fixed in 003), and it's verified by the 003 cross-provider scenarios.

### D10: Generic validation with named rules

- **Decision**: Two generic checks at registration.
  - **Provider references.** A param schema annotation `x-wso2-refers-to: attached-provider` means "this string must name a provider attached to this API or proxy".
  - **Other policies on the same operation.** `retryBehavior.sameOperation` rules take one of two forms:
    - `{policy, allowed: never}`;
    - `{policy, allowed: onlyIf, onlyIf: {runsBeforeThisPolicy: true, paramNotSet: <path>}}`.

    Round-robin's rule reads directly as "only if it runs before this policy and none of its entries name a provider".
- **Rationale**: This removes `llm_validator_failover.go`'s policy-specific rules, using a small closed set of conditions instead of expressions. A new condition kind is a one-time gateway addition.

### D11: Rename, don't alias

- **Decision**:
  - `x-wso2-failover-*` → `x-wso2-attempt-*`;
  - `failover_dispatch` → `attempt_dispatch`;
  - `router.failover.*` → `router.attempts.*`.

  No aliases.
- **Rationale**: These are internal names, and 001–003 are unreleased.

### D12: Delivery order

- **Decision**: Four increments:
  1. the generic contract with model-failover migrated;
  2. `selected_provider` targets;
  3. generic validation;
  4. OAuth2 retry-on-unauthorized as a policy-only change.

  Increment 4's gateway diff must be empty (SC-001).

## Revision during implementation (2026-09-27)

### D6/D7 revised: the store and the signals live in the SDK, not the kernel

- **Finding**: The policy engine builds against the published `sdk/core` (v0.4.1). Every policy is compiled into the same engine binary, so an SDK package is linked once and shared by all of them.
- **Decision**: New package `sdk/core/attempts` holds:
  - the process-wide attempt-scope store: TTL, sweeper, a bound of 100,000 scopes, per-policy state, allowance accounting, timeout inference;
  - the policy-facing helpers: `Current`, `Attempt.Get`/`Set`, `Retry(shared, policy, reason, giveUp)`, `TransportFailure`.

  One gateway system policy, `wso2_apip_sys_attempts`, runs first on both routes:
  - as `front`: opens the scope, strips client attempt headers, and on the final response strips tags and applies a policy's give-up answer;
  - as `attempt`: admits the attempt, rejects unknown or foreign scopes untagged, and sends the hop secret.

  The kernel needs no change; T007 is covered by this.
- **Consequence**: Until `sdk/core` is tagged with the package, the policy engine, the system policy and the sample policy build against the local `sdk/core` through `replace` directives. **Before merge**, tag `sdk/core` and remove the replaces (new task T034).
- **Retry signal**: `Retry` returns ordinary response modifications carrying `x-wso2-attempt-retry`, or the policy's give-up response when its allowance is used up. `StopAttempts` is just returning an `ImmediateResponse`, which carries no tag, so it is never retried.

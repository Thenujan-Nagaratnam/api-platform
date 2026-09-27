# Implementation Plan: A Per-Attempt Retry Hop Any Policy Can Use

**Branch**: `004-per-attempt-retry-hop` in both repos (api-platform based on `001-model-failover-policy`; gateway-controllers based on `model-failover-envoy-retry`) | **Date**: 2026-09-27 | **Spec**: [spec.md](./spec.md)

## Summary

Turn model failover's per-attempt hop into a general gateway capability, paid for once:
- **Policy definitions** gain a `retryBehavior` block in plain words (`runs`, `canRetry`, `sameOperation`); budgets come from a fixed number, a param, or the longest list at a param path.
- **The controller** splits, places and sizes an operation only from those declarations.
- **The policy engine** gains an attempt-scope store.
- **The SDK** gains `RetryAttempt` / `StopAttempts` and `Attempt()` context.
- **A gateway-owned `attempt-coordinator` system policy** takes over the front-route duties.
- **Targets** reuse `selected_provider` routing instead of per-target plumbing.

Model failover migrates first, keeping every current test green. OAuth2 retry-on-unauthorized then ships as a policy-only change: that change set must contain no gateway files.

## Technical Context

**Language/Version**: Go 1.26.x; Envoy v1.39.0

**Primary Dependencies**: existing only; no new modules

**Storage**: in-memory attempt-scope store in the policy engine (TTL, swept); no DB changes

**Testing**: policy-engine and SDK unit tests; controller unit tests (placement, sizing from each `maxAttempts*` source, validation rules); Envoy `--mode validate`; the existing model-failover godog and Postman suites as the regression gate; new godog scenarios for a test retry policy and for OAuth2 refresh

**Target Platform**: gateway-controller, gateway-runtime (router + policy engine), policy SDK, gateway-controllers policies

**Project Type**: distributed gateway (controller translation + data-plane engine + SDK)

**Performance Goals**: unsplit operations unchanged (SC-004). Split operations add one in-process listener pass per attempt, as today. The scope store costs one map lookup per attempt.

**Constraints**:
- SC-001: the OAuth2 increment has an empty gateway diff.
- FR-008: no internal header leaks.
- Retries stay an upper bound, with the policies stopping early.

**Scale/Scope**: `router.attempts.max_retries` default 10 per request; scope store bounded by in-flight split requests

No open questions: decisions D1–D12 are in [research.md](./research.md).

## Constitution Check

`.specify/memory/constitution.md` is still the template; the gate is `.claude/rules/`, as in 001–003.

| Rule | Status |
|---|---|
| authentication_authorization | ✅ Attempt signals come only from the policy engine. Client-sent `x-wso2-attempt-*` headers are stripped (GO-AUTH-001 fail-closed: an unknown scope id is rejected, never retried). The OAuth2 refresh never logs tokens (GO-AUTH-003: fingerprints only). |
| go-network-service-hardening | ✅ A global retry cap, per-attempt timeouts, a bounded replay buffer, and a TTL-bounded scope store (directive 3: bounded state, rejects past the bound). The OAuth2 refresh uses single-flight to avoid stampeding the IdP. |
| error-handling | ✅ Fixed error bodies (`upstream_auth_failed`, exhaustion) that never echo backend or IdP details. |
| go-control-plane-xds-security | ✅ No new listener type. The internal listener keeps path normalization and has no port. |
| dependency-management | ✅ N/A: no new modules (named sources replaced CEL, D2). |
| ssrf-prevention | ✅ No new outbound targets; per-attempt upstreams are the API's or proxy's declared ones. The OAuth2 token endpoint keeps its existing validation. |
| TODO/FIXME deferral | ✅ None. |

Post-design re-check: passes.

## Project Structure

### Documentation

```text
specs/004-per-attempt-retry-hop/
├── spec.md, plan.md, research.md, data-model.md, quickstart.md
├── contracts/policy-author-contract.md
├── checklists/requirements.md
└── tasks.md            # /speckit-tasks (not created yet)
```

### Source Code (one-time gateway changes)

```text
sdk/core/policy/v1alpha2/
├── action.go                  # RetryAttempt, StopAttempts
└── context.go                 # AttemptContext, Attempt() on request/response contexts
gateway/gateway-runtime/policy-engine/
├── internal/attempts/         # attempt-scope store (TTL, sweep, per-policy state, allowance accounting)
└── internal/kernel/…          # map SDK actions ⇄ x-wso2-attempt-* tags; expose Attempt()
gateway/gateway-controller/
├── pkg/models/policy_definition.go     # RetryBehavior: Runs, CanRetry, SameOperation
├── pkg/attempts/              # replaces pkg/failover: split decision, sizing from named sources, placement, internal params
├── pkg/transform/…            # generic RouteAttempts on the RDC (replaces RouteFailover)
├── pkg/xds/attempt_listener.go # renamed failover_listener.go; names generalized
├── pkg/config/…               # x-wso2-refers-to / sameOperation generic validation (replaces llm_validator_failover.go)
├── pkg/utils/llm_*.go         # delete buildFailover/buildProviderFailover-specific wiring; selected_provider targets
└── pkg/utils/system_policies.go # attempt-coordinator injection
gateway/gateway-runtime/router/config/  # attempt_dispatch internal listener name
```

### Policy changes (the only kind needed after this feature)

```text
gateway-controllers/policies/model-failover/     # runs: onBoth; canRetry.maxAttemptsFromLongestList; uses SDK signals + scope store
gateway-controllers/policies/oauth2-generator/   # runs: onEveryAttempt; canRetry: {enabledByParam: retryOnUnauthorized, maxAttempts: 2}
gateway-controllers/policies/<transformers>, set-headers   # runs: onEveryAttempt
```

**Structure Decision**: `pkg/failover` becomes `pkg/attempts`, a policy-agnostic package. model-failover stops being special in gateway code and becomes the first user of `pkg/attempts`.

## Complexity Tracking

| Added complexity | Why needed | Simpler alternative rejected because |
|---|---|---|
| Named budget sources (`maxAttempts`, `…FromParam`, `…FromLongestList`) | Budgets such as "longest chain" must come from the policy's own params without the gateway understanding them | CEL expressions are opaque in definition files and add a controller dependency; a fixed ceiling inflates route timeouts |
| Attempt-scope store in the policy engine | Every retrying policy needs state across attempts, and each attempt is a separate stream | Per-policy private registries duplicate nonce, forgery and TTL logic in every policy |
| `attempt-coordinator` system policy | Policies that only run per attempt still need the front duties (strip tags, scope id, stop mapping) | Giving every retrying policy a front role pushes gateway duties into policies |

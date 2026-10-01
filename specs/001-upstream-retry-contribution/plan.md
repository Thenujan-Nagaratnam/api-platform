# Implementation Plan: Upstream Retry Contribution

**Branch**: `001-upstream-retry-contribution` | **Date**: 2026-10-01 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/001-upstream-retry-contribution/spec.md`

## Summary

A policy currently has no way to make the gateway retry a request against the same backend when that policy itself can recognize and fix a transient, backend-reported failure (canonical case: oauth2-generator seeing a 401, purging its cached token, and wanting a retry with a fresh one). Today, `gateway-controller`'s xDS translator only ever emits an Envoy `RetryPolicy` for a route when a multi-target `model-failover` chain is attached (`rdcRoute.Upstream.Failover != nil`) — a route with any other policy attached, alone, gets no retry policy at all, regardless of what that policy could otherwise recover from.

This plan adds a new, optional, policy-definition-level field — `retry` — that a policy declares once, in its own `policy-definition.yaml`, stating which response/connection conditions warrant a retry and how many times (`maxAttempts`, total sends including the first — translated to Envoy's off-by-one `NumRetries` exactly once, at merge time). Each condition may be a fixed literal (oauth2-generator's `401`, a true domain constant) or, where a policy's own retry-worthy condition genuinely varies by how an operator configured that instance, a reference to one of that same policy's already-existing params (`statusCodesFromParam: rateLimitStatusCode`) — resolved per attachment, never introducing a new retry-specific config surface either way. An optional `enabledByParam` lets a policy-chosen existing boolean param gate the whole contribution on or off per attachment, and `maxAttemptsFromLongestList` lets a chain-shaped policy (the deferred future case: `model-failover` itself) derive its attempt count from the length of one of its own list params instead of a plain number. A new, policy-name-agnostic pass in `gateway-controller` collects every attached policy instance's contribution for a route and merges them (union of conditions, max of retry counts) into the route's `RetryPolicy`, coexisting with — never replacing — the existing `model-failover`-driven path. No changes are needed to the SDK, the policy-engine kernel, or any policy's own Go code: the "re-run fresh on every attempt" mechanism this depends on (the `upstream: true` / `upstreamPolicies:` attachment point) already exists and already works correctly for this purpose.

## Technical Context

**Language/Version**: Go 1.26 (matches `gateway/gateway-controller`'s existing `go.mod`)

**Primary Dependencies**: `github.com/envoyproxy/go-control-plane` (already used to construct `route.RetryPolicy` in `pkg/xds/translator.go`); the existing `models.PolicyDefinition` / `utils.PolicyLoader` policy-definition loading pipeline (`pkg/models/policy_definition.go`, `pkg/utils/policy_loader.go`)

**Storage**: N/A — deploy-time only, no persisted state; mirrors `model-failover`'s own `RouteFailover` compilation, which is also stateless at the controller layer

**Testing**: Go `testing` + `stretchr/testify`, matching every existing `pkg/xds`/`pkg/transform` test in this module

**Target Platform**: `gateway-controller` (control plane), compiled into the same binary/container that already performs all other xDS translation

**Project Type**: Backend library addition within an existing Go module — no new service, no new deployable artifact

**Performance Goals**: N/A for the request path (this never executes per-request); deploy-time compilation cost must stay proportional to the number of attached policies per route, consistent with every other per-route compilation step already in `translator.go`

**Constraints**: MUST NOT change behavior for any route that has zero policies declaring `upstreamRetryOn` (spec FR-004/SC-002); MUST NOT special-case any policy by name in the merge pass (the same architectural principle `models.RouteFailover`'s own translator gate already follows at the cluster-building layer, just not yet at the retry-policy layer); MUST compose with, not replace, the existing `model-failover`-driven `RetryPolicy` construction on a route that has both

**Scale/Scope**: One new optional field on `models.PolicyDefinition`; one new generic function in `pkg/xds` (or a new small file) that merges contributions into a `route.RetryPolicy`; one call-site change in `translator.go` where `RetryPolicy` is currently built; one `policy-definition.yaml` change for `oauth2-generator` to adopt it as the first real consumer

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

`.specify/memory/constitution.md` has not yet been filled in for this project (template placeholders only) — there are no ratified principles to check this plan against. No gate violations are possible against an empty constitution; this check trivially passes. If a constitution is adopted later, this plan's one architectural commitment worth checking against it in retrospect is: **no policy-name special-casing in generic controller infrastructure** (see Constraints above) — the same principle `RouteFailover`'s translator gate already follows.

**Post-Phase-1 re-check**: Confirmed holding after design — the merge pass in data-model.md reads only `PolicyDefinition.UpstreamRetryOn` by value, never by policy name; `oauth2-generator` is the first *consumer* of the contract, not a special case baked into the merge logic itself. No new violation introduced by Phase 1 design.

## Project Structure

### Documentation (this feature)

```text
specs/001-upstream-retry-contribution/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── contracts/           # Phase 1 output (policy-definition.yaml schema contract)
├── quickstart.md        # Phase 1 output
└── tasks.md             # Phase 2 output (/speckit-tasks — not created here)
```

### Source Code (repository root)

This feature lives entirely inside the existing `gateway/gateway-controller` Go module — no new project, no new top-level directory.

```text
gateway/gateway-controller/
├── pkg/
│   ├── models/
│   │   ├── policy_definition.go       # MODIFY: add UpstreamRetryOn field to PolicyDefinition
│   │   └── runtime_deploy_config.go   # REFERENCE ONLY: existing RouteFailover shape this parallels
│   ├── xds/
│   │   ├── translator.go              # MODIFY: the one call-site that currently builds RetryPolicy
│   │   │                              #   only inside `if rdcRoute.Upstream.Failover != nil`
│   │   └── upstream_retry.go          # NEW: generic per-route contribution collection + merge
│   └── transform/
│       └── model_failover_policy.go   # REFERENCE ONLY: existing analogous "detect my policy,
│                                       #   populate a generic route-level field" pattern
│
gateway/dev-policies/ and github.com/wso2/gateway-controllers/policies/oauth2-generator/
└── policy-definition.yaml             # MODIFY (oauth2-generator's): add upstreamRetryOn: {statusCodes: [401], numRetries: 1}
```

**Structure Decision**: Single-module addition inside `gateway-controller`, following the existing `pkg/models` (schema) / `pkg/xds` (xDS generation) / `pkg/transform` (policy-attachment detection) layering already established by every comparable feature in this codebase (most directly, `model-failover`'s own `RouteFailover` compilation). No new package boundaries are introduced beyond one new file (`pkg/xds/upstream_retry.go`) sized the same way `pkg/xds/failover_cluster.go` is scoped to its own concern.

## Complexity Tracking

*No constitution violations to justify — table intentionally omitted (no constitution is ratified yet; see Constitution Check above).*

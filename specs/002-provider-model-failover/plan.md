# Implementation Plan: Model Failover on LLM Providers

**Branch**: `002-provider-model-failover` (work lands on `001-model-failover-policy`) | **Date**: 2026-09-27 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `specs/002-provider-model-failover/spec.md`

## Summary

Let the model-failover policy run on an **LlmProvider**, falling back across that provider's own models. The provider transformer emits the same front and dispatch operations as the proxy transformer. The dispatch route keeps the provider's normal routing to its own upstream, and the dispatch role rewrites the model per attempt without selecting an upstream (`_routeToTarget: false`). Targets name models only; the controller fills in the provider's name.

Two safety changes come with it:
- The internal listener gets the hop-secret filter, so a provider-mode dispatch route never forwards the secret to the real provider. Proxy-mode dispatch routes turn that filter off per route.
- Registration rejects every configuration that can't work, instead of today's route that returns `500` on every request.

## Technical Context

**Language/Version**: Go 1.26.x; Envoy v1.39.0 (spikes on local 1.36.4)

**Primary Dependencies**: as feature 001; no new modules

**Storage**: N/A (in-memory policy state, as 001)

**Testing**: Go unit tests (policy, controller), Envoy `--mode validate` test, godog scenarios, Postman/newman folder

**Target Platform**: gateway-controller + gateway-runtime containers

**Project Type**: Distributed gateway (controller translator + data-plane policy)

**Performance Goals**: as 001; provider mode has one fewer hop than proxy mode (no loopback)

**Constraints**: FR-009 (no internal header reaches the provider); FR-012 (001 behaviour unchanged)

**Scale/Scope**: ≤ 10 models per chain; any template model location (body, header, query, path)

No `NEEDS CLARIFICATION` items: decisions P1–P7 are in [research.md](./research.md), with spike S6 as verification.

## Constitution Check

`.specify/memory/constitution.md` is still the unfilled template. The gate is the repo's `.claude/rules/`, as in 001:

| Rule | Status |
|---|---|
| authentication_authorization | ✅ No new external surface. Registration rejects cross-provider targets. The secret never leaves the gateway (P5). |
| go-network-service-hardening | ✅ The same bounded retries, timeouts and buffers as 001. |
| error-handling | ✅ The same fixed exhaustion body. Registration errors name the field. |
| go-control-plane-xds-security | ✅ The internal listener keeps path normalization. The new filter is inline, with no admin or port exposure. |
| db-schema-changes / dependency-management | ✅ N/A |
| TODO/FIXME deferral | ✅ None. |

Post-design re-check: passes.

## Project Structure

### Documentation

```text
specs/002-provider-model-failover/
├── spec.md, plan.md, research.md, data-model.md, quickstart.md
├── contracts/provider-attachment.md
├── checklists/requirements.md
└── tasks.md            # /speckit-tasks
```

### Source Code (changes on top of 001)

```text
api-platform/gateway/gateway-controller/
├── pkg/failover/failover.go            # ParseSettingsFor, ValidateParamsFor, BodyModelTemplate, ParamRouteToTarget
├── pkg/utils/llm_failover.go           # buildProviderFailover (front + dispatch ops for a provider)
├── pkg/utils/llm_transformer.go        # transformProvider hook; upstream auth moves to dispatch ops
├── pkg/transform/llm.go                # run applyFailoverRoutes for providers too; SameUpstream on dispatch routes
├── pkg/transform/llm_failover.go       # set RouteFailover.SameUpstream from _routeToTarget
├── pkg/models/runtime_deploy_config.go # RouteFailover.SameUpstream
├── pkg/xds/failover_listener.go        # hop filter + metadata mapper on the internal listener; LuaPerRoute disable on proxy-mode dispatch routes
├── pkg/config/llm_validator.go / llm_validator_failover.go  # provider-mode validation
└── tests for each of the above
gateway-controllers/policies/model-failover/
├── config.go                           # parse _routeToTarget
├── dispatch.go                         # no UpstreamName / selected_provider when _routeToTarget is false
└── policy-definition.yaml              # targets[].provider optional
api-platform/gateway/it/                # provider scenarios in model-failover.feature, steps, Postman folder
```

**Structure Decision**: Extend the 001 code in place, on the same branches. Provider mode is a second entry point into the same mechanism, not a separate module.

## Complexity Tracking

| Added complexity | Why needed | Simpler alternative rejected because |
|---|---|---|
| A per-route Lua switch on dispatch routes | The hop secret must be stripped where the hop ends (provider mode) and kept where it continues (proxy mode). | Stripping on the internal listener for every route breaks proxy mode's transport-failure detection. Never stripping leaks the secret to providers. |

---
description: "Task list for model failover on LLM providers (builds on 001)"
---

# Tasks: Model Failover on LLM Providers

**Input**: `specs/002-provider-model-failover/` (plan.md, spec.md, research.md, data-model.md, contracts/, quickstart.md)

**Tests**: Included, as in 001: Go unit tests, an Envoy validation test, godog scenarios, a Postman folder.

**Paths**:
- `GC/` = `api-platform-worktrees/model-failover/gateway/gateway-controller/`
- `POL/` = `gateway-controllers-worktrees/model-failover-envoy-retry/policies/model-failover/`
- `IT/` = `api-platform-worktrees/model-failover/gateway/it/`

Work lands on the 001 branches; `model-failover-impl` is refreshed at the end.

## Phase 1: Setup

- [X] T001 Spike S6 (research.md P5) in `specs/002-provider-model-failover/spikes/s6-lua-per-route.yaml`, on the local Envoy:
  - an internal listener with the hop Lua filter and a metadata-matching local-reply mapper;
  - route A disables the filter with `LuaPerRoute{disabled:true}` and forwards the header to a backend;
  - route B keeps the filter, and its upstream is a closed port.

  Confirm that route A's backend receives `x-wso2-failover-hop`, and that route B's local reply carries `x-wso2-upstream-failure` while the header never reaches a live backend. Record the result in research.md.

## Phase 2: Foundational (blocks all stories)

- [X] T002 [P] Unit-test the `pkg/failover` provider helpers in `GC/pkg/failover/failover_test.go`:
  - `ParseSettingsFor` fills an omitted provider with its own name;
  - it accepts the provider's own name;
  - it rejects another provider with the message in `contracts/provider-attachment.md`;
  - `ValidateParamsFor` applies every proxy-mode rule;
  - `BodyModelTemplate` is true only for `{payload, $.model}`, as a typed enum and as a plain string.

  The helpers are already written: `ParseSettingsFor`, `ValidateParamsFor`, `BodyModelTemplate`, `ParamRouteToTarget`.
- [X] T003 Add `SameUpstream bool` to `RouteFailover` in `GC/pkg/models/runtime_deploy_config.go`, with a doc comment per data-model.md.
- [X] T004 [P] Policy: parse the internal param `_routeToTarget` (bool, default `true` when absent) in `POL/config.go`. In `POL/dispatch.go`, when it is `false`, set neither `UpstreamName` nor `selected_provider`; the hop header, model rewrite and response classification are unchanged. Make `targets[].provider` optional in `POL/policy-definition.yaml`, with a description saying it's required on an LlmProxy. Tests in `POL/roles_test.go`.

**Checkpoint**: shared pieces in place; 001 tests still green.

## Phase 3: User Story 1 — Fall back to another model on the same provider (P1) 🎯 MVP

**Independent test**: quickstart.md scenarios 1–5.

- [X] T005 [P] [US1] Controller test in `GC/pkg/utils/llm_failover_test.go` (provider fixture with the openai template):
  - the front op has model-failover [front], and no upstream-auth policy;
  - the dispatch op is header-matched on the chain token and carries model-failover [dispatch] (`_routeToTarget: false`, `_targetNative` all true, filled-in provider names, hop secret) followed by the provider's upstream-auth policy;
  - the deny routes and other operations keep today's policies, upstream auth included;
  - a global attachment expands per forwarding operation.
- [X] T006 [US1] Implement `buildProviderFailover` in `GC/pkg/utils/llm_failover.go`, and hook it into `transformProvider` in `GC/pkg/utils/llm_transformer.go` before the upstream-auth loop:
  - take a global model-failover out of `GlobalPolicies`;
  - for each forwarding op with the policy, check `failover.BodyModelTemplate(params["requestModel"])` and `ParseSettingsFor(params, provider name)`;
  - rewrite `targets` with the provider filled in;
  - build the front and dispatch instances and the dispatch op;
  - skip the upstream-auth policy on front ops and put it on the dispatch op instead.
- [X] T007 [US1] Run `applyFailoverRoutes` for `LLMProviderConfiguration` too in `GC/pkg/transform/llm.go`. In `GC/pkg/transform/llm_failover.go`, set `RouteFailover.SameUpstream = true` on dispatch routes whose instance has `_routeToTarget: false`. Test in `GC/pkg/transform/llm_failover_test.go`: the provider dispatch route is `SameUpstream`, and its chain keeps the upstream-auth policy.
- [X] T008 [P] [US1] Add provider-mode scenarios to `IT/features/model-failover.feature`, plus a provider fixture step in `IT/steps_model_failover.go`, covering quickstart scenarios 1–5 with `seq:` mock modes.

**Checkpoint**: provider model fallback works end to end, on unit level.

## Phase 4: User Story 2 — Clear errors for configurations that can't work (P1)

**Independent test**: quickstart scenarios 8–9, plus the proxy rules on a provider.

- [X] T009 [P] [US2] Validator tests in `GC/pkg/config/llm_validator_failover_test.go` for a provider:
  - another provider named as a target is rejected, with the field path;
  - the provider's own name is accepted;
  - proxy-mode rule violations are rejected;
  - global attachment at most once;
  - rejected together with `model-round-robin`.
- [X] T010 [US2] Implement `validateProviderModelFailover` in `GC/pkg/config/llm_validator_failover.go`, sharing the collection and selector logic with the proxy variant, and call it from `validateProviderSpec` in `GC/pkg/config/llm_validator.go`.
- [X] T011 [P] [US2] Controller test: a template's unusable model location or a fixed-model path makes `Transform` fail (revised: Gemini/Bedrock on a wildcard path are supported, see research P4) (`GC/pkg/utils/llm_failover_test.go`). Add IT scenarios registering a Gemini provider with the policy (expect 400) and a target naming another provider (expect 400).

## Phase 5: User Story 3 — Same health and safety behaviour (P2)

- [X] T012 [P] [US3] Policy test in `POL/lifecycle_test.go`: with `_routeToTarget: false`, the chain suspends and recovers per model exactly as in proxy mode.
- [X] T013 [P] [US3] IT scenario in `IT/features/model-failover.feature`: provider-mode suspension of the first model and probe recovery (quickstart scenario 6).

## Phase 6: User Story 4 — Nothing internal reaches the provider (P2)

- [X] T014 [US4] In `GC/pkg/xds/failover_listener.go`:
  - add the `wso2.failover.hop` filter to the internal listener's HCM (after Lua, before the router);
  - switch its mapper to `failoverLocalReplyConfig(true)`;
  - in `applyRouteFailover`, give proxy-mode dispatch routes (`!SameUpstream`) `TypedPerFilterConfig{wso2.failover.hop: LuaPerRoute{disabled:true}}`.

  Apply the S6 outcome.
- [X] T015 [P] [US4] xDS tests in `GC/pkg/xds/translator_failover_test.go`:
  - the internal listener has the hop filter, with a metadata mapper;
  - a proxy-mode dispatch route disables it;
  - a provider-mode dispatch route doesn't;
  - extend `translator_failover_envoy_test.go` with a provider-mode dispatch route and re-run Envoy validation.
- [X] T016 [P] [US4] IT: in the provider scenarios, assert the mock never receives `x-wso2-failover-hop`, `-plan` or `-chain`, and that a connection reset on the provider fails over (quickstart 4 and 7).

## Phase 7: Polish

- [X] T017 [P] Add a "08b / 11 Provider-mode model failover" folder to `IT/postman/model-failover/generate_collection.py` covering quickstart 1–10 (including nested proxy-over-provider). Regenerate the collection, syntax-check its scripts, and newman smoke-run the setup folder.
- [X] T018 [P] Docs: a provider section in `gateway-controllers/docs/model-failover/v0.1/docs/model-failover.md` (models-only targets, template rule, Azure note, nesting), and a provider-mode section in the design doc artifact.
- [X] T019 Sync `gateway/dev-policies/model-failover` from `POL/`. Run the full controller, policy and SDK suites, Envoy validation and IT vet. Record the results.
- [ ] T020 Refresh the `model-failover-impl` branches with the non-test changes. Commit only when the user asks.

## Dependencies & order

- T001 gates T014.
- Phase 2 (T002–T004) gates every story.
- US1 (T005–T008) gates US3's IT (T013) and US4's IT (T016).
- US2 depends only on Phase 2.
- US4's xDS work (T014–T015) depends on T003 and T007.
- Polish comes last.

**Parallel**: T002, T004; T005 with T008; T009 with T011; T012, T013, T015, T016; T017, T018.

## Implementation strategy

MVP = Phases 1–3 (provider fallback working) together with US2 (no more silent 500s). Then US4, which **must ship with US1**: without T014 a provider-mode attempt forwards the secret to the provider. So US4 isn't optional for release, even though it's P2 for ordering. Then US3 and polish.

## Results (T019, 2026-09-27)

- Policy (`POL/`): `go test -race -cover` passes, 88.9% coverage. The `dev-policies/model-failover` mirror is rsynced (go.mod/go.sum kept) and passes `-race` with `GOWORK=off`.
- Controller: `go test ./...` passes, 33 packages. `TestFailover_GeneratedConfigPassesEnvoyValidation` passes with `ENVOY_BIN=/opt/homebrew/bin/envoy`.
- `sdk/core`, `sdk/ai`, policy-engine: pass.
- IT: `go vet ./...` is clean; steps have 0 unmatched.
- Still to do: a live godog/newman run against a built gateway (the user runs the builds).
- Revision (2026-09-27, research P4): provider mode now honours the template's `requestModel` at any location (body JSONPath, header, query, path capture group), as model-round-robin does. Gemini and Bedrock on a wildcard path are supported; a path that fixes the model is rejected. All the suites above were re-run and pass.

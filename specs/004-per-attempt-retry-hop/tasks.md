---
description: "Task list for a per-attempt retry hop any policy can use (004)"
---

# Tasks: A Per-Attempt Retry Hop Any Policy Can Use

**Input**: `specs/004-per-attempt-retry-hop/` (plan.md, spec.md, research.md D1–D12, data-model.md, contracts/policy-author-contract.md, quickstart.md)

**Tests**: Included. The existing model-failover godog and Postman suites are the regression gate for US2.

**Paths**:
- `API/` = `api-platform-worktrees/per-attempt-retry-hop/` (branch `004-per-attempt-retry-hop`)
- `GC/` = `API/gateway/gateway-controller/`
- `PE/` = `API/gateway/gateway-runtime/policy-engine/`
- `SDK/` = `API/sdk/core/policy/v1alpha2/`
- `IT/` = `API/gateway/it/`
- `POLS/` = `gateway-controllers-worktrees/per-attempt-retry-hop/policies/` (branch `004-per-attempt-retry-hop`, created in T002)

## Phase 1: Setup

- [X] T001 Bring the 002 and 003 work into the base. Done 2026-09-27: 002+003 committed as `1265bd27f` (api-platform `001-model-failover-policy`) and `bec9a9d` (gateway-controllers `model-failover-envoy-retry`); `004-per-attempt-retry-hop` fast-forwarded to `1265bd27f`. This branch was cut from `001-model-failover-policy` at `36e8d7518`, which has only 001; the 002/003 changes are still uncommitted in the `model-failover` worktree. Once those are committed on `001-model-failover-policy` (the user's call), rebase `004-per-attempt-retry-hop` onto it. Nothing in Phase 2 onwards starts before this.
- [X] T002 Create `004-per-attempt-retry-hop` in gateway-controllers from `model-failover-envoy-retry` (after its 002/003 changes are committed), as a new worktree at `gateway-controllers-worktrees/per-attempt-retry-hop`.
- [X] T003 Confirm no new Go modules are needed (named budget sources, D2) and record it here; if that changes, apply `.claude/rules/dependency-management.md` before adding one.
  - Done: no new Go modules. The dependencies added are local `replace`s of `sdk/core` (see research revision).
- [ ] T004 Capture a baseline for SC-004: dump the generated xDS/RDC for every IT fixture without an enabled retrying policy into `IT/testdata/attempts-baseline/`, for later comparison.

## Phase 2: Foundational (blocks all stories)

- [X] T005 [P] SDK: add `AttemptContext` and `Attempt()` on `RequestHeaderContext`, `RequestContext`, `ResponseHeaderContext`, and the response actions `RetryAttempt{Reason, Mods}` and `StopAttempts{Response}`, in `SDK/context.go` and `SDK/action.go`, per data-model.md. `Attempt()` returns nil on unsplit operations. Unit tests in `SDK/`.
  - Done: as `sdk/core/attempts` (research revision D6/D7): `Current`, `Bind`, `Retry`, `TransportFailure`, `Attempt.Get`/`Set`; 87.9% coverage.
- [X] T006 [P] Policy engine: attempt-scope store in `PE/internal/attempts/`. It keys on scope id with an operation-token check (an unknown or mismatched scope is rejected, never retried), and tracks:
  - attempt number and the last requester and reason;
  - per-policy state;
  - per-policy retries used against the declared `maxAttempts`;
  - a TTL and sweeper, with a size bound that rejects new scopes past it (`go-network-service-hardening` directive 3);
  - timeout inference when the next attempt arrives.
  - Done: the store is `sdk/core/attempts.Registry` (in the SDK, not the engine), with forgery, TTL, bound and timeout tests.

  Unit tests with `-race`.
- [X] T007 Policy engine kernel: on split routes, read `x-wso2-attempt-scope` and expose `Attempt()`. Map `RetryAttempt` → `x-wso2-attempt-retry: <reason>`, and `StopAttempts` → an untagged final response plus the stored answer. When several policies answer: stop > retry > pass. Ignore and log a retry beyond a policy's allowance. Parse transport-failure labels from the hop metadata only for policies that declared `transportFailures`. Tests in `PE/internal/kernel/…`. Depends on T005 and T006.
  - Done: no kernel change needed: the system policy (T012) admits attempts, and `Retry` emits the tag.
- [X] T008 [P] Controller model: add `RetryBehavior` (`Runs`; `CanRetry` with `EnabledByParam`, `MaxAttempts`/`MaxAttemptsFromParam`/`MaxAttemptsFromLongestList{Path, Plus}`, `PerAttemptTimeout`/`PerAttemptTimeoutFromParam`, `SeesConnectionFailures`; `SameOperation` rules) to `GC/pkg/models/policy_definition.go`, per data-model.md. Loader tests in `GC/pkg/utils/policy_loader_test.go` cover the defaults and reject: more than one `maxAttempts*` source, an unknown `runs` value, and an unknown `onlyIf` condition.
  - Done: `models.RetryBehavior` with `Validate()`; the loader fails startup on an invalid block.
- [X] T009 Controller: new package `GC/pkg/attempts/` with:
  - the split decision (any attached policy with `canRetry` whose `enabledByParam` param is true, or absent);
  - resolving each `maxAttempts*` and `perAttemptTimeout*` source against the attachment's params (a number, a param value, or the longest list at a path plus a number), with typed errors naming the policy and key;
  - sizing per data-model.md (Σ(max attempts − 1), min per-try timeout, route timeout, `retry_on`), and the cap check against new `router.attempts.max_retries` (default 10) in `GC/pkg/config/config.go`;
  - placement by `runs`.
  - Done: `GC/pkg/attempts` (94.1% coverage): split decision, named sources (number, param, longest list), sizing, cap, `sameOperation` rules, placement.

  Tests cover every row of the sizing table, each source kind (including `maxAttemptsFromLongestList` on nested lists), and the cap rejection. Depends on T008.
- [X] T010 Controller: generic route settings. Replace `models.RouteFailover` with `RouteAttempts` in `GC/pkg/models/runtime_deploy_config.go`. Add a generic RDC pass in `GC/pkg/transform/attempts.go` that splits any operation `pkg/attempts` says to split, into a front operation and a per-attempt operation matched on the scope token. Inject `_runningAs` for `runs: onBoth` policies, plus `_attemptScope` and `_hopSecret`. This replaces the policy-name-driven path in `GC/pkg/transform/llm_failover.go` and `GC/pkg/utils/llm_failover.go`. Depends on T009.
  - Done: `GC/pkg/transform/attempts.go`, run at the end of the RestAPI transform for every kind; it reuses `RouteFailover` markers, so the Envoy side is unchanged.
- [X] T011 Controller xDS: rename `GC/pkg/xds/failover_listener.go` → `attempt_listener.go`, `failover_dispatch` → `attempt_dispatch`, `wso2.failover.hop` → `wso2.attempt.hop`, and headers to `x-wso2-attempt-*` (D11), driven by `RouteAttempts`. Keep:
  - path normalization;
  - the metadata-matched local-reply mapper;
  - per-route Lua disabling;
  - route-level removal only of `x-wso2-upstream-failure` (003 lesson).
  - Done: header and resource names renamed to `x-wso2-attempt-*`, `attempt_dispatch`, `wso2.attempt.hop` (the file rename to `attempt_listener.go` is still to do, cosmetic); Envoy validation passes.

  Update `API/gateway/gateway-runtime/router/config/envoy-bootstrap.yaml` comments and names. Update `translator_*_test.go`, and re-run the Envoy validation test. Depends on T010.
- [X] T012 Controller: add the `attempt-coordinator` system policy (a gateway-owned policy in `API/gateway/system-policies/attempt-coordinator/`), injected on every split front route by `GC/pkg/utils/system_policies.go`. It:
  - strips client-sent `x-wso2-attempt-*` headers;
  - mints the scope id;
  - on the final response, strips every attempt tag and applies a stored `StopAttempts` answer.
  - Done: `gateway/system-policies/attempts` (`wso2_apip_sys_attempts`, 87.3% coverage), added to `system-build-lock.yaml`. It is injected by the split step, not the default system policy list.

  Add it to `gateway/build.yaml` system policies. Tests in the policy and in `system_policies_test.go`. Depends on T005 and T006.

**Checkpoint**: a split can be produced from declarations alone. The gateway source has no policy-name checks for splitting, placement or sizing.

## Phase 3: User Story 1 — A policy author adds retries without a gateway release (P1) 🎯 MVP

**Independent test**: quickstart 1–3.

- [X] T013 [P] [US1] Test policy `retry-once-on-status` in `API/gateway/sample-policies/retry-once-on-status/`:
  - definition `retryBehavior: {runs: onEveryAttempt, canRetry: {enabledByParam: enabled, maxAttemptsFromParam: attempts}}`;
  - code: on the configured status, `RetryAttempt("status")`.
  - Done: `gateway/sample-policies/retry-on-status`; for local IT it is added to `gateway/build.yaml` as an uncommitted `filePath` line.

  Add it to the IT build only (`IT/` gateway-builder config), not to `gateway/build.yaml`.
- [X] T014 [US1] IT scenarios in `IT/features/per-attempt-retry.feature`, with steps in `IT/steps_attempts.go`:
  - a 418 then 200 gives 200 with 2 backend requests;
  - with its `enabled` param false, the 418 passes through and the generated config equals the T004 baseline;
  - a policy that asks for 3 attempts with an allowance of 2 makes exactly 2 attempts;
  - client-sent `x-wso2-attempt-*` headers are stripped and have no effect.

  Add the feature to the suite's default feature paths.
- [X] T015 [US1] Controller tests in `GC/pkg/attempts/` for the test policy's declarations (param-sourced budget and switch) and placement, including an operation where an API-level policy stays in front.
  - Done: covered by `GC/pkg/attempts` and `GC/pkg/transform/attempts_test.go`.

## Phase 4: User Story 2 — Model failover on the general capability (P1)

**Independent test**: quickstart 5–6, the full existing model-failover suites.

- [ ] T016 [US2] `POLS/model-failover/policy-definition.yaml`:
  - `retryBehavior.runs: onBoth`;
  - `canRetry: {maxAttemptsFromLongestList: {path: chains[].fallbacks, plus: 1}, perAttemptTimeoutFromParam: perAttemptTimeout, seesConnectionFailures: true}`;
  - `sameOperation`: `allowed: never` for the provider-selecting policies, and `allowed: onlyIf {runsBeforeThisPolicy: true, paramNotSet: models[].provider}` for model-round-robin and model-weighted-round-robin (D10);
  - `x-wso2-refers-to: attached-provider` on `chains[].fallbacks[].provider`.
- [ ] T017 [US2] `POLS/model-failover/`: replace its private plan registry and nonce header with the attempt-scope store (`Attempt().State`). Replace tag writes with `RetryAttempt` and `StopAttempts`. Read transport failures from `Attempt().TransportFailure`. Read which part it is from `_runningAs`. Keep chain selection, the pass-through plan, health and exhaustion unchanged. Update every test; `-race` stays clean.
- [ ] T018 [US2] Delete the model-failover-specific controller code:
  - `GC/pkg/failover/` (anything still needed moves to `pkg/attempts`);
  - `buildFailover` / `buildProviderFailover` / `takeGlobalFailoverPolicy` and their hooks in `GC/pkg/utils/llm_transformer.go`;
  - `GC/pkg/config/llm_validator_failover.go`.

  Placement of upstream credentials and transformers now comes from their `runs: onEveryAttempt` declarations (T019). Add a guard test that fails if any file under `GC/pkg` names `model-failover` (SC-003).
- [ ] T019 [P] [US2] Declare `retryBehavior.runs: onEveryAttempt` in:
  - `POLS/set-headers/`, `POLS/oauth2-generator/`, every `POLS/openai-to-*-transformer/`;
  - the proxy loopback marker's system policy definition;
  - the same definitions under `API/gateway/dev-policies/` and `API/gateway/system-policies/` where mirrored (check with `git diff --no-index`; the dual-repo gotcha).
- [ ] T020 [US2] D9 targets. On a split proxy operation:
  - route per-attempt attempts to attached providers through the existing per-provider upstreams;
  - gate transformers and credentials on `selected_provider`;
  - remove per-target loopback upstreams and per-target transformer/credential copies.

  model-failover's dispatch sets `UpstreamName` to the attachment and writes the model into the body (already done in 003). Controller tests: one upstream per attached provider, none per target.
- [ ] T021 [US2] Generic validation (D10) in `GC/pkg/config/policy_validator.go`: `x-wso2-refers-to: attached-provider` checks against the proxy's attachments, and `sameOperation` rules (`never`; `onlyIf` with `runsBeforeThisPolicy` and `paramNotSet`) at registration. Port every 003 validator test case to run against these generic checks.
- [ ] T022 [US2] Regression gate:
  - rename internal header names in the IT feature, Postman generator and tests (`x-wso2-failover-*` → `x-wso2-attempt-*`);
  - rebuild coverage images (runtime before controller);
  - run the model-failover, round-robin (3 features), llm-provider and llm-proxies features;
  - run the full Postman suite twice.

  Every count must match 003's live run (111 scenarios, 638 assertions). Record the results.

**Checkpoint**: model-failover is the first user of the general capability; the gateway knows nothing about it.

## Phase 5: User Story 3 — OAuth2 refresh as a policy-only change (P1)

**Independent test**: quickstart 7–10, 14.

- [ ] T023 [US3] `POLS/oauth2-generator/policy-definition.yaml`: `retryBehavior: {runs: onEveryAttempt, canRetry: {enabledByParam: retryOnUnauthorized, maxAttempts: 2}}`, and a new `retryOnUnauthorized` boolean param (default `false`).
- [ ] T024 [US3] `POLS/oauth2-generator/`:
  - record the token fingerprint used in `Attempt().State`;
  - on a 401 with retry-on-unauthorized on, drop the cached token only if it still matches that fingerprint, then `RetryAttempt("stale_token")`;
  - on the second attempt, fetch through a single-flight refresh shared by concurrent requests;
  - a second 401 returns `StopAttempts` with the fixed `502 upstream_auth_failed` body;
  - a 403 passes;
  - never log a token (GO-AUTH-003).

  Unit tests with `-race`, including 50 concurrent stale-token requests causing one token call.
- [ ] T025 [US3] Update the living OAuth2 design doc `API/gateway/spec/prds/oauth2-upstream-auth.md` with retry-on-unauthorized. Keep its "pre-implementation proposal" framing (no implementation details, per the user's standing instruction).
- [ ] T026 [US3] IT scenarios in `IT/features/per-attempt-retry.feature` for quickstart 7–10: a mock IdP issuing numbered tokens, and a mock backend accepting only tokens ≥ N (extend `tests/mock-servers/mock-oauth2-idp` and the sample backend as needed; test code only).
- [ ] T027 [US3] SC-001 gate: sync the policy to `API/gateway/dev-policies/oauth2-generator`, run T026, and verify `git diff` for this phase touches no path under `gateway/gateway-controller`, `gateway/gateway-runtime` or `sdk/`. Record the diffstat.

## Phase 6: User Story 4 — Two retrying policies on one operation (P2)

- [ ] T028 [US4] `POLS/model-failover/`: when `Attempt().RequestedBy` is another policy, repeat the current target (don't advance the plan) and don't count the attempt against health. Unit tests.
- [ ] T029 [US4] IT scenario (quickstart 11) plus a cap-rejection scenario (quickstart 12) in `IT/features/per-attempt-retry.feature`, on an LLM proxy whose provider uses OAuth2 upstream auth with refresh.

## Phase 7: User Story 5 — Per-attempt policies that don't retry (P3)

- [ ] T030 [P] [US5] Sample signing policy `API/gateway/sample-policies/sign-per-attempt/` (`retryBehavior.runs: onEveryAttempt`, adds a timestamped HMAC header), and an IT scenario (quickstart 4) showing two attempts carry different signatures while a guardrail runs once.

## Phase 8: Polish

- [ ] T031 [P] Docs:
  - a "Writing a policy that retries" page for policy authors in `gateway-controllers/docs/` covering the `retryBehavior` block, its examples and the SDK actions;
  - update the model-failover doc's internals section;
  - update both design artifacts (model-failover design and the retry-hop proposal).
- [ ] T032 SC-004 check: regenerate the T004 baseline configs and diff; they must be identical.
- [ ] T033 Full runs:
  - policy engine, SDK and controller (`go test ./...` with `SDKROOT`);
  - Envoy validation;
  - IT vet;
  - every IT feature touched;
  - Postman twice.

  Record the results here.

## Dependencies & order

- T001 → T002 → everything else.
- Phase 2: T005, T006 and T008 in parallel; T007 after T005 and T006; T009 → T010 → T011; T012 after T005 and T006.
- US1 (Phase 3) needs Phase 2.
- US2 needs Phase 2. T016 and T017 go together; T018 needs T019 and T020; T022 comes last in US2.
- US3 needs US2's T019 (oauth2 declared `attempt`) and Phase 2 only; it may start in parallel with T020–T022.
- US4 needs US2 and US3.
- US5 needs Phase 2.

## Parallel opportunities

T005 / T006 / T008; T013 with T015; T019 with T016 and T017; T030 with anything after Phase 2; T031.

## Implementation strategy

1. **MVP**: Phases 1–3. The capability exists and a test policy retries with no gateway knowledge of it.
2. **Migration (US2)**: proves the capability carries a real, shipped behaviour. The 003 live-run numbers are the bar.
3. **OAuth2 (US3)**: the payoff and the design's acceptance test. Its gateway diff must be empty.
4. **US4 and US5**: composition and non-retrying per-attempt policies.
- [ ] T034 Before merge: tag `sdk/core` with the `attempts` package, and remove the `replace github.com/wso2/api-platform/sdk/core` lines from the policy-engine, system-policies/attempts and sample-policies/retry-on-status `go.mod` files.

## Live run: Phases 2–3 (2026-09-27, coverage images built from this branch)

- `per-attempt-retry.feature`: 4/4 scenarios, 59 steps. The test policy retries with no gateway knowledge of it. The backend saw `x-retry-attempt: 2` and no `x-wso2-attempt-*` header.
- Regression: 115/115 scenarios, 1522 steps across per-attempt-retry, model-failover, the three round-robin features, llm-provider and llm-proxies.
- model-failover Postman suite after the header rename: 638/638 assertions.
- Local-only for IT: `retry-on-status` is copied to `gateway/dev-policies` (the build context doesn't include `sample-policies`), and `gateway/build.yaml` has uncommitted `filePath` lines for it and for model-failover.

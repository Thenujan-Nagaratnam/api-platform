---
description: "Task list for failover chains keyed by the requested model (builds on 001 and 002)"
---

# Tasks: Failover Chains Keyed by the Requested Model

**Input**: `specs/003-model-keyed-chains/` (plan.md, spec.md, research.md, data-model.md, contracts/chains.md, quickstart.md)

**Paths**:
- `GC/` = `api-platform-worktrees/model-failover/gateway/gateway-controller/`
- `POL/` = `gateway-controllers-worktrees/model-failover-envoy-retry/policies/model-failover/`
- `IT/` = `api-platform-worktrees/model-failover/gateway/it/`

## Phase 1: Foundational: parameters and flattening (blocks all stories)

- [X] T001 [P] Policy config in `POL/config.go`:
  - parse `chains` into `Config.Targets` (distinct pairs, R1 flattening order), `Config.Chains map[string][]int` and `Config.PassThrough`;
  - reject `targets` ("replaced by chains"), a duplicate primary, a repeated pair within a chain, and out-of-range bounds (1–20 chains, 1–9 fallbacks, ≤ 50 distinct);
  - reject `primary.provider` in `ValidateAuthoredParams` only;
  - append the pass-through target last; `_targetIds`/`_targetNative` must cover every target including it.

  Tests in `POL/config_test.go`.
- [X] T002 [P] `POL/policy-definition.yaml`: replace `targets` with the `chains` schema (data-model.md); update the description.
- [X] T003 [P] Controller `GC/pkg/failover/failover.go`:
  - `Settings` parses `chains` with the same flattening (`Settings.Targets`, `Settings.Chains`, `Settings.PassThrough`);
  - `NumRetries` = longest chain − 1; `FrontTimeout` from the longest chain;
  - `ParseSettingsFor(params, own)` fills providers and rejects foreign fallbacks;
  - authored checks as T001;
  - a helper that rewrites `chains` with providers filled in.

  Tests in `failover_test.go`.

## Phase 2: User Story 1: each model fails over along its own chain (P1) 🎯 MVP

- [X] T004 [US1] `POL/health.go`: `admit(cfg, indices []int)` admits only the given targets. Update callers.
- [X] T005 [US1] `POL/plan.go`:
  - `attemptPlan.passThrough`;
  - `planRegistry.retarget(nonce, targets, probes, onDone)` replaces a not-yet-advanced plan's targets and clears `passThrough`.
- [X] T006 [US1] `POL/front.go` + `POL/model_failover.go`:
  - **Header phase**: strip internal headers (unchanged); create a pass-through plan and set its nonce. For a non-body model location, read the model and retarget now.
  - **Body phase**: read the body model (`$.model` on a proxy, the JSONPath on a provider) and retarget to the matched chain's admitted targets. Return the exhaustion response when the chain has none. No match or unreadable → leave pass-through.
  - The front buffers the body only when the model lives in the body.
- [X] T007 [US1] `POL/dispatch.go`: when the plan is pass-through, route to the pass-through target (proxy) or nothing (provider), and skip the model rewrite, response tagging and health recording. `frontResponseHeaders` keeps its behaviour, since no tag means no exhaustion.
- [X] T008 [P] [US1] Policy tests in `POL/roles_test.go`, `POL/lifecycle_test.go`: the chain is selected per model; the fallback order is per chain; a shared target's health is shared across chains; a suspended primary starts at its fallback; header/path models are read in the header phase.
- [X] T009 [US1] Controller proxy builder `GC/pkg/utils/llm_failover.go` `buildFailover`:
  - resolve the primary provider (`IsPrimary` attachment);
  - flatten chains;
  - one loopback upstream, transformer and credential per flattened target, plus the pass-through target (transformer without a `model` override);
  - rewrite `chains` with providers;
  - inject `_targetIds`, `_targetNative` (pass-through last).

  Update `buildProviderFailover` the same way (own provider; the pass-through target has no upstream).
- [X] T010 [P] [US1] Controller tests in `GC/pkg/utils/llm_failover_test.go` and `GC/pkg/transform/llm_failover_test.go`: flattening and deduplication; target IDs line up; the pass-through upstream is the primary; `num_retries` follows the longest chain.

**Checkpoint**: per-model chains work at unit level for proxy and provider.

## Phase 3: User Story 2: unmatched models pass through (P1)

- [X] T011 [US2] Policy tests: an unmatched model makes 1 attempt with no rewrite and no tag, and the 503 reaches the client (front strip only); a non-JSON or body-less request is a pass-through; health is untouched.

## Phase 4: User Story 3: round-robin before failover (P2)

- [X] T012 [US3] `GC/pkg/config/llm_validator_failover.go`:
  - group selectors by operation (path + method, operation and global attachments);
  - allow `model-round-robin`/`model-weighted-round-robin` without providers when they come earlier on that operation;
  - reject otherwise, with the order message;
  - other operations are unaffected.

  In `buildFailover`/`buildProviderFailover`, insert a global model-failover's front instance after the last model selector on the operation.
- [X] T013 [P] [US3] Validator and transformer tests for T012.

## Phase 5: User Story 4: registration errors (P2)

- [X] T014 [US4] Validator tests in `GC/pkg/config/llm_validator_failover_test.go` for every contract row (`targets`, duplicate primary, `primary.provider`, bounds, unattached provider, foreign provider on a provider). Update existing tests that used `targets`.

## Phase 6: Integration and polish

- [X] T015 IT: rewrite `IT/features/model-failover.feature` and `IT/steps_model_failover.go` fixtures to `chains`, sending each scenario's primary model; add quickstart scenarios 1–12.
- [X] T016 Postman: rewrite `IT/postman/model-failover/generate_collection.py` to `chains`, add folders for per-model chains, pass-through and round-robin stacking; regenerate; syntax-check.
- [X] T017 [P] Docs: `gateway-controllers/docs/model-failover/v0.1/docs/model-failover.md` and the design doc artifact (config, flow, pass-through, stacking).
- [X] T018 Sync `gateway/dev-policies/model-failover`; run the policy (`-race`), controller, SDK, Envoy validate and IT vet suites. Record the results here.

## Dependencies & order

- Phase 1 gates everything.
- T004–T007 are sequential (same package flow); T009 depends on T003.
- US2 depends on T005–T007.
- US3 and US4 depend only on Phase 1 and T009.
- Phase 6 comes last.

## Results (T018, 2026-09-27)

- Policy (`POL/`): `go vet` and gofmt clean; `go test -race -cover` passes at 89.8%. New `chains_test.go` covers flattening and deduplication, per-model chain walks, health shared across chains, pass-through (no rewrite, tag or health change; the 503 is returned as is), an unreadable body, and a pass-through timeout (untagged 504). The `dev-policies/model-failover` copy is synced and passes with `-race`.
- Controller: `go test ./...` passes (33 packages). Envoy `--mode validate` passes with `ENVOY_BIN=/opt/homebrew/bin/envoy`.
- `sdk/core`: passes.
- IT: `go vet` clean; every step line matches a definition (0 unmatched). The feature was converted to `chains`, and 6 scenarios were added for 003.
- Postman: regenerated with folder 12. All 463 scripts pass `node --check`. The single-target case was dropped, since a chain now needs at least one fallback.
- Not yet run: live godog and newman against a built gateway (the user runs the builds).

## Live run (2026-09-27, gateway coverage images built from this branch)

- godog: 111/111 scenarios, 1463/1463 steps across model-failover, model-round-robin (+ weighted, multi-provider), llm-provider and llm-proxies. `features/model-failover.feature` is now in the suite's default feature list (it wasn't before).
- Postman/newman: 638/638 assertions, run twice back to back.
- Bugs the live run found, all fixed and covered by unit tests:
  1. The front route's `response_headers_to_remove` stripped `x-wso2-failover-retry` and `-exhausted` in the router, before the front ext_proc saw the final response, so an exhausted chain returned the last provider's raw error (001 bug). Only `x-wso2-upstream-failure` is removed at the route now; the front role strips the other two itself.
  2. The provider-mode front instance lacked `_routeToTarget:false`, so it read `$.model` instead of the template's location (Gemini path models passed through).
  3. The released openai-to-anthropic transformer prefers the body's model over its param, so fallbacks sent the primary's model (001 bug). Dispatch now writes the attempt's model into the body for every chain target, not only native ones.
- Test fixes: mocks reset before each model-failover scenario and Postman folder (a pass-through readiness probe exposes a mock left scripted to fail); readiness probes ask for the primary model; the failover matrices no longer trip suspension; the readiness step reports the last status and body.

# Implementation Plan: Failover Chains Keyed by the Requested Model

**Branch**: `003-model-keyed-chains` (work lands on `001-model-failover-policy` / `model-failover-envoy-retry`) | **Date**: 2026-09-27 | **Spec**: [spec.md](./spec.md)

## Summary

Replace the single `targets` list with `chains` keyed by primary model. The front role reads the requested model, picks that model's chain, and builds the attempt plan from it. A model with no chain gets a one-attempt pass-through plan that is never tagged for retry. The controller flattens every chain into the existing per-target machinery (IDs, loopback upstreams, transformers, credentials) and adds a pass-through target. The retry budget follows the longest chain. The selector conflict rule becomes per operation, and it allows model-only round-robin placed before failover.

## Technical Context

**Language/Version**: Go 1.26.x; Envoy v1.39.0 (validation locally on 1.36.4)

**Primary Dependencies**: as 001/002; `sdk/core/utils` JSONPath helpers (already a dependency)

**Storage**: none (in-memory plans and health)

**Testing**: policy unit tests (`-race`), controller unit tests, Envoy `--mode validate`, godog, Postman/newman

**Constraints**:
- FR-005: pass-through is never retried or altered.
- FR-009: no internal header leaks.
- SC-005: one body parse at most.

**Scale/Scope**: ≤ 20 chains, ≤ 10 models per chain, ≤ 50 distinct targets per attachment

No open questions: decisions R1–R8 are in [research.md](./research.md).

## Constitution Check

The gate is `.claude/rules/`, as in 001 and 002. `.specify/memory/constitution.md` is still the template.

| Rule | Status |
|---|---|
| authentication_authorization | ✅ No new surface. Providers are resolved by the controller only (the authored `primary.provider` is rejected). |
| go-network-service-hardening | ✅ Bounded chains, targets, retries and timeouts. A pass-through is a single attempt. |
| error-handling | ✅ Fixed exhaustion body. A pass-through returns the upstream's own reply. Registration errors name the field. |
| go-control-plane-xds-security | ✅ No new listener or cluster type. The per-target loopback clusters are bounded by the 50-target limit. |
| TODO/FIXME deferral | ✅ None. |

## Project Structure

```text
gateway-controllers/policies/model-failover/
├── config.go        # chains → Targets + Chains + PassThrough; targets rejected
├── front.go         # model extraction; pass-through plan in headers; retarget in body
├── plan.go          # passThrough flag; retarget(nonce, ...)
├── dispatch.go      # pass-through: no rewrite / tag / record
├── model_failover.go# front buffers body when the model is in the body
├── health.go        # admit(cfg, indices)
└── policy-definition.yaml
api-platform/gateway/gateway-controller/
├── pkg/failover/failover.go           # Settings from chains: flatten, NumRetries by longest chain, validation
├── pkg/utils/llm_failover.go          # proxy + provider builders flatten chains, fill providers, pass-through target
├── pkg/config/llm_validator_failover.go # per-operation selector rule, order check
└── tests
api-platform/gateway/it/               # feature + Postman rewritten for chains
docs/model-failover.md, design doc artifact
```

## Complexity Tracking

| Added complexity | Why needed | Simpler alternative rejected because |
|---|---|---|
| Two-step plan (header phase, then body phase) | The body model is only known in the body phase, but every request must carry a plan | Plan only in the body phase: a body-less request reaches dispatch with no plan and gets a 500 |
| Pass-through target | "Forward unchanged" still has to go through the dispatch hop, because the front route's cluster is fixed | Per-request retry override headers: they depend on Envoy header trust settings and still need an upstream |

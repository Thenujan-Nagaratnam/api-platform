# Implementation Plan: Model Failover Policy

**Branch**: `001-model-failover-policy` (spec dir; work currently on `mcp-hub`) | **Date**: 2026-09-26 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/001-model-failover-policy/spec.md` (GitHub issue wso2/api-platform#3356)

## Summary

The new `model-failover` policy on an LlmProxy walks an ordered chain of targets (provider + model). Envoy's **native retry policy** performs the walk. The client-facing proxy route (the **front hop**) retries into a new **dispatch hop** on an Envoy internal listener, with one fresh dispatch request per attempt.

**Front hop** (Envoy + `model-failover` in its front role):
- Envoy supplies body replay, per-try timeout until response headers, no retry once the response has started, and a bounded attempt count.
- The policy builds an in-process attempt plan per request that skips suspended targets and claims probe slots.

**Dispatch hop**:
- Picks the plan's next target.
- Reuses the existing transformer policies, the loopback provider routes and the credential injection unchanged. Each attempt gets its own policy context, so a conversion never carries over between attempts.
- Classifies the attempt's response as success, non-eligible, or eligible failure. Envoy retries on a response header tag, and on `reset` for per-attempt timeouts.

**Health and exhaustion**:
- Health tracking, suspension and probe-limited recovery live in the policy engine, because Envoy outlier detection can't count 429 or limit probes.
- Exhaustion returns one fixed OpenAI-format 503.
- A small SDK addition lets policies export Prometheus metrics.

## Technical Context

**Language/Version**: Go 1.26.x (gateway-controller and policy-engine at 1.26.5, sdk/core at 1.26.2). Envoy v1.39.0.

**Primary Dependencies**: `github.com/wso2/api-platform/sdk/core` (`policy/v1alpha2`, plus a new `metrics` package), `envoyproxy/go-control-plane` (controller xDS), `prometheus/client_golang` (already in the policy engine). No new third-party modules are expected. If one is added, the dependency-management rule applies.

**Storage**: N/A. In-memory, per gateway instance (attempt plans and target health). Policy config is stored through the existing policy attachment, with no DB schema change.

**Testing**: Go `testing` unit tests (policy, controller translator and validation, SDK metrics); godog BDD integration tests in `gateway/it` with scripted mock providers; the existing Postman e2e harness pattern optional.

**Target Platform**: Linux containers: gateway-runtime (Envoy + policy-engine in one pod) and gateway-controller.

**Project Type**: Distributed gateway (control-plane translator + data-plane policy module).

**Performance Goals**:
- Once targets are suspended, overhead versus calling the fallback directly is below 50 ms median (SC-002).
- A hung primary adds no more than `perAttemptTimeout` plus under 100 ms (SC-003).
- Target overhead for the healthy-primary path: below 5 ms median (one extra in-process internal-listener hop plus one extra ext_proc exchange).

**Constraints**:
- No more than N upstream attempts for N targets (SC-004).
- No retry after the response has started (SC-005).
- Retry buffer defaults to 4 MiB and is configurable.
- Every retry parameter is bounded (network-hardening rule).
- No credentials or bodies in logs.

**Scale/Scope**:
- Up to 10 targets per chain.
- Chains are per LlmProxy route.
- Initial conversion targets: OpenAI-native, Azure OpenAI, Anthropic, Bedrock, Gemini, Mistral (FR-005b).

No `NEEDS CLARIFICATION` items remain. All unknowns are resolved in [research.md](./research.md), and five verification spikes (S1–S5) each have a decided fallback.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

`.specify/memory/constitution.md` is still the unfilled template, so it sets no project principles. The gate is instead evaluated against the repo's checked-in engineering rules (`.claude/rules/`):

| Rule | Relevance | Status |
|---|---|---|
| go-network-service-hardening | Bounded retries, per-try timeout, buffer limit, bounded probe concurrency. The outer route timeout is derived from the inner per-try timeout (directive 5), not a generic multiple. | ✅ Pass |
| authentication_authorization | The internal listener is unreachable externally. Client-supplied `x-wso2-failover-*` headers are stripped. Plan nonces are random and unguessable. Existing per-provider credential injection is reused, so no credential crosses to another target (FR-004). No `os.Exit` on remote input. | ✅ Pass |
| error-handling | The exhaustion response is sterile and fixed. Upstream error bodies are not forwarded on exhaustion. There are no source-tagged IDs. | ✅ Pass |
| ssrf-prevention | Targets reference already-registered LlmProviders only, and no URL comes from the request. | ✅ Pass (N/A: no new outbound URL surface) |
| go-control-plane-xds-security | The new internal listener's HCM gets `NormalizePath`, `MergeSlashes` and `PathWithEscapedSlashesAction`, the same as the main listener (directive 6). | ✅ Pass (must be applied; tracked in tasks) |
| db-schema-changes | No schema change. | ✅ N/A |
| dependency-management | No new modules planned. | ✅ N/A |
| KB gotchas (team knowledge base) | Internal keys go in params, not `systemParameters`. Local-reply mapper ordering must merge with the OpenAI error-format mapper. Validation runs at registration. Chain-build failure fails closed. Per-attempt `:path` is recomputed per dispatch request (research R6, R10, R11, R13, R14). | ✅ Addressed in design |
| TODO/FIXME deferral clauses | No gap is deferred behind a comment. Spikes have concrete fallbacks. | ✅ Pass |

**Post-design re-check (after Phase 1)**: still passes. The design adds no new external ports, no new credential stores and no unbounded state. The attempt-plan registry is bounded by concurrent in-flight requests and has TTL sweeping.

## Project Structure

### Documentation (this feature)

```text
specs/001-model-failover-policy/
├── plan.md              # This file
├── research.md          # Phase 0: decisions R1–R12, spikes S1–S5
├── data-model.md        # Phase 1: config, plan, health state machine
├── quickstart.md        # Phase 1: end-to-end validation scenarios
├── contracts/
│   ├── policy-definition.yaml     # user-facing policy schema
│   ├── internal-hop-headers.md    # internal headers + generated Envoy config
│   ├── exhaustion-response.md     # client-facing exhaustion error
│   └── observability.md           # metrics + log events
├── checklists/requirements.md
└── tasks.md             # Phase 2 (/speckit-tasks)
```

### Source Code

```text
gateway-controllers/policies/model-failover/          # NEW policy module (separate repo)
├── policy-definition.yaml                             # from contracts/policy-definition.yaml
├── model_failover.go                                  # GetPolicy, Mode() per role, hooks
├── front.go                                           # plan build, exhaustion, status restore
├── dispatch.go                                        # target pick, model rewrite, response classification
├── health.go                                          # healthRegistry + state machine
├── plan.go                                            # attemptPlan registry, nonce, TTL sweeper
├── metrics.go                                         # metric definitions (sdk/core/metrics)
└── *_test.go

api-platform/gateway/dev-policies/model-failover/     # local build mirror (filePath entry) until tagged
api-platform/gateway/build.yaml                        # add model-failover entry

api-platform/sdk/core/metrics/                         # NEW: Registerer injection for policies
└── metrics.go, metrics_test.go

api-platform/gateway/gateway-runtime/policy-engine/
├── cmd/policy-engine/main.go                          # inject engine registry into sdk/core/metrics
└── internal/metrics/                                  # expose wrapped registerer

api-platform/gateway/gateway-runtime/router/config/envoy-bootstrap.yaml   # enable internal_listener bootstrap extension

api-platform/gateway/gateway-controller/
├── pkg/config/            # router.failover.max_request_body_bytes, per-boot hop secret
├── pkg/utils/llm_transformer.go      # split proxy chain: front chain vs dispatch chain when a retry-chain policy is attached
├── pkg/utils/llm_failover.go         # NEW: parse/validate model-failover params, derive targets, per-target upstreams/transformers/auth
├── pkg/xds/translator.go             # front-route retry_policy, failover_dispatch internal cluster, internal listener + dispatch routes, local_reply_config mapper
├── pkg/xds/failover_listener.go      # NEW: internal listener + HCM (with path normalization) + dispatch routes
└── pkg/**/..._test.go

api-platform/gateway/it/
├── features/model-failover.feature   # NEW: quickstart scenarios 1–15
└── (mock provider modes: status/hang/reset/refuse, Anthropic streaming)
```

**Structure Decision**:
- The feature spans the controller (translation, validation), the runtime (Envoy bootstrap, policy engine metrics injection), the SDK (metrics) and a new policy module in `gateway-controllers`.
- The policy is developed against `dev-policies/model-failover` through a `filePath:` build entry, then published to `gateway-controllers` and switched to a `gomodule:` entry.
- Build order: SDK, then policy, then gateway-runtime, then gateway-controller.

## Phasing (input to /speckit-tasks)

1. **Spikes S1–S5** against a hand-written Envoy config. They lock in the internal listener, the retry trigger semantics and the local-reply flag mapping.
2. **SDK metrics** plus policy-engine wiring.
3. **Policy module**: health state machine, plan registry, front and dispatch roles, all unit-tested in isolation with fake contexts.
4. **Controller**: synchronous parameter validation at registration, then the dispatch chain split, then xDS generation (front retry, internal listener, dispatch routes, local reply mapper). Snapshot tests of the generated config.
4b. **Policy engine**: fail-closed chain-build handling for routes marked `wso2.route/failover` (research R13).
5. **Integration**: mock provider modes and the `model-failover.feature` scenarios (P1 stories first: 1, 2, 3, 4, 7, 8), then suspension and probing, then streaming and timeouts.
6. **Docs**: policy docs, gateway config reference, and a known limitation: a request body over the buffer limit is tried only once.

## Complexity Tracking

| Added complexity | Why needed | Simpler alternative rejected because |
|---|---|---|
| Extra internal-listener hop (dispatch) | Envoy retries re-send the same request, so per-attempt target selection and conversion need a per-attempt processing point. | Upstream ext_proc needs a new engine processing mode and moves credential injection. Policy-made HTTP calls lose streaming and bypass provider credentials (research R1). |
| In-process plan and health registries | 429-aware suspension and probe-limited recovery aren't available in Envoy 1.39. | Outlier detection can't count 429 and has no probe limit, and all targets share one loopback host (research R7). |
| New SDK metrics package | Per-target metrics (FR-020) have no export path today. | Envoy stats can't see targets behind the shared loopback cluster (research R9). |

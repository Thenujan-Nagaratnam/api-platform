# Feature Specification: Failover Chains Keyed by the Requested Model

**Feature Branch**: `003-model-keyed-chains` (work lands on `001-model-failover-policy`)

**Created**: 2026-09-27

**Status**: Draft

**Input**: User description: "Model failover works per requested model. If the client's model matches a chain's primary and that primary fails, fall back to the next model in *that primary's* chain. An attachment holds several chains, each a primary model plus ordered fallbacks. A request whose model matches no chain passes through with no failover. On an LlmProxy the primary matches on model only; the provider is the one the request was already routed to. This also lets model-round-robin run before model-failover on the same operation: round-robin picks the primary, failover supplies that model's fallbacks."

## Overview

Features 001 and 002 give each model-failover attachment a single ordered `targets` list. Every request starts at `targets[0]`, whatever model it asked for. That is the wrong unit: an application calling `gpt-4o` and one calling `gpt-4.1` through the same route need different fallbacks, and a request for a model that has no fallbacks configured should not be silently switched to another model.

This feature replaces `targets` with `chains`. Each chain is a **primary model** and its ordered **fallbacks**. The model in the request selects the chain: the gateway tries the primary, and only on an eligible failure walks that primary's fallbacks. A request for any other model is forwarded unchanged, with a single attempt. Everything else from 001 and 002 applies to every chain unchanged:
- failover conditions, per-attempt timeout, streaming safety;
- health tracking (suspension, probe recovery);
- the exhaustion response;
- provider mode.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Each requested model fails over along its own chain (Priority: P1)

A publisher configures two chains on one operation: `gpt-4o` → `gpt-4o` on another region → `claude-sonnet-4-5`, and `gpt-4.1` → `gpt-4.1-mini`. A client asking for `gpt-4o` gets `gpt-4o` while it is healthy; when it fails, the request moves along the `gpt-4o` chain, never the `gpt-4.1` one.

**Why this priority**: This is the feature. Without it, every request is forced onto one chain regardless of the model the application chose.

**Independent Test**: Register a proxy with two chains. Send one request per primary model with the primary failing (429), and check which fallback served each.

**Acceptance Scenarios**:

1. **Given** chains for `gpt-4o` and `gpt-4.1`, **When** a client requests `gpt-4o` and it answers, **Then** exactly one attempt is made, to `gpt-4o`.
2. **Given** the same chains, **When** `gpt-4o` answers 429, **Then** the next attempt goes to `gpt-4o`'s first fallback, and never to a `gpt-4.1` model.
3. **Given** the same chains, **When** `gpt-4.1` fails, **Then** the request is served by `gpt-4.1-mini`.
4. **Given** a fallback that names a different provider, **When** it is used, **Then** the gateway applies that provider's credentials and request format, as in 001.
5. **Given** a chain whose primary and fallbacks all fail, **When** the chain runs out, **Then** the client gets the fixed exhaustion response.

---

### User Story 2 - Models without a chain pass through untouched (Priority: P1)

A client asks for a model the publisher configured no chain for. The request goes where it would have gone without the policy: its model unchanged, one attempt, and the upstream's answer returned as it is (including a 429 or 503).

**Why this priority**: Rewriting an unconfigured model into some other model changes what the application gets without anyone deciding it should.

**Independent Test**: Request a model with no chain while the upstream answers 503; confirm one attempt, the original model, and a 503 from the upstream rather than the exhaustion response.

**Acceptance Scenarios**:

1. **Given** chains only for `gpt-4o`, **When** a client requests `gpt-4o-mini`, **Then** one attempt is made with model `gpt-4o-mini` to the route's usual provider.
2. **Given** the same, **When** that attempt answers 503, **Then** the client receives that 503, not the exhaustion response, and no retry happens.
3. **Given** a request with no readable model (missing, or a body that isn't JSON), **When** it arrives, **Then** it is passed through.

---

### User Story 3 - Round-robin picks the primary, failover supplies its fallbacks (Priority: P2)

A publisher balances traffic across `gpt-4o` and `gpt-4.1` with model-round-robin, and wants each to fail over along its own chain. They attach model-round-robin before model-failover on the same operation.

**Why this priority**: Balancing plus failover is a common need. Chains keyed by the requested model make stacking the two policies meaningful instead of conflicting.

**Independent Test**: Attach model-round-robin over [`gpt-4o`, `gpt-4.1`] followed by model-failover with both chains. Make the primary fail on two consecutive requests and check each fell back along its own chain.

**Acceptance Scenarios**:

1. **Given** round-robin before model-failover, **When** round-robin picks `gpt-4.1` and it fails, **Then** the request is served by `gpt-4.1`'s fallback.
2. **Given** model-failover placed before round-robin on the same operation, **When** the configuration is registered, **Then** it is rejected, because round-robin's choice would come after the chain was chosen.
3. **Given** a model-selecting policy that also switches providers (round-robin entries naming a provider, the header router, and similar) on the same operation, **When** registered, **Then** it is rejected, as in 001.
4. **Given** model-round-robin on one operation and model-failover on another, **When** registered, **Then** it is accepted.

---

### User Story 4 - Clear registration errors (Priority: P2)

**Acceptance Scenarios**:

1. **Given** two chains with the same primary model, **When** registered, **Then** it is rejected, naming the duplicated model.
2. **Given** a chain with no fallbacks, a chain longer than ten models, more than twenty chains, or a model repeated within one chain, **When** registered, **Then** it is rejected with the field path.
3. **Given** the old `targets` parameter, **When** registered, **Then** it is rejected with a message pointing to `chains`.
4. **Given** a provider attachment whose fallback names another provider, **When** registered, **Then** it is rejected, as in 002.

### Edge Cases

- **Same model in two chains as a fallback.** Health is shared per provider and model, so a suspended model is skipped in every chain that lists it.
- **Suspended primary.** New requests for that model start at its first available fallback. A primary in probing takes limited probe traffic, as any target does.
- **Every model of a chain suspended.** The exhaustion response is returned immediately, with no upstream call.
- **Model matching.** Matching is exact and case-sensitive. A dated variant such as `gpt-4o-2024-08-06` matches only a chain whose primary is exactly that string.
- **Provider mode with a path, header or query model.** The model is read from where the template says (as in 002) and matched the same way.
- **Round-robin's own suspension.** It sees only the final reply. With failover after it, a failed primary rescued by a fallback returns 200, so round-robin won't suspend that primary. This is documented as expected.
- **Streaming.** Unchanged: no failover after the response has started.
- **Pass-through timeout.** The single pass-through attempt is still bounded by the per-attempt timeout. Past it the client gets a `504`, not the exhaustion response.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The policy MUST accept `chains`: a list of 1–20 chains, each with a `primary` naming a model, and 1–9 ordered `fallbacks` each naming a model and optionally a provider, with at most 50 distinct provider-and-model pairs across all chains.
- **FR-002**: The gateway MUST read the model from the request (the body's model on a proxy, the template's model location on a provider) and select the chain whose primary equals it exactly.
- **FR-003**: For a matched chain, the first attempt MUST use the primary model on the provider the request was routed to. Later attempts MUST walk that chain's fallbacks in order, only after an eligible failure.
- **FR-004**: A fallback without a provider MUST use the primary's provider: the proxy's routed provider on a proxy, or the provider itself on a provider.
- **FR-005**: A request whose model matches no chain, or has no readable model, MUST be forwarded unchanged with exactly one attempt. Its response MUST reach the client unchanged, including failure statuses.
- **FR-006**: Health tracking, suspension, probe recovery, per-attempt timeout, failover conditions, streaming safety and the exhaustion response MUST behave per chain exactly as in 001 and 002. Health is per provider and model and shared across chains.
- **FR-007**: Registration MUST reject:
  - duplicate primaries;
  - a model repeated within one chain;
  - chains or fallbacks outside their bounds;
  - a `provider` on a primary;
  - the removed `targets` parameter;
  - the 002 provider-mode rules (another provider as a fallback, an unusable model location, a path that fixes the model).
- **FR-008**: model-round-robin and model-weighted-round-robin MAY share an operation with model-failover when they run before it and name no provider. Every other combination on the same operation MUST be rejected. Selecting policies on other operations MUST be accepted.
- **FR-009**: Nothing internal (plan, chain, hop, retry headers) may reach a client or a provider, including on the pass-through path.

### Key Entities

- **Chain**: a primary model plus ordered fallbacks; identified by its primary model within one attachment.
- **Target**: a provider and model; the unit of health. The flattened list of every chain's models is what the gateway tracks.
- **Attempt plan**: per request, the selected chain's available targets in order, or a single pass-through attempt.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For every configured primary, a failing primary is answered from that primary's own fallbacks in 100% of test runs, and never from another chain.
- **SC-002**: A request for an unconfigured model reaches the upstream once, with its model unchanged, in 100% of test runs.
- **SC-003**: Round-robin plus failover: each round-robin pick fails over along its own chain in 100% of test runs.
- **SC-004**: Every invalid configuration in User Story 4 and FR-007 is rejected at registration with an error naming the field.
- **SC-005**: Healthy-path overhead stays within 001's budget. Reading the model adds no more than one body parse per request.

## Assumptions

- **Breaking change**: `targets` is removed. 001 and 002 have not been released, so there is nothing to migrate.
- **Client format**: on a proxy, the model is read from the OpenAI-format body's `model`. On a provider, it is read from the template's model location (002).
- **Primary's provider on a proxy**: the proxy's primary provider, which is where a request without failover goes today. Provider-switching selectors stay disallowed on the same operation, so this is always well defined.
- **Limits**: 20 chains per attachment and 10 models per chain. The Envoy retry count is set by the longest chain; shorter chains stop early through the plan.

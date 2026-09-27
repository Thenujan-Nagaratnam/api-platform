# Feature Specification: Model Failover on LLM Providers

**Feature Branch**: `002-provider-model-failover`

**Created**: 2026-09-27

**Status**: Draft

**Input**: User description: "Model failover on LlmProvider (same-provider model fallback). Extend the model-failover policy (specs/001-model-failover-policy) so it can also be attached to an LlmProvider, not only an LlmProxy. On a provider, the failover targets name models only (for example gpt-4o then gpt-4o-mini); every attempt goes to that provider's own upstream with its own credentials, and the request's model is replaced per attempt. It reuses the same Envoy-native retry front route + internal dispatch listener design, the same failover conditions (status codes, connection failure, reset, per-attempt timeout), target health (suspension, probe recovery), exhaustion response and streaming safety. Only templates that carry the model in the request body ($.model) are supported; templates with the model in the URL path (Gemini, AWS Bedrock) are rejected at registration. Naming another provider as a target on an LlmProvider is rejected (use an LlmProxy for cross-provider failover). The per-boot hop secret must never be forwarded to the real provider in this mode. Today attaching model-failover to an LlmProvider is accepted but makes every request on that route fail with 500; that must stop."

## Overview

Feature 001 lets an LLM **proxy** fail over across providers. Many publishers only have one provider but several models on it: when the preferred model is rate-limited or overloaded, a cheaper or older model on the same provider would still answer. Today they must build a proxy around a single provider to get failover, and attaching the policy to the provider directly silently breaks the route. This feature lets the model-failover policy be attached to an **LLM provider**, where it falls back across that provider's own models.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Fall back to another model on the same provider (Priority: P1)

An API publisher attaches model-failover to an LLM provider with a chain of models, for example `gpt-4o` then `gpt-4o-mini`. When the first model returns a configured failure (for example `429`), the gateway sends the same request to the same provider with the next model, and the client gets that answer.

**Why this priority**: This is the whole feature. Without it the only way to get model fallback is an extra proxy.

**Independent Test**: Attach a two-model chain to a provider backed by a mock that fails the first request and succeeds on the second. Send one request and confirm the client gets `200`, the mock received two requests, and the second carried the second model.

**Acceptance Scenarios**:

1. **Given** a provider with targets [gpt-4o, gpt-4o-mini], **When** the first attempt returns `429`, **Then** the request is resent to the same provider with model `gpt-4o-mini` and the client receives that response.
2. **Given** the same chain, **When** the first attempt succeeds, **Then** only one request reaches the provider and its model is `gpt-4o`.
3. **Given** the same chain, **When** the first attempt returns a status that is not a failover condition (for example `400`), **Then** the client receives it unchanged and no second request is sent.
4. **Given** the same chain, **When** the provider cannot be reached, resets the connection, or doesn't answer within the per-attempt timeout, **Then** the next model is tried (each condition subject to its setting).
5. **Given** every model fails, **When** a request arrives, **Then** the client receives the same fixed exhaustion error as a proxy chain, and each model was tried once.

---

### User Story 2 - Clear errors for configurations that can't work (Priority: P1)

A publisher who attaches model-failover to a provider in a way that can't work gets a clear registration error instead of a route that fails on every request.

**Why this priority**: Today this configuration is accepted and then every request on the route fails with `500`. That is a correctness bug users hit silently.

**Independent Test**: Register each invalid configuration below and confirm each is rejected with a validation error naming the problem, and that no route starts returning `500`.

**Acceptance Scenarios**:

1. **Given** a provider whose template carries the model in the URL path (Gemini, AWS Bedrock), **When** model-failover is attached to a path that names one model, **Then** the deployment is rejected with an error asking for a wildcard path.
2. **Given** a provider, **When** a target names a different provider, **Then** registration is rejected with an error pointing to an LlmProxy for cross-provider failover.
3. **Given** a provider, **When** the policy's settings break the same rules as on a proxy (empty chain, duplicate model, bad timeout, bad status code, internal parameter), **Then** registration is rejected with the same errors as on a proxy.
4. **Given** a provider, **When** model-failover is combined with another policy that chooses the model or provider on the same provider, **Then** registration is rejected.

---

### User Story 3 - Same health and safety behaviour as proxy failover (Priority: P2)

A model that keeps failing is suspended and later brought back through limited probes, exactly as targets are on a proxy. Streaming answers are never failed over once they start.

**Why this priority**: Consistency with feature 001. It reuses that behaviour rather than adding new rules.

**Independent Test**: With a threshold of 2 and a short suspension, fail the first model twice, confirm the third request skips it, wait out the suspension, and confirm a probe brings it back.

**Acceptance Scenarios**:

1. **Given** a threshold of N, **When** a model fails N times in a row, **Then** later requests skip it until its suspension ends.
2. **Given** a suspended model whose suspension has ended, **When** requests arrive, **Then** limited probe traffic reaches it and it returns to normal after the configured number of successful probes.
3. **Given** a streamed answer has started, **When** the stream fails, **Then** no other model is tried.

---

### User Story 4 - Nothing internal reaches the provider (Priority: P2)

In provider mode the gateway calls the real provider directly from its internal hop. No internal header, including the gateway's per-boot secret, may reach the provider.

**Why this priority**: A leaked secret would let an outside party read internal failure labels. This is a security requirement, not a feature.

**Independent Test**: Run a failover on a provider and inspect every request the mock received for internal failover headers.

**Acceptance Scenarios**:

1. **Given** a provider with model-failover, **When** any attempt reaches the provider, **Then** it carries none of the gateway's internal failover headers.
2. **Given** a provider with model-failover, **When** the provider can't be reached, **Then** the gateway still recognises it as a connection failure and tries the next model.

---

### Edge Cases

- **Proxy calling a provider that has its own failover**: a proxy target whose provider also has model-failover works as nested failover. Each layer walks its own chain, and the attempt limits multiply (proxy targets × provider models). This is allowed and documented.
- **Target naming the provider itself**: `provider` equal to the provider's own name is accepted and treated as if omitted.
- **Access-control deny routes**: routes that only return the deny response are never split for failover.
- **Global attachment**: attaching model-failover in the provider's global policies applies the chain to every forwarding operation, as on a proxy.
- **Template whose model field is in the body but ignored by the provider** (for example Azure OpenAI, where the deployment in the URL picks the model): the gateway rewrites the body as configured. Whether the provider honours it is the provider's behaviour; the documentation points to a proxy with two providers for Azure deployments.
- **Chain of one model**: allowed. It gets the per-attempt timeout, health tracking and the exhaustion response, with nothing to fall back to.

## Requirements *(mandatory)*

### Functional Requirements

**Attachment**

- **FR-001**: The model-failover policy MUST be accepted on an LLM provider, attached to an operation or globally, as well as on an LLM proxy.
- **FR-002**: On a provider, each target MUST name a model. A target MAY omit its provider, or name the provider itself. A target naming any other provider MUST be rejected at registration.
- **FR-003**: On a provider, every attempt MUST go to that provider's own upstream and use that provider's own configured upstream credentials.
- **FR-004**: On a provider, the request's model MUST be replaced with the attempt's target model before the attempt is sent, at the location the provider's template reads it from (request body, header, query parameter or URL path). All other request content MUST be forwarded unchanged.
- **FR-005**: Model-failover on a provider MUST be rejected at registration when the template's model location is unusable (unknown location, a path pattern without a capture group) or when a path-located model is fixed by the operation's own path.

**Behaviour shared with proxy failover (feature 001)**

- **FR-006**: The failover conditions, per-attempt timeout, bounded attempts, streaming safety, target health (suspension and probe recovery) and exhaustion response MUST behave exactly as for a proxy chain, with a target identified by its model.
- **FR-007**: The provider's other policies (user policies, access control, token and cost accounting) MUST run once per client request, not once per attempt.
- **FR-008**: The same parameter validation as on a proxy MUST apply at registration. Model-failover MUST also be rejected on a provider that carries another model- or provider-selecting policy.

**Safety**

- **FR-009**: No internal failover header, including the per-boot hop secret, MUST reach the provider's upstream on any attempt.
- **FR-010**: Connection failures, resets and timeouts on the provider's upstream MUST still be recognised as such, so their settings apply.
- **FR-011**: A provider configuration that attaches model-failover MUST NOT result in a route that fails every request. It is either served correctly or rejected at registration.
- **FR-012**: Existing proxy failover (feature 001) MUST keep working unchanged, including when a proxy's target provider itself has model-failover.

### Key Entities

- **Provider Failover Chain**: A model-failover attachment on an LLM provider: an ordered list of models on that provider, plus the same conditions and health settings as a proxy chain.
- **Model Target**: One entry in a provider chain: a model name, on the provider's own upstream. It is the unit of health tracking.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With the first model always failing and the second healthy, at least 99.9% of requests to a provider with model-failover succeed.
- **SC-002**: Every invalid provider configuration listed in User Story 2 is rejected at registration. No accepted configuration produces a route that answers every request with `500`.
- **SC-003**: Across a test run with injected failures, no request makes more attempts than the number of models in the chain.
- **SC-004**: Zero requests captured at the provider carry an internal failover header.
- **SC-005**: Every existing proxy-failover scenario from feature 001 still passes.

## Assumptions

- **Model location**: the template's request-model location decides where each attempt's model goes: the body (OpenAI, Anthropic, Mistral, Azure AI Foundry, Azure OpenAI) or the URL path (Gemini, AWS Bedrock). This matches how model-round-robin rewrites the model.
- **Client format**: the client sends the provider's own format. No format conversion happens in provider mode, because every attempt goes to the same provider.
- **Credentials**: the provider's own upstream authentication (API key, OAuth2 or other) is applied on every attempt, exactly as on a normal request.
- **Health scope**: as in feature 001, health is tracked per gateway instance and per attachment.
- **Out of scope**: failing over from a provider to another provider (use an LlmProxy), and rewriting a model carried in the URL path.

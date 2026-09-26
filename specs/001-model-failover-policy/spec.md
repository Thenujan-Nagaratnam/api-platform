# Feature Specification: Model Failover Policy

**Feature Branch**: `001-model-failover-policy`

**Created**: 2026-09-26

**Status**: Draft

**Input**: User description: "GitHub issue wso2/api-platform#3356 — [Feature]: Model Failover Policy. Ordered fallback models, same-model regional failover, cross-provider failover, configurable failure conditions (429 / selected 5xx), transport failure handling, consecutive failure detection, target-specific health tracking, temporary target suspension, controlled recovery with probe traffic, complete fallback-chain traversal, per-attempt timeout, bounded retries, streaming safety, failover exhaustion response, logs and metrics."

## Overview

Teams that put AI applications behind the gateway depend on third-party LLM providers that rate-limit, suffer regional outages, and occasionally hang. Today a single failing model or region fails the client's request. The Model Failover Policy lets an API publisher declare an ordered chain of model targets. The gateway then routes each request to the first healthy target, moves down the chain when a target fails, keeps unhealthy targets out of rotation for a while, and brings them back carefully. The client sees a single successful response, or one clear error if every target fails.

## Clarifications

### Session 2026-09-26

- Q: How far should cross-provider request/response adaptation go? → A: Full two-way conversion between supported provider formats, streaming included, reusing the gateway's existing provider transformation policies wherever possible (FR-005 to FR-005d).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Ordered fallback when the primary model fails (Priority: P1)

An API publisher sets a primary model and one or more fallback models in priority order. When the primary returns a failure the publisher marked as eligible (for example `429` or `503`), or can't be reached at all, the gateway sends the same request to the next target in the chain. The client gets the fallback's response and never sees the primary's failure.

**Why this priority**: This is the core value of the feature. Without it, nothing else in the policy matters. It works as an MVP even without health tracking, because each request walks the chain on its own.

**Independent Test**: Set up a two-target chain with a mock primary that always returns `429` and a mock fallback that returns `200`. Send a request through the gateway and confirm the client gets the fallback's `200`.

**Acceptance Scenarios**:

1. **Given** a chain of [Model A, Model B] and `429` configured as a failover condition, **When** Model A returns `429`, **Then** the request is sent to Model B and the client receives Model B's response.
2. **Given** a chain of [Model A, Model B], **When** Model A returns `200`, **Then** Model B is never contacted.
3. **Given** a chain of [Model A, Model B] where `400` is *not* a failover condition, **When** Model A returns `400`, **Then** the client receives Model A's `400` and no failover happens.
4. **Given** a chain of [Model A, Model B], **When** the connection to Model A is refused, reset, or times out, **Then** the request is sent to Model B.

---

### User Story 2 - Same-model regional failover (Priority: P1)

An API publisher runs the same model in two regions of the same provider (for example Azure OpenAI in Sweden as primary and Spain as secondary). When the primary region is failing, traffic moves to the secondary region with no change visible to the client.

**Why this priority**: Regional capacity exhaustion and outages are the most common real-world reason for failover, and this is the simplest case because both targets share one request format.

**Independent Test**: Set up two targets that point at two provider configurations serving the same model. Make the first one fail and confirm the request succeeds through the second.

**Acceptance Scenarios**:

1. **Given** targets [Provider-Sweden/gpt-x, Provider-Spain/gpt-x], **When** Provider-Sweden returns a configured `5xx`, **Then** the request succeeds via Provider-Spain using Provider-Spain's own credentials and endpoint.
2. **Given** the two regional targets, **When** Provider-Sweden is marked unhealthy, **Then** Provider-Spain's health is unaffected.

---

### User Story 3 - Cross-provider failover (Priority: P2)

An API publisher adds a fallback model from a different provider (for example an OpenAI model as primary and an Anthropic model as secondary). When the gateway fails over, it applies the fallback provider's authentication and any request adaptation the fallback needs, so the client never changes its request.

**Why this priority**: This protects against a whole provider going down, not just one region. It depends on the ordered-chain behavior from Story 1.

**Independent Test**: Set up a chain whose fallback target belongs to a different provider configuration. Make the primary fail and confirm the fallback receives a correctly authenticated request that it accepts.

**Acceptance Scenarios**:

1. **Given** a chain of [Provider-X/model-1, Provider-Y/model-2], **When** Provider-X fails, **Then** the request reaches Provider-Y with Provider-Y's credentials and the target model set to `model-2`, and Provider-X's credentials are never sent to Provider-Y.
2. **Given** a client sending OpenAI Chat Completions requests and a chain of [OpenAI/model-1, Anthropic/model-2], **When** OpenAI fails and Anthropic serves the request, **Then** the gateway converts the request to Anthropic's format and converts Anthropic's response back to OpenAI Chat Completions format, so the client receives a response it can consume without changing its client code.
3. **Given** the same cross-provider chain and a streaming request, **When** the Anthropic fallback serves it, **Then** the client receives an OpenAI-format stream, including incremental content deltas, tool-call deltas, the finish reason, and the stream terminator.
4. **Given** a chain that includes a target whose provider format the gateway has no conversion for, **When** the policy is configured, **Then** the configuration is rejected with a clear validation error naming that target.

---

### User Story 4 - Suspending unhealthy targets and recovering them safely (Priority: P2)

After a target fails a configured number of times in a row, the gateway stops sending normal traffic to it for a configured period. Requests go straight to the next healthy target instead of paying for a failed attempt first. When the period ends, the gateway sends a limited number of probe requests. The target returns to normal traffic only after the configured number of probes succeed.

**Why this priority**: Without suspension, every request pays the latency of the failing primary before failing over. This matters at scale but isn't needed for correctness.

**Independent Test**: Set a consecutive-failure threshold of 3 and a suspension period of 30 seconds. Send 3 requests that fail on the primary, then confirm that request 4 skips the primary entirely. After 30 seconds, confirm that limited probes reach the primary and normal traffic comes back only after the required number of probes succeed.

**Acceptance Scenarios**:

1. **Given** a consecutive-failure threshold of N, **When** a target fails N times in a row, **Then** it is suspended and later requests skip it without trying it.
2. **Given** a target has failed N-1 times in a row, **When** its next attempt succeeds, **Then** its consecutive-failure count goes back to zero and it is not suspended.
3. **Given** a suspended target whose suspension period has ended, **When** requests arrive, **Then** only the configured probe share of traffic goes to it and the rest keeps going to the next healthy target.
4. **Given** recovery requires M successful probes, **When** M probes succeed in a row, **Then** the target returns to normal traffic.
5. **Given** a target in probe state, **When** a probe fails, **Then** the target is suspended again for a new suspension period.

---

### User Story 5 - Bounded, time-boxed attempts (Priority: P2)

An API publisher sets a per-attempt timeout so that a slow target can't use up the whole request budget. The gateway tries each target at most once per request and stops when the chain runs out.

**Why this priority**: Without bounds, failover can make latency worse and multiply upstream cost. This is a safety guarantee on top of Story 1.

**Independent Test**: Set up a primary that hangs longer than the per-attempt timeout. Confirm the gateway gives up on it at the timeout and the fallback serves the request within roughly the per-attempt timeout plus the fallback's response time.

**Acceptance Scenarios**:

1. **Given** a per-attempt timeout of T, **When** the primary doesn't respond within T, **Then** the attempt is abandoned and the next target is tried.
2. **Given** a chain of 3 targets that all fail, **When** a request is processed, **Then** each target is attempted at most once and the chain is never restarted.
3. **Given** a chain where the second target is suspended, **When** the first target fails, **Then** the gateway skips the second target and tries the third.

---

### User Story 6 - Clear exhaustion response and operational visibility (Priority: P3)

When every target has failed or is unavailable, the client gets one consistent, clearly identified error. Operators can see from logs and metrics which target served each request, why failover happened, when targets were suspended or recovered, and when a chain was exhausted.

**Why this priority**: This is needed for production operation and debugging but is not part of the request-path behavior itself.

**Independent Test**: Make every target fail and confirm that the client error follows the documented format and that logs and metrics record each attempt, each failover reason, and the exhaustion event.

**Acceptance Scenarios**:

1. **Given** every target fails or is suspended, **When** a request arrives, **Then** the client receives the documented exhaustion error, with a status and body that stay the same no matter which specific failures happened.
2. **Given** a failover occurred, **When** an operator checks logs, **Then** they can see the failed target, the reason (status code, connection failure, or timeout), and the target that eventually served the request.
3. **Given** a target is suspended or recovered, **When** an operator checks metrics, **Then** the state change is recorded against that target.

---

### Edge Cases

- **Streaming started**: If the selected target has already sent response headers or streamed content to the client and then fails mid-stream, the gateway MUST NOT fail over. The stream ends with an error and no second target's output is mixed in.
- **Every target suspended**: If every target in the chain is suspended and none is due for a probe, the gateway returns the exhaustion error right away without contacting any upstream.
- **Single-target chain**: A chain with only a primary still gets health tracking, suspension, and the exhaustion response. It simply has nothing to fail over to.
- **Duplicate targets**: A configuration that lists the same provider and model pair twice in one chain is rejected when the policy is configured, so a target can't be tried twice per request.
- **Non-eligible errors**: Client errors such as `400`, `401`, and `404` are passed straight to the client and don't trigger failover or count toward suspension unless explicitly configured as failover conditions.
- **Request body replay**: The gateway must be able to send the original request body again, unchanged, to each later target.
- **Concurrent requests during probe**: When many requests arrive while a target is in probe state, no more than the configured probe share reaches that target.
- **Invalid configuration**: A per-attempt timeout of zero or less, a consecutive-failure threshold below 1, a suspension period of zero or less, or a failover condition outside the allowed status ranges is rejected when the policy is configured, with a clear validation error.
- **Provider-specific error bodies**: Whether a target's failure counts as a failover condition depends on its HTTP status or transport outcome, not on its provider-specific error body. When the chain is exhausted, no raw provider error body (in any provider's format) reaches the client.
- **Client disconnects**: If the client disconnects partway through the chain, the gateway stops trying further targets.

## Requirements *(mandatory)*

### Functional Requirements

**Chain configuration**

- **FR-001**: The policy MUST let an API publisher define an ordered list of model targets, with one primary and one or more fallbacks.
- **FR-002**: Each target MUST identify a provider configuration and a model. The same model MAY appear more than once under different provider configurations (regional failover). Different providers MAY appear in the same chain (cross-provider failover).
- **FR-003**: The policy MUST reject, when configured, a chain that contains the same provider-configuration and model pair more than once.
- **FR-004**: When sending to a target, the gateway MUST use that target's own authentication, endpoint, and model identifier, and MUST NOT send another target's credentials to it.
- **FR-005**: For a target whose provider uses a different API format from the client, the gateway MUST convert the request into that provider's format and convert the provider's response back into the client's format, both ways, for non-streaming and streaming responses. Conversion MUST cover message content, tool/function calls, finish reasons, and token-usage reporting.
- **FR-005a**: Conversion MUST be applied only for the target actually being attempted. Each attempt MUST start from the client's original, unconverted request, so a conversion made for one target never leaks into the request sent to another.
- **FR-005b**: The supported target formats for this version MUST at minimum be: OpenAI (native, no conversion), Azure OpenAI, Anthropic, AWS Bedrock, Google Gemini, and Mistral. This matches the provider conversions the gateway already offers as standalone transformation policies.
- **FR-005c**: The policy MUST reject, when configured, any target whose provider format has no supported conversion from the client's format.
- **FR-005d**: Cross-provider conversion MUST reuse the gateway's existing provider transformation capabilities rather than introducing a second, separate conversion implementation, so conversion behaviour stays the same whether a provider is reached directly or through failover.

**Failure conditions**

- **FR-006**: The policy MUST let the publisher choose which upstream HTTP statuses trigger failover. It MUST support `429` and a publisher-selected set of `5xx` statuses.
- **FR-007**: The gateway MUST fail over on transport-level failures: connection refused, connection reset, upstream unavailable, and per-attempt timeout.
- **FR-008**: Statuses that are not configured as failover conditions MUST be returned to the client unchanged, without failover.

**Attempt bounds**

- **FR-009**: The policy MUST support a per-attempt timeout that limits how long the gateway waits for each target.
- **FR-010**: Within a single request, each target MUST be attempted at most once. The gateway MUST stop when the chain is exhausted and MUST NOT restart it.
- **FR-011**: The gateway MUST skip suspended targets and go on to the next available target in order.
- **FR-012**: The gateway MUST NOT fail over after any response headers or response body bytes from a target have been sent to the client.

**Health tracking, suspension, and recovery**

- **FR-013**: The gateway MUST track health separately for each target (each provider configuration and model pair), so a failure on one target never changes the health of another.
- **FR-014**: The policy MUST support a configurable number of consecutive failures that causes a target to be suspended. A successful response MUST reset the target's consecutive-failure count.
- **FR-015**: A suspended target MUST receive no normal traffic for a configurable suspension period.
- **FR-016**: When the suspension period ends, the gateway MUST send only a limited, configurable amount of probe traffic to the target, and MUST return it to normal traffic only after a configurable number of consecutive successful probes.
- **FR-017**: A failed probe MUST suspend the target again for a new suspension period.

**Exhaustion and observability**

- **FR-018**: When every target has failed or is unavailable, the gateway MUST return one documented error response whose status and body format don't change based on which failures occurred, and which doesn't reveal upstream credentials, internal endpoints, or raw upstream error bodies.
- **FR-019**: The gateway MUST log, for each request that used failover: every target attempted, the reason each failed attempt failed, and the target that served the request, or the fact that the chain was exhausted.
- **FR-020**: The gateway MUST emit metrics for target selection, failover events by reason, suspensions, recoveries, and chain exhaustion, each labelled with the target it relates to.
- **FR-021**: Logs and metrics MUST NOT contain upstream credentials or request and response payload content.

**Configuration validation**

- **FR-022**: The policy MUST reject invalid settings when configured: an empty chain, a per-attempt timeout of zero or less, a consecutive-failure threshold below 1, a suspension period of zero or less, a required probe-success count below 1, or failover statuses outside `429` and the `5xx` range.
- **FR-023**: Every tunable setting (per-attempt timeout, failure threshold, suspension period, probe amount, required probe successes, failover statuses) MUST have a documented safe default, so a publisher can enable failover by supplying only the chain.

### Key Entities

- **Failover Chain**: The ordered list of targets attached to an API, plus the failure conditions, timeouts, and health settings that govern it.
- **Target**: One entry in the chain. It pairs a provider configuration (which carries endpoint, credentials, and request format) with a model identifier. It is the unit of health tracking.
- **Target Health State**: The runtime state of a target: *healthy*, *suspended* (until a given time), or *probing* (with a count of successful probes so far). It also holds the consecutive-failure count.
- **Failover Condition**: An upstream outcome that makes a target eligible for failover: a configured HTTP status or a transport failure.
- **Exhaustion Response**: The standard error returned to the client when no target can serve the request.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: When the primary target is fully unavailable and at least one fallback is healthy, 99.9% or more of client requests succeed.
- **SC-002**: Once a failing target is suspended, requests reach a healthy fallback with no added wait on the suspended target. Median added latency compared with calling the fallback directly is under 50 ms.
- **SC-003**: A primary that hangs longer than the per-attempt timeout never makes a request take longer than the per-attempt timeout plus the serving fallback's own response time plus a small fixed overhead (under 100 ms).
- **SC-004**: In a chain of N targets, no request produces more than N upstream attempts. This holds across a load test of at least 10,000 requests with injected failures.
- **SC-005**: No streamed response that has already started is ever combined with output from a second target. There are zero such incidents across a test run with injected mid-stream failures.
- **SC-006**: A recovered target gets back its normal share of traffic within one suspension period plus the time needed for the required number of successful probes.
- **SC-007**: An operator can tell, for any single failed-over request, which targets were tried, why each failed, and which one served it, using only logs and metrics, within 5 minutes of starting to investigate.

## Assumptions

- **Attachment point**: The policy attaches to the gateway's existing LLM-facing APIs (LLM proxies and providers) and reuses provider configurations the platform already manages, including their credentials. No new credential store is introduced.
- **Health state scope**: Target health is tracked per gateway instance and per policy attachment. It is not shared across gateway replicas or across different APIs that happen to use the same provider. Cluster-wide health sharing is out of scope for this version.
- **Failure eligibility defaults**: If the publisher doesn't configure failure conditions, the defaults are `429`, `500`, `502`, `503`, `504`, and all transport failures.
- **Request replay**: Request bodies for LLM calls are small enough to buffer so they can be sent again to later targets.
- **Client-perceived model**: The response the client receives may come from a different model than the one it asked for. The served target is visible to operators through logs and metrics. Whether it is also exposed to the client (for example in a response header) is decided during planning.
- **Client format**: Clients call the policy-protected API using the OpenAI Chat Completions format. Clients sending other formats (for example native Anthropic Messages) are out of scope for this version, because every existing transformation goes from OpenAI to another provider.
- **Dependency: existing transformation policies**: Cross-provider conversion depends on the existing gateway transformation policies (`openai-to-anthropic-transformer`, `openai-to-azure-openai-transformer`, `openai-to-bedrock-transformer`, `openai-to-gemini-transformer`, `openai-to-mistral-transformer` in the `gateway-controllers` repository). These already handle request conversion, response conversion, and streaming conversion, and they already run only when their provider is the selected one for a request. The failover policy is expected to drive them by marking the currently attempted target as the selected provider on each attempt. Any gap found during planning (for example a converter that can't be re-run cleanly for a second attempt in the same request) is treated as a change to that transformation policy, not as a reason to duplicate it.
- **Out of scope**: Load balancing or weighted distribution across healthy targets (this feature is strictly priority-ordered), cost- or latency-based dynamic routing, and semantic or content-based failover (for example, failing over because a response was refused or was low quality).

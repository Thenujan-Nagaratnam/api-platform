# Feature Specification: A Per-Attempt Retry Hop Any Policy Can Use

**Feature Branch**: `004-per-attempt-retry-hop` (its own branch, based on `001-model-failover-policy`)

**Created**: 2026-09-27

**Status**: Draft

**Input**: User description: "A proper, reusable retry mechanism: make the internal listener and per-attempt retry available to all policies without changing the gateway each time a policy needs it. Model failover built this for itself; OAuth2 (refresh the token on a backend 401 and retry, so the client never sees the gateway's stale credential) is the next policy that needs it, and more will follow."

## Overview

Model failover resends a request with different per-attempt work (another model, another provider, other credentials). It does this with a second route that the gateway enters once per attempt, so that the policies on it run again for every attempt. Today that capability is switched on only by a policy named `model-failover`, and the gateway reads that policy's own parameters to size the retries. Every further policy that needs to retry would need its own gateway change.

This feature turns the per-attempt hop into a general gateway capability. A policy declares, in its own definition, that it runs once per attempt, whether it may ask for retries and at most how many. At runtime it signals "try again", "stop" or "carry on" through the policy SDK. The gateway supplies the mechanism and never learns any policy's meaning. After this feature, a policy that needs retries ships as a policy release, with no gateway change.

Model failover is migrated onto the capability as its first user. OAuth2 token refresh on a backend 401 is the second, and it is the proof: shipping it must need no gateway change.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A policy author adds retry behaviour without a gateway release (Priority: P1)

A policy author wants their policy to retry a request under a condition only the policy understands. They declare the capability in the policy's definition and use the SDK's retry signals in the policy's code. The gateway, unchanged, runs the policy once per attempt and retries when the policy asks.

**Why this priority**: This is the feature's purpose. Without it, every retrying policy costs a gateway change and a coordinated release.

**Independent Test**: Build a small test policy that asks for one retry on a chosen status, deploy it on an operation through an unchanged gateway, and check that the client receives the retried answer.

**Acceptance Scenarios**:

1. **Given** a policy whose definition declares that it runs on every attempt and can cause at most 2 attempts, **When** it is attached to an operation and the backend's first answer triggers it, **Then** the request is sent a second time, the policy runs again on that attempt, and the client receives the second answer.
2. **Given** the same policy, **When** the backend answers normally, **Then** exactly one attempt is made.
3. **Given** the same policy with its enabling parameter turned off, **When** it is attached, **Then** the operation is configured exactly as it would be without the capability (a single route, no extra hop).
4. **Given** the policy asks for more retries than it declared, **When** the limit is reached, **Then** no further attempt is made and the last answer (or the policy's chosen final answer) reaches the client.

---

### User Story 2 - Model failover keeps working, with no failover-specific gateway code (Priority: P1)

Operators using model failover see no change: chains, fallbacks, pass-through, suspension, recovery, exhaustion, streaming safety and provider mode behave as today. Inside the gateway, nothing names or parses model-failover any more.

**Why this priority**: The migration proves the capability is general and keeps a shipped behaviour intact.

**Independent Test**: Run the existing model-failover godog scenarios and Postman suite against a gateway where model-failover uses only the general capability, and check that the gateway's translation code contains no model-failover-specific paths.

**Acceptance Scenarios**:

1. **Given** the existing model-failover test suites, **When** run after the migration, **Then** they pass unchanged, apart from renamed internal header names in tests that assert on them.
2. **Given** the gateway-controller source after the migration, **When** searched for model-failover-specific parsing or placement logic, **Then** none remains; model-failover's needs are expressed only in its policy definition and policy code.

---

### User Story 3 - OAuth2 recovers from a stale token without the client noticing (Priority: P1)

An API uses OAuth2 upstream authentication. The identity provider revokes or rotates a token before the gateway's cache expects it. The backend answers 401. The gateway drops that token, obtains a new one, and resends the request once. The client receives the backend's answer to the resent request.

**Why this priority**: It is the motivating need, and the acceptance test of the whole feature is that it ships as a policy-only change.

**Independent Test**: With a mock backend that rejects the first token and accepts the next one, send a request through an API with OAuth2 upstream auth and retry-on-unauthorized turned on. Check that the client gets 200 and the identity provider was asked for a token twice in total.

**Acceptance Scenarios**:

1. **Given** retry-on-unauthorized is on, **When** the backend answers 401 to a cached token, **Then** the gateway fetches a new token, resends the request once, and returns the backend's answer to that attempt.
2. **Given** the new token also gets 401, **When** the second attempt finishes, **Then** no third attempt is made, and the client receives a fixed gateway error saying the upstream credential was rejected, not the backend's 401.
3. **Given** many concurrent requests all hit 401 with the same stale token, **When** they retry, **Then** the identity provider receives one token request for them, not one per request.
4. **Given** another request already refreshed the token, **When** a slower request's stale-token 401 arrives, **Then** the newer token is kept, not discarded.
5. **Given** the backend answers 403, **When** the attempt finishes, **Then** no retry happens.
6. **Given** retry-on-unauthorized is off (the default), **When** the backend answers 401, **Then** behaviour is as today.
7. **Given** the change set that delivers this story, **When** reviewed, **Then** it contains no gateway-controller or gateway-runtime changes.

---

### User Story 4 - Several retrying policies share one operation safely (Priority: P2)

An LLM route uses model failover and OAuth2 upstream auth with refresh. A stale token triggers a refresh on the same target; an unavailable target triggers failover to the next. Neither policy misreads the other's retries.

**Why this priority**: Combining retrying policies is where a general mechanism can go wrong; it must be well defined, but single-policy use comes first.

**Independent Test**: Put both policies on one operation. Script the first target to reject the first token and then fail with 503. Check that the request refreshes once on the first target, then moves to the second target, and that the first target's health counts only the 503.

**Acceptance Scenarios**:

1. **Given** both policies on one operation, **When** a stale token is refreshed, **Then** the next attempt goes to the same target and model failover does not count it as a target failure.
2. **Given** both policies, **When** a target fails with an eligible status, **Then** the next attempt goes to the next target, where OAuth2 may again refresh once.
3. **Given** the combined retry allowance would exceed the gateway-wide cap, **When** the configuration is registered, **Then** it is rejected with a message naming the cap.

---

### User Story 5 - Per-attempt policies that don't retry run correctly on retries (Priority: P3)

Some policies never ask for retries but must run once per attempt because another policy might retry: request signing, credential injection, request transformers.

**Acceptance Scenarios**:

1. **Given** a signing policy declared as per-attempt, **When** another policy on the operation triggers a retry, **Then** the resent request carries a fresh signature and timestamp.
2. **Given** a per-request policy (guardrail, quota, analytics), **When** a request is retried, **Then** it runs once for the client request, not once per attempt.

### Edge Cases

- **No retrying policy enabled.** The operation keeps its single route; nothing about the extra hop is generated.
- **Declared allowance vs actual need.** The gateway configures an upper bound; a policy that needs fewer attempts stops early. No policy can exceed its declared allowance.
- **Request body too large to keep for resending.** The first attempt is sent; no retry is possible; the client receives that attempt's answer (or the policy's final answer).
- **Streaming.** No retry once a response has started, whichever policy asks.
- **Transport failures.** Connection failures, resets and per-attempt timeouts are reported to policies that declared interest, which decide whether to retry.
- **Unknown or forged internal signals from a client.** Stripped on arrival; never trusted.
- **A policy upgrade changes its declaration.** Takes effect on the next deployment of each operation; no gateway change.
- **Policy removed from an operation.** The operation returns to a single route on redeploy.

## Requirements *(mandatory)*

### Functional Requirements

**Declaring the capability**
- **FR-001**: A policy definition MUST be able to declare where the policy runs on a split operation: once per client request (default) or once per attempt.
- **FR-002**: A policy definition MUST be able to declare that the policy may request retries, with a maximum number of attempts given as a fixed number, as the value of one of the policy's own parameters, or as the length of the longest list at a parameter path plus a fixed number, and optionally the boolean parameter that turns retries on. Every declaration key MUST be readable without knowledge of gateway internals and MUST NOT require an expression language.
- **FR-003**: A policy definition MUST be able to declare a per-attempt timeout (constant or parameter reference) and whether the policy wants transport failures reported to it.

**Gateway behaviour**
- **FR-004**: The gateway MUST split an operation into a once-per-request part and a once-per-attempt part exactly when at least one attached policy declares retries and has them turned on, and MUST NOT otherwise.
- **FR-005**: The gateway MUST place each attached policy by when it declares it runs (per client request, per attempt, or both), without knowledge of specific policy names.
- **FR-006**: The gateway MUST size the retry allowance and timeouts only from declared values (summing across retrying policies on an operation), never from a policy's other parameters, and MUST reject configurations exceeding a gateway-wide cap.
- **FR-007**: The gateway MUST resend the original client request for each retry, and run every per-attempt policy again for each attempt.
- **FR-008**: The gateway MUST remove every internal signal before a response reaches the client or a request reaches a backend, and MUST ignore such signals when a client sends them.
- **FR-009**: The gateway MUST report transport failures (connection failure, reset, per-attempt timeout) to per-attempt policies that declared interest, in the same form for every policy.

**SDK signals**
- **FR-010**: The policy SDK MUST let a per-attempt policy, on a response, request another attempt with a reason; declare that no more attempts should be made, optionally with the answer the client should receive; or let the response pass.
- **FR-011**: The policy SDK MUST let a policy know the attempt number and keep per-request state across attempts, and MUST tell each policy whether the current attempt was requested by itself or by another policy.

**Migration and first new user**
- **FR-012**: Model failover MUST be expressed entirely through FR-001–FR-011, with all its current behaviour preserved, and the gateway MUST contain no model-failover-specific translation logic afterwards.
- **FR-013**: OAuth2 upstream authentication MUST offer retry-on-unauthorized (off by default) delivered with no gateway change: on a 401 it drops the token that attempt used (only if still current), obtains a new token shared by concurrent requests, and retries once; a second 401 ends with a fixed gateway error.
- **FR-014**: Validation that today lives in gateway code for model failover (provider references, conflicting policies) MUST be expressible through policy-definition annotations that the gateway checks generically.

### Key Entities

- **Execution mode**: per request or per attempt; declared by each policy.
- **Attempt allowance**: the most attempts a policy may cause, declared; summed per operation and capped.
- **Retry signal**: what a per-attempt policy answers on a response: retry (with reason), stop (with optional final answer), or pass.
- **Attempt context**: attempt number, who requested it, and per-request state shared across attempts.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Delivering OAuth2 retry-on-unauthorized requires zero changes to gateway-controller and gateway-runtime.
- **SC-002**: 100% of the existing model-failover scenarios (111 godog, 638 Postman assertions at the time of writing) pass after migration.
- **SC-003**: After migration, the gateway-controller contains no code path that checks for a specific policy name to decide splitting, placement or retry sizing.
- **SC-004**: Operations without an enabled retrying policy have identical generated configuration before and after this feature.
- **SC-005**: With a stale token, 100% of test requests reach the client with the backend's answer to the refreshed attempt, and concurrent stale-token requests cause one token request.
- **SC-006**: A new retrying test policy can be added and exercised end to end by changing only policy files.

## Assumptions

- **One-time gateway investment.** This feature itself changes the gateway, once, to make later policies gateway-free.
- **The SDK is shared.** New SDK signals ship in a policy SDK release that policies adopt when they need them.
- **Streaming limits stand.** Nothing retries after a response has started; this is an HTTP property.
- **Upstreams are declared.** A policy that routes to another upstream uses upstreams the API or proxy already declares; the capability adds no new way to reach undeclared hosts.
- **Header renames are internal.** Internal signal names change from failover-specific to general; they were never client-visible.

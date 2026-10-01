# Feature Specification: Upstream Retry Contribution

**Feature Branch**: `001-upstream-retry-contribution`

**Created**: 2026-10-01

**Status**: Draft

**Input**: User description: "Generic upstream retry contribution mechanism for the gateway policy framework. A policy (concrete motivating case: oauth2-generator) needs the gateway to retry a request against the same backend when the backend rejects it for a reason only that policy understands (a 401 means the bearer token was rejected), with the policy getting a fresh chance to fix the problem (fetch a new token) before the retry goes out. Today a plain route with no multi-target failover chain gets zero retry policy at all. Agreed direction: a new, generic, policy-definition-level field (`upstreamRetryOn`) that a policy declares once in its own definition — never as separate per-attachment configuration — merged across every policy attached to a route into one combined retry behavior."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A policy's known-recoverable failure becomes retryable with zero extra configuration (Priority: P1)

A policy author (for example, the author of a credential-injection policy) already knows that a specific backend response means "this attempt failed for a reason my policy can fix on the next try" — a rejected credential being the canonical case. Today, attaching that policy to a route with no multi-target failover chain gets no retry at all: the backend's rejection reaches the client directly, even though the policy itself already has the logic to produce a usable credential on a second attempt. The policy author wants declaring this once, as part of their policy's own definition, to be enough — no operator attaching the policy anywhere should need to separately configure which status codes mean "retry."

**Why this priority**: This is the core value of the feature. Without it, every policy with a self-correctable failure mode either ships without retry support, or the platform re-solves this per-policy, each time reinventing a config surface that can quietly drift out of sync with the policy's own existing behavior.

**Independent Test**: Attach a policy that declares a retry condition to a route with no other retry mechanism configured. Trigger the declared backend condition. Confirm the gateway retries automatically, the policy runs again on the retry, and no additional configuration was needed beyond the policy's own normal params.

**Acceptance Scenarios**:

1. **Given** a policy declares a retry condition for a specific response status with a retry count of one, and is attached to a route with no failover chain, **When** the backend returns that status on the first attempt, **Then** the gateway retries the request once, the policy runs again fresh on the retry, and the client receives the retry's response.
2. **Given** the same policy and route, **When** the backend returns a status that does not match the declared condition, **Then** the gateway does not retry, and the client receives the original response unchanged.
3. **Given** the same policy and route, **When** the backend returns the declared retryable status on both the first attempt and the retry, **Then** the gateway does not retry beyond the declared count, and the client receives the final attempt's response.

---

### User Story 2 - Coexists safely with the existing multi-target failover mechanism (Priority: P2)

A platform engineer may attach a policy with its own retry contribution to a route that also has the existing multi-target failover chain configured. They need this new mechanism to combine sensibly with the route's existing retry behavior rather than silently overriding it or being overridden by it.

**Why this priority**: Most routes won't combine both in the near term, but a silent conflict here would be a real regression on routes that already rely on multi-target failover today.

**Independent Test**: Configure a route with both a multi-target failover chain and a separately attached policy that declares its own retry contribution. Trigger each mechanism's own failure condition independently and confirm both still produce a retry.

**Acceptance Scenarios**:

1. **Given** a route has both an existing failover chain's own retry needs and an attached policy's retry contribution, **When** a backend failure matching either one occurs, **Then** the route retries according to the broader of the two (the larger retry count, the union of retryable conditions), and neither mechanism's coverage is silently dropped.
2. **Given** a route has only an existing failover chain configured and no policy declaring a retry contribution, **When** the route is evaluated, **Then** its retry behavior is unchanged from before this feature existed.

---

### User Story 3 - Multiple policies on one route each contribute their own retry condition (Priority: P3)

A platform engineer attaches several policies to one route, each of which independently knows about a different backend failure condition worth retrying (for example, two different credential-injection policies in a chain). They want the gateway to combine all of these into one coherent retry behavior for the route, rather than only the first or last declared condition taking effect.

**Why this priority**: A real but less common case than a single contributing policy; still needs to behave correctly rather than silently dropping coverage.

**Independent Test**: Attach two policies, each declaring a different retryable response condition, to the same route. Trigger each condition independently and confirm both result in a retry.

**Acceptance Scenarios**:

1. **Given** two attached policies each declare a different retryable response status, **When** the backend returns either status, **Then** the gateway retries the request.
2. **Given** the two policies declare different retry counts, **When** a retry is triggered, **Then** the route uses the larger of the two declared counts.

---

### Edge Cases

- What happens when no attached policy on a route declares a retry contribution, and the route has no existing failover chain? The route's retry behavior is unchanged — no retries, exactly as today.
- What happens when a route already has a failover chain and no other policy declares a contribution? The route's retry behavior is unchanged from before this feature existed.
- What happens when the retry count is exhausted and the backend still returns the retryable condition? The client receives the final attempt's response, same as existing retry-exhaustion behavior elsewhere in the system.
- What happens when two policies on the same route declare conflicting retry counts or conditions? The combined behavior uses the union of retryable conditions and the larger retry count, so no contributing policy's declared need is under-served.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The system MUST allow a policy to declare, as part of its own definition, the response conditions under which the gateway should retry a request against the same backend target.
- **FR-002**: The system MUST retry a request when the backend response matches any declared retry condition from any policy attached to that route, up to the declared retry count.
- **FR-003**: On a retry, the system MUST re-run every policy that participates in individual request-attempt processing, so a policy has the same opportunity to correct the condition that caused the retry before the retry is sent.
- **FR-004**: A route with no attached policy declaring a retry condition, and no other existing retry mechanism, MUST behave exactly as it does today.
- **FR-005**: When multiple attached policies each declare a retry condition, the system MUST combine them so every declared condition is honored, using the broadest combination (the union of conditions, the largest retry count) rather than only one declaration taking effect.
- **FR-006**: The system MUST NOT require whoever attaches a policy to a route to separately configure which conditions trigger a retry — that information comes solely from the policy's own declaration.
- **FR-007**: This mechanism MUST coexist with the existing multi-target failover retry system on the same route without silently overriding or being overridden by it.
- **FR-008**: The system MUST support a policy optionally declaring that connection-level failures, not only a specific response status, should also trigger a retry.
- **FR-009**: A policy's retry declaration MUST be fixed as part of that policy's own definition — it is not configurable per route or per attachment in this version.

### Key Entities

- **Retry Contribution**: A declaration, owned by a policy's own definition, stating which response or connection conditions warrant retrying a request and how many times. Not something an operator configures per attachment.
- **Route Retry Behavior**: The combined, effective retry behavior for one route, derived by merging every attached policy's retry contribution — and, where present, the existing failover chain's own retry needs — into one outcome.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A policy author makes their policy's known-recoverable failure condition retryable by declaring it once in the policy's own definition, with zero additional configuration required from anyone who attaches that policy to a route.
- **SC-002**: A route with no policy declaring a retry contribution, and no existing failover chain, shows no behavior change compared to before this feature existed.
- **SC-003**: When a declared retry condition is hit and the retry succeeds, the client never sees the initial failing response.
- **SC-004**: A route combining this mechanism with the existing multi-target failover chain shows no loss of retry coverage from either mechanism.

## Assumptions

- The mechanism that re-runs a policy fresh on every individual request attempt already exists and works correctly; this feature only addresses *when* a retry is triggered, not *how* a policy re-runs on one.
- A policy's retry contribution is fixed and declared once as part of that policy's own definition — a per-route or per-attachment override is out of scope for this version.
- Combining retry contributions from multiple policies on one route, and from an existing failover chain on the same route, always takes the broadest/most-permissive combination; a way for an author to opt out of combining is out of scope for this version.
- This feature governs how the gateway decides to retry a request against the *same* backend target. It does not change how a request is retried across *different* backend targets — that remains owned by the existing multi-target failover mechanism.

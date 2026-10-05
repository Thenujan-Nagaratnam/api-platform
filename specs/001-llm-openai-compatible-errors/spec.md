# Feature Specification: OpenAI-Compatible Error Responses for LLM APIs

**Feature Branch**: `001-llm-openai-compatible-errors` (based on wso2/api-platform PR #3622, `SavinduDimal:feature-fault-flow`)

**Created**: 2026-10-04

**Status**: Draft

**Input**: User description: "I want to work on top of this one https://github.com/wso2/api-platform/pull/3622. get his branch and we will start working on top of it. The task is: we need to have a config which enabled all the llm kind apis fault responses to be formatted into openai compatible error response."

## Context

PR #3622 adds a fault flow to the gateway. When a request fails, the failure is described as a structured fault with a code, a class, a message and a source. The API's **fault policies** then run, and they can change what the client receives.

LLM APIs (`LlmProvider`, `LlmProxy`) are mostly called through OpenAI SDKs. Those SDKs expect every error body to use the OpenAI error envelope, `{"error":{"message","type","param","code"}}`. Today the gateway's errors on LLM APIs come in whatever shape produced them, such as a policy's own JSON or a plain-text router reply. An SDK client can't parse these bodies, so it sees a generic parsing failure instead of the reason the request was rejected.

This feature is **a fault policy and nothing else**. The OpenAI error-format policy uses only the fault-policy contract PR #3622 provides, with no gateway, controller or engine change and no new gateway setting. An API author attaches it to an LLM provider or proxy, and that API's gateway-produced errors are returned in the OpenAI envelope.

It is per-API by design, for upgrades. Existing LLM clients may depend on today's error bodies, so an upgraded gateway must not change them. New LLM providers and proxies on the same gateway can opt in individually.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - New LLM APIs opt in on an upgraded gateway (Priority: P1)

An operator upgrades a gateway that already serves LLM providers with existing clients. Those providers keep returning the error bodies their clients depend on. The operator then creates a new LLM provider for a team using OpenAI SDKs and attaches the OpenAI error-format fault policy to it. When a policy on the new provider rejects a request (authentication, rate limit, guardrail), the client receives the OpenAI envelope.

**Why this priority**: This is the core of the feature and the migration path. New APIs get SDK-readable errors, and existing clients are not broken.

**Independent Test**: Deploy two LLM providers, one with the policy attached and one without. Trigger the same rejection on both. The first returns the OpenAI envelope, and the second returns exactly what it returned before.

**Acceptance Scenarios**:

1. **Given** a provider with the policy attached and a guardrail, **When** the guardrail rejects a request, **Then** the body is `{"error":{"message":...,"type":"invalid_request_error","param":null,"code":...}}` and the status is the one the guardrail chose.
2. **Given** a provider with the policy attached, **When** a request fails authentication, **Then** the body is the OpenAI envelope with `type` `authentication_error`.
3. **Given** a provider with the policy attached, **When** a request is rate limited, **Then** the body is the OpenAI envelope with `type` `rate_limit_error`.
4. **Given** an LLM proxy with the policy attached, **When** a policy on it rejects a request, **Then** the proxy returns the OpenAI envelope.
5. **Given** providers built from different templates (OpenAI, Azure OpenAI, Anthropic, Gemini, Mistral, AWS Bedrock, etc.) with the policy attached, **When** the same rejection happens on each, **Then** all of them return the OpenAI envelope.

---

### User Story 2 - Nothing changes unless an API opts in (Priority: P1)

An operator upgrades the gateway and attaches the policy nowhere. Every API behaves exactly as it did before. Attaching it to one API never changes another.

**Why this priority**: Backward compatibility is why the feature is a per-API policy at all.

**Independent Test**: Run the full existing gateway test suite with the policy present in the build but attached nowhere. Every test passes unchanged.

**Acceptance Scenarios**:

1. **Given** no API attaches the policy, **When** any API returns an error, **Then** the response is identical to the gateway without this feature.
2. **Given** one API attaches the policy, **When** any other API returns an error, **Then** its response is unaffected.

---

### User Story 3 - Rejection reasons survive formatting (Priority: P2)

Some policies on an LLM API were written for REST APIs. They reject requests with their own body shape, such as `{"message": "..."}`, `{"error_description": "..."}`, or plain text. On an API with the policy attached, the client gets the OpenAI envelope, and its `message` field is the policy's own explanation, not a generic phrase.

**Why this priority**: Formatting must not hide why the request was rejected. Without that information, clients can't fix their request.

**Independent Test**: Trigger rejections from policies that use different body shapes. Confirm that `error.message` carries each policy's text.

**Acceptance Scenarios**:

1. **Given** the policy is attached and a policy rejects with `{"message":"Too many words"}`, **When** the client reads the error, **Then** `error.message` is `"Too many words"`.
2. **Given** the policy is attached and a rejection body has no recognisable message, **When** the client reads the error, **Then** `error.message` is the standard text for the HTTP status.
3. **Given** the policy is attached and a guardrail supplies structured detail, **When** the error is formatted, **Then** that detail appears inside the `error` object and the top level holds only `error`.

---

### User Story 4 - Router failures, when the gateway sends them to fault policies (Priority: P2)

Failures written by the gateway's proxy, such as no healthy upstream, connection refused or upstream timeout, reach fault policies only when the operator has turned on the existing gateway setting `handle_upstream_faults`. When it is on, the policy formats those failures too, while the backend provider's own errors still pass through. When it is off, router failures keep the gateway's existing plain-text reply. This limitation is documented.

**Why this priority**: Upstream unavailability is a common error, but covering it with the flag off would need a gateway change, which this feature avoids by design.

**Independent Test**: With `handle_upstream_faults` on, attach the policy and make the upstream unreachable. Confirm that the client receives the OpenAI envelope. Then have the upstream return its own error and confirm that it passes through. With the flag off, confirm that the router failure is unchanged from the gateway without this feature.

**Acceptance Scenarios**:

1. **Given** `handle_upstream_faults` on and the policy attached, **When** the upstream is unreachable, times out or refuses the connection, **Then** the body is the OpenAI envelope with `type` `server_error`.
2. **Given** `handle_upstream_faults` on and the policy attached, **When** the backend provider returns its own error response, **Then** the client receives the provider's body and status unchanged.
3. **Given** `handle_upstream_faults` off and the policy attached, **When** a router failure occurs, **Then** the response is identical to the gateway without this feature.

---

### User Story 5 - Composing with other fault policies (Priority: P3)

An API can have other fault policies alongside this one, for example one that writes a custom error body. Fault policies run in declaration order, and a later entry sees and can replace what an earlier one wrote. An operator controls the outcome by where they place the OpenAI error-format policy: first, so their own fault policies have the last word, or last, so the OpenAI envelope is final.

**Why this priority**: Most APIs that need OpenAI errors won't combine this policy with a body-writing fault policy. The rule only matters when they do, and it must be predictable.

**Independent Test**: Attach the policy together with a fault policy that writes a custom body, in both orders. Confirm that the later entry's output is what the client receives.

**Acceptance Scenarios**:

1. **Given** the policy is declared before a fault policy that writes a custom body, **When** a request is rejected, **Then** the client receives the custom body.
2. **Given** the policy is declared after a fault policy that writes a custom body, **When** a request is rejected, **Then** the client receives the OpenAI envelope, keeping the custom body's message where one can be found.
3. **Given** the policy is declared after a fault policy that already wrote an OpenAI envelope, **When** a request is rejected, **Then** that envelope is left unchanged.

---

### Edge Cases

- **Request matches no API**: no API means no fault policies, so the default unmatched-route response is returned.
- **Failure after a streamed (SSE) response has already started**: the fault flow can observe it but can't change it, so this is out of scope.
- **Successful responses**: never altered, because fault policies never run on them.
- **Rejection with an empty body**: the client receives a complete OpenAI envelope, built from the fault description or the status.
- **Policy attached at operation level only**: only failures on the matching operations are formatted.
- **Policy attached at both API and operation level**: the second run finds an OpenAI envelope already in place and leaves it, so the result is the same as one run.
- **Policy attached to a non-LLM API**: it changes nothing on that API, and the response is as if it weren't attached.
- **Router failure with `handle_upstream_faults` off**: unchanged plain-text reply. This is the documented limitation.

## Requirements *(mandatory)*

### Functional Requirements

**Delivery and opt-in**

- **FR-001**: The feature MUST be delivered entirely as a fault policy. It MUST NOT require any change to the gateway, controller or policy engine, or any new gateway configuration setting.
- **FR-002**: The policy MUST use only the fault-policy contract that PR #3622 provides to policy authors.
- **FR-003**: An API author MUST be able to opt an `LlmProvider` or `LlmProxy` in by attaching the policy as a fault policy, at API level (`globalFaultPolicies`) or operation level (`operationFaultPolicies`).
- **FR-004**: When no API attaches the policy, every API MUST behave exactly as the gateway does without this feature.
- **FR-005**: On a non-LLM API, the policy MUST leave the response unchanged.

**What gets formatted**

- **FR-006**: On an API with the policy attached, every gateway-produced error that reaches the fault chain MUST be returned in the OpenAI error envelope `{"error":{"message":string,"type":string,"param":null,"code":string|null}}`. That always includes policy rejections and internal errors. It includes router failures when `handle_upstream_faults` is on.
- **FR-007**: An error response from the backend provider itself MUST pass through unchanged, both body and status.
- **FR-008**: An error body that is already a valid OpenAI error envelope MUST be left unchanged. Applying the policy more than once MUST give the same result as applying it once.
- **FR-009**: The HTTP status code MUST NOT be changed.
- **FR-010**: Formatting MUST apply regardless of the provider template or the request's `Accept` header.
- **FR-011**: The policy MUST reshape the body it finds at its position in the fault chain. Fault policies declared after it may replace its output, as the fault flow defines.

**Envelope contents**

- **FR-012**: `error.type` MUST map each fault class to OpenAI's error type names: `authentication` → `authentication_error`; `authorization` → `permission_error`; `throttling` → `rate_limit_error`; `guardrail`, `validation` and `requestSize` → `invalid_request_error`; `routing` → `not_found_error`; `upstream` and `internal` → `server_error`. When there is no fault class, the type MUST be derived from the HTTP status.
- **FR-013**: `error.message` MUST come from the fault's message if one is set. Otherwise it MUST be the message found in the current error body (common fields such as `error.message`, `message`, `error_description`, `detail`, `error`, or plain text). Otherwise it MUST be the HTTP status text.
- **FR-014**: `error.code` MUST carry the fault code when one exists, otherwise `null`. `error.param` MUST always be `null`.
- **FR-015**: Any extra detail, such as an error id or guardrail detail, MUST be placed inside the `error` object, never at the top level.

**Safety and documentation**

- **FR-016**: If the policy fails while formatting, the client MUST receive the error it would have received without the policy.
- **FR-017**: The policy's documentation MUST cover: how to attach it, the upgrade path, the field mapping, declaration-order behaviour with other fault policies, and the router-failure limitation (formatted only when `handle_upstream_faults` is on, and what turning that flag on does to response policies).

### Key Entities

- **OpenAI error-format fault policy**: a fault policy that API authors attach to LLM APIs. It renders gateway-produced errors in the OpenAI envelope.
- **OpenAI error envelope**: the response body shape `error{message, type, param, code}`, with optional nested detail.
- **Fault**: the structured failure description from PR #3622 (code, class, message, source). When present, it is the primary input to the envelope, and its source separates gateway errors from backend errors.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On an upgraded gateway, a newly created LLM provider with the policy attached returns OpenAI-format errors. On the same gateway, 100% of existing providers return byte-identical error responses to those before the upgrade.
- **SC-002**: On an API with the policy attached, an OpenAI SDK client parses 100% of policy rejections into an API error with a non-empty message. This holds for every built-in provider template and for authentication, rate limiting and guardrail rejections.
- **SC-003**: With `handle_upstream_faults` on, upstream-unavailability errors on an API with the policy attached are parsed by an OpenAI SDK client, and 100% of the backend's own error responses pass through unchanged.
- **SC-004**: The feature ships with zero changes to gateway, controller or policy-engine code, and zero new gateway settings.
- **SC-005**: With the policy attached nowhere, the entire existing gateway test suite passes unchanged.
- **SC-006**: An API author can opt one API in by adding one fault-policy entry.
- **SC-007**: In 100% of tested policy rejection shapes, the policy's own rejection reason appears in `error.message`.

## Assumptions

- PR #3622's fault flow (fault policies on LLM kinds, `FaultContext` with status, body, fault and source, and `FaultResponse` for body and header replacement) is the foundation, and is available in the SDK version the policy builds against.
- "LLM-kind APIs" means `LlmProvider` and `LlmProxy`. MCP and Agent APIs are not included.
- There is no gateway-wide switch. Every API that wants OpenAI-format errors attaches the policy. A control plane such as AI Workspace may attach it when creating new LLM APIs. That control-plane and UI work is out of scope.
- Making the policy available in the default gateway build is a distribution-list entry, not a gateway code change.
- Router failures are formatted only when `handle_upstream_faults` is on. Covering them with the flag off would need a gateway change and is out of scope.
- Attaching any fault policy turns on response-body processing for that API's routes, as PR #3622 defines. Only APIs that opt in pay that cost.
- Requests that match no API, and failures after a streamed response has started, are out of scope.
- Normalising status codes to OpenAI's conventions (for example, guardrail rejections as 400) is out of scope.

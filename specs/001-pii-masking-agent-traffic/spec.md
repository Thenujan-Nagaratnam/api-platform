# Feature Specification: Safe PII Masking and Restore for Agent Traffic

**Feature Branch**: `001-pii-masking-agent-traffic`

**Created**: 2026-11-25

**Status**: Draft

**Input**: User description: "https://github.com/wso2-enterprise/wso2-apim-internal/issues/21254 — patch the `pii-masking-regex` policy so it can safely mask and restore personal data for an agent that calls the Anthropic Messages API (streaming, tools, extended thinking) through the AI gateway. Nine items must be fixed: (1) invalid JSON when an email follows a JSON escape; (2) the phone pattern rewrites JSON numbers; (3) streamed tool-call arguments are not restored; (4) placeholder numbers change between requests when more than one entity type is on; (5) streamed thinking is not restored; (6) text of two content blocks is merged; (7) several policy instances overwrite each other's mapping; (8) the system prompt and tool definitions are masked, and only user and tool content should be; (9) the default phone pattern matches identifiers and parts of longer numbers."

## Overview

An organization runs an agent that talks to an Anthropic-style Messages API through the AI gateway. The agent streams responses and uses tools and extended thinking. The organization wants the gateway to hide personal data from the model: replace it with placeholders before the request leaves the gateway, and put the original values back in everything that returns to the agent. Today the masking policy breaks this traffic in nine ways, so the organization cannot turn it on. This feature makes the policy safe and predictable for that traffic.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Masked requests always reach the model as valid, unaltered-in-structure messages (Priority: P1)

A platform operator turns on masking for an agent route. Every request the agent sends, including long conversation histories that are re-sent on each turn, reaches the model as a well-formed message. Masking changes only personal data inside readable text values. It never changes keys, numbers or message structure. Everything that is validly masked today stays masked.

**Why this priority**: Without this, the model provider rejects requests and every later turn in the same conversation fails too. This blocks all use of the policy for agent traffic (items 1, 2, 8).

**Independent Test**: Send requests containing (a) an email directly after a line break or tab inside a user message, (b) a tool whose schema contains a very large numeric limit, and (c) an email in the system prompt and in a tool description. Inspect what the model provider receives.

**Acceptance Scenarios**:

1. **Given** a user message whose text has an email address immediately after a line break, **When** the request is masked, **Then** the provider receives a valid message in which the line break is intact and only the email is replaced by a placeholder.
2. **Given** the same for tab, carriage return and other escaped characters, **When** masked, **Then** the result is valid in every case.
3. **Given** a tool definition with a numeric limit such as 9007199254740991 and phone masking on, **When** the request is masked, **Then** the number is delivered unchanged and the message is valid.
4. **Given** the whole-payload setting and an email in the system prompt and another in a tool description, **When** the request is masked, **Then** both are still replaced by placeholders, as today, and the message is valid.
5. **Given** an operator who selects only user and tool content, **When** a request with an email in the system prompt, a tool description, a user message and a tool result is masked, **Then** only the user message and tool result emails are replaced, and the system prompt and tool description reach the model exactly as written.
6. **Given** a conversation history re-sent on a later turn, **When** it is masked, **Then** the request is valid and every earlier turn is masked the same way as before.

---

### User Story 2 - Streamed answers are fully restored for the agent (Priority: P1)

The model answers with a stream that contains text, tool calls and reasoning. The agent must receive the original values everywhere, so that tools run with real values and the agent's own reasoning view is accurate.

**Why this priority**: A tool that runs with a placeholder instead of the real email is a functional failure, not a cosmetic one (items 3, 5, 6).

**Independent Test**: Make the provider stream a response that contains a placeholder in tool-call arguments, in reasoning, and in two separate text blocks. Compare what the agent receives with the original values.

**Acceptance Scenarios**:

1. **Given** a streamed tool call whose arguments contain a placeholder, **When** the stream reaches the agent, **Then** the arguments contain the original value, so the tool runs with the real value.
2. **Given** streamed reasoning that contains a placeholder, **When** it reaches the agent, **Then** the reasoning shows the original value.
3. **Given** one batch of streamed output that holds two separate text blocks, **When** it reaches the agent, **Then** each block holds its own text and the second block is not empty.
4. **Given** a placeholder split across several stream chunks, **When** the stream ends, **Then** the agent still receives the original value (no placeholder fragments leak).
5. **Given** the same content in a non-streamed response, **When** it reaches the agent, **Then** the result is identical to the streamed case (restore parity).

---

### User Story 3 - Placeholders are stable so prompt caching and preserved reasoning keep working (Priority: P1)

The same personal value always becomes the same placeholder, for every entity type, in every request. Requests that repeat the same conversation prefix stay byte-for-byte identical.

**Why this priority**: The provider's prompt caching needs an identical prefix, and its preserved-reasoning feature drops reasoning blocks when earlier content changes. Unstable placeholders silently cost money and quality (item 4).

**Independent Test**: With email and phone masking both on, send the same request 20 times and compare the bodies the provider receives; then add turns and compare the shared prefix.

**Acceptance Scenarios**:

1. **Given** email and phone masking both on, **When** the identical request is sent 20 times, **Then** the provider receives 20 identical bodies.
2. **Given** a conversation that grows turn by turn, **When** each turn is masked, **Then** the part of the request that is unchanged from the previous turn is byte-identical.
3. **Given** the same value appearing twice in one request, **When** masked, **Then** both occurrences get the same placeholder.
4. **Given** two different values of the same type, **When** masked, **Then** they get different placeholders.

---

### User Story 4 - Several policy instances on one route do not interfere (Priority: P2)

An operator attaches more than one instance of the policy to the same route, for example one instance per entity type. Each instance masks and restores its own values.

**Why this priority**: One instance per entity type is a natural way to configure the policy, and it must not lose restores (item 7).

**Independent Test**: Attach one instance with email masking and one with phone masking. Send a request that contains both kinds of value, in both streamed and non-streamed modes.

**Acceptance Scenarios**:

1. **Given** two instances (email, phone), **When** a request with an email and a phone number is answered, **Then** both values are restored in the response.
2. **Given** the same setup in streamed mode, **When** the answer streams back, **Then** both values are restored.
3. **Given** a single instance configured as before, **When** requests are processed, **Then** behavior is unchanged except for the fixes in this feature.

---

### User Story 5 - The default phone pattern stops damaging identifiers (Priority: P3)

A tool returns identifiers, large integers or millisecond timestamps. The default phone and similar-number patterns no longer cut digits out of the middle of longer numbers, so the model reads accurate data. Anything that is a complete phone-shaped number is still masked.

**Why this priority**: It causes wrong answers but has a workaround (a custom pattern), so it ranks lowest (item 9).

**Independent Test**: With phone masking on, send a tool result containing a build id of 2147483647 and a millisecond timestamp of 1795104230191, and a genuine phone number.

**Acceptance Scenarios**:

1. **Given** a tool result containing a 13-digit millisecond timestamp, **When** masked, **Then** no part of it is replaced.
2. **Given** a number that is part of a longer run of digits, **When** masked, **Then** nothing inside the run is replaced.
3. **Given** a standalone 10-digit number such as 4155550199 or 2147483647, **When** masked, **Then** it is still masked as today, because digits alone cannot tell it from a phone number, and the documentation says how to narrow this with a custom pattern.
4. **Given** a phone number written in a common phone format, **When** masked, **Then** it is still replaced and later restored.
5. **Given** an operator-supplied custom pattern, **When** masked, **Then** it behaves as the operator defined it, unaffected by the default change.

---

### Edge Cases

- An email directly after any escaped character, and after an escaped backslash or quote, which already worked and must keep working.
- A placeholder-looking string that the user typed themselves (it must not be "restored" into something else, or must be handled deterministically).
- The same email in the system prompt (outside the selection) and in a user message (inside it): the user-message occurrence is masked and restored, and the system prompt is left as written.
- Messages whose content is a plain string versus a list of typed blocks.
- Tool results that carry nested content blocks.
- Empty text blocks and responses with no personal data at all (unchanged, no extra work visible to the agent).
- A stream that ends before a split placeholder completes.
- A request body that is not valid JSON or not a messages request: the policy must not make it worse.
- Very long conversation histories with many repeated values.
- Two instances that both match the same value.

## Requirements *(mandatory)*

### Functional Requirements

**Safe request handling (items 1, 2, 8)**

- **FR-001**: The system MUST mask only the readable text of string values, never object keys, numbers, booleans or structure, so a masked request is always valid.
- **FR-002**: The system MUST NOT produce an invalid message for any text, including text where an email, phone number or similar value directly follows an escaped character.
- **FR-003**: The system MUST keep masking every personal value that is validly masked today. With the whole-payload setting this includes text in the system prompt and tool definitions.
- **FR-004**: The operator MUST be able to select exactly which parts of a request are masked, with enough expressiveness to pick all user and tool content in a conversation history and exclude the system prompt and tool definitions. Content outside the selection MUST reach the model unchanged.
- **FR-005**: If masking would still produce an invalid message, the system MUST NOT forward the invalid message. It MUST fail in a defined, visible way instead of letting the provider reject it.

**Complete restore (items 3, 5, 6)**

- **FR-006**: The system MUST restore original values in streamed tool-call arguments.
- **FR-007**: The system MUST restore original values in streamed reasoning.
- **FR-008**: The system MUST keep the text of each content block in its own block when one batch of stream output holds several blocks.
- **FR-009**: The system MUST restore a placeholder that is split across stream chunks, and MUST NOT emit placeholder fragments to the agent.
- **FR-010**: Restore behavior for streamed and non-streamed responses MUST match for text, tool arguments and reasoning.

**Stable placeholders (item 4)**

- **FR-011**: The same value of the same entity type MUST receive the same placeholder on every request, regardless of how many entity types are enabled, what else is in the request, or the order of processing.
- **FR-012**: Different values MUST receive different placeholders.
- **FR-013**: For a conversation that grows turn by turn, the masked form of earlier turns MUST stay byte-identical so provider prompt caching and preserved reasoning continue to work.

**Instance isolation (item 7)**

- **FR-014**: Each policy instance MUST keep its own mapping of placeholders to original values. One instance MUST NOT overwrite or hide another's mapping.
- **FR-015**: Every instance's values MUST be restored in streamed and non-streamed responses when several instances are attached to one route.

**Default patterns (item 9)**

- **FR-016**: The default phone pattern, and any other built-in numeric pattern with the same weakness, MUST NOT match inside a longer run of digits.
- **FR-017**: A standalone phone-shaped number MUST stay masked, including an unformatted 10-digit one. The documentation MUST state that such numbers are masked and how to narrow detection with a custom pattern.
- **FR-018**: The default phone pattern MUST continue to mask phone numbers written in common phone formats.
- **FR-019**: Operators MUST still be able to supply their own pattern, and its behavior MUST not change.

**Compatibility and documentation**

- **FR-020**: Existing configurations MUST keep working without edits. Anything masked today that is a valid masking MUST stay masked. The only behavior that deliberately changes is that matches cutting into longer digit runs, matches that would corrupt the message, and the visible placeholder text are no longer produced. These MUST be listed in the release notes for the patch.
- **FR-021**: The policy documentation MUST describe how to scope masking to user and tool content, the new default phone behavior, and how to restore the old broad behavior with a custom pattern.
- **FR-022**: The change MUST be delivered as a patch to the 1.2.0 AI gateway so that the managing cloud service can adopt it.

### Key Entities

- **Masking policy instance**: One attachment of the policy to a route, with its own selection of content, enabled entity types and mapping.
- **Entity type**: A category of personal data, such as email or phone number, with its own placeholder family.
- **Placeholder**: A stand-in token that replaces one personal value and always maps to that value for a given instance.
- **Mapping**: The relation between placeholders and original values, held per instance and per request, used to restore responses.
- **Content scope**: The part of a request or response the policy is allowed to change, namely user text and tool results in a request, and every text, tool-argument and reasoning field in a response.
- **Streamed content block**: A unit of streamed output, such as text, tool-call arguments or reasoning, which must be restored independently of other blocks.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For a test suite covering all nine reported cases, 100% of masked requests reach the provider as valid messages and 0 are rejected for being malformed.
- **SC-002**: With the user-and-tool-content selection in use, the system prompt and tool definitions reach the provider byte-identical to what the agent sent in 100% of test requests.
- **SC-003**: In 100% of streamed test responses, tool-call arguments, reasoning and text contain the original values and no placeholder remains.
- **SC-004**: Sending the same request 20 times with email and phone masking both on produces 1 distinct provider body, not 2 or more.
- **SC-005**: For a conversation of at least 10 turns, the unchanged prefix of each turn is byte-identical to the previous turn's prefix.
- **SC-006**: With two instances attached, 100% of the values from both instances are restored in streamed and non-streamed answers.
- **SC-007**: In a test set of timestamps and long digit runs, 0% are cut by the default phone pattern, while 100% of the standalone phone-shaped numbers masked today are still masked.
- **SC-009**: Every value masked by the previous release in a regression set of requests that were valid and well-formed is still masked after the patch, with no regressions.
- **SC-008**: The managing cloud service can turn the policy on for its console agent route and remove the agent's own masking, with no further reported agent failures caused by masking in an acceptance run covering streaming, tools and thinking.

## Assumptions

- The nine items in the issue are all in scope for this patch, including item 8, because the cloud service cannot enable the policy for its console agent until it is fixed. If the shared helper work that item 8 needs is not ready, the patch is blocked on it rather than shipping without item 8.
- The relevant traffic is the Anthropic Messages shape, including the Amazon Bedrock variant, with streaming, tools and extended thinking.
- The default phone pattern keeps masking standalone phone-shaped numbers, including bare 10-digit ones (decided with the requester: anything masked today stays masked). It only stops matching inside longer digit runs. The documentation says how to narrow detection with a custom pattern.
- Placeholder text changes from a 4-digit counter to an 8-hex-digit value-derived token so that it is stable. Release notes call this out.
- The whole-payload setting keeps masking all text values. Excluding the system prompt and tool definitions is opt-in through the selection setting (item 8).
- Mapping stability is within one instance's configuration. Two different instances may use the same placeholder text for different values, so long as each restores correctly.
- Placeholders for the same value stay the same across requests and across turns without storing conversation state between requests.
- Out of scope: new entity types, changes to non-Messages request shapes, a redesign of the policy configuration, and the separate secret-handling concerns in policy parameters.
- Dependencies: the patch builds on the shared selection capability that lets an operator choose user and tool content (tracked as wso2/api-platform#3571), and it resolves wso2/api-platform#3686, #3705 and #4427 and the internal issue 21214.
- Patch tracking, dates and the release process follow the internal issue 21254 and are not part of the feature behavior.

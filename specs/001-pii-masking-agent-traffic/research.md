# Research: Safe PII Masking and Restore for Agent Traffic

All decisions are derived from the spec and the released v1.1.0 policy source (`policies/pii-masking-regex/v1.1.0` tag in gateway-controllers). No earlier fix attempt was consulted.

## R1. How to mask JSON without breaking it

- **Decision**: Validate the body with `json.Valid`, then walk the raw bytes with a small scanner that finds string tokens, skips tokens that are object keys (a string followed by `:`), decodes each value token, masks it, and re-encodes only changed tokens. Everything else is copied byte for byte.
- **Rationale**: Unmarshalling into `interface{}` and re-marshalling would change number formatting (9007199254740991 becomes 9.007199254740991e+15), key order and escapes. Item 2 shows numbers must survive. Masking decoded text fixes item 1 because an escape such as `\n` is a decoded newline that no regex can include.
- **Alternatives**: Full unmarshal and re-marshal (rejects: changes numbers and order). Regex with escape-aware patterns (rejects: fragile, still hits keys and numbers). `json.Decoder.Token` (rejects: does not expose byte offsets of strings cheaply).

## R2. Stable placeholders

- **Decision**: `[ENTITY_xxxxxxxx]` where the 8 hex digits are the first 32 bits of SHA-256 over `entity + NUL + value`. On a collision between two different values in one request, the one that sorts later by value takes the next free suffix.
- **Rationale**: Same value gives the same placeholder on every request and every turn, in any entity mix, with no state (items 4, FR-011, FR-013). 32 bits gives a negligible collision chance for a conversation.
- **Alternatives**: Keep the counter but sort first (rejects: still depends on earlier content, breaks prompt-cache prefix stability on later turns). 16 hex digits (rejects: longer placeholder, no real gain). Keep 4 hex digits (rejected by the requester's choice, 16-bit collisions are likely).

## R3. Merging mappings across instances

- **Decision**: Keep the existing shared metadata key, but merge into any map already there instead of replacing it.
- **Rationale**: Placeholders are value-derived, so the same value from two instances is the same entry and different values never clash. Restore by whichever instance runs first covers all values. No per-instance key discovery is needed.
- **Alternatives**: Per-instance key with an instance id (rejects: the policy has no stable instance id in its parameters, and every restore would have to enumerate keys).

## R4. Streaming restore per content block

- **Decision**: Parse each SSE data event, classify its text by block `index` and delta kind (`text_delta`, `input_json_delta`, `thinking_delta`), concatenate within a group, restore, and write the result to the first event of that group only, dropping the group's remaining text events. Fall back to today's path list for OpenAI and other shapes.
- **Rationale**: Fixes items 3, 5 and 6 together and keeps today's behavior for non-Anthropic shapes.
- **Alternatives**: Restore each event independently (rejects: placeholders arrive split across events).

## R5. Restoring inside tool-argument fragments

- **Decision**: For `partial_json`, replace a placeholder with the JSON-string-escaped original, then rely on the normal outer encoding of the event. Add the fragment text to the "placeholder may still be arriving" check.
- **Rationale**: The fragment is JSON text inside a JSON string, so an original containing a quote or backslash must be escaped for the inner JSON as well.
- **Alternatives**: Restore only whole-tool-call JSON at block end (rejects: delays the stream and changes event structure).

## R6. Number boundaries (item 9)

- **Decision**: Post-filter matches of the built-in phone and SSN patterns by rejecting any match whose preceding byte is an ASCII digit. The existing patterns already end with a word boundary.
- **Rationale**: Go's `regexp` has no lookbehind. The requester's rule keeps standalone 10-digit numbers masked, so only matches inside longer runs are removed.
- **Alternatives**: Require phone formatting (rejects: would stop masking values masked today). Leading `\b` (rejects: fails for `(415)` after a space and adds no protection after a letter).

## R7. Selecting user and tool content (item 8)

- **Decision**: Keep `"$"` and the default path as they are. Allow `jsonPath` to be any RFC 9535 expression. The SDK (#3571) provides a read-only evaluator that returns the normalized path of every matched node, for example `$['messages'][0]['content'][1]['text']`. At request time the policy evaluates the path once, builds a set of normalized paths, and the byte-preserving scanner from R1 tracks its own current path while it walks the body, masking a string token only if its path is in the set. Ship a documented example path for Messages traffic.
- **Rationale**: Matches the analysis that `"$"` masks everything by design and the fix is a selecting path. Selecting by path while writing by byte span keeps the R1 guarantee (numbers, keys, spacing and order untouched) in scoped mode too. Setting values through a parsed map would rewrite numbers such as 9007199254740991 and bring item 2 back.
- **Alternatives**: A new boolean such as `maskOnlyUserContent` (rejects: Messages-specific and duplicates what a path can express). An SDK multi-set helper that unmarshals and re-marshals (rejects: rewrites numbers and key order). Returning byte offsets from the SDK (rejects: couples the SDK to raw-byte positions; normalized paths are standard and reusable).

## R8. Failure behavior when masking cannot stay valid

- **Decision**: If the masked body fails `json.Valid`, return the policy's existing mediation error response instead of forwarding it.
- **Rationale**: FR-005. A visible gateway error is better than an upstream 400 that repeats on every later turn.

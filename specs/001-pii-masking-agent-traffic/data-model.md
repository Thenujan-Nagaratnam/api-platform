# Data Model: Safe PII Masking and Restore for Agent Traffic

No persistent storage. All state is per request and lives in request metadata.

## Policy instance parameters (existing, unchanged)

| Field | Type | Notes |
|---|---|---|
| `email`, `phone`, `ssn` | boolean | Built-in detectors. At least one detector is required overall. |
| `customPIIEntities[]` | `{piiEntity, piiRegex}` | `piiEntity` normalised to upper case, letters and underscores only. Duplicate names rejected. |
| `jsonPath` | string | `"$"` or empty: every string value. Default `$.messages[-1].content`. New: any RFC 9535 expression selecting many nodes. |
| `redactPII` | boolean | `true`: replace with `*****`, nothing restored. |

## Entity detector

- **name**: upper-case entity name (`EMAIL`, `PHONE`, `SSN`, or custom)
- **pattern**: compiled regex
- **digitBoundary**: true for built-in `PHONE` and `SSN`. A match whose previous byte is a digit is discarded.

## Selection set (scoped mode)

- **source**: the `jsonPath` evaluated once per request by the SDK, returning normalized paths
- **content**: set of normalized paths of the matched string nodes
- **use**: the scanner tracks its current normalized path and masks a string token only if the path is in the set. For `"$"` or an empty path there is no set and every string value is in scope.

## Placeholder

- **text**: `[` + entity name + `_` + 8 lowercase hex digits + `]`
- **derivation**: first 32 bits of SHA-256 over entity name, a NUL byte and the original value
- **uniqueness**: unique within one request. A rare collision between two different values is resolved by value order, deterministically.
- **validation shape**: `^\[[A-Z_]+_[0-9a-f]{8}\]$`. Matches already in this shape are never masked again.

## Request mapping

- Held in metadata key `piimaskingregex:pii_entities`, a map of original value to placeholder.
- **Write rule**: merge, never replace. An existing entry with the same original is kept.
- **Read rule**: invert to placeholder to original before restoring.
- Not written when `redactPII` is true or nothing matched.

## Streamed text group (restore time)

- **key**: content block `index` plus delta kind (`text`, `tool_arguments`, `thinking`), or a single group for shapes without blocks
- **members**: the text fragments of that group within one flushed batch, in order
- **state transition**: fragments are joined, restored as a whole, and the result is written back into the group's first event. The rest of the group's text events are dropped. Other groups are untouched.
- **hold rule**: a batch is held back while the joined text of any group ends in an unclosed `[` that could still become a placeholder.

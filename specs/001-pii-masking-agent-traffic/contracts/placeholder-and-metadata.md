# Contract: Placeholder Format and Shared Metadata

## Placeholder

```
[<ENTITY>_<8 lowercase hex>]      example: [EMAIL_3fa91c0b]
```

- Same entity and value give the same text on every request.
- Different values give different text within a request.
- Safe inside a JSON string: contains no characters needing escaping.
- The previous format was `[<ENTITY>_<4 hex counter>]`, for example `[EMAIL_0000]`. Responses that contain an old-format token are left alone.

## Shared metadata

| Key | `piimaskingregex:pii_entities` |
|---|---|
| Value | map of original text to placeholder |
| Writers | every non-redacting instance, merge semantics |
| Readers | every non-redacting instance, in both buffered and streamed response paths |
| Compatibility | Same key and same value type as before, so other readers keep working |

## Streamed restore contract

Input: a flushed batch of SSE events from a Messages response.

| Event content | Restore behavior |
|---|---|
| `text_delta.text` | Restored as plain text. |
| `input_json_delta.partial_json` | Restored with the original JSON-escaped, so the fragment stays valid JSON text. |
| `thinking_delta.thinking` | Restored as plain text. |
| `signature_delta`, other fields | Untouched. |
| Two blocks in one batch | Each block is restored and written into its own event. Neither block's text moves into the other. |
| Placeholder split across events | The batch is held until it is complete or cannot become a placeholder. |
| Non-Messages shapes | Same path list and behavior as today. |

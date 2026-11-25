# Contract: Policy Parameters

The parameter schema is **unchanged** except for the documented meaning of `jsonPath` and the policy version.

| Parameter | Before | After |
|---|---|---|
| `version` | v1.1.0 | v1.2.0 |
| `jsonPath` = `""` or `"$"` | The whole body is treated as one text string and written back as text. | If the body is valid JSON: every string value is masked, keys and numbers are never touched, output is valid JSON. If the body is not JSON: unchanged plain-text behavior. |
| `jsonPath` = path with one match | Masks that one node. | Same. |
| `jsonPath` = RFC 9535 path with many matches | Not supported. | Every matched string node is masked. All other bytes of the body, including numbers, keys, spacing and key order, are identical to the input. A matched node that is not a string is skipped. |
| `redactPII` | One-way `*****`. | Same. |

## Example: user and tool content only (Anthropic Messages)

```yaml
jsonPath: "$.messages[*].content[?(@.type=='text' || @.type=='tool_result')].text"
```

The final expression is fixed with the SDK work in #3571. The contract is: it must select every user text block and every tool-result text, and must not select `system` or `tools`.

## SDK contract (item 8 dependency)

The SDK evaluator takes the body and an RFC 9535 expression and returns the list of matched nodes as normalized paths, for example `$['messages'][2]['content'][0]['text']`. It is read-only: masking never goes through an SDK write helper, because that would re-marshal the body and rewrite numbers.

## Errors

| Condition | Result |
|---|---|
| Path matches nothing | Same as today: request passes through unmasked. |
| Path is invalid | Same as today: policy error response. |
| Masked body is not valid JSON | New: policy error response with the standard mediation fault. The invalid body is never forwarded. |

## Documentation obligations (FR-021)

- Describe the `"$"` behavior and the user-and-tool-content example.
- State that standalone phone-shaped numbers, including bare 10-digit ones, are masked, and that a custom pattern narrows this.
- Describe the placeholder format change.

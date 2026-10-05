# Data Model: OpenAI-Compatible Error Responses for LLM APIs

This feature stores nothing. Its "data" is the policy's configuration, the input it reads from the fault flow, and the envelope it writes.

## 1. Policy instance configuration

| Field | Type | Default | Rules |
|---|---|---|---|
| `fault.maxInspectBytes` | integer | `65536` | ≥ 0. Largest error body parsed for a message. `0` disables body inspection, so only `Fault.Message` or the status text is used |

There are no other parameters. The `fault` block is optional, so an empty `params` or `params: {fault: {}}` is valid. Unknown keys are rejected by the definition schema (`additionalProperties: false`).

## 2. Input: what the policy reads (`FaultContext`, from PR #3622)

| Field | Used for |
|---|---|
| `APIKind` | Gate: only `LlmProvider` and `LlmProxy` are formatted |
| `Source` | Gate: see the decision table below |
| `Policy` | Disambiguates `Source = unknown` |
| `ResponseCommitted` | Gate: a committed (streamed) response is never touched |
| `ResponseStatus` | Status-derived type and fallback message |
| `ResponseHeaders` | `Content-Encoding` check |
| `ResponseBody` | Message extraction and the idempotency check |
| `Fault.Type` | `error.type` |
| `Fault.Code` | `error.code` |
| `Fault.Message` | `error.message` (highest priority) |
| `Fault.Guardrail` | `error.guardrail` |
| `Fault.Description` | **Never read into output** |

## 3. Decision table (evaluated in order; the first match wins)

| # | Condition | Result |
|---|---|---|
| 1 | `APIKind` ∉ {LlmProvider, LlmProxy} | nil (unchanged) |
| 2 | `ResponseCommitted` | nil |
| 3 | `Source` = `backend` or `noRoute` | nil |
| 4 | `Source` = `unknown` and `Policy` = "" | nil |
| 5 | Current body is already an OpenAI envelope | nil |
| 6 | Otherwise | Replace the body with the envelope, set `Content-Type: application/json`, remove `Content-Encoding` if present. The status is unchanged |

Any panic → nil (unchanged), and it is logged.

## 4. Output: OpenAI error envelope

```json
{
  "error": {
    "message": "string, non-empty",
    "type": "string",
    "param": null,
    "code": "string | null",
    "guardrail": { "...": "only when Fault.Guardrail is set" }
  }
}
```

The only top-level key is `error`. Field derivation is in [contracts/openai-error-envelope.md](contracts/openai-error-envelope.md).

## 5. "Already an OpenAI envelope" predicate

The body parses as a JSON object with exactly one top-level key, `error`. Its value is an object with a string `message` and a string `type`. Other keys inside `error` are allowed.

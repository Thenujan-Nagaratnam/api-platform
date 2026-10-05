# Contract: OpenAI error envelope written by `openai-error-format`

This is the client-facing contract. It is what an OpenAI SDK parses into an `APIError`.

## Shape

```json
{"error":{"message":"<string>","type":"<string>","param":null,"code":"<string>"|null}}
```

Optional key inside `error`: `guardrail` (object), present only when a guardrail intervened. It comes from `Fault.Guardrail`, or otherwise from the assessment object a shipped guardrail wrote under `message` in its own body. The field names are the gateway's own (`interveningGuardrail`, `action`, `actionReason`, `assessments`), and `assessments` appears only if the guardrail itself chose to show it.

- `Content-Type: application/json`
- **Status**: unchanged from the error the client would otherwise have received.
- **Other headers**: unchanged (for example `WWW-Authenticate` or `Retry-After`). `Content-Encoding` is removed if the original body was encoded.

## `error.type`

| `Fault.Type` | `error.type` |
|---|---|
| `authentication` | `authentication_error` |
| `authorization` | `permission_error` |
| `throttling` | `rate_limit_error` |
| `guardrail`, `validation`, `requestSize` | `invalid_request_error` |
| `routing` | `not_found_error` |
| `upstream`, `internal` | `server_error` |
| absent, or any other value | derived from the status, as below |

| Status | `error.type` |
|---|---|
| 401 | `authentication_error` |
| 403 | `permission_error` |
| 404 | `not_found_error` |
| 429 | `rate_limit_error` |
| other 4xx | `invalid_request_error` |
| 5xx, or anything else | `server_error` |

## `error.message` (first non-empty source wins)

1. `Fault.Message`.
2. The current body, if present, not encoded (`Content-Encoding` other than `identity`), and ≤ `fault.maxInspectBytes`. JSON lookup order: `error.message`, `message` (string), `message.actionReason` (the assessment object every shipped guardrail writes under `message`), `error_description`, `detail`, `error` (when a string). A non-JSON body is used as plain text only if it declares no content type or `text/plain`, and is a single line of ≤ 1024 characters.
3. The standard status text (`http.StatusText`), for example `"Service Unavailable"`.

`Fault.Description` is never used.

## `error.code`, `error.param`

- `code`: `Fault.Code` (for example `"900901"` or `"906201"`), or `null`.
- `param`: always `null`.

## Examples

**Guardrail rejection (422), with the policy describing the fault:**
```json
{"error":{"message":"Word count exceeds the allowed range","type":"invalid_request_error","param":null,"code":"906201"}}
```

**Legacy REST-style rejection `{"message":"Too many words"}`, with no fault class, status 422:**
```json
{"error":{"message":"Too many words","type":"invalid_request_error","param":null,"code":null}}
```

**Router failure (503, `no healthy upstream`), with `handle_upstream_faults` on:**
```json
{"error":{"message":"…","type":"server_error","param":null,"code":"303001"}}
```
The message is the gateway's fault description if one is set, otherwise the router's text, otherwise `"Service Unavailable"`.

**Unchanged (no envelope written):**
- the backend's own error;
- a non-LLM API;
- a streamed response that has already started;
- a body that is already an OpenAI envelope.

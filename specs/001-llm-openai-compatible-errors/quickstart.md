# Quickstart: validating `openai-error-format`

This guide proves the feature works end to end. Contracts: [policy-definition](contracts/policy-definition.md), [envelope](contracts/openai-error-envelope.md).

## Prerequisites

- `api-platform` on branch `001-llm-openai-compatible-errors`, which is PR #3622 plus this feature.
- The policy is present in `gateway/dev-policies/openai-error-format/`, mirrored from `gateway-controllers`.
- `gateway/build.yaml` lists it with `filePath: ./dev-policies/openai-error-format`.
- Gateway images built from this branch (the user runs the build), and the stack started with `docker compose` from `gateway/`.
- Check host port owners before starting: `lsof -iTCP -sTCP:LISTEN` and `docker ps`.

## 1. Unit tests (policy package)

```bash
cd gateway-controllers/policies/openai-error-format   # worktree off wso2 main
go test ./...
cd api-platform/gateway/dev-policies/openai-error-format
GOWORK=off go test ./...
diff -r gateway-controllers/policies/openai-error-format api-platform/gateway/dev-policies/openai-error-format
```

**Expected**: all tests pass. The only diff is the `replace` line in `go.mod`.

## 2. Integration tests

```bash
cd gateway/it && make test   # or run features/openai-error-format-policy.feature alone
```

**Expected**: the new feature passes, and the existing suite passes unchanged (SC-005).

## 3. Manual scenarios

Deploy two providers on a `word-count-guardrail` (`max: 5`), with the upstream pointed at the echo backend:
- `oai-on`, with `globalFaultPolicies: [{name: openai-error-format, version: v0}]`;
- `oai-off`, without it.

| # | Action | Expected |
|---|---|---|
| 1 | POST a 10-word body to `oai-on` | Status unchanged (422). Body `{"error":{"message":…,"type":"invalid_request_error","param":null,"code":…}}` |
| 2 | Same request to `oai-off` | Byte-identical to the gateway without the policy |
| 3 | Add `api-key-auth` to `oai-on` and call it without a key | 401, `error.type` = `authentication_error`, `WWW-Authenticate` kept if it was sent |
| 4 | Point `oai-on`'s upstream at a dead host (`handle_upstream_faults = true`) | 503, `error.type` = `server_error` |
| 5 | Point `oai-on`'s upstream at a backend returning its own 400 JSON | The backend's body and status, unchanged |
| 6 | Attach the policy to a `RestApi` and trigger a rejection | Unchanged body |
| 7 | Attach the policy at both API and operation level and trigger a rejection | One valid envelope, not nested |

## 4. OpenAI SDK interop (SC-002)

```python
from openai import OpenAI, APIStatusError
c = OpenAI(base_url="http://localhost:8080/oai-on", api_key="x")
try:
    c.chat.completions.create(model="gpt-4o", messages=[{"role":"user","content":"one two three four five six seven"}])
except APIStatusError as e:
    assert e.status_code == 422 and e.message and e.body["type"] == "invalid_request_error"
```

**Expected**: the SDK raises `APIStatusError` with a non-empty message, not a JSON or parse error.

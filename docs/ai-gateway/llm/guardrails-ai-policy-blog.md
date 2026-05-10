# I Added Guardrails AI Support to WSO2 API Platform in Under an Hour

I'd been looking at [Guardrails AI](https://www.guardrailsai.com/) for a while. Their Hub has validators for toxicity, PII detection, jailbreak detection, hallucination checks, JSON format validation — a solid catalogue of ready-made validators that run as a local server. What I wanted to know was: how hard would it be to wire this up as a first-class guardrail policy in WSO2 API Platform?

Turns out, not hard at all. I had a working policy attached to an LLM Provider in under an hour.

Here's exactly what I did.

## The Idea

WSO2's AI Gateway already handles authentication, rate limiting, token budgets, routing to LLM providers like OpenAI, and even has built-in guardrails. What I specifically wanted was to bring [Guardrails AI](https://www.guardrailsai.com/) into the mix — their Hub has a rich catalogue of validators that I didn't want to reimplement from scratch, and I wanted to know how easily I could plug it in as a custom policy alongside the guardrails already there.

The approach: a custom gateway policy that calls a Guardrails AI server's `/validate` endpoint on every request. The model never sees flagged content. The application gets a structured `422` it can handle.

```
User App ──► WSO2 AI Gateway ──► OpenAI
                  │
                  ▼
         Guardrails AI Server
         (toxic-language-guard)
```

## Step 1: Set Up the Guardrails AI Server

Guardrails AI ships a REST server (`guardrails-api`) that exposes a `/guards/{name}/validate` endpoint for each guard you configure. Getting it running took maybe five minutes.

Install the library and the toxic language validator from the Hub:

```bash
pip install guardrails-ai
guardrails hub install hub://guardrails/toxic_language
```

The guard configuration is a single Python file — this is the entire thing ([guard/config.py](https://github.com/Thenujan-Nagaratnam/api-platform/blob/main/guard/config.py)):

```python
# guard/config.py
from guardrails import Guard
from guardrails.hub import ToxicLanguage

_GUARD_TOXIC_ID = "toxic-language-guard"

guard = Guard(id=_GUARD_TOXIC_ID, name=_GUARD_TOXIC_ID)
guard.use(ToxicLanguage())
```

Guardrails AI picks up every `Guard` instance your config module defines and registers a validate endpoint for each one automatically. Start the server:

```bash
guardrails start --config guard/config.py
```

Quick sanity check:

```bash
curl -X POST http://localhost:8000/guards/toxic-language-guard/validate \
  -H "Content-Type: application/json" \
  -d '{"llmOutput": "Hello, how can I help you?", "numReasks": 0}'
# → {"status": "pass", ...}
```

Server is running. On to the policy.

## Step 2: Write the Gateway Policy

WSO2 gateway policies are written in Go and compiled into the gateway image. A policy implements two lifecycle hooks — `OnRequestBody` and `OnResponseBody` — and declares what body processing mode it needs. That's the entire interface.

I created the policy directory ([gateway/dev-policies/guardrails-ai/](https://github.com/Thenujan-Nagaratnam/api-platform/tree/main/gateway/dev-policies/guardrails-ai)):

```
gateway/dev-policies/guardrails-ai/
├── go.mod
├── go.sum
├── guardrailsai.go
└── policy-definition.yaml
```

### The Policy Definition

[`policy-definition.yaml`](https://github.com/Thenujan-Nagaratnam/api-platform/blob/main/gateway/dev-policies/guardrails-ai/policy-definition.yaml) tells the gateway what parameters the policy accepts. I wanted two things configurable per-attachment: which guard to call (`guardName`), and whether to validate the request, the response, or both. Everything else — the server URL, API key, timeout — lives in operator-level `systemParameters` so individual teams can't accidentally misconfigure them.

```yaml
name: guardrails-ai
version: v1.0.0
description: |
  Validate LLM request and/or response content against a pre-configured guard on a
  Guardrails AI server. Supports any validator from the Guardrails AI Hub.

parameters:
  type: object
  properties:
    guardName:
      type: string
      description: The name of the pre-configured guard on the Guardrails AI server.
    request:
      type: object
      properties:
        enabled:
          type: boolean
          default: true
        jsonPath:
          type: string
          default: "$.messages[-1].content"
        passthroughOnError:
          type: boolean
          default: false
        showAssessment:
          type: boolean
          default: false
    response:
      type: object
      properties:
        enabled:
          type: boolean
          default: false
        jsonPath:
          type: string
          default: "$.choices[0].message.content"
        passthroughOnError:
          type: boolean
          default: false
        showAssessment:
          type: boolean
          default: false
  required:
    - guardName

systemParameters:
  type: object
  properties:
    guardrailsApiEndpoint:
      type: string
      "wso2/defaultValue": "${config.guardrails_ai.endpoint}"
    apiKey:
      type: string
      "wso2/defaultValue": "${config.guardrails_ai.api_key}"
    requestTimeout:
      type: number
      default: 10
      "wso2/defaultValue": "${config.guardrails_ai.request_timeout}"
  required:
    - guardrailsApiEndpoint
```

### The Go Implementation

The full implementation is in [`guardrailsai.go`](https://github.com/Thenujan-Nagaratnam/api-platform/blob/main/gateway/dev-policies/guardrails-ai/guardrailsai.go) and tests in [`guardrailsai_test.go`](https://github.com/Thenujan-Nagaratnam/api-platform/blob/main/gateway/dev-policies/guardrails-ai/guardrailsai_test.go). The entry point is `GetPolicy`, called once per policy attachment at startup:

```go
func GetPolicy(
    metadata policy.PolicyMetadata,
    params map[string]interface{},
) (policy.Policy, error) {
    // validate required fields, parse timeouts, build the struct
    p := &GuardrailsAIPolicy{
        apiEndpoint: strings.TrimSuffix(getStringParam(params, "guardrailsApiEndpoint"), "/"),
        apiKey:      getStringParam(params, "apiKey"),
        guardName:   getStringParam(params, "guardName"),
        httpClient:  &http.Client{Timeout: timeout},
    }
    // parse optional request/response flow config
    return p, nil
}
```

`Mode()` tells the gateway what to buffer. I need to read both bodies, so I buffer them; I don't touch headers, so I skip them:

```go
func (p *GuardrailsAIPolicy) Mode() policy.ProcessingMode {
    return policy.ProcessingMode{
        RequestHeaderMode:  policy.HeaderModeSkip,
        RequestBodyMode:    policy.BodyModeBuffer,
        ResponseHeaderMode: policy.HeaderModeSkip,
        ResponseBodyMode:   policy.BodyModeBuffer,
    }
}
```

`OnRequestBody` extracts the last message from the OpenAI-format payload using JSONPath (`$.messages[-1].content`), sends it to Guardrails AI, and returns either a pass-through or an immediate error:

```go
func (p *GuardrailsAIPolicy) OnRequestBody(
    ctx context.Context,
    reqCtx *policy.RequestContext,
    _ map[string]interface{},
) policy.RequestAction {
    if !p.hasRequestParams || !p.requestParams.Enabled {
        return policy.UpstreamRequestModifications{}
    }
    return p.validatePayload(reqCtx.Body.Content, p.requestParams, false).(policy.RequestAction)
}
```

The call to Guardrails AI is a plain HTTP POST:

```go
func (p *GuardrailsAIPolicy) callValidateAPI(text string) (*guardrailsValidateResponse, error) {
    validateURL := fmt.Sprintf("%s/guards/%s/validate", p.apiEndpoint, p.guardName)
    reqBody := guardrailsValidateRequest{LLMOutput: text, NumReasks: 0}
    // marshal, POST, read response, decode JSON
}
```

When validation fails the policy returns a `422` with a structured body. With `showAssessment: true` the failed validator names are included so callers know exactly why they were blocked:

```json
{
  "type": "GUARDRAILS_AI_GUARDRAIL",
  "message": {
    "action": "GUARDRAIL_INTERVENED",
    "interveningGuardrail": "Guardrails AI",
    "guardName": "toxic-language-guard",
    "direction": "REQUEST",
    "actionReason": "Violation detected by Guardrails AI guard.",
    "assessments": {
      "failedValidators": ["ToxicLanguage"],
      "callId": "abc-123"
    }
  }
}
```

The `AnalyticsMetadata` on the response marks it as a guardrail hit so it flows through to WSO2's analytics pipeline automatically.

### Error Handling

I made two deliberate choices here:

**`passthroughOnError`** — if the Guardrails AI server is down or returns a non-200, the default is to block (fail closed). Setting this to `true` lets the request through so availability isn't coupled to the guard server's uptime.

**JSONPath errors** — if the request body doesn't match the expected shape, the same flag applies. Teams with non-standard payloads just override `jsonPath` in their attachment.

## Step 3: Bundle It into the Gateway Image

Add the policy to `build.yaml`:

```yaml
version: v1
gateway:
  version: 1.2.0
policies:
  - name: guardrails-ai
    filePath: ./dev-policies/guardrails-ai
  # ... other policies
```

Build the image:

```bash
ap gateway image build
```

The CLI compiles the policy into the gateway binary. No sidecar, no plugin loading at runtime — it's part of the process.

## Step 4: Configure the Gateway

The Guardrails AI server address goes in the gateway config, set once by the operator:

```toml
[guardrails_ai]
endpoint = "http://guardrails-server:8000"
api_key  = ""         # leave empty if no auth
request_timeout = 10  # seconds
```

The `systemParameters` in the policy definition reference these via `${config.guardrails_ai.endpoint}` etc. Teams attaching the policy only ever configure `guardName` and flow options — they can't touch the server address.

## Step 5: Attach It to an LLM Provider

Policies are part of the LLM Provider body, not a separate sub-resource. Use `PUT /llm-providers/{id}` with the full provider config and add a `policies` array:

```bash
curl -X PUT http://localhost:9090/api/management/v0.9/llm-providers/my-provider \
  -H "Content-Type: application/json" \
  -H "Authorization: Basic YWRtaW46YWRtaW4=" \
  -d '{
    ... existing provider config ...,
    "policies": [
      {
        "name": "guardrails-ai",
        "version": "v1.0.0",
        "paths": [
          {
            "path": "/chat/completions",
            "methods": ["POST"],
            "params": {
              "guardName": "toxic-language-guard",
              "request": {
                "enabled": true,
                "showAssessment": true
              }
            }
          }
        ]
      }
    ]
  }'
```

That's it. Every `POST /chat/completions` request through that LLM Provider now runs through the toxic language guard before reaching OpenAI.

## Does It Work?

Clean message — straight through:

```bash
curl -X POST http://localhost:8080/my-assistant/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages": [{"role": "user", "content": "What is the capital of France?"}]}'
# → 200 OK, normal OpenAI response
```

Toxic message — blocked at the gateway, model never sees it:

```bash
curl -X POST http://localhost:8080/my-assistant/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages": [{"role": "user", "content": "I want to hurt someone"}]}'
# → 422 Unprocessable Entity
# {
#   "type": "GUARDRAILS_AI_GUARDRAIL",
#   "message": {
#     "action": "GUARDRAIL_INTERVENED",
#     "direction": "REQUEST",
#     "guardName": "toxic-language-guard",
#     ...
#   }
# }
```

## What Surprised Me

**The policy SDK does more than I expected.** JSONPath extraction, body buffering, analytics metadata propagation — all provided. The actual logic I wrote was the HTTP call to Guardrails AI and the response mapping. The whole Go file is around 400 lines, including the full test suite.

**Writing tests was fast.** The SDK's test helpers let you spin up a mock HTTP server in a few lines and assert directly on the `RequestAction`/`ResponseAction` return values. I covered all the edge cases — JSONPath failures, API errors, passthrough mode, nil bodies — without much effort.

**One policy, any validator.** I started with `ToxicLanguage`. Within minutes I was also testing with `DetectPII` on a second guard, just by changing the `guardName` in the attachment. The gateway policy didn't change at all. That composability is the best part of the Guardrails AI Hub approach.

## What's Next

- Add a PII guard: create a second `Guard` in `config.py`, attach the same policy with `guardName: pii-guard`
- Enable response validation: flip `response.enabled: true` to inspect model output too
- Chain multiple guards: attach the policy twice with different guard names — each runs independently

The whole thing took under an hour. The policy SDK made the gateway integration straightforward, and Guardrails AI did the actual validation work. Neither piece required much ceremony to get going.

---

**Source code:**
- [guard/config.py](https://github.com/Thenujan-Nagaratnam/api-platform/blob/main/guard/config.py) — Guardrails AI server configuration
- [gateway/dev-policies/guardrails-ai/guardrailsai.go](https://github.com/Thenujan-Nagaratnam/api-platform/blob/main/gateway/dev-policies/guardrails-ai/guardrailsai.go) — policy implementation
- [gateway/dev-policies/guardrails-ai/guardrailsai_test.go](https://github.com/Thenujan-Nagaratnam/api-platform/blob/main/gateway/dev-policies/guardrails-ai/guardrailsai_test.go) — test suite
- [gateway/dev-policies/guardrails-ai/policy-definition.yaml](https://github.com/Thenujan-Nagaratnam/api-platform/blob/main/gateway/dev-policies/guardrails-ai/policy-definition.yaml) — policy schema and parameters

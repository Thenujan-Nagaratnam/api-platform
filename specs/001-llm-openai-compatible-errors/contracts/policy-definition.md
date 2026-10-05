# Contract: `openai-error-format` policy definition and attachment

This is the author-facing contract: how an API opts in.

## Policy definition (`policy-definition.yaml`)

```yaml
name: openai-error-format
version: v0.1.0
description: |
  Fault policy for LlmProvider and LlmProxy APIs. Returns gateway-produced errors
  (policy rejections, internal errors, and router failures when
  handle_upstream_faults is on) in the OpenAI error envelope
  {"error":{"message","type","param","code"}} so OpenAI SDKs can parse them.
  The status code is never changed. Backend errors pass through unchanged.
  Has no effect on other API kinds.
parameters:
  type: object
  additionalProperties: false
  properties:
    fault:
      type: object
      x-wso2-policy-advanced-param: false
      additionalProperties: false
      description: Fault-path configuration.
      properties:
        maxInspectBytes:
          type: integer
          minimum: 0
          default: 65536
          x-wso2-policy-advanced-param: true
          description: |
            Largest error body (bytes) read to recover the rejecting policy's message.
            Larger or encoded bodies fall back to the fault description or status text.
            0 disables body inspection.
systemParameters:
  type: object
  properties: {}
```

The policy implements only `OnFault`. Attached anywhere other than a fault list, it is dropped when the chain is built, as the PR defines.

## Attaching it

**API level, every operation:**
```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: openai-sdk-provider
spec:
  displayName: OpenAI SDK Provider
  version: v1.0
  template: openai
  context: /openai
  upstream:
    url: https://api.openai.com/v1
  accessControl:
    mode: allow_all
  globalFaultPolicies:
    - name: openai-error-format
      version: v0
```

**Operation level:**
```yaml
  operationFaultPolicies:
    - name: openai-error-format
      version: v0
      paths:
        - path: /chat/completions
          methods: [POST]
          params: {}        # required by the operation-level schema
```

**LlmProxy**: use the same `globalFaultPolicies` / `operationFaultPolicies` fields.

## Ordering with other fault policies

Fault policies run in declaration order, and each sees the previous entry's output.

- **Declare it first**: your own body-writing fault policies have the last word.
- **Declare it last**: the OpenAI envelope is final. A body that is already an OpenAI envelope is never rewritten.

## Router failures

Router failures reach any fault policy only when the gateway runs with:

```toml
[policy_engine.fault_policies]
handle_upstream_faults = true
```

That setting is gateway-wide. It stops response policies from seeing upstream and router errors on every API. With it off (the default), router failures keep the gateway's plain-text reply.

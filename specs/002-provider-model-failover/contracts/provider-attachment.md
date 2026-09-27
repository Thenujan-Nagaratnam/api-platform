# Contract: model-failover on an LlmProvider

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: openai
spec:
  template: openai            # must read the model from the body ($.model)
  context: /openai
  upstream:
    url: https://api.openai.com
    auth: { type: api-key, header: Authorization, value: Bearer <key> }
  accessControl: { mode: allow_all }
  operationPolicies:
    - name: model-failover
      version: v0
      paths:
        - path: /chat/completions
          methods: [POST]
          params:
            targets:
              - model: gpt-4o
              - model: gpt-4o-mini
            perAttemptTimeout: 20s
```

## Registration responses

| Configuration | Result |
|---|---|
| As above | `201` |
| A target with `provider: openai` (its own name) | `201` |
| A target with `provider: anthropic` | `400` `targets[i].provider: on an LlmProvider, targets name models of this provider only; attach "anthropic" to an LlmProxy to fail over to it` |
| `template: gemini` or `awsbedrock` on a wildcard path (`/models/*`) | `201`; each attempt rewrites the model in the path |
| `template: gemini` on `/models/gemini-2.5-pro:generateContent` | `400` `the path ... fixes the model to "gemini-2.5-pro"; use a wildcard path (for example /models/*) ...` |
| Also carries `model-round-robin` (or another selecting policy) | `400` |
| Any proxy-mode rule broken (bounds, duplicates, `_` keys) | `400`, same messages as on a proxy |

## Envoy (generated)

- **Front route:** as in 001 (cluster `failover_dispatch`, retry policy, no `RegexRewrite`).
- **Dispatch route (provider mode):** prefix `/` plus the chain header. Fixed cluster = the provider's main upstream, with the provider's `RegexRewrite`, route timeout 0. The hop filter is **enabled**, so the secret is moved to metadata and stripped before the provider.
- **Dispatch route (proxy mode):** as in 001, plus `typed_per_filter_config: { wso2.failover.hop: LuaPerRoute{disabled: true} }`.
- **Internal listener:** filters are ext_proc, Lua, `wso2.failover.hop`, router. The local-reply mapper matches the hop secret in dynamic metadata.

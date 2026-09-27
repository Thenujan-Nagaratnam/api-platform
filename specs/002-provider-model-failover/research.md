# Phase 0 Research: Model Failover on LLM Providers

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Builds on**: [001 research](../001-model-failover-policy/research.md) | **Date**: 2026-09-27

## Baseline (verified in code, branch `001-model-failover-policy`)

| Area | Today | Reference |
|---|---|---|
| Provider transform | Passes a model-failover instance through as an ordinary policy. No internal `_role`/`_chainId`, so `GetPolicy` fails, the chain doesn't build, and every request on the route gets `500 no policy chain`. | `pkg/utils/llm_transformer.go` `transformProvider` (538+); policy `config.go` `parseInternal` |
| Provider validation | `validateModelFailover` runs for proxies only. | `pkg/config/llm_validator.go:880` |
| Provider routing | A provider's routes forward straight to its main upstream (no upstream definitions unless `upstream.ref`), with a `RegexRewrite` that strips the context and prepends the upstream base path. | `pkg/transform/restapi.go` |
| Provider credentials | One upstream-auth policy (API key, OAuth2 or other) is appended to every forwarding operation. | `transformProvider` step 3 and 884–894 |
| Template model location | Policies attached to a provider get the template's `requestModel` `{location, identifier}` merged into their params. OpenAI, Anthropic, Mistral, Azure OpenAI and Azure AI Foundry use `payload` + `$.model`; Gemini and Bedrock use `pathParam`. | `buildTemplateParams`; `default-llm-provider-templates/*.yaml` |
| Registration errors | A provider transform error is returned as `400` by `POST /llm-providers`. | `llm_deployment.go:303`, `llm_provider_handler.go` |

## Decisions

### P1: Reuse the two-route split, with the dispatch route going to the provider's own upstream

- **Decision**: For each provider operation that carries model-failover, `transformProvider` emits the same front operation and dispatch operation as a proxy (header-matched on the chain token). The dispatch operation keeps the provider's normal routing: fixed cluster = the provider's main upstream, and the provider's `RegexRewrite`. The dispatch role selects **no** upstream (new internal param `_routeToTarget: false`), so the kernel applies the route's default upstream.
- **Rationale**: Every piece already exists: the Envoy retry on the front route, per-attempt entry into the internal listener, and the dispatch role's model rewrite. Provider mode is proxy mode with one upstream for every target.
- **Alternatives rejected**: Per-target loopback upstreams pointing at the provider's own route (would re-enter the front route and loop). Rewriting the model on the front route (Envoy replays the front's copy, so every attempt would carry the same model).

### P2: Targets are models; the controller fills in the provider

- **Decision**: On a provider, `failover.ParseSettingsFor(params, providerName)` accepts targets with only `model`. A `provider` equal to the provider's own name is accepted; any other name is rejected ("attach it to an LlmProxy"). The controller rewrites `params.targets` with `provider = <own name>` before building the instances, so the policy keeps one target shape (provider + model) for health keys and logs, unchanged.
- **Rationale**: No change to the policy's parser or health keying. The definition's `targets[].provider` becomes optional, documented as required on proxies.

### P3: What runs once and what runs per attempt

- **Decision**: The front chain keeps every provider policy: user policies, access control, token and cost accounting. The front model-failover instance joins them. The dispatch chain carries model-failover [dispatch] followed by the provider's upstream-auth policy. The front operation no longer gets the upstream-auth policy.
- **Rationale**: FR-007 (accounting runs once) and symmetry with proxy mode, where credentials sit on the dispatch route. Credentials on the front would also reach the upstream through Envoy's replay, but keeping them on the dispatch route keeps the rule "the front route holds no upstream specifics" true in both modes.

### P4: Every template model location (revised)

- **Decision**: The dispatch role reads the template's `requestModel`, which the controller already merges into provider-attached policy params, and writes each attempt's model at that location. This is the same approach model-round-robin takes:
  - `payload`: a JSONPath into the body; the common `$.model` keeps every other member's exact encoding;
  - `header`: the header is set;
  - `queryParam`: the parameter is replaced;
  - `pathParam`: the first capture group of the pattern is replaced, with the model path-escaped.

  The last three happen in the request-header phase, so no body is buffered for them. The controller rejects an unknown location or a pattern without a capture group. It also rejects a path-located model on an operation whose own path fixes the model, since the rewritten request would stop matching the dispatch route when the route cache is cleared. A wildcard such as `/models/*` is required.
- **Rationale**: The first version allowed only `{payload, $.model}`, on the grounds that a path rewrite was missing. But the dispatch role runs once per attempt, before the router, and a policy path mutation is exactly what model-round-robin already uses. Gemini and Bedrock providers need nothing extra.
- **Scope**: This applies on an LlmProvider only. On an LlmProxy, native targets speak the client's OpenAI format, so the model stays the top-level body `model`.

### P5: Keeping the hop secret off the provider

- **Problem**: In proxy mode the dispatch hop sends `x-wso2-failover-hop` to the provider route on the client-facing listener, whose hop Lua filter strips it before the real provider. In provider mode the dispatch hop **is** the last hop: its router forwards straight to the real provider, so the header would leak (FR-009). Its transport failures are also local replies on the **internal** listener, whose mapper matches the header, and the header must be gone before forwarding.
- **Decision**:
  - Add the same `wso2.failover.hop` Lua filter to the internal listener (header → dynamic metadata, then strip), and switch the internal listener's local-reply mapper to the metadata match. This is the same config as the main listener.
  - Dispatch routes in **proxy** mode disable that filter per route (`LuaPerRoute{disabled: true}`), so the header still travels to the provider hop, where the main listener strips it.
  - The RDC route carries `RouteFailover.SameUpstream` to choose between the two.
- **Rationale**: One mechanism (Lua then metadata mapper) on both listeners, with a per-route switch only where the hop continues. No change to the policy.
- **Spike S6: done** (2026-09-27, local Envoy 1.36.4, `spikes/s6-lua-per-route.yaml`):
  - a route with `LuaPerRoute{disabled:true}` forwarded `x-wso2-failover-hop: s3cret` to its backend;
  - a route with the filter on stripped it (the backend saw none);
  - that route's connect failure came back `503` with `x-wso2-upstream-failure: UF`;
  - without the secret, no label was added.

### P6: Validation at registration

- **Decision**: `validateProviderSpec` calls a provider variant of `validateModelFailover`. It checks `failover.ValidateParamsFor(params, <provider name>)` for each attachment, at most one global attachment, and no model- or provider-selecting policy on the same provider (the same list as proxy mode). The template rule (P4) runs in the transform, which also returns `400`, because the validator has no template access.

### P7: Proxy over a provider with failover

- **Decision**: Allowed. The proxy's dispatch hop loops back into the provider's route. That route is a provider front route with its own retry, so it walks the provider's models. Attempts multiply (proxy targets × provider models), and the docs say so.
- **Rationale**: Nothing breaks. The proxy's plan and the provider's plan have different chain tokens and nonces. Rejecting it would need a cross-resource check at every registration.

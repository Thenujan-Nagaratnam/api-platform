# Research: Failover Chains Keyed by the Requested Model

## Context found in the code

| Topic | Finding | Where |
|---|---|---|
| Plan creation | The front role builds the plan in the request-header phase from every target (`health.admit(cfg)`), and hands its nonce to dispatch in `x-wso2-failover-plan`. | `POL/front.go` |
| Body-phase actions | `UpstreamRequestModifications` (body phase) can set headers and return `ImmediateResponse`, so a plan can be finished once the body has been read. | `sdk/core/policy/v1alpha2/action.go:125` |
| Health keys | `chainID + provider + model`, so a target listed in two chains of one operation already shares health. | `POL/health.go:89` |
| Proxy primary | `NormaliseLLMProxyAttachments` puts the primary first (`IsPrimary`); a request without failover goes to it. | `pkg/models/llm_proxy_attachments.go`, `transformProxy` |
| Retry count | `num_retries` is fixed per route. Dispatch already answers "plan exhausted" when a plan is shorter, and the front turns that into the exhaustion response. | `POL/dispatch.go`, `pkg/failover` `Settings.NumRetries` |
| Selector conflict | Selecting policies anywhere on the resource block model-failover, even on other operations. | `llm_validator_failover.go` `failoverAttachments` |
| Policy order | API-level policies run before operation policies. A global model-failover's front instance is prepended to the operation's policies. | `transform/llm_failover_test.go`; `buildFailover` |

## Decisions

### R1: One flattened target list plus chains of indices

- **Decision**: The policy parses `chains` into `Config.Targets`, a list of distinct (provider, model) pairs built by walking the chains in order (primary, then fallbacks) and skipping pairs already seen. `Config.Chains` maps each primary model to its ordered list of target indices. The controller flattens the same way, so `_targetIds`, `_targetNative` and the per-target upstreams line up index for index.
- **Rationale**: Health, target IDs, loopback upstreams, transformers and credentials all stay per target, unchanged. Deduplication keeps Envoy clusters bounded when a model appears in several chains.
- **Limit**: 50 distinct targets per attachment. 20 chains × 10 models is 200 before deduplication, and each target is a cluster.

### R2: The controller resolves providers

- **Decision**: On a proxy, the controller writes the primary provider's attachment name into every primary and into every fallback without a provider. On a provider, it writes the provider's own name. The authored `primary.provider` is rejected at registration. The runtime policy accepts it, because only the controller can set it.
- **Rationale**: Only the controller knows the proxy's primary. The policy stays mode-agnostic.

### R3: Plan in two steps; a pass-through plan by default

- **Decision**: The front role's header phase always creates a plan and sets its nonce, as today. The plan starts as **pass-through**: one attempt at the pass-through target. The chain is chosen when the model is known:
  - **Header, query or path model** (provider mode): in the same header phase.
  - **Body model**: in the body phase, which retargets the same nonce to the chain's admitted targets (`planRegistry.retarget`). The front role buffers the body only when the model lives there.

  If every target of the matched chain is suspended, the body or header phase returns the exhaustion response.
- **Rationale**: A request whose body never arrives, or can't be parsed, still carries a valid plan and simply passes through. Dispatch always finds a plan.
- **Alternative rejected**: Creating the plan only in the body phase. A body-less request would reach dispatch without a plan and get a 500.

### R4: The pass-through target

- **Decision**: The controller and the policy each append one more target after every chain target: the primary's provider with an empty model. It is always last, so no extra parameter is needed. Dispatch for a pass-through plan:
  - routes to that target, a loopback to the primary provider on a proxy, or the route's own upstream on a provider;
  - does not rewrite the model;
  - does not tag the response;
  - does not record health.

  A transformer on the proxy's primary gets no `model` override for this target. If Envoy abandons the single attempt on its per-attempt timeout and retries, dispatch answers an untagged `504`, so the client gets a timeout rather than the exhaustion response.
- **Rationale**: The request goes exactly where it would have gone without the policy, with its own model. The upstream's answer, even a 429, reaches the client unchanged because nothing tags it for retry.

### R5: Reading the model

- **Decision**: The model is read with the same location rules 002 writes with: a body JSONPath (`sdk/core/utils.ExtractStringValueFromJsonpath`), a header, a query parameter, or a path capture group (unescaped). On a proxy it is always `$.model`. A missing or non-string value means pass-through.

### R6: Model selectors on the same operation

- **Decision**: The conflict check becomes per operation (method + path). On the same operation, `model-round-robin` and `model-weighted-round-robin` are allowed when no entry names a provider and they come before model-failover. Anything else is rejected. A global model-failover's front instance is inserted after the operation's last model selector, instead of being prepended, so round-robin still runs first.
- **Rationale**: Round-robin then writes the primary model on the front route, and the front role reads it. Envoy retries replay the front's modified request, and dispatch rewrites only on fallback attempts (the primary attempt writes the same model back). Provider-switching selectors would still fight the dispatch route's upstream choice.

### R7: Retry budget

- **Decision**: `num_retries` = longest chain − 1, and the route timeout is based on the longest chain. Shorter chains stop through plan exhaustion (existing behaviour). A pass-through plan never tags, so it never retries.

### R8: `targets` removed

- **Decision**: `targets` is rejected with "use chains". 001 and 002 haven't been released, so no compatibility shim is needed.

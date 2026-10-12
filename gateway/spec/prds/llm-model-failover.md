# LLM Model Failover — Design

## What is the problem we are trying to solve? And why should it be solved now?

An application calling an LLM through the gateway asks for one model. When that provider has an outage, rate-limits (429) or returns 5xx, the application gets the error, and every following request hits the same failing provider. Today each application must build its own retries across providers, each with different APIs, keys and payload formats.

**Why now:** Equivalent Classic Gateway feature, and comparable AI gateways already offer cross-provider fallback.

- Slides: https://claude.ai/artifact/XjAevYyCY8Cd4ELmTqaCLu#slide-c183a42ed7b4
- Diagram: https://excalidraw.com/#json=OWfXrYHTPCYZzqqYYh-kM,2vDyGWPbFOIxN8etFlaA5A

## Who are we solving the problem for

- **API publishers / AI platform teams** who configure an LlmProxy and want declarative fallback chains.
- **Application developers** who want one reliable model endpoint without client-side retry logic.
- **Platform operators** who need failing providers taken out of rotation automatically, and visibility into it.

## Solution

A `model-failover` policy on an LlmProxy operation declares, per requested model, an ordered chain of `{model, provider}` members. Within a request, Envoy retries the next member on a qualifying failure. Across requests, the policy suspends members that keep failing and routes new requests past them.

### Configuration

```yaml
- name: model-failover
  version: v0
  params:
    targets:
      - model: gpt-4o
        provider: openai
        fallbacks:
          - model: claude-sonnet
            provider: anthropic
          - model: gpt-4o
            provider: azure-openai
    statusCodes: [429, 500, 502, 503]
    suspendDuration: 30
    suspendAfterFailures: 2
    maxSuspendMultiplier: 8
    recoverAfterSuccesses: 3
    probeTimeout: 60
    perTryTimeout: 20
```

| Param | Default | Meaning |
|---|---|---|
| `targets` | required | One chain per client model; `"*"` catches any model without an exact entry. Max 10 members. |
| `statusCodes` | any 5xx | Codes that fail over and count as failures (400–599). Replaces the default. |
| `suspendDuration` | 0 (off) | Seconds: a failing member is skipped. |
| `suspendAfterFailures` | 1 | Consecutive failures before suspending. |
| `maxSuspendMultiplier` | 8 | Cap on window doubling for repeat trips. |
| `recoverAfterSuccesses` | 3 | Successful probes in a row that restore a suspended member. |
| `probeTimeout` | 60 | Seconds a probe may go without a result before it counts as failed. |
| `perTryTimeout` | route timeout | Seconds per attempt; × chain length must fit the route timeout. |

- **Provider defaults:** a target without `provider` uses the proxy's primary; a fallback without `provider` uses its target's.
- **Wildcard:** a `"*"` member keeps the client's model; an exact entry always wins.
- **Rejected at deploy:** unknown provider, target with no fallback, duplicate model or member, chain over 10, bad status codes, `perTryTimeout` × chain over the route timeout, any parameter not listed above (at the top level, on a target or on a fallback), and model-failover attached to an upstream's `policies` list.

### Per-provider upstream policies

Each provider on the LlmProxy has its own `policies` list. Those policies run once per attempt, and only on attempts that go to that provider, including a failover retry that lands there.

```yaml
spec:
  provider:
    id: openai
    policies:
      - name: set-headers
        version: v0
        params: { ... }
  additionalProviders:
    - id: anthropic
      policies:
        - name: pii-masking-regex
          version: v0
          params: { ... }
```

- Authors attach upstream policies only through these lists. There is no per-operation upstream flag.
- Policies run in the order listed, after model-failover. The provider's transformer and auth always run after them.
- model-failover itself is attached once, in the operation policies, never in a provider's list. The controller adds its per-attempt copy automatically, ahead of every provider's list. It must run on every attempt to set `selected_provider`, which is what the provider lists are gated on. The validator rejects it in a provider's list.

### Deploy time

The gateway controller validates the chains and compiles them into Envoy and policy-engine config.

- **Clusters:** one leaf cluster per member, one `envoy.clusters.composite` per chain, and one suffix composite per fallback position (linear in chain length).
- **Retry policy:** `retry_on` = 5xx (or the exact `statusCodes`) + `connect-failure`, `reset`, `reset-before-request`; per-try timeout; 1024 concurrent retries per leaf.
- **Policy chains:**
  - **Downstream:** model-failover.
  - **Upstream (per attempt):** model-failover first. Then each provider gets its own list, gated on `selected_provider`, built in this order:
    1. the author's `policies` for that provider
    2. that provider's transformer
    3. that provider's auth

  The controller adds the transformer and auth at deploy time, so the provider key is always set last and no author policy sees it.

**Why a composite cluster:** a route's cluster is chosen once per request, so a plain retry re-dials the same failing backend. The composite routes attempt N to member N. Our policies can't re-route a retry: the downstream phase runs once, and the upstream phase runs after the cluster is chosen.

### Invocation time

| Who | Decides |
|---|---|
| Downstream policy | Which composite to enter (full chain or a suffix) and the retry budget, based on circuit state. |
| Router | Whether to retry (`retry_on`, retries left). |
| Composite | Which leaf: attempt N → member N. |
| Upstream policies | That attempt's model, path, credentials and payload shape; record the outcome. |

1. OpenAI returns 503; Envoy retries the next member and Anthropic answers in the same request. The client sees one 200.
2. After 2 failures in a row OpenAI is suspended.
3. The downstream policy enters the suffix composite starting at Anthropic, so OpenAI is not called; Azure is still one retry away.
4. If every member is suspended, the policy returns `503 failover_exhausted` immediately.

### Circuit breaker

- **Open:** after `suspendAfterFailures` consecutive failures; skipped for the backoff window.
- **Half-open:** probes are real requests, one at a time; others still skip. `recoverAfterSuccesses` successes (default 3) close it; a failed probe, or no result within `probeTimeout` (default 60 s), reopens it.
- **State:** per member (model + provider), in memory per replica, shared by the downstream and upstream policy instances; evicted after 24 h with no traffic or redeploy.

### Upstream ext_proc and existing policies

Existing policies run per attempt unchanged. The engine calls the same `On*` methods with `Downstream == nil`. Request steps run in chain order and response steps in reverse. Each attempt starts from the original request.

Upstream policies are attached per upstream, not per operation:

- **LlmProxy:** `provider.policies` and `additionalProviders[].policies`.
- **LlmProvider:** `upstream.policies`. The controller appends the provider's auth to this list.
- **RestApi:** `upstream.main.policies` and `upstream.sandbox.policies`.

> **Exception:** when an LlmProxy has more than one provider and no model-failover, the transformer and auth stay downstream. Without model-failover, nothing tells the upstream phase which provider an attempt is going to. The primary's gate would then match every attempt and send its key to the other providers.

## API and data changes

- **Policy schema:** new `model-failover` policy definition (params above).
- **Management API:** a new `policies` list on each upstream:
  - LlmProxy: `provider` and `additionalProviders[]`
  - LlmProvider: `upstream`
  - RestApi: `upstream.main` and `upstream.sandbox`

  `OperationPolicy.upstream` is removed. The validator rejects `upstream: true` on any policy an author writes, because the controller uses that flag internally.
- **Persistent data:** none — circuit state is in memory.

## Alternatives considered

| Alternative | Why not |
|---|---|
| Aggregate cluster + `previous_priorities` | Health/priority based; Envoy documents the plugin as incompatible with aggregate clusters. |
| Envoy outlier detection for suspension | Disproven live: the composite still sends attempt 1 to an ejected leaf → 503 "no healthy upstream", not retryable (envoy#11307). |
| Bypass to one fallback's leaf | Single shot; the rest of the chain is lost. |
| Composite per suspended subset | Exponential cluster count. |
| Custom Envoy extension | Would allow mid-chain skip, but a new data-plane component to own. |
| Rolling failure-rate and p95 latency rules | Rate rule needs 20+ attempts/min per replica; latency tracks prompt size, not health. Removed. |

## Surprises

- The composite cluster ignores health entirely; skipping is the policy's job.
- ext_proc is not called for Envoy's local replies, so connection failures retry but never suspend.
- The upstream filter must sit on the composite cluster, not the leaves, or it never fires.
- The upstream filter needs system-header mutation (`:path`) and a buffered response body for transforms.

## The challenges and how the proposed solutions handle them

| Area | How it is handled |
|---|---|
| Security | Only the selected provider's key is applied on each attempt, as the last policy in that provider's list. No author policy sees the key, and no provider key is sent downstream. The upstream filter fails closed. The internal provider header is removed before the response reaches the client. |
| Control/data plane | Controller compiles clusters, retry policy and policy params over xDS; cluster names are injected, never guessed. |
| User-provided code | None. |
| Cloud cost | Retries add provider spend; bounded by chain length and the per-leaf retry breaker. Suspension avoids paying for known-bad attempts. |
| Architecture limits | No mid-chain skip; transport failures don't suspend; state is per replica; no SDK metrics hook (logs and analytics only). |
| Performance | Request body is buffered (model lookup and replay); 25 ms retry back-off; failed attempts add latency only on failure. |
| Trade-off | Deterministic order over health-aware selection; local per-replica state over a shared store. |
| Scale | Replicas need no coordination; clusters grow linearly with chain length. |

## Appendix A — Example LlmProxy

A three-provider proxy with model-failover on `/chat/completions`. Uses the `providers[]` shape from `main`, where each provider's logical name is `alias`.

```yaml
apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProxy
metadata:
  name: my-llm-proxy
spec:
  displayName: My LLM Proxy
  version: v1.0
  context: /my-llm-proxy
  inboundTemplate: openai
  providers:
    - id: openai-primary
      isPrimary: true
      auth:
        type: api-key
        header: Authorization
        value: 'Bearer {{ secret "openai-api-key" }}'
    - id: anthropic-provider
      alias: anthropic
      auth:
        type: api-key
        header: x-api-key
        value: '{{ secret "anthropic-api-key" }}'
      transformer:                      # runs after this provider's policies, before its auth
        type: openai-to-anthropic-transformer
        version: v0
        params:
          model: claude-sonnet-4-5-20250929
    - id: azure-openai-provider
      alias: azure-openai
      auth:
        type: api-key
        header: api-key
        value: '{{ secret "azure-openai-key" }}'
  # Once per client request (downstream); the controller adds the per-attempt copy itself
  operationPolicies:
    - name: model-failover
      version: v0
      paths:
        - path: /chat/completions
          methods: [POST]
          params:
            targets:
              - model: gpt-4o
                fallbacks:
                  - model: gpt-4o-mini
                  - model: claude-3-5-sonnet-20241022
                    provider: anthropic
              - model: gpt-4.1
                fallbacks:
                  - model: gpt-4.1
                    provider: azure-openai
              - model: "*"
                fallbacks:
                  - model: "*"
                    provider: azure-openai
            statusCodes: [429, 500, 502, 503, 504]
            suspendDuration: 30
            maxSuspendMultiplier: 8
            suspendAfterFailures: 3
            perTryTimeout: 15
```

- Targets without `provider` (`gpt-4o`, `gpt-4.1`, `"*"`) run on the primary, `openai-primary`.
- `gpt-4o-mini` has no `provider`, so it inherits its target's provider (the primary).
- The longest chain has 3 members: 3 × 15 s = 45 s must fit inside the route timeout (`spec.resilience`).

## Appendix B — model-failover policy definition

`gateway/dev-policies/model-failover/policy-definition.yaml`:

```yaml
name: model-failover
version: v0.1.0
description: |
  Retries a failed LLM request against a declared fallback chain of
  {model, provider} targets on upstream failure, with correct per-attempt
  credential injection and payload translation.

parameters:
  type: object
  properties:
    targets:
      type: array
      x-wso2-policy-advanced-param: false
      description: |
        Ordered list of client-requested-model failover chains. Each entry's
        own model/provider are authored at the same level as fallbacks[]
        entries use.
      items:
        type: object
        properties:
          model:
            type: string
            description: |
              The client-requested model this chain matches. "*" makes this
              the catch-all chain for any model no other entry names (an exact
              entry always wins). A "*" member passes the client's own model
              through unchanged, and one circuit covers every model it serves.
            minLength: 1
          provider:
            type: string
            description: |
              Optionally names the provider this target's own request is sent
              to: a provider's alias (its id when no alias is set) on the
              LlmProxy. Omitted, defaults to the proxy's primary provider.
            minLength: 1
          fallbacks:
            type: array
            description: Ordered fallback chain tried in sequence after the target fails.
            items:
              type: object
              properties:
                model:
                  type: string
                  description: |
                    The model to request from this fallback. "*" keeps the
                    client's requested model, e.g. to fail over to the same
                    model on another provider.
                  minLength: 1
                provider:
                  type: string
                  description: |
                    Names the provider this fallback is sent to: a
                    provider's alias (its id when no alias is set) on the
                    LlmProxy. Omitted, inherits this chain's target provider
                    — name the primary provider explicitly to fail over to it
                    from a target on another provider.
                  minLength: 1
              required:
                - model
        required:
          - model
      minItems: 1
    statusCodes:
      type: array
      x-wso2-policy-advanced-param: true
      description: |
        Response status codes that trigger escalation to the next chain
        member, and count as this target's failure for suspension purposes.
        Omitted, defaults to any 5xx. When set, this REPLACES that default
        entirely rather than extending it — list every code that should
        trigger failover, 5xx codes included if still wanted.
      items:
        type: integer
        minimum: 400
        maximum: 599
      minItems: 1
    suspendDuration:
      type: integer
      x-wso2-policy-advanced-param: true
      description: |
        Seconds a failing target is skipped for. If it fails again as soon as
        it comes back, the window doubles each time, up to maxSuspendMultiplier
        times this value. When a window ends, probe requests are sent to the
        target one at a time: recoverAfterSuccesses successes in a row
        restore it, any failure suspends it again. Set to 0 to disable suspension tracking entirely.
      minimum: 0
      default: 0
    suspendAfterFailures:
      type: integer
      x-wso2-policy-advanced-param: true
      description: |
        Number of CONSECUTIVE qualifying failures (per statusCodes) required
        before a target is suspended. A success resets the count. Omitted or
        1 suspends on the very first qualifying failure.
      minimum: 1
      default: 1
    maxSuspendMultiplier:
      type: integer
      x-wso2-policy-advanced-param: true
      description: |
        Caps how far suspendDuration doubles for a target that keeps failing
        right after it comes back, as a multiple of suspendDuration (8 with
        suspendDuration 30 caps the window at 240 seconds). 1 disables the
        doubling.
      minimum: 1
      default: 8
    recoverAfterSuccesses:
      type: integer
      x-wso2-policy-advanced-param: true
      description: |
        Successful probe requests in a row needed to restore a suspended
        target once its window ends. Probes are sent one at a time; any
        failed probe suspends the target again.
      minimum: 1
      default: 3
    probeTimeout:
      type: integer
      x-wso2-policy-advanced-param: true
      description: |
        Seconds a probe request may go without a result (for example a
        connection failure the policy never sees) before it counts as failed
        and the target is suspended again.
      minimum: 1
      default: 60
    perTryTimeout:
      type: integer
      x-wso2-policy-advanced-param: true
      description: |
        Seconds each individual chain-member attempt may take, so a hung
        target fails over instead of consuming the whole request. perTryTimeout
        x chain length must fit inside the route's overall timeout. Omitted or 0
        leaves the route's overall timeout in effect.
      minimum: 0
  required:
    - targets

  # Note: no requestModel property here. It is never author-configured — like
  # every other LLM-aware policy (e.g. model-round-robin), it's extracted from
  # the LLM provider template and merged into this policy's params
  # automatically by gateway-controller's llm_transformer.go
  # (buildTemplateParams/mergeParams), the same place primaryProvider/
  # operationPath are injected rather than authored. The runtime policy
  # (modelfailover.go) parses and defaults it from its own raw params map.

systemParameters:
  type: object
  properties: {}
```

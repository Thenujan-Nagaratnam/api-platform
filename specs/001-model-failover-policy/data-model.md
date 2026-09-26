# Data Model: Model Failover Policy

**Feature**: [spec.md](./spec.md) | **Research**: [research.md](./research.md)

All state is held **in memory in the policy engine**. There are no database tables and no schema changes. Configuration lives in the existing LlmProxy policy attachment, which is stored the same way as every other policy's params.

## 1. FailoverPolicyConfig (authored by the publisher)

This is attached to an LlmProxy (at API or operation level) as policy `model-failover`. The full schema is in [contracts/policy-definition.yaml](./contracts/policy-definition.yaml).

| Field | Type | Default | Validation |
|---|---|---|---|
| `targets[]` | list of `Target` | — | 1–10 entries. No duplicate `(provider, model)` pairs (FR-003). |
| `failoverOn.statusCodes` | int[] | `[429,500,502,503,504]` | Each value is `429` or `500`–`599`. Unique. |
| `failoverOn.connectFailure` | bool | `true` | — |
| `failoverOn.reset` | bool | `true` | — |
| `failoverOn.timeout` | bool | `true` | When `false`, a per-attempt timeout is final and returns the exhaustion response. |
| `perAttemptTimeout` | duration | `30s` | `1s`–`300s` |
| `suspendAfterConsecutiveFailures` | int | `3` | `1`–`100` |
| `suspendDuration` | duration | `30s` | `1s`–`3600s` |
| `probeConcurrency` | int | `1` | `1`–`10` |
| `recoverAfterSuccessfulProbes` | int | `2` | `1`–`20` |

### Target

| Field | Type | Validation |
|---|---|---|
| `provider` | string | Handle of an LlmProvider that is either the proxy's `provider` or listed in its `additionalProviders`. Its template must map to a supported format: OpenAI-native, or one that has a transformer (Azure OpenAI, Anthropic, Bedrock, Gemini, Mistral) (FR-005b/c). |
| `model` | string | Non-empty, 1–256 characters. For OpenAI-native targets it replaces the body's `model`. For transformer targets it is passed as the transformer's `model` param. |

**Derived by the controller (not authored)**:
- `targetId`: `t<index>`, which is stable for a given config revision.
- `chainId`: `<proxyId>:<routeKey>`.
- A per-target loopback upstream named `failover-<targetId>`.

### Cross-policy validation (controller, when the policy is configured)

The configuration is rejected if:
- the same route also has another provider-selecting policy (`llm-header-router`, `model-round-robin`, `model-weighted-round-robin`, `intelligent-model-routing`, `cost-based-model-routing`), because their `selected_provider` writes would conflict with dispatch;
- `model-failover` is attached more than once to the same route;
- the authored params contain any `_`-prefixed (controller-internal) key.

This validation runs **synchronously at registration**, not only in the async xDS transform (research R11).

## 2. AttemptPlan (created per request, in memory)

Created by the front hop in the request phase and looked up by the dispatch hop.

| Field | Type | Notes |
|---|---|---|
| `nonce` | 128-bit random (hex) | Registry key. Travels in `x-wso2-failover-plan`. |
| `chainId` | string | Must match the dispatch route's chain. A mismatch is rejected. |
| `targets` | `[]targetId` | Configured order, with suspended targets removed. Probing targets are included only if a probe slot was claimed. |
| `probeClaims` | set of targetId | Released when the outcome arrives or the plan expires. |
| `cursor` | atomic int | Advanced once per dispatch arrival. |
| `outcomes` | `map[index]Outcome` | Used for timeout inference (R7) and logs. |
| `expiresAt` | time | `front route timeout + 5s`. A sweeper releases the claims. |

**Lifecycle**: `created (front OnRequestHeaders)` → `advanced ×k (dispatch arrivals)` → `closed (front OnResponseHeaders final)` or `expired (sweeper)`.

## 3. TargetHealth (per `(chainId, targetId)`, in memory, per gateway instance)

| Field | Type |
|---|---|
| `state` | `healthy` \| `suspended` \| `probing` |
| `consecutiveFailures` | int |
| `suspendedUntil` | time |
| `probeSuccesses` | int |
| `probesInFlight` | int (bounded by `probeConcurrency`) |

### State transitions

```text
healthy   --(eligible failure; consecutiveFailures reaches N)--> suspended  [suspendedUntil = now + suspendDuration]
healthy   --(success or non-eligible response)-----------------> healthy    [consecutiveFailures = 0]
suspended --(now >= suspendedUntil, at plan build)-------------> probing    [probeSuccesses = 0]
probing   --(probe success; probeSuccesses reaches M)----------> healthy    [consecutiveFailures = 0]
probing   --(probe success; probeSuccesses < M)----------------> probing
probing   --(probe failure)------------------------------------> suspended  [new suspendDuration]
```

- **Outcome classes**:
  - `success`: 2xx.
  - `non_eligible`: any status not in `statusCodes`, or a disabled transport reason. Counted as healthy.
  - `eligible_failure`: carries a reason, one of `status_<code>`, `connect_failure`, `reset`, `timeout`.
- **Config reload**: a new config revision for the chain resets health for any target whose `(provider, model)` changed. Unchanged targets keep their state.

## 4. Outcome (per attempt)

| Field | Values |
|---|---|
| `targetId` | — |
| `position` | 0-based index within the plan |
| `class` | `success` \| `non_eligible` \| `eligible_failure` |
| `reason` | `status_<code>` \| `connect_failure` \| `reset` \| `timeout` \| empty |
| `probe` | bool |
| `latency` | duration |

## 5. Internal hop headers

These are internal to the gateway. The contract is in [contracts/internal-hop-headers.md](./contracts/internal-hop-headers.md).

| Header | Direction | Purpose |
|---|---|---|
| `x-wso2-failover-chain` | front → dispatch | Selects the dispatch route. |
| `x-wso2-failover-plan` | front → dispatch | Attempt plan nonce. |
| `x-wso2-failover-hop` | dispatch → loopback provider | Per-boot secret that enables the local-reply failure flags. Stripped into dynamic metadata on the provider hop, never forwarded upstream. |
| `x-wso2-upstream-failure` | loopback provider → dispatch | Envoy response flags on local replies. |
| `x-wso2-failover-retry` | dispatch → front | Marks an eligible failure. Envoy retries on it. |
| `x-wso2-failover-exhausted` | dispatch → front | The plan cursor ran past the end. |

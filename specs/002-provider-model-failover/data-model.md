# Data Model: Model Failover on LLM Providers

Extends [001 data model](../001-model-failover-policy/data-model.md). Only the differences are listed.

## FailoverPolicyConfig on an LlmProvider

| Field | Change from proxy mode |
|---|---|
| `targets[].provider` | Optional. Omitted or equal to the provider's own name. Any other value is rejected. The controller fills in the provider's name before building the policy instances. |
| `targets[].model` | Required, as before. Duplicates are rejected, as before. |
| every other field | Unchanged: same defaults and bounds. |

Validation (registration, `400`):
- the proxy rules (bounds, duplicates, internal keys, at most one global attachment);
- no other model- or provider-selecting policy on the provider;
- the template's `requestModel` must be `payload` at `$.model` (checked in the transform).

## Controller-internal params

| Param | Proxy mode | Provider mode |
|---|---|---|
| `_role`, `_chainId`, `_targetIds`, `_hopSecret` | as 001 | as 001 |
| `_targetNative` | per attachment | always `true` |
| `_routeToTarget` (new) | `true` (default when absent) | `false`: the dispatch role selects no upstream and sets no `selected_provider` |

## RouteFailover (RDC)

| Field | Meaning |
|---|---|
| `SameUpstream` (new) | `true` on a provider-mode dispatch route: its router forwards to the real provider, so the internal listener's hop filter must strip the secret. `false` on proxy-mode dispatch routes, which disable that filter per route so the secret reaches the provider hop. |

Target health, attempt plans and outcomes are unchanged. A target's health key is still chain + provider + model, with provider being the provider's own name.

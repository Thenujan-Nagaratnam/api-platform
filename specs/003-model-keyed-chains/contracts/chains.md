# Contract: `chains` Parameter and Request Handling

## Registration

| Configuration | Result |
|---|---|
| Two chains, distinct primaries, 1–9 fallbacks each | `201` |
| `targets: [...]` | `400` `targets was replaced by chains; list a primary model and its fallbacks` |
| Duplicate primary `gpt-4o` | `400` `chains[1].primary.model: "gpt-4o" already has a chain (chains[0])` |
| `primary.provider` set | `400` `chains[0].primary.provider: the primary is the requested model on the provider the request is routed to; name providers on fallbacks` |
| No fallbacks, or more than 9 | `400` with the field path |
| More than 20 chains, or more than 50 distinct targets | `400` |
| Proxy: a fallback's provider is not attached | `400` `chains[i].fallbacks[j].provider "x" is not a provider attached to this proxy` |
| Provider: a fallback names another provider | `400` (002 message) |
| `model-round-robin` (no providers) before model-failover on the same operation | `201` |
| `model-round-robin` after model-failover on the same operation | `400` `model-round-robin must come before model-failover on the same operation` |
| `model-round-robin` with a `provider` entry, or `llm-header-router` etc., on the same operation | `400` (001 message) |
| Any selector on a different operation | `201` |

## Request handling

| Request model | Plan | Attempts | Client sees |
|---|---|---|---|
| Equals a chain's primary | That chain's available targets | 1 + fallbacks after eligible failures | The first success, a non-eligible reply as is, or the exhaustion response |
| Equals a chain's primary, all suspended | none | 0 | Exhaustion response |
| Matches no chain / unreadable | Pass-through | Exactly 1, model unchanged | The upstream's reply as is; `504` if it exceeds `perAttemptTimeout` |

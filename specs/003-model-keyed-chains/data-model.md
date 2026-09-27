# Data Model: Failover Chains Keyed by the Requested Model

## Authored parameters (policy-definition.yaml)

```yaml
chains:                    # required, 1–20
  - primary:               # required
      model: gpt-4o        # required, 1–256 chars; unique across chains
    fallbacks:             # required, 1–9
      - provider: anthropic   # optional; omitted = the primary's provider
        model: claude-sonnet-4-5
# failoverOn, perAttemptTimeout, suspendAfterConsecutiveFailures, suspendDuration,
# probeConcurrency, recoverAfterSuccessfulProbes: unchanged from 001
```

The following are rejected:
- `targets`, which was removed;
- `primary.provider`;
- a duplicate `primary.model`;
- the same (provider, model) twice in one chain;
- more than 50 distinct targets after deduplication.

## Controller-written parameters

| Key | Meaning |
|---|---|
| `chains[*].primary.provider`, `chains[*].fallbacks[*].provider` | Filled in (R2) |
| `_targetIds`, `_targetNative` | One entry per flattened target (R1), plus the pass-through target, which is always last (R4) |
| `_role`, `_chainId`, `_hopSecret`, `_routeToTarget`, `requestModel` | Unchanged from 001 and 002 |

## Policy runtime

- `Config.Targets []Target`: distinct (provider, model) pairs in flattening order, followed by the pass-through target (`Model == ""`).
- `Config.Chains map[string][]int`: primary model → ordered target indices.
- `Config.PassThrough int`: index of the pass-through target (always `len(Targets)-1`).
- `attemptPlan.passThrough bool`: dispatch neither rewrites, tags nor records.

## Flattening rule (controller and policy must agree)

```
seen := {}
for chain in chains (authored order):
  for (provider, model) in [primary] + fallbacks:
    if (provider, model) not in seen: append; seen += it
    chain.indices += index of (provider, model)
append pass-through target (primary provider of the proxy / own provider, model "")
```

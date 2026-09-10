# Model Failover Policy — Design

**Status**: Proposed
**Date**: 2026-09-03
**Author**: Thenujan (with Claude)

## 1. Problem

LLM upstreams fail in ways that are routine, not exceptional: rate limits, transient
5xx, regional outages, provider incidents. Today the gateway has no automatic
recovery from any of these for LLM traffic — a failed request just fails. This
document proposes a **model failover policy** that automatically retries a failed
LLM request against configured alternatives before giving up, covering three
distinct tiers of "alternative":

1. **Same backend** — a different model on the same provider/credential
   (e.g. `gpt-4o` → `gpt-4o-mini`).
2. **Same provider, different backend** — a backup region or deployment of the
   same provider (e.g. primary region → backup region, same provider shape and
   credential type, different dial target).
3. **Cross-provider** — a different provider entirely (e.g. OpenAI → Anthropic),
   with a different request/response shape and a different, independent credential.

## 2. Scope

### In scope (v1)

- Automatic failover across all three tiers, unified under one policy and one
  configuration model — an operator can freely mix tiers within a single
  fallback list.
- Non-streaming requests only.
- Configurable failure-trigger status codes. No hardcoded default set — what
  counts as "retriable" varies materially by provider (see Appendix A), so the
  operator/API author supplies the exact set per policy instance.
- Fully transparent behavior to the client: exactly one final response body,
  with no failover-state leakage into the response contract (no synthesized
  headers, no altered response shape).
- Redial attempts are excluded from rate-limiting and analytics — the client's
  original request is counted once, regardless of how many internal attempts
  the gateway made on its behalf.
- Per-replica, in-memory circuit/suspend state. No new shared-store dependency
  introduced for v1.

### Out of scope (v1) — explicit future work

- Streaming/SSE failover. A streaming request either bypasses this policy or is
  limited to pre-first-byte failover (fail before any chunk reaches the
  client); mid-stream recovery is not attempted.
- Cross-replica shared circuit-breaker/suspend state.
- Active health-probing of targets. This policy is purely reactive
  (failure-triggered) — it does not proactively probe target health.

## 3. Current state

There is no committed model-failover implementation in this repository today.
An untracked, gitignored prototype exists locally at
`dev-policies/model-failover/` (~2,600 lines: policy logic, tests, README,
Postman e2e collection) implementing an application-level "self-redial"
mechanism. It has never been merged; `gateway-controller` carries no
awareness of it. A separate, older design — documented only in a now-stale
section of `oauth2-generator`'s `TESTING.md` — proposed an Envoy-native
retry mechanism (`RouteAction.RetryPolicy` against aggregate clusters,
`x-wso2-retry-trigger`). That design was implemented and then fully reverted
(commit `01fd59014`, "Decouple model-failover from gateway-controller
entirely") and no longer exists anywhere in current gateway-controller code.

Relevant existing infrastructure this design builds on, unchanged:

- **`llm-header-router`** and per-provider **translator** policies, which
  already conditionally transform a request into a target provider's native
  shape based on `SharedContext.Metadata['selected_provider']`.
- **`additionalProviders`** on an `LlmProxy`, each with its own `auth` config
  and its own conditionally-attached upstream-auth policy (`set-headers` for
  `api-key`, `oauth2-generator` for `oauth2`).
- The **`requestModel` systemParameter**, already injected into every
  operation policy's params by `gateway-controller`'s generic
  `buildTemplateParams`/`mergeParams` convention — no gateway-controller code
  needs to know about model-failover specifically for this to work.
- `Resilience{IdleTimeout, Timeout}` — route/stream timeouts only; no retry or
  circuit-breaker fields exist in `gateway-controller` config today.

## 4. Why not Envoy-native retry

The most obvious-looking alternative — configure Envoy's own retry policy
against an aggregate cluster listing each fallback as a member — does not
work for tiers 2–3 of this problem, for a structural reason:

**Envoy's retry logic lives in the Router filter, downstream of the ext_proc
filter chain.** Request-path filters (including the translator policies that
build a provider-specific payload) run once per client request. When the
Router filter retries against a different upstream member, it resends the
*already-built* request body — it does not re-invoke the request-path filter
chain. This means an Envoy-native retry can change *which host* receives the
retry, but not *what shape* the retried request has. Tier 1 (same shape, same
provider) would work; tiers 2–3, wherever the retried target expects a
different payload shape or a different credential, would not — the retried
request would arrive at the fallback still shaped for the original target.

This is almost certainly why the earlier Envoy-native design was reverted.
It's also why this document doesn't propose reviving it as anything more
than a partial mechanism (see Approach 3 below).

## 5. Approaches considered

### Approach 1 — Self-redial, refined (recommended)

On a triggering response, the policy re-dials the gateway's own loopback
listener (`selfBaseURL`, e.g. `http://127.0.0.1:8080`) with the original
request and a header identifying the next target to try. Because this is a
fresh request entering the gateway's normal request-path filter chain, every
per-target policy (translator, upstream-auth) that would normally run for
that target via `x-provider`/`additionalProviders` runs again, naturally,
producing the correct shape and credential for that specific fallback. A
recursion guard (`x-wso2-model-failover-redial`) marks a request as already
mid-failover so it isn't reprocessed as a fresh top-level request.

**Trade-offs**: each attempt costs a full additional Envoy round-trip
(latency multiplies with fallback depth); redial attempts must be explicitly
excluded from rate-limiting/analytics rather than that falling out for free;
circuit/suspend state is per-replica only. All three are addressed directly
in this design (§7–§9) rather than left as gaps.

**Why recommended**: it is the only one of the three approaches that
handles all three tiers with a single, uniform mechanism, and it evolves a
substantially-proven prototype (45 unit tests, an e2e Postman suite, mock
provider backends) rather than requiring new invention. The reuse of
existing conditionally-attached translator/auth policies means cross-provider
support requires zero new per-vendor adapter code in this policy — it is
exactly as capable as `additionalProviders` already is today, just
triggered automatically instead of by an `x-provider` header.

### Approach 2 — In-process dispatch

The failover policy calls translator/upstream-auth logic directly as
library functions and performs the outbound upstream HTTP call itself,
bypassing Envoy's router entirely for retry attempts — no loopback hop, no
recursion guard.

**Trade-offs**: today's translator/auth policies are built as
Envoy-attached, conditionally-triggered policies, not as a directly-callable
library — making them callable this way is a real structural change to how
the policy engine executes policies, not an incremental one. It would also
mean these retry attempts bypass whatever per-route observability Envoy
normally provides. Lower latency and a cleaner dispatch model, but
materially higher implementation risk and cost for this iteration.

### Approach 3 — Hybrid (Envoy retry + self-redial)

Use Envoy-native retry for tier 1 (same backend, no translation needed) and
self-redial only for tiers 2–3.

**Trade-offs**: reduces round-trip overhead for the tier-1 case specifically,
but introduces two dispatch mechanisms to build, test, and reason about. A
fallback list that mixes tiers (which this design explicitly wants to
support) would need a rule for which mechanism handles which hop —
additional operator-facing and implementation complexity for a latency
saving, not a capability gain. Not recommended for v1; worth revisiting only
if tier-1 redial latency proves to be a real-world problem.

### Decision

**Approach 1, refined**, per the trade-offs above.

## 6. Configuration schema

```yaml
targets:
  - model: "gpt-4o"                    # matched against the requestModel systemParameter
    provider: "openai-primary"         # OR upstreamDefinition — mutually exclusive
    fallbacks:
      - model: "gpt-4o-mini"           # tier 1: same backend
        provider: "openai-primary"
      - model: "gpt-4o"                # tier 2: same provider, backup region
        upstreamDefinition: "openai-backup-region"
      - model: "claude-3-5-sonnet"     # tier 3: cross-provider
        provider: "anthropic-primary"
  - model: "*"                         # optional catch-all target entry
    fallbacks: [...]

statusCodes: [429, 500, 503]           # required — no default, operator-supplied
requestTimeout: 10s                    # per-attempt timeout
suspendDuration: 30s                   # per-replica in-memory deprioritization window
maxResponseBytes: 10MiB
selfBaseURL: http://127.0.0.1:8080     # loopback target for redial
```

Each `targets[]` entry names the primary `{model, provider|upstreamDefinition}`
it covers, and carries an ordered `fallbacks[]` list where each entry is
itself a full `{model, provider|upstreamDefinition}` pair — a single fallback
list can freely span any of the three tiers, tried in the order listed.

## 7. Dispatch flow

1. The request routes normally to its primary target — no change to existing
   routing.
2. The model-failover policy runs on the **response phase**. If the response
   status is in the configured `statusCodes`, it matches the current
   `requestModel` against `targets[].model` and selects the next untried
   `fallbacks[]` entry (skipping any entry currently suspended, §9).
3. To dispatch that fallback, the policy issues a new HTTP request to
   `selfBaseURL` plus the original downstream path, carrying:
   - The original request body/headers unchanged, so downstream auth
     (`api-key-auth`/`jwt-auth`) still passes on the redialed request.
   - A header naming the selected fallback, read by an early request-phase
     hook so the rest of the chain treats it exactly like a request destined
     for that provider/model — this is what makes tier-3 translation work:
     the same conditionally-attached translator/upstream-auth policies that
     already serve `additionalProviders` today run for the redial, just
     selected via this header instead of via `x-provider`.
   - The recursion guard `x-wso2-model-failover-redial`, kept distinct from
     gateway-controller's own `x-wso2-internal-loopback` marker so a
     mid-failover request isn't conflated with unrelated loopback-detection
     logic.
4. Repeat until a non-triggering response is returned, the fallback list is
   exhausted, or `requestTimeout`/`maxResponseBytes` is hit on an attempt.
   The final response — success or the last failure's actual response — is
   what reaches the client, unmodified.

## 8. Credential handling per fallback

No new credential mechanism — each fallback resolves auth exactly as it
would if reached today via `additionalProviders`:

- Same-provider fallbacks (tiers 1–2) inherit that provider's existing
  `Upstream.Auth`, resolved by the same `set-headers`/`oauth2-generator`
  policies already conditionally attached for it.
- Cross-provider fallbacks (tier 3) resolve via that provider's own
  `additionalProviders[]` entry (or standalone `provider`), each carrying its
  own independent `auth` config and conditionally-attached auth policy.

**Known, carried-over limitation**: a fallback provider configured with
`auth: none` or `auth: other` and no additional auth policy attached will
forward the client's original credential to that upstream unchanged. This is
existing `additionalProviders` behavior, not new risk introduced here — but
failover makes it easier to reach unintentionally, since a misconfigured
cross-provider fallback is now reachable automatically rather than only via
an explicit `x-provider` header a client would have to send deliberately.
This design does not attempt a runtime fix; it requires this be surfaced as
an explicit warning in schema validation/documentation for any cross-provider
fallback target lacking a real `auth` configuration.

## 9. Suspend / circuit-breaking behavior

Per-replica, in-memory:

- After a target fails (matches `statusCodes`) a threshold number of times
  within a window, that replica marks the target suspended for
  `suspendDuration` and skips directly to the next fallback for subsequent
  requests — bounding latency for a known-bad target instead of paying its
  full `requestTimeout` on every request.
- State is keyed by target identity (`model` + `provider`/
  `upstreamDefinition`), shared across all requests on that replica — not
  per-client.
- **v1 limitation, explicit**: convergence across a multi-replica fleet is
  eventually consistent — each replica learns independently, with no
  synchronization. A shared-cache-backed version is a natural future
  extension, not built now (§2).

## 10. Metering (rate limiting & analytics)

Redial attempts must not be counted as separate client requests:

- Every redial carries the recursion-guard header (§7); rate-limiting and
  analytics policies must recognize this marker and skip metering for
  requests carrying it, counting only the original top-level request.
- This requires a small, explicit change on top of the prototype — confirm
  with the rate-limiting/analytics policy owners whether they already
  special-case `x-wso2-internal-loopback`-style markers or need a new check
  added for `x-wso2-model-failover-redial` specifically.

## 11. Observability & error handling

- Every attempt (primary + each fallback tried) is logged internally with
  target identity and outcome, for operator debugging — never exposed to the
  client, consistent with the fully-transparent behavior in §2.
- Metrics: failover-triggered count broken down by originating target and by
  which tier resolved it (1/2/3); count of fully-exhausted fallback chains
  (total failure across every configured target).
- If every target in the chain fails, the client receives the **last
  attempt's actual response**, not a synthesized generic error — preserving
  whatever detail the last real upstream returned, consistent with existing
  gateway error-handling elsewhere in the codebase.
- `maxResponseBytes` and `requestTimeout` bound every individual attempt so
  one hung or oversized upstream can't stall or memory-balloon the whole
  chain.

## 12. Testing strategy

- **Unit**: per dispatch decision — trigger match, fallback selection,
  suspend state transitions, recursion-guard behavior. The prototype's ~45
  test functions are a strong starting base to carry forward and adapt
  rather than write from scratch.
- **E2E**: reuse/extend the existing Postman collection and mock provider
  backends (`mock-anthropic-backend`, `mock-model-backend`) for at least one
  scenario per tier (1/2/3), a fully-exhausted-chain scenario, and a
  metering-exclusion assertion (a redial does not double-count rate limit or
  analytics).

## Appendix A — Provider-specific retriable status codes (reference, not defaults)

Research carried over from the prototype's README, useful as operator
guidance since `statusCodes` has no built-in default:

| Provider | Commonly retriable codes |
|---|---|
| OpenAI | 429, 500, 503 |
| Anthropic | 404, 429, 500, 529 |
| AWS Bedrock | 404, 408, 424, 429, 500, 503 |
| Azure OpenAI | 404, 429, 500, 503 |
| Gemini | 404, 429, 500, 503 |
| Mistral | 429, 500, 502, 503, 504 |

## Appendix B — Open questions for implementation planning

- Exact mechanism for tagging/recognizing a redial in rate-limiting/analytics
  policies (§10) — needs confirmation from those policies' current owners.
- Whether `selfBaseURL`'s default (`127.0.0.1:8080`) holds across all
  supported deployment topologies, or needs to be topology-aware.
- Threshold (failure count / window) for triggering suspend (§9) — not yet
  specified; needs a default and an operator override.

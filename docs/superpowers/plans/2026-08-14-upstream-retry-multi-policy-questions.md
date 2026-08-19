# Upstream Retry Mechanism — Open Questions for Multi-Policy Dispatch

## Context

Brainstorming session for fixing two problems with the current model-failover
implementation:

1. Adding an unrelated policy (e.g. `oauth2-generator`) to a route shouldn't
   require touching an unrelated part of the spec (`resilience.retry`) to
   make it work — attaching a policy should be self-contained.
2. Model-failover's `translator.go` special-casing (`p.Name ==
   "model-failover"`, bespoke aggregate-cluster synthesis) doesn't generalize
   — a future policy needing similar multi-target retry would need its own
   equivalent special-casing.

Before picking an approach, this doc captures the open question: **if
multiple policies can hook into the upstream-retry mechanism
(`UpstreamAttemptPolicy`), how do we know which one should act on a given
retry, given retries can happen for different reasons?**

Grounded in the actual dispatch code
(`gateway-runtime/policy-engine/internal/kernel/upstream_extproc.go`) and
`dev-policies/oauth2-generator/oauth2_generator.go`'s real implementation —
not speculation.

## A. Dispatch mechanics (fully answered — confirmed in code)

**A1. When multiple policies implement `UpstreamAttemptPolicy`, who gets called?**
All of them, unconditionally, every attempt, in chain order
(`upstream_extproc.go:162-175`). No filtering, no "pick the relevant one"
step.

**A2. If two policies both set the same header key, what wins?**
Last-write-wins by chain order — plain map overwrite (`headersToSet[k] =
v`), no error, no merge logic.

**A3. If two policies both try to mutate the body, what wins?**
Also last-write-wins — the code comment says it outright: *"last write wins,
matching HeadersToSet's own convention"* (`upstream_extproc.go:221`).
Whichever policy is later in chain order silently discards an earlier
policy's body mutation.

**A4. Can a policy see what another policy in the chain already decided, or veto it?**
No. Each policy's `OnUpstreamAttemptRequestHeaders` call gets the same
`actx`, but there's no shared "this attempt is already spoken for" signal
between them.

## B. "Why is this retry happening" — the core gap

**B1. Does `UpstreamAttemptContext` tell a policy *why* Envoy is retrying (which status code, which failure)?**
**No.** The struct carries only `AttemptCount`, `Headers`, `Body`. Nothing
about the previous attempt's response status or failure reason.

**B2. Does oauth2-generator actually check that the retry was auth-related before refreshing the token?**
No — confirmed in `oauth2_generator.go:958-987`. It refreshes on any
`AttemptCount > 1`, unconditionally. The doc comment admits this is an
assumption, not a check: *"the previous attempt's response is assumed
rejected... per the configured resilience.retry.statusCodes."* It can't
verify that assumption — the information isn't available to it.

**B3. Is this a latent bug today?**
Sort of, but benign *today*: it only works safely because there's currently
at most one retry-source per route, so there's only ever one possible
"reason" a retry could be happening on any given route. If
`resilience.retry` is configured with `statusCodes: [503]` (nothing to do
with auth) and oauth2-generator is also attached, it needlessly purges and
refetches a valid token on every transient 503 retry — wasteful, not
harmful (fail-open). Tolerated as an accepted trade-off, not actually fixed.

**B4. Could Envoy even give us "why" if we wanted it?**
**Unanswered — needs direct investigation of Envoy's ext_proc protocol.**
Not verified yet whether the upstream filter chain has access to the
previous attempt's response status/failure at all.

## C. What this means for "multiple retry-observer policies"

**C1. If two independent retry-observer policies both existed on one route, could either reliably tell "was this retry for me"?**
No. Given B1, neither policy can distinguish "this attempt is happening
because of my trigger condition" from "this attempt is happening because of
some *other* policy's trigger condition, or a plain resilience.retry status
code unrelated to either." Every observer fires blindly on every attempt.

**C2. Does that make multiple observers unsafe today?**
Not unsafe, but silently wasteful/wrong in a way nobody would notice: each
observer does its own thing on every attempt regardless of cause, and per
A2/A3, if two observers' mutations collide, one silently overwrites the
other with no error — the failure mode is "wrong behavior with no signal,"
not a crash.

## D. Direct implication for the coupling concern (#2)

**D1. Is the classification of "retry-source vs retry-observer" a real SDK concept today, or still hardcoded?**
Still hardcoded — confirmed via grep, there's no `RetrySourcePolicy`-style
interface; it's `p.Name == "model-failover"` string checks in
`translator.go`.

**D2. Is body-buffering (`RequestBodyMode: BUFFERED` on the upstream filter) generic, or also policy-name-specific?**
Also policy-name-specific — per the original design doc, it's enabled "only
for clusters actually backing a model-failover route" (a capability flag
keyed to that one policy by name). A future retry-observer wanting body
access on a route using plain `resilience.retry` (no model-failover) would
need new special-casing to get body access at all — the same coupling
problem, extended forward.

## Unanswered questions (need investigation or a design decision, not guessed)

- **Can Envoy's ext_proc protocol expose the previous attempt's response
  status/failure to the upstream filter chain at all?** Determines whether
  "cause-aware" dispatch is physically possible, or whether the system is
  permanently stuck with attempt-count-only blind dispatch. (= B4)
- **If multiple retry-observer policies are ever allowed on one route,
  should there be an explicit ordering/priority declaration**, or is chain
  order (today's implicit answer) good enough once cause-awareness exists?
- **Should collision (two policies fighting over the same header/body)
  become a hard validation-time error** instead of silent last-write-wins,
  once a real classification system exists to detect it? (Today it's silent
  because nothing exists to check against.)

## Working framing to carry forward

Two categories of "retry-related" policy:

- **Retry-source** — needs to configure `RouteAction.RetryPolicy` itself
  (model-failover; `resilience.retry` reframed as one too). At most one per
  route — a real Envoy constraint (one `RetryPolicy` field per route, one
  `RetryPriority` extension slot), not a design choice.
- **Retry-observer** — just reacts to attempts happening (oauth2-generator).
  Composes freely with *any* retry-source, or safely no-ops if there isn't
  one — **provided** it stays cause-agnostic (per B1-B3), which is only
  safe as long as being wrong/over-triggered is harmless, not as a general
  guarantee.

Open fork depending on B4's answer:

- If Envoy can't expose failure-cause to the upstream filter chain:
  observers must stay purely attempt-count-driven and cause-agnostic by
  design — same shape as oauth2-generator today, generalized.
- If Envoy can expose it: worth threading real failure-cause information
  into `UpstreamAttemptContext` so observers can condition on it, before
  finalizing an approach for #1/#2.

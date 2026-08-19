# Upstream Retry Mechanism — Requirements Questions (Implementation-Agnostic)

## Context

Follow-up to `2026-08-14-upstream-retry-multi-policy-questions.md`, which was
grounded in the *current* implementation's behavior. This doc deliberately
sets that aside and asks: if we were designing a multi-policy upstream-retry
mechanism from first principles, what does it actually need to satisfy?
Purpose: surface real requirements and correctness invariants before picking
an approach for fixing model-failover's `resilience.retry` coupling and
`translator.go` special-casing.

Each question is answered where it's derivable from general
distributed-systems/API-gateway design principles or hard correctness
constraints; flagged as an open product decision where it genuinely isn't.

## 1. Retry causality — what triggers a retry at all?

**Q1.1: What are the distinct *kinds* of reason a retry might be needed?**
Answerable in general terms: broadly (a) transient infrastructure failure
(network blip, upstream momentarily overloaded — retry the *same* target),
(b) credential/auth rejection (retry the same target with fresh
credentials), (c) target itself is bad for this request (wrong/unavailable
model, quota exhausted — retry a *different* target). These are
qualitatively different: (a) and (b) want "same destination, modified
request," (c) wants "different destination." Any design needs to treat
"retry same place differently" and "retry somewhere else" as distinct
capabilities, not one undifferentiated "retry" concept.

**Q1.2: Should the system require a request to declare *why* it's retrying, or is "a retriable condition was matched" sufficient?**
**Open product decision.** But narrowed: if more than one policy should ever
coexist safely with different retry motivations, *something* has to carry
"why," even coarsely (a category like a/b/c above, not necessarily the exact
status code). Without it, every observer is forced to guess. Treat "cause
must be knowable, at least at a/b/c granularity" as a hard requirement, not
optional.

**Q1.3: Is a retry ever justified by something *other* than the response (e.g. a timeout with no response at all)?**
Yes, structurally — a connect timeout or reset produces no status code to
key off of. Any cause taxonomy needs a "no response" bucket, not just
status-code buckets.

## 2. Ownership — who decides *that* a retry happens?

**Q2.1: Should exactly one thing own "should this request retry, how many times, against what," or can multiple things each independently vote to extend the retry budget?**
**The crux decision — open, everything else branches from it.** Two coherent
models:
- **Single owner, declared per route** — one thing (a policy, or a config
  block) fully owns the retry envelope (count, targets, conditions) for a
  route. Everything else only *reacts*. Simple, but requires that one owner
  to enumerate every reason retry might be needed up front.
- **Multiple independent voters, combined at runtime** — several things can
  each say "I think this specific failure warrants a retry, here's my
  reason," and the system unions/arbitrates. More flexible, but now needs
  real conflict/precedence rules and a hard ceiling so competing voters
  can't produce unbounded retries.

Genuine product-philosophy choice (simplicity + predictability vs.
flexibility + complexity) — not something to derive from first principles
alone.

**Q2.2: Is there a hard ceiling on total attempts, independent of how many things want to trigger a retry?**
**Answerable confidently: yes, unconditionally.** Without a hard,
non-negotiable ceiling, two independently-reasoning voters can trivially
produce a retry loop that never terminates (A retries because of reason X,
which incidentally produces condition Y, which makes B retry, which
reproduces X...). This should be a non-negotiable invariant of the design,
not left to individual policies to self-limit.

## 3. Safety invariants (correctness, not preference)

**Q3.1: Should retries ever be allowed for a non-idempotent request without explicit acknowledgment?**
**No — hard distributed-systems correctness requirement.** Retrying a `POST`
that already partially executed on the origin (charged a card, sent an
email, created a duplicate resource) can cause real duplicate side effects.
Any design needs either (a) restrict automatic retry to safe methods by
default, or (b) require the API author to explicitly assert idempotency
(e.g. via an idempotency key) before retry is allowed on a mutating method.
This holds regardless of which ownership model (Q2.1) is chosen.

**Q3.2: If a retry mutates the request (new credential, different target, rewritten body), must the *original* client-sent bytes always be the base for that mutation, or can mutations stack across attempts?**
**Treat as a requirement: always mutate from the original, never from a
previous attempt's mutation.** Stacking mutations across attempts makes
behavior order-dependent and nearly impossible to reason about or test,
especially once multiple independent things can mutate.

## 4. Declaration model — how does the system know what each policy needs?

**Q4.1: Should a policy declare its retry needs statically (at config/registration time — "I need up to N attempts, triggered by conditions X"), or decide dynamically at request time with no upfront declaration?**
**Fairly clear best-practice answer: static declaration.** It lets the
system validate *before* deployment whether a combination of policies is
coherent (conflicting attempt budgets, overlapping trigger conditions)
rather than discovering a conflict live in production traffic. Fully
dynamic, undeclared retry decisions are much harder to reason about, test,
or safely combine.

**Q4.2: If two policies both declare a need to retry on overlapping conditions, is that a configuration-time error, or does one need explicit precedence?**
**Open product decision, but narrowed.** Given Q4.1 (static declaration),
this becomes checkable up front. Lean toward "error by default, precedence
must be explicit if you want to allow it" as the safer default — but whether
to allow explicit precedence at all is still an open choice.

## 5. Observability requirement

**Q5.1: Does the system need to distinguish, after the fact, "succeeded on attempt 1" vs "succeeded on attempt N because of reason X" vs "exhausted all attempts and failed"?**
**Yes, as a hard requirement.** Without this, retry behavior (suspend
durations, budgets, trigger conditions) can't be debugged or tuned from real
traffic data. This needs to be an explicit, structured signal — not just
inferred from logs — regardless of the ownership model chosen.

## Summary — what's actually open vs. settled

**Settled (hard requirements, not preference):**
- Q2.2 — hard ceiling on total attempts, non-negotiable, independent of how many things want to trigger retry.
- Q3.1 — no automatic retry on non-idempotent requests without explicit acknowledgment.
- Q3.2 — every retry mutation is always based on the original request, never a stacked prior mutation.
- Q4.1 — retry needs are declared statically, validated at config time, not decided fully dynamically with zero upfront declaration.
- Q5.1 — attempt outcome (which attempt succeeded, why, or exhaustion) must be an explicit structured signal.

**Open — need a decision, not derivable from first principles:**
- **Q2.1 (the big one)** — single retry-owner per route vs. multiple independent voters combined at runtime. Everything else follows from this.
- Q1.2 — exact granularity of "cause" that must be threaded through (coarse category vs. something richer).
- Q4.2 — whether overlapping retry-trigger declarations are always a hard error, or allowed with explicit precedence.

## Next step

Resolve Q2.1 first — it's the fork everything else branches from.

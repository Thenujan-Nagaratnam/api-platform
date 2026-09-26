# Security Checklist: Model Failover Policy

**Purpose**: T059. Checks the implementation against `.claude/rules/` and the spec's security requirements.
**Created**: 2026-09-26
**Legend**: `[x]` verified by code and tests · `[ ]` needs a live gateway run (listed under "Live checks")

## Trust boundaries

- [x] Client-supplied `x-wso2-failover-*` and `x-wso2-upstream-failure` headers are removed by the front policy before anything reads them, and the plan header is overwritten (`front.go` `clientSuppliedInternalHeaders`; `TestFrontStripsClientSuppliedInternalHeaders`; IT scenario "Client-supplied internal failover headers are ignored").
- [x] The chain header is set by the front route with `OVERWRITE_IF_EXISTS_OR_ADD`, so a client value can't select another chain (`failover_listener.go`; `TestFailover_FrontRoute`).
- [x] A dispatch request with an unknown, expired or mismatched plan nonce gets an untagged 500 and is never forwarded or retried (`TestDispatchRejectsForgedOrMismatchedPlan`).
- [x] Plan nonces are 128-bit `crypto/rand`; the hop secret is 256-bit `crypto/rand`, generated per controller process (`plan.go` `newNonce`; `failover.HopSecret`; `TestHopSecretIsStableAndRandom`).
- [x] The dispatch hop runs on an Envoy internal listener with no socket address (`TestFailover_DispatchResources` asserts `GetAddress() == nil`).
- [x] Dispatch routes match `prefix "/"` only together with the chain header, and every dispatch chain requires a valid plan, so a dispatch route is not usable without a front route.

## Secrets and credentials

- [x] The hop secret is never forwarded to a provider: the client-facing listener's `wso2.failover.hop` Lua filter moves it into dynamic metadata and removes the header before the router (spike S3b; `TestFailover_MainListenerAnnotatesTransportFailuresOnlyForHopSecret`; IT asserts the mock never sees `x-wso2-failover-hop`).
- [x] Transport-failure flags are added only for requests carrying the secret, with `match_if_key_not_found: false`, so direct callers of a provider route never see Envoy response flags (spike S3b).
- [x] Each target's credential policy is gated on that target's own `selected_provider` value and never fires when none is selected, so no credential crosses to another target (`TestTransformProxy_FailoverCredentialsAreIsolatedPerTarget`; IT checks openai-b and Anthropic receive only their own key).
- [x] The front route carries no provider credential or transformer (`TestTransformProxy_FailoverSplitsFrontAndDispatch`).
- [x] Logs carry target ids, provider and model names, reasons and latencies only: no header values, bodies, keys, nonces or the hop secret (`logging.go`; `TestLogsNeverContainSecretsOrBodies`).

## Error handling (error-handling rule)

- [x] The exhaustion response is fixed and byte-identical whatever failed. It has no provider names, endpoints or upstream bodies (`TestExhaustionReplacesTaggedOrExhaustedFinalResponse`; IT exact body match).
- [x] Internal headers are stripped from every client response twice: by the front policy, and by the front route's `response_headers_to_remove`.
- [x] A route whose chain fails to build returns 500 and never forwards (engine behaviour on main, research R13).

## Resource bounds (network-hardening rule)

- [x] Attempts per request are bounded by `num_retries = targets - 1`, with at most 10 targets.
- [x] `perAttemptTimeout` is bounded to 1s–300s. The front route timeout is derived from it (`targets × perAttemptTimeout + 2s`) instead of a generic default (`TestFailover_FrontRouteKeepsLargerConfiguredTimeoutAndDisabledTimeout`).
- [x] The retry buffer is bounded (`router.failover.max_request_body_bytes`, default 4 MiB, at most 100 MiB) and validated at startup.
- [x] Retries in flight on the dispatch cluster are capped (`max_retries: 1024`); retry back-off is 1–10 ms.
- [x] Probe traffic is capped per target (`probeConcurrency` 1–10). Probe slots are always returned when a plan closes or expires (`TestUnusedProbeSlotIsReleasedOnClose`).
- [x] Plans expire (`targets × perAttemptTimeout + 7s`) and are swept every second, so an abandoned request can't grow memory.
- [x] Path canonicalization on the internal listener matches the client-facing listener (`NormalizePath`, `MergeSlashes`, escaped-slash action; xDS rule directive 6).

## Deferral clauses

- [x] No gap is deferred behind a `TODO`/`FIXME` comment. Open items are tracked as tasks with a stated reason (T011, T050).

## Live checks (T059, pending your gateway run)

- [ ] From the host, no published port reaches a dispatch route (send a request with a guessed `x-wso2-failover-chain` header to `:8080` and expect the proxy's normal behaviour or 404, never a provider).
- [ ] A provider mock never receives `x-wso2-failover-hop`, `x-wso2-failover-plan` or `x-wso2-failover-chain` (covered by the IT assertions once they run).
- [ ] Policy-engine and gateway logs from the IT run contain no API keys (`grep -E 'Bearer mf-|mf-.*-key'` on the logs returns nothing).

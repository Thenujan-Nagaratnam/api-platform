# OAuth2 Upstream Authentication for LLM Providers/Proxies

## Overview

Extends the existing typed `upstream.auth` field (previously `api-key` only)
with a second value, `oauth2`, so the gateway can authenticate to a backend
LLM (or any OAuth2-secured upstream) using a short-lived bearer token instead
of a static, pre-shared API key. The gateway acts as a confidential client:
it exchanges credentials for an access token at a configured token endpoint,
caches it, and injects it as `Authorization: Bearer <token>` before
forwarding the request.

## Motivation

* [ ] Many LLM providers and enterprise-hosted backends — Azure OpenAI behind an
  Entra ID app registration, self-hosted models behind a corporate OAuth2
  proxy, any IdP-fronted backend — require OAuth2 rather than a static key.
  Without native support, customers work around this themselves (a
  token-minting sidecar, or a long-lived token pasted in that silently
  expires), which pushes complexity and operational risk onto the customer
  that the gateway should absorb. Implementing this as a first-party policy
  automates token acquisition, caching, and rotation.

## Requirements / Design

### Grants

- **`client_credentials`** (RFC 6749 §4.4) — the default, standard
  machine-to-machine grant. Delegates entirely to
  `golang.org/x/oauth2/clientcredentials`.
- **`password`** (RFC 6749 §4.3, Resource Owner Password Credentials) —
  supported for bridging to legacy IdPs that only expose this grant.
  Discouraged for new integrations per current OAuth2 security guidance
  (requires the gateway to handle a raw end-user credential). Delegates to
  `golang.org/x/oauth2.Config.PasswordCredentialsToken`.

`grantType` is a first-class, forward-compatible parameter specifically so
further grants can be added later without a breaking schema change.

### Client authentication (`clientAuthMethod`)

How `clientId`/`clientSecret` are presented to the token endpoint, selectable
per grant:

- **`client_secret_basic`** (default) — HTTP Basic auth header. RFC 6749's
  preferred convention when the IdP supports it.
- **`client_secret_post`** — `client_id`/`client_secret` sent as form fields
  in the token request body instead. Required by some identity providers.

Both grants respect this identically: `clientcredentials.Config.AuthStyle`
and `xoauth2.Config.Endpoint.AuthStyle` are consumed by the exact same
internal function in `golang.org/x/oauth2` (`AuthStyleInHeader` /
`AuthStyleInParams`), so one mapping covers both grants with zero hand-built
HTTP code.

Not in scope for this iteration: `private_key_jwt` and mTLS
(`tls_client_auth`) — both require the gateway to manage asymmetric key
material or client certificates, a materially bigger feature than this
policy's current scope. Documented as a known constraint.

### Custom token-request parameters (`params`)

An optional flat map of extra form fields merged into the token request body
alongside `grant_type` and the grant's own fields — the primary use is
`scope` (e.g. `params: {scope: "read write"}`), but also covers IdP-specific
fields like `resource`, `audience`, or `tenant`.

**`params` only applies to `client_credentials`.** `clientcredentials.Config`
exposes an `EndpointParams` hook designed for exactly this. The password
grant's library helper (`PasswordCredentialsToken`) hardcodes its own
request body with no equivalent hook — extending it would require
hand-building a replacement HTTP client (raw `net/http`, manual JSON
response parsing) purely to carry a rarely-needed field on a grant that's
already discouraged for new integrations. Decided against that added
complexity/maintenance surface: `params` set alongside `grantType: password`
is accepted but silently has no effect, same as `username`/`password` being
accepted-but-unused for `client_credentials`.

There is deliberately no first-class `scope` field — it's just one entry
among `params`.

### Resilience (`tokenRequestTimeout`, `defaultTokenTTL`)

Two operational tunables, added after cross-checking against Kong's
equivalent plugin (see below) surfaced both as real, confirmed gaps rather
than theoretical ones:

- **`tokenRequestTimeout`** (default `10s`) bounds a single token-endpoint
  HTTP call. Without it, `golang.org/x/oauth2` falls back to
  `http.DefaultClient`, which has `Timeout: 0` — an unresponsive IdP would
  block a token fetch indefinitely. Injected via
  `context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: ...})`,
  which both `clientcredentials.Config.TokenSource` and
  `xoauth2.Config.PasswordCredentialsToken` forward down to
  `internal.RetrieveToken` identically — one fix covers both grants.
- **`defaultTokenTTL`** (default `1h`) is applied only when the token
  endpoint's response omits `expires_in` entirely. `golang.org/x/oauth2`
  leaves `Token.Expiry` as the zero value in that case, and `Token.Valid()`
  always treats a zero-value `Expiry` as already-expired — without this
  fallback, *neither* cache tier would ever consider such a token cacheable,
  silently forcing a fresh fetch on every single request. Applied by
  mutating the token in place (not a copy) in `token_cache.go`'s `Token()`,
  so the inner `xoauth2.ReuseTokenSource`'s own reuse-until-expiry behavior
  picks up the fix too, not just this policy's own cache tiers.

Both apply identically to both grants; both are simple string params
(`params`'s own permissive fallback-on-parse-failure style, not enum
type-validated).

### API Specification

```yaml
UpstreamAuth:
  auth:
    type: { enum: [api-key, oauth2] }
    oauth2GrantType: { enum: [client_credentials, password], default: client_credentials }
    oauth2TokenEndpoint: string
    oauth2ClientId: string
    oauth2ClientSecret: string
    oauth2ClientAuthMethod: { enum: [client_secret_basic, client_secret_post], default: client_secret_basic }
    oauth2Username: string   # required only when oauth2GrantType: password
    oauth2Password: string   # required only when oauth2GrantType: password
    oauth2Params: { additionalProperties: string }   # optional, client_credentials only, e.g. { scope: "read write" }
    oauth2TokenRequestTimeout: string   # Go duration, default "10s", both grants
    oauth2DefaultTokenTTL: string       # Go duration, default "1h", both grants - fallback when expires_in is omitted
```

No new endpoint — extends the existing `LlmProvider`/`LlmProxy` create/update
payload (both the nested `UpstreamAuth` used by `LlmProvider.upstream.auth`
and the top-level `LLMUpstreamAuth` used by `LlmProxy`'s additional-provider
auth).

### Architecture

```
Registers LlmProvider (upstream.auth.type: oauth2)
        │
        ▼
gateway-controller validates params, attaches oauth2 policy
        │  (existing PolicyChainConfig xDS — no new channel)
        ▼
gateway-runtime: local cache? → Redis cache? → fetch from IdP
        │ (hit at any tier)
        ▼
inject Authorization: Bearer <token>, forward upstream
```

### Token caching (Redis-backed, two-tier)

Tokens are cached in two tiers — in-process, then Redis — scoped **per
API**, not per resource/route: since `oauth2` config lives on
`upstream.auth`, one value for the whole API, every resource it exposes
(`/chat/completions`, `/embeddings`, ...) shares the exact same credentials
and should share the exact same cached token. `apiId` is resolved lazily at
request time from `SharedContext.APIId` (control-plane `PolicyChainConfig`
never carries a stable API identifier at policy-construction time — a
pre-existing gap affecting every policy attached this way).

Redis is an optimization, not a hard dependency: `systemParameters.redis. failureMode` controls whether an outage falls back to fetching directly from
the token endpoint (`open`, default) or is treated as an auth failure
(`closed`).

### Impact on Persistent Data

No new tables. `oauth2` credentials are stored exactly like `api-key`'s
`value` — inside the provider's existing config JSON blob. Encryption is
available today, opt-in, via the existing secrets mechanism: create a secret
(`POST /api/management/v1/secrets`) and reference it as
`'{{ secret "handle" }}'` instead of a literal in any field, including
values nested inside the `params` map — `gateway-controller`'s
config-rendering step (`renderValue` in `pkg/templateengine/spec.go`)
recurses into `map[string]any` unconditionally regardless of field name, so
this already works with zero additional code. A literal value, if used
instead, is stored and returned in plaintext.

## Challenges and Solutions

### Challenge 1: Information Leakage during Authentication Failures

**Risk:** Exposing detailed IdP errors to the client could leak sensitive
backend configuration or credentials.

**Solution:** The policy fails-closed on every auth failure, returning a
generic `502 Bad Gateway` with a masked message. Underlying errors
(`invalid_client`, malformed response, etc.) are restricted to server-side
logs only.

### Challenge 2: Limited IdP Compatibility with Client Auth Methods

**Risk:** Hardcoding a single auth method excludes IdPs that require
alternate schemes.

**Solution:** `clientAuthMethod` now supports both `client_secret_basic`
(default) and `client_secret_post`, covering the large majority of real
IdPs with no hand-built HTTP code (see Design above). `private_key_jwt` and
mTLS remain a known, documented constraint for a future iteration.

### Challenge 3: Security of Cached Bearer Tokens

**Risk:** Unauthorized access to the Redis cache tier could expose active
bearer tokens.

**Solution:** Network access to the Redis instance must be restricted using
the same security posture as the token endpoint itself; cache entries expire
with the token's own TTL, no separate longer-lived retention.

### Challenge 4: Plaintext Storage of Client Secrets

**Risk:** Literal secrets stored in config blobs are vulnerable to exposure
if the database is compromised.

**Solution:** The existing secrets management mechanism
(`{{ secret "handle" }}`) resolves secrets at render time rather than
storing them in plaintext — available for `clientSecret`, `password`, and
any value nested inside `params`.

## Status

Implemented and live-verified end to end (Docker rebuild + real requests
against a mock IdP/backend) for:

- `client_credentials` and `password` grants
- `clientAuthMethod: client_secret_basic` (default) and `client_secret_post`,
  both grants
- `params` custom token-request fields for `client_credentials`, including
  a value referencing a stored secret
- Redis-backed two-tier caching with `failureMode: open`/`closed`
- `tokenRequestTimeout` — verified against a real IdP configured to delay
  3s while the policy's timeout was 500ms: request failed with `502` in
  ~526ms, well before the 3s delay would have completed
- `defaultTokenTTL` — verified against a real IdP configured to genuinely
  omit `expires_in` (confirmed via a direct, gateway-bypassing request to
  the mock): 3 chat-completion requests through the gateway produced
  exactly 1 token-endpoint call, proving caching still worked

Shipped as `oauth2` policy `v0.8.0` in both `gateway-controllers/policies/oauth2`
and the in-repo mirror `gateway/dev-policies/oauth2` (kept byte-identical —
see the dual-repo gotcha this caused mid-implementation, captured in team
memory as it isn't otherwise enforced by tooling).

`TESTING.md` and the companion Postman collection
(`gateway/dev-policies/oauth2/e2e/postman/oauth2.postman_collection.json`)
both cover `client_secret_post` (E.11), `tokenRequestTimeout` (E.12), and
`defaultTokenTTL` (E.13), alongside the pre-existing happy path, caching,
expiry, and failure-mode flows. The mock IdP (`mocks/mock-oauth2-idp`) gained
two params to support this: `delayMs` (artificially delay the response) and
`omitExpiresIn=true` (drop `expires_in` from the response entirely). All of
`TESTING.md`, the mocks, and the Postman collection now live under a single
`e2e/` subdirectory, with `e2e/run-e2e.sh` as a one-command runner that
starts both mocks and runs the full collection via newman against an
already-running gateway stack.

**Note:** `gateway/dev-policies/` is gitignored repo-wide
(`.gitignore:165: dev-policies/`) — none of the dev-policies-side work above
(the mirrored policy code, `TESTING.md`, the Postman collection, the mock
changes) is tracked in git or visible in a diff/PR. It's used by the local
Docker build/test workflow but isn't shared via `git clone`. Worth revisiting
if the intent is for other developers to get this test tooling for free.

### Caching mechanism — verified against standard practice

The two-tier (in-process, then Redis) cache-aside pattern was checked
against the actual `golang.org/x/oauth2` library source, not just written
from memory:

- Matches the standard L1(process)/L2(shared) cache architecture gateways
  use for this class of artifact (e.g. Kong's `lua-resty-mlcache` for
  tokens/JWKS/discovery docs).
- TTL is derived from the token's own `expires_in`, never longer.
- The library's own `Token.Valid()` early-expiry buffer (10s,
  `defaultExpiryDelta` in `oauth2/token.go`) applies uniformly to both the
  local and Redis-sourced token, avoiding a token expiring mid-flight.
- In-process concurrent-request stampede protection is real: the inner
  token source is always wrapped in `xoauth2.ReuseTokenSource`, whose mutex
  is held across the entire fetch (`oauth2.go`'s `reuseTokenSource.Token()`),
  so concurrent misses within one process serialize onto one IdP call
  rather than firing N.
- **Known, accepted gap:** no cross-replica dedup. Simultaneous cold-start
  across several `gateway-runtime` replicas can produce one IdP call per
  replica rather than one for the whole fleet. Common, generally-accepted
  tradeoff for this workload (idempotent, side-effect-free token requests);
  a stricter design could add a distributed lock (e.g. Redis `SETNX`-based)
  to close this, not implemented here.

### Cross-check against Kong's Upstream OAuth plugin

Compared against Kong's [Upstream OAuth
plugin](https://developer.konghq.com/plugins/upstream-oauth/) (fetched and
read directly, not from memory) — the closest existing product doing the
same job.

**Adopted from the comparison (implemented, see Status above):**
- `tokenRequestTimeout` — confirmed gap (we had no timeout at all); Kong
  defaults to `10000ms`, we matched that default.
- `defaultTokenTTL` — confirmed gap (we silently disabled caching entirely
  when `expires_in` was missing); Kong's `cache.default_ttl` defaults to
  `3600s`, we matched that default.

**Deliberately not adopted:**
- Kong's fully-templated error response
  (`idp_error_response_status_code`/`message`/`content_type`/`body_template`,
  configurable per plugin instance) — conflicts with this repo's own
  `error-handling.md`/`authentication_authorization.md` conventions, which
  mandate a fixed, org-wide-consistent generic auth-failure body specifically
  to prevent information-leakage variance across policies. Kept the
  hardcoded 502 + fixed message.

**Noted, not yet acted on:**
- Kong's `purge_token_on_upstream_status_codes` (default `[401]`) evicts the
  cached token when the *upstream* (not the IdP) rejects it, so the next
  request gets a fresh one instead of retrying a revoked token until it
  naturally expires. We can't do this today — the policy only implements
  `RequestHeaderMode: Process`, `ResponseHeaderMode: Skip`, so it never sees
  the upstream's response. Would need a response-phase hook - a real
  architectural change, not a small tweak. Highest-value remaining gap.
- Kong exposes `token_headers` (extra headers on the token request) in
  addition to `token_post_args` (our `params`, body only) — no way today to
  inject a custom header some IdPs might require.
- Kong types `scopes`/`audience` as explicit arrays (default `scopes:
  ["openid"]`) rather than folding them into a generic map; better
  per-field UX/validation for the common case, at the cost of needing a
  schema change for anything not explicitly typed. Our generic `params` map
  is more flexible but has zero validation. Worth reconsidering if `scope`
  usage turns out to dominate.
- Kong exposes IdP-reaching proxy/SSL settings (`http_proxy`, `ssl_verify`,
  etc.) — more niche, matters mainly in locked-down enterprise egress
  setups.
- Kong's `password` grant gets the same scopes/headers/params support as
  `client_credentials` (their own Lua token-request logic, not delegating to
  a limited library) — our `params`-only-for-`client_credentials`
  restriction is a real, self-imposed capability gap versus Kong, not one
  Kong itself has to make.

## Open Follow-ups

- `private_key_jwt` / mTLS client authentication — bigger feature, not
  scoped here.
- Token purging on upstream rejection (Kong's
  `purge_token_on_upstream_status_codes` equivalent) — requires a
  response-phase hook this policy doesn't have today. Highest-value item
  from the Kong cross-check above.
- `headers` alongside `params` (extra headers on the token request, not just
  body fields).
- No named customer or competitive reference is attached to the original
  motivation for this feature — worth attaching one before treating this as
  a validated priority for further investment (e.g. `authorization_code`
  support, additional grants).

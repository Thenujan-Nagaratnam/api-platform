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

Shipped as `oauth2` policy `v0.7.0` in both `gateway-controllers/policies/oauth2`
and the in-repo mirror `gateway/dev-policies/oauth2` (kept byte-identical —
see the dual-repo gotcha this caused mid-implementation, captured in team
memory as it isn't otherwise enforced by tooling).

`TESTING.md` and the companion Postman collection
(`gateway/dev-policies/oauth2/postman/oauth2.postman_collection.json`) both
cover the `client_secret_post` flow (E.11) alongside the pre-existing happy
path, caching, expiry, and failure-mode flows.

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

## Open Follow-ups

- `private_key_jwt` / mTLS client authentication — bigger feature, not
  scoped here.
- No named customer or competitive reference is attached to the original
  motivation for this feature — worth attaching one before treating this as
  a validated priority for further investment (e.g. `authorization_code`
  support, additional grants).

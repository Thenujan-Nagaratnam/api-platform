# OAuth2 Upstream Authentication for LLM Providers/Proxies

## What is the problem we are trying to solve? And why should it be solved now?

An API publisher connecting the gateway to an OAuth2-secured backend — Azure
OpenAI behind an Entra ID app registration, a self-hosted model behind a
corporate OAuth2 proxy, or any other IdP-fronted service — has no native way
to do it today. The gateway's `upstream.auth` field only supports a static,
pre-shared API key. To reach an OAuth2-secured backend, the publisher has to
build and operate their own token-minting sidecar, or paste in a long-lived
token that silently expires and breaks traffic with no warning. Either way,
the complexity and operational risk of token acquisition, caching, and
rotation sits with the customer instead of the platform.

This is driven by a competitive/capability gap rather than a specific named
customer ask: comparable gateways (e.g. Kong's Upstream OAuth plugin) already
support this natively. No specific customer has requested this by name — worth
validating before treating further investment here (additional grants,
`private_key_jwt`/mTLS support) as a confirmed priority.

## Who are we solving the problem for

The API publisher / platform operator who configures an `LlmProvider` or
`LlmProxy` resource — the person who decides how the gateway authenticates to
the upstream backend. Not the end consumer calling the gateway's public API;
that caller is unaffected by upstream auth mechanics.

## Solution

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

The gateway acts as a confidential OAuth2 client: it exchanges its own
configured credentials for an access token at a configured token endpoint,
caches it, and injects it as `Authorization: Bearer <token>` before
forwarding each request. Two grants are supported:

- **`client_credentials`** (RFC 6749 §4.4) — the default, standard
  machine-to-machine grant. Delegates entirely to
  `golang.org/x/oauth2/clientcredentials`.
- **`password`** (RFC 6749 §4.3, Resource Owner Password Credentials) —
  supported only for bridging to legacy IdPs that don't expose
  `client_credentials`. Discouraged for new integrations per current OAuth2
  security guidance, since it requires the gateway to handle a raw end-user
  credential.

`grantType` is a first-class, forward-compatible parameter so further grants
(e.g. `authorization_code`) can be added later without a breaking schema
change.

Client credentials are presented to the token endpoint per a selectable
`clientAuthMethod`:

- **`client_secret_basic`** (default) — HTTP Basic auth header, RFC 6749's
  preferred convention.
- **`client_secret_post`** — `client_id`/`client_secret` as form fields in
  the request body, required by some identity providers.

Not in scope for this iteration: `private_key_jwt` and mTLS
(`tls_client_auth`) client authentication — both require the gateway to
manage asymmetric key material or client certificates, a materially bigger
feature than this policy's current scope.

Tokens are cached in two tiers — an in-process cache, then a shared Redis
tier — keyed by the oauth2 configuration itself (not by API/route identity),
so every resource an API exposes shares one cached token, and Redis lets
every `gateway-runtime` replic  a reuse a token instead of each independently
hitting the IdP. Redis is an optimization, not a hard dependency:
`systemParameters.redis.failureMode` controls whether an outage falls back
to fetching directly from the token endpoint (`open`, default) or is treated
as an auth failure (`closed`).

### API Specification

`components.schemas.UpstreamAuth.properties.auth` in
`gateway-controller/api/management-openapi.yaml` (mirrored field-for-field
in the standalone `LLMUpstreamAuth` schema used by an `LlmProxy`'s
provider/`additionalProviders` auth):

```yaml
auth:
  type: object
  required:
    - type
  properties:
    type:
      type: string
      enum: [ api-key, oauth2 ]
    header:
      type: string
      description: HTTP header to set on outbound requests. Applies when type is api-key.
    value:
      type: string
      description: Credential value. Applies when type is api-key.
    oauth2GrantType:
      type: string
      description: >
        OAuth2 grant type. "client_credentials" (RFC 6749 4.4) is the
        standard machine-to-machine grant and should be preferred.
        "password" (RFC 6749 4.3, Resource Owner Password
        Credentials) is supported for legacy identity providers only
        - it requires the gateway to handle the resource owner's raw
        username/password directly, which current OAuth2 security
        guidance discourages for new integrations. Defaults to
        client_credentials when type is oauth2 and this field is
        omitted.
      enum: [ client_credentials, password ]
      default: client_credentials
    oauth2TokenEndpoint:
      type: string
      description: OAuth2 token endpoint URL. Required when type is oauth2.
    oauth2ClientId:
      type: string
      description: OAuth2 client ID. Required when type is oauth2.
    oauth2ClientSecret:
      type: string
      description: OAuth2 client secret. Required when type is oauth2.
    oauth2ClientAuthMethod:
      type: string
      description: >
        How oauth2ClientId/oauth2ClientSecret are presented to the
        token endpoint. "client_secret_basic" (HTTP Basic auth,
        RFC 6749's preferred convention) or "client_secret_post"
        (client_id/client_secret as form fields). Applies to both
        grants. Defaults to client_secret_basic when omitted.
      enum: [client_secret_basic, client_secret_post]
      default: client_secret_basic
    oauth2Username:
      type: string
      description: Resource owner username. Required when oauth2GrantType is password; unused otherwise.
    oauth2Password:
      type: string
      description: Resource owner password, paired with oauth2Username. Required when oauth2GrantType is password; unused otherwise.
    oauth2Params:
      type: object
      additionalProperties:
        type: string
      description: >
        Optional extra parameters appended to the token request body, as
        a flat map of string key/value pairs. Applies when type is
        oauth2 and oauth2GrantType is client_credentials (the default) -
        the password grant's request body is fixed and does not forward
        this field. There is no first-class "scope" field: if the
        identity provider needs one, add it here, e.g. {"scope":
        "chat.completions embeddings"}.
    oauth2TokenRequestTimeout:
      type: string
      description: >
        Maximum time to wait for a single token-endpoint HTTP call, as
        a Go duration string (e.g. "10s", "2500ms"). Applies to both
        grants. Defaults to "10s" when omitted.
      default: "10s"
    oauth2DefaultTokenTTL:
      type: string
      description: >
        Fallback token lifetime, as a Go duration string (e.g. "1h",
        "30m"), used only when the token endpoint's response omits
        expires_in entirely - without it, an omitted expires_in would
        silently disable caching, forcing a fresh token fetch on every
        request. Has no effect when the identity provider does return
        expires_in. Applies to both grants. Defaults to "1h" when
        omitted.
      default: "1h"
    oauth2PurgeOnUpstreamStatusCodes:
      type: array
      items:
        type: integer
        minimum: 100
        maximum: 599
      description: >
        Upstream response status codes that purge the cached token from
        both cache tiers, so the next request fetches a fresh one instead
        of reusing the same rejected token. Does not retry the request
        that triggered the purge. Defaults to [401]; 403 is deliberately
        excluded by default (usually insufficient scope, not a bad token).
        Set to an empty list to disable response-phase processing
        entirely.
      default: [401]
```

No new endpoint — this extends the existing `LlmProvider`/`LlmProxy`
create/update payload (both the nested `UpstreamAuth` used by
`LlmProvider.upstream.auth`, and the top-level `LLMUpstreamAuth` used by an
`LlmProxy`'s additional-provider auth).

### Impact on persistent data

No new tables. `oauth2` credentials are stored exactly like `api-key`'s
`value` field is today — inside the provider's existing config JSON blob.
Encryption is available today, opt-in, via the existing secrets mechanism:
create a secret (`POST /api/management/v1/secrets`) and reference it as
`'{{ secret "handle" }}'` instead of a literal, in any field — including
values nested inside the `params` map. A literal value, if used instead, is
stored and returned in plaintext (see Security Considerations below).

### Alternatives considered

- **Ask customers to run their own token-minting sidecar, or paste in a
  long-lived token.** This is the status quo the feature replaces — rejected
  because it pushes operational risk (silent token expiry, manual rotation)
  onto the customer instead of the platform absorbing it.
- **A first-class typed `scope`/`audience`/`resource` field**, matching
  Kong's typed approach, instead of a generic `params` map. Rejected for this
  iteration in favor of the more flexible generic map (lower schema-churn
  cost for arbitrary IdP-specific fields), at the cost of weaker per-field
  validation — worth reconsidering if `scope` usage turns out to dominate in
  practice.
- **A fully-templated error response** (configurable status
  code/message/body per policy instance, as Kong's plugin offers). Rejected:
  conflicts with this platform's own fixed, generic auth-failure response
  convention, which exists specifically to prevent information-leakage
  variance across policies.
- **`private_key_jwt` / mTLS client authentication.** Rejected for this
  iteration — both require the gateway to manage asymmetric key material or
  client certificates, a materially bigger feature than this policy's
  current scope.

### Surprises

- **A cache-key design choice was found to be a real vulnerability during
  implementation, not just a theoretical one.** The token cache was
  originally keyed by the API's identity, on the assumption "one API, one
  oauth2 config." That assumption breaks for an `LlmProxy` with multiple
  providers (a primary provider and `additionalProviders`, each with
  independent OAuth2 credentials, attached to the *same* API) — one
  provider's cached token could be served to another provider's backend.
  Fixed by re-keying the cache to a hash of the oauth2 configuration itself
  (mirroring Kong's own documented approach: identical config shares a cache
  entry regardless of which route/API it's attached to). A second, more
  subtle version of the same class of bug was caught live: an earlier
  iteration of the new key deliberately excluded `clientSecret`/`password`
  from the hash (reasoning: a secret rotation shouldn't invalidate a
  still-valid cached token) — but a live end-to-end test proved that two
  *different* provider configs sharing a `clientId`/`tokenEndpoint` but a
  different (e.g. deliberately wrong, for a negative test) secret would then
  wrongly share a cache entry, letting the wrong-secret config spuriously
  succeed using the other's legitimately-cached token. Both the client secret
  and the resource-owner password are now included in the key, hashed (never
  stored raw).
- **xDS route propagation timing is more variable than expected.**
  Registering a provider returns success as soon as the control plane
  persists it, but the data plane only becomes reachable after an async
  config push observed, empirically, to take anywhere from well under a
  second to several seconds under load — not instant, and not tightly
  bounded. The end-to-end test harness has to retry rather than assume a
  fixed propagation delay.

## The challenges and how the proposed solutions handle them

### Security considerations

- **Information leakage during authentication failures.** Risk: exposing
  detailed IdP errors to the client could leak backend configuration or
  credentials. Solution: the policy fails closed on every auth failure,
  returning a generic `502 Bad Gateway` with a masked message; underlying
  errors (`invalid_client`, malformed response, etc.) are restricted to
  server-side logs only.
- **Cross-credential token reuse via the cache** (see Surprises above).
  Risk: two differently-credentialed configurations sharing enough fields
  could be served each other's cached token. Solution: the cache key is a
  hash of every field that determines token issuance or entitlement —
  `grantType`, `tokenEndpoint`, `clientId`, `clientAuthMethod`, `username`,
  `params` (scope/audience/etc.), and a SHA-256 hash of
  `clientSecret`/`password` (never the raw value, consistent with how the
  Redis connection password is already handled elsewhere in this codebase).
- **Security of cached bearer tokens in Redis.** Risk: unauthorized access
  to the Redis tier could expose active bearer tokens. Solution: network
  access to Redis must be restricted to the same security posture as the
  token endpoint itself; cache entries expire with the token's own TTL, with
  no separate longer-lived retention.
- **Plaintext storage of client secrets.** Risk: a literal secret stored in
  the config blob is exposed if the database is compromised. Solution: the
  existing secrets mechanism (`{{ secret "handle" }}`) resolves secrets at
  config-render time instead of storing them in plaintext — available for
  `clientSecret`, `password`, and any value nested inside `params`. This is
  opt-in, not enforced; a literal value is still accepted and stored in
  plaintext if the publisher chooses not to use it.
- **Limited IdP compatibility with client auth methods.** Risk: hardcoding a
  single auth method excludes IdPs that require an alternate scheme.
  Solution: `clientAuthMethod` supports both `client_secret_basic` (default)
  and `client_secret_post`, covering the large majority of real IdPs with no
  hand-built HTTP code.

### Communication between data and control planes

No new channel. The oauth2 policy attaches to an API's existing
`PolicyChainConfig`, delivered to `gateway-runtime` over the same xDS
mechanism every other policy already uses.

### Running user-provided code

Not applicable — this feature runs no user-supplied code; it is a fixed
policy implementation configured via typed parameters.

### Cloud cost considerations

No new infrastructure cost beyond the Redis instance the platform's other
caching/rate-limiting policies (e.g. `advanced-ratelimit`) already depend on
— this policy shares that same Redis deployment rather than requiring its
own. The known cross-replica cache-stampede gap (see Limitations below)
means a simultaneous cold start or token expiry across N `gateway-runtime`
replicas can produce up to N redundant calls to the customer's IdP instead
of one — a minor, momentary cost, not a standing one, but worth noting if an
IdP bills per token request or rate-limits aggressively.

### Limitations imposed by current architecture

- **No cross-replica cache-stampede protection.** Within one
  `gateway-runtime` process, concurrent requests hitting an expired token
  correctly collapse onto one real IdP call (`golang.org/x/oauth2`'s own
  internal locking already guarantees this — verified directly). Across
  replicas, there is no equivalent — also verified directly: simulating N
  independent replicas racing the same expiry produced N independent IdP
  calls, not one. Accepted as a tradeoff for this workload (token requests
  are idempotent and side-effect-free); closing it would require a
  distributed lock (e.g. Redis `SETNX`-based), not implemented here.
- ~~No response-phase visibility.~~ **Resolved.** The policy now
  additionally implements `OnResponseHeaders` (`ResponseHeaderMode: Process`,
  still `ResponseBodyMode: Skip` — the status code alone is enough, so this
  stays safe for streamed upstream responses) and purges the cached token
  from both cache tiers when the upstream responds with a status in
  `purgeTokenOnUpstreamStatusCodes` (default `[401]`, mirroring Kong's
  `purge_token_on_upstream_status_codes`; `403` is deliberately excluded by
  default since it usually means insufficient scope for an otherwise-valid
  token). Response-header processing itself only turns on when this list is
  non-empty, so configurations that don't need it pay no extra per-request
  cost. This does not retry the request that triggered the purge — only the
  *next* request is guaranteed a fresh token. Implementing this surfaced a
  second bug: the per-grant `xoauth2.TokenSource` built in `buildTokenSource`
  (a `clientcredentials.Config.TokenSource`/`ReuseTokenSource` wrapper) keeps
  its own internal cached token independent of this policy's two-tier cache,
  so purging only the two tiers wasn't enough to force a real refetch before
  that inner token's own natural expiry — confirmed by an end-to-end test
  that primes the cache, purges it, and asserts a second real token-endpoint
  call happens. Fixed by having `Purge()` rebuild the inner token source via
  `buildTokenSource` under the same mutex that guards the two-tier cache,
  rather than only clearing local/Redis.
- **Backward compatibility.** This extends the existing typed
  `upstream.auth` field (previously `api-key` only) with a second value
  rather than introducing a new field or endpoint — existing `api-key`
  configurations are unaffected.

### Need for high-performance or low latency

The two-tier cache (in-process, then Redis) exists specifically so the hot
path — a token already cached and valid — never pays a network round trip to
the IdP, and a shared-cache miss only costs one round trip to Redis before
falling back to the IdP. A `tokenRequestTimeout` (default `10s`) bounds a
single token-endpoint call so an unresponsive IdP cannot block a request
indefinitely.

### Key tradeoffs

- **Generic `params` map vs. typed `scope`/`audience` fields.** Chosen: a
  generic, unvalidated map, favoring flexibility over per-field UX/validation
  (see Alternatives Considered).
- **`params` support is `client_credentials`-only.** The password grant's
  underlying library helper has no equivalent extension hook; supporting it
  there would require hand-building a replacement HTTP client purely for a
  rarely-needed field on an already-discouraged grant. Accepted as a
  self-imposed capability gap.
- **Excluding vs. including credentials in the cache key.** Excluding
  `clientSecret`/`password` from the cache key would make a secret rotation
  "free" (no forced re-fetch), but was proven live to allow a
  wrong-credential configuration to spuriously succeed by reusing another
  configuration's cached token. Including them (hashed) costs one extra
  token fetch after a credential rotation — judged the correct tradeoff,
  since it closes a real cross-credential reuse gap for a negligible,
  one-time cost.
- **No cross-replica cache-stampede protection, and no response-phase token
  eviction on upstream rejection** (see Limitations above) — both accepted
  for now as lower-value than shipping the core feature, both documented as
  the highest-value remaining follow-ups.

### Scaling with Choreo/Asgardeo/Product

No specific integration with Choreo or Asgardeo control planes — this ships
as a standard gateway policy delivered over the existing `PolicyChainConfig`
xDS channel, so it works identically regardless of which control-plane
product deploys the gateway. No product-specific scaling concerns
identified.

---

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
- Cache isolation across independently-credentialed providers sharing an
  API (the cache-key fix described in Surprises above), and cache *sharing*
  across independently-registered APIs with byte-identical oauth2 config
  (proven live: two APIs received the exact same bearer token string)
- `purgeTokenOnUpstreamStatusCodes` (response-phase token purging) —
  verified with Go-level integration tests (prime cache → purge → assert a
  second real token-endpoint call happens), including the inner-token-source
  rebuild fix described in Limitations above. Not yet re-run through the
  live Docker/E2E harness the bullets above went through.

Shipped as the `oauth2-upstream-authentication` policy `v0.8.0` in both `gateway-controllers/policies/oauth2-upstream-authentication`
and the in-repo mirror `gateway/dev-policies/oauth2-upstream-authentication` (kept byte-identical —
see the dual-repo gotcha this caused mid-implementation, captured in team
memory as it isn't otherwise enforced by tooling).

`TESTING.md` and the companion Postman collection
(`gateway/dev-policies/oauth2-upstream-authentication/e2e/postman/oauth2.postman_collection.json`)
cover the happy path, caching, expiry/refresh, all three failure modes,
`client_secret_post`, `tokenRequestTimeout`, `defaultTokenTTL`, and the two
cache-isolation/sharing scenarios above. All of `TESTING.md`, the mocks, and
the Postman collection live under a single `e2e/` subdirectory, with
`e2e/run-e2e.sh` as a one-command runner that starts both mocks and runs the
full collection via newman against an already-running gateway stack,
retrying the register-through-cleanup cycle if xDS propagation doesn't land
in time (see Surprises above).

**Note:** `gateway/dev-policies/` is gitignored repo-wide
(`.gitignore:165: dev-policies/`) — none of the dev-policies-side work above
(the mirrored policy code, `TESTING.md`, the Postman collection, the mock
changes, the `e2e/` test harness) is tracked in git or visible in a diff/PR.
It's used by the local Docker build/test workflow but isn't shared via
`git clone`. Worth revisiting if the intent is for other developers to get
this test tooling for free.

## Open Follow-ups

- `private_key_jwt` / mTLS client authentication — bigger feature, not
  scoped here.
- ~~Token purging on upstream rejection~~ **Shipped** — see Limitations above.
- No cross-replica distributed lock for token fetches (see Limitations
  above) — accepted tradeoff for now, revisit if IdP request volume from
  simultaneous cache misses becomes a real cost concern.
- `headers` alongside `params` (extra headers on the token request, not just
  body fields) — Kong supports this, we don't.
- No named customer or competitive reference is attached to the original
  motivation for this feature — worth attaching one before treating this as
  a validated priority for further investment (e.g. `authorization_code`
  support, additional grants).

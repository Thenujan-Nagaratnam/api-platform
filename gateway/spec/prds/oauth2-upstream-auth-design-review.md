# OAuth2 Upstream Auth — Design Review Notes

Companion to the full PRD: `oauth2-upstream-auth.md`. Bullet-form talking
points only — see the PRD for prose/rationale.

## Problem

- `upstream.auth` only supports a static pre-shared API key.
- No way to reach an OAuth2-secured backend without a customer-run
  token-minting sidecar or a pasted long-lived token.
- Competitive gap (Kong Upstream OAuth plugin has this), not a named
  customer ask — worth validating before further investment.

## Who it's for

- API publisher / operator configuring `LlmProvider` / `LlmProxy`.
- Not the end caller — upstream auth is invisible to them.

## Architecture

```
Register LlmProvider or LlmProxy (auth.type: oauth2)
  → gateway-controller validates params, attaches upstream-oauth2-authentication policy
  → xDS (existing PolicyChainConfig channel, nothing new)
  → gateway-runtime: local cache? → Redis cache? → fetch from IdP
  → inject `Authorization: Bearer <token>`, forward upstream
  → upstream responds → status in tokenPurgeStatusCodes (default [401])?
      → yes: purge cached token (both tiers), response still passes through
      → no: pass response through unchanged
```

- No new endpoint, no new xDS channel, no new tables.
- Credentials stored in the existing config JSON blob, same as `api-key`
  today. Opt-in encryption via existing `{{ secret "handle" }}` mechanism.

## Grants & client auth

| | |
|---|---|
| `grantType` | `client_credentials` (default) or `password` |
| `client_credentials` | RFC 6749 §4.4, delegates to `golang.org/x/oauth2/clientcredentials` |
| `password` | RFC 6749 §4.3, legacy-IdP bridging only, discouraged for new use |
| `clientAuthMethod` | `client_secret_basic` (default, HTTP Basic) or `client_secret_post` (form fields) |
| Not in scope | `private_key_jwt`, mTLS (`tls_client_auth`) — needs key/cert management, bigger feature |

## Caching — two tiers

1. In-process (local) — near-zero latency, per-replica.
2. Redis (shared) — cross-replica reuse, survives replica restart.

- Redis is an optimization, not a hard dependency.
- `redis.failureMode`: `open` (default, falls back to token endpoint after
  local-cache miss) or `closed` (Redis error ⇒ `502`).
- TTL = token's own `expires_in`; falls back to `defaultTokenTTL` (default
  `1h`) only when the IdP omits it.

### Cache key — evolved across versions (real bug history, not just design)

| Design | Key | Problem |
|---|---|---|
| v0.4 | `<prefix><apiId>:<routeName>:<grantType>` | route-scoped, no credential in key |
| v0.5–v0.7 | `<prefix><apiId>` | apiId falls back to route name → collision risk |
| **v0.8 (current)** | SHA-256 hash of `grantType`, `tokenEndpoint`, `clientId`, `clientAuthMethod`, `username`, `params`, hash(`clientSecret`), hash(`password`) | fixed |

- Current design keys on **config identity**, not API/route identity.
- Two different APIs with identical oauth2 config → share a token (correct).
- One API with two different oauth2 configs (primary + `additionalProviders`)
  → never collide (this was the real bug).
- Any config change (secret rotation, grant change, scope change) →
  different hash → old entry just ages out, never reused.
- Secrets go into the key **hashed** (SHA-256), never raw — Redis key names
  leak into `MONITOR`/slowlog.

## Bugs found & fixed this cycle

- **Cross-credential cache collision (critical, found live).** apiId-only
  key let a wrong-secret config reuse another config's cached token — 200
  instead of expected 502. Caught by an actual E2E run, not review. Fixed by
  hashing `clientSecret`/`password` into the key.
- **Data race on shared token pointer.** `Token()` mutated `tok.Expiry` in
  place on the same `*Token` the inner `ReuseTokenSource` retains and can
  hand to a concurrent caller with no lock held. Fixed: copy before mutating.
- **Redis/Basic hashing duplication.** `hashSecret` duplicated
  `hashRedisPassword` — consolidated to one `hashSensitiveValue`.
- **E2E harness flakiness.** Fixed `trap` ordering, a 404-vs-000
  false-positive in readiness polling, a silent-failure gap in a
  recreate step, and a too-tight fixed sleep (measured propagation lag
  live, bumped 5s → 10s).
- **8 stale/inaccurate doc claims** across `v0.3`–`v0.8` docs (found via
  review, verified against actual source, fixed or annotated as historical
  limitation — not silently rewritten).
- **Purge not actually forcing a refetch (found while implementing response-phase purging).**
  Clearing the two-tier cache alone left `buildTokenSource`'s own inner
  `ReuseTokenSource` still serving its own cached token until its natural
  expiry. Caught by an end-to-end test, not review. Fixed by rebuilding the
  inner token source on `Purge()`.

## Failure modes

| Failure | Mode | Result |
|---|---|---|
| Token endpoint unreachable / bad response | always | `502`, generic message |
| Redis unavailable | `open` (default) | falls back to token endpoint |
| Redis unavailable | `closed` | `502` |
| Any auth failure | always | generic `502`, no detail leaked to client; real reason logged server-side only |

## Known limitations (not fixed, by design/scope)

- **No cross-replica stampede protection.** N replicas racing an expiry ⇒ N
  IdP calls, not 1. Verified directly. Would need distributed lock
  (Redis `SETNX`) — not implemented.
- ~~No response-phase visibility~~ **Resolved** — see Response-phase token
  purging below.
- **`params` is `client_credentials`-only, except `scope`** — `scope`
  specifically is mapped to `Config.Scopes` for the password grant too (the
  one extension point `PasswordCredentialsToken` actually has); every other
  `params` key (`audience`, `resource`, `tenant`, ...) still has no effect
  on that grant.
- Both accepted as lower-value than shipping the core feature.

## Response-phase token purging (shipped)

- `OnResponseHeaders` purges both cache tiers when the upstream responds
  with a status in `tokenPurgeStatusCodes` (default `[401]`,
  mirrors Kong's `purge_token_on_upstream_status_codes`; `403` excluded by
  default — usually insufficient scope, not a bad token).
- Response-header processing (`ResponseHeaderMode: Process`) only turns on
  when the list is non-empty; body is always `Skip` — status code alone is
  enough, so this stays safe for streamed upstream responses.
- Does **not** retry the request that triggered the purge — only the next
  request is guaranteed a fresh token.
- **Real bug caught while implementing this, not just a design nuance:**
  purging local + Redis alone wasn't enough. `buildTokenSource`'s per-grant
  `xoauth2.TokenSource` (a `clientcredentials.Config.TokenSource`/
  `ReuseTokenSource` wrapper) keeps its *own* internal cached token,
  independent of this policy's two-tier cache — so the next `Token()` call
  after a purge would still return the same rejected token until that inner
  cache's own natural expiry. Caught by an end-to-end test asserting a
  second real token-endpoint call happens post-purge, not by inspection.
  Fixed: `Purge()` now rebuilds the inner token source via
  `buildTokenSource` under the same mutex guarding the two-tier cache.
- Exposed on the typed schema as `oauth2TokenPurgeStatusCodes` (both
  `UpstreamAuth` and `LLMUpstreamAuth`) — nil (field omitted) vs. an
  explicit empty list both carry meaning end to end (Go struct field →
  transformer → policy param), the latter being how a publisher disables
  response-phase processing entirely rather than getting the `[401]`
  default.
- Verified with Go-level tests only so far (not re-run through the live
  Docker/E2E harness the rest of this feature went through).

## Testing / verification status

- Live E2E (Docker, real mock IdP + backend), not just unit tests.
- Verified live: both grants, both `clientAuthMethod`s, `params` (incl.
  secret-reference value), Redis caching both failure modes,
  `tokenRequestTimeout` (502 in ~526ms vs 3s IdP delay),
  `defaultTokenTTL` (3 requests ⇒ 1 IdP call when `expires_in` omitted),
  cache isolation across differently-credentialed providers on one API,
  cache sharing across identically-configured providers on different APIs.
- `go test -race` clean after the data-race fix.
- One-command runner: `gateway/dev-policies/upstream-oauth2-authentication/e2e/run-e2e.sh`.
- **Note:** `gateway/dev-policies/` is gitignored repo-wide — none of this
  test tooling is in git/visible in a PR diff.

## Key tradeoffs

- Generic `params` map vs. typed `scope`/`audience` fields → chose generic
  (flexibility over validation).
- Credentials excluded vs. included in cache key → included (hashed); proven
  live that excluding them is an actual vulnerability, not just a nuance.
- No stampede lock → accepted for now, logged as the remaining top
  follow-up (response-phase eviction shipped — see above).

## Shipped

- `upstream-oauth2-authentication` policy, `v0.8.0`.
- Lives in `gateway-controllers/policies/upstream-oauth2-authentication`, mirrored byte-identical
  into `gateway/dev-policies/upstream-oauth2-authentication` (dual-repo — must stay in sync
  manually, no tooling enforces it).

## Open follow-ups (priority order)

1. ~~Token purging on upstream rejection~~ **Shipped.**
2. Cross-replica stampede lock.
3. `private_key_jwt` / mTLS client auth.
4. `headers` support alongside `params`.
5. Attach a named customer/competitive reference before treating further
   grants as a validated priority.

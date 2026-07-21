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
Register LlmProvider (upstream.auth.type: oauth2)
  → gateway-controller validates params, attaches oauth2 policy
  → xDS (existing PolicyChainConfig channel, nothing new)
  → gateway-runtime: local cache? → Redis cache? → fetch from IdP
  → inject `Authorization: Bearer <token>`, forward upstream
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
- **No response-phase visibility.** Policy is request-header-only. Can't
  detect/evict a token the upstream rejects (e.g. revoked out-of-band) —
  stale token keeps being served until its own TTL expires. Kong has
  `purge_token_on_upstream_status_codes`; we don't. **Highest-value
  follow-up.**
- **`params` is `client_credentials`-only** — password grant's library
  helper has no equivalent extension hook.
- Both accepted as lower-value than shipping the core feature.

## Testing / verification status

- Live E2E (Docker, real mock IdP + backend), not just unit tests.
- Verified live: both grants, both `clientAuthMethod`s, `params` (incl.
  secret-reference value), Redis caching both failure modes,
  `tokenRequestTimeout` (502 in ~526ms vs 3s IdP delay),
  `defaultTokenTTL` (3 requests ⇒ 1 IdP call when `expires_in` omitted),
  cache isolation across differently-credentialed providers on one API,
  cache sharing across identically-configured providers on different APIs.
- `go test -race` clean after the data-race fix.
- One-command runner: `gateway/dev-policies/oauth2-upstream-authentication/e2e/run-e2e.sh`.
- **Note:** `gateway/dev-policies/` is gitignored repo-wide — none of this
  test tooling is in git/visible in a PR diff.

## Key tradeoffs

- Generic `params` map vs. typed `scope`/`audience` fields → chose generic
  (flexibility over validation).
- Credentials excluded vs. included in cache key → included (hashed); proven
  live that excluding them is an actual vulnerability, not just a nuance.
- No stampede lock, no response-phase eviction → accepted for now, both
  logged as top follow-ups.

## Shipped

- `oauth2-upstream-authentication` policy, `v0.8.0`.
- Lives in `gateway-controllers/policies/oauth2-upstream-authentication`, mirrored byte-identical
  into `gateway/dev-policies/oauth2-upstream-authentication` (dual-repo — must stay in sync
  manually, no tooling enforces it).

## Open follow-ups (priority order)

1. Token purging on upstream rejection (response-phase hook needed).
2. Cross-replica stampede lock.
3. `private_key_jwt` / mTLS client auth.
4. `headers` support alongside `params`.
5. Attach a named customer/competitive reference before treating further
   grants as a validated priority.

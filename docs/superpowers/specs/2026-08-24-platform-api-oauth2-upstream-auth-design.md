# Platform API: OAuth2 Upstream Auth Support — Design

## Context

The AI Workspace UI (see the companion spec,
`2026-08-24-ai-workspace-oauth2-upstream-auth-design.md`) now lets a user
select `oauth2` as the upstream auth type for LLM Providers, LLM Proxies,
and MCP servers, and sends the full `{type, policyName, policyParams}`
shape gateway-controller's own `UpstreamAuth`/`LLMUpstreamAuth` schema has
supported for some time.

Live testing surfaced that this silently fails: Platform API — the actual
backend the AI Workspace frontend talks to, not gateway-controller directly
— has its own, older `UpstreamAuth` shape with only `type`/`header`/`value`,
and `type`'s enum doesn't even include `oauth2`. A PUT carrying
`policyParams` returns `200 OK` (Platform API's Go JSON decoder silently
drops unrecognized fields — there is no strict schema-validation middleware
in this codebase), but the persisted provider ends up as
`{"auth": {"type": "oauth2"}}` — no credentials, unusable, no error
surfaced anywhere. Confirmed directly: PUT request body captured from the
live UI included a full `policyParams` object; the subsequent GET showed
it gone.

This spec covers making that field survive the trip: Platform API request
→ DB model → deployment payload handed to gateway-controller.

## Goals

- `oauth2` becomes a valid `UpstreamAuth.type` value in Platform API's
  OpenAPI contract, matching gateway-controller's existing semantics.
- `policyName`/`policyParams`/`policyVersion` survive create, update, get,
  and — critically — the deployment payload Platform API hands to
  gateway-controller, for all three resource kinds that share this type:
  LlmProvider, LlmProxy, MCP.
- No plaintext secret ever gets logged, stored, or round-tripped outside
  the existing `{{ secret "..." }}` placeholder convention this repo
  already uses for `value`.

## Non-goals

- Reconciling Platform API's `type` enum having `basic`/`bearer` values
  gateway-controller's own enum doesn't have — pre-existing, unrelated
  inconsistency, not touched here.
- Any change to gateway-controller itself (already fully supports this
  shape) or to the AI Workspace frontend (already sends the right shape).
- New secret-ref validation code — `ValidateSecretRefs`
  (`internal/service/secret_service.go:273`) scans the *entire* marshaled
  request JSON for `{{ secret "..." }}` placeholders via
  `marshalUpstreamForValidation(req)` (`internal/service/llm.go:3368`,
  called with the whole `*api.LLMProvider`, not just `.Upstream`) — this is
  not field-scoped, so it already covers `policyParams` once the field
  exists on the struct. Confirmed by reading both functions directly; a
  test proves it rather than just asserting it (see Testing).

## Architecture

One shared `UpstreamAuth` type threads through 3 layers, referenced from
3 places in the OpenAPI spec (`resources/openapi.yaml:6796`, `:8030`,
`:8425` — one for each of LlmProvider/LlmProxy/MCP's upstream endpoint) but
defined once (`:6798`), and reused as one shared Go type at every layer:

```
AI Workspace UI
      │  PUT/POST {type, header, value, policyName, policyParams, policyVersion}
      ▼
platform-api/api/generated.go  →  api.UpstreamAuth        (openapi.yaml:6798, generated)
      │  mapUpstreamAPIToModel / toProviderAPI (internal/service/llm.go)
      ▼
platform-api/internal/model/upstream.go  →  model.UpstreamAuth   (DB-persisted, JSON blob column)
      │  mapModelAuthToAPI (internal/service/llm_deployment.go:1929)
      ▼
deployment payload  →  api.UpstreamAuth (same generated type, reused)
      │  sent to gateway-controller
      ▼
gateway-controller's own UpstreamAuth/LLMUpstreamAuth  (already supports oauth2 — no change)
```

Because LlmProxy and MCP reuse the exact same `api.UpstreamAuth` /
`model.UpstreamAuth` types and call largely the same helper functions
(`preserveUpstreamAuthValue`, `mainUpstreamAuthValue`,
`normalizeUpstreamAuthType`, `isCredentialLessUpstreamAuthType`), widening
the shared type and its core helpers fixes all three resource kinds
together — each resource's own call sites still get individually verified
and tested, not assumed fixed by proxy.

## Changes

### 1. OpenAPI schema (`platform-api/resources/openapi.yaml:6798`)

Add to the existing `UpstreamAuth` component:
- `oauth2` added to the `type` enum (`[basic, bearer, api-key, oauth2,
  other, none]`).
- `policyName` (string, optional).
- `policyParams` (object, `additionalProperties: true`, optional —
  required-when-oauth2/other is a semantic/handler-level rule, not a
  schema-level `required`, matching how gateway-controller's own spec
  documents this).
- `policyVersion` (string, optional, pattern `^v\d+$` matching
  gateway-controller's convention).

Regenerate `platform-api/api/generated.go` via the existing Makefile
target (`oapi-codegen` v2.5.1 pipeline, confirmed runnable:
`Makefile:157-159`).

### 2. Internal DB model (`platform-api/internal/model/upstream.go`)

Add the same three fields to `model.UpstreamAuth`, `db:"-"` tagged like the
existing fields (confirmed: the whole `Configuration` is JSON-serialized
into one DB column — **no database migration required**).

### 3. Mapper/helper functions (`platform-api/internal/service/llm.go`,
`internal/service/llm_deployment.go`, `internal/service/mcp.go`)

Each of these currently only reads/writes `Type`/`Header`/`Value` and
needs the same treatment for the 3 new fields — verify each individually,
not by assumption:
- `mapUpstreamAPIToModel` / `toProviderAPI` (`llm.go`) — the two-way
  mapper between REST payload and DB model; this is where the live test's
  `policyParams` was silently dropped.
- `preserveUpstreamAuthValue` (`llm.go:2012`) — currently preserves only
  `Value` when an update submits an empty one; extend to preserve
  `PolicyParams` the same way, so a partial update can't accidentally wipe
  stored oauth2 config.
- `mapModelAuthToAPI` (`llm_deployment.go:1929`) — builds the deployment
  payload sent to gateway-controller; must copy the new fields through, or
  a correctly-stored oauth2 config still never reaches the gateway.
- `defaultUpstreamAuthToNone`, `isCredentialLessUpstreamAuthType`,
  `mainUpstreamAuthType`, `mainUpstreamAuthValue`,
  `normalizeUpstreamAuthType` — audit each; most look type-agnostic
  already (they key off `Type` as a string, not a fixed set), but confirm
  rather than assume.
- MCP's own call sites (`mcp.go`, e.g. `preserveMCPUpstreamAuthValue`,
  the `api.UpstreamAuth` construction around `mcp.go:799-829`) — reuses
  the shared type/helpers, but gets its own explicit verification and test
  coverage, not a "fixed by proxy" assumption.
- LlmProxy's own call site (`llm_deployment.go:1878`,
  `proxyDeployment.Spec.Provider.Auth = mapModelAuthToAPI(...)`) — already
  calls the shared mapper being fixed in this same change; verify with its
  own test.

### 4. Out of scope, confirmed dead code

`platform-api/internal/dto/upstream.go`'s separate `UpstreamAuth`/
`UpstreamConfig`/`UpstreamEndpoint` types — confirmed via
`grep -rln "dto\.UpstreamAuth\b"` to have zero call sites anywhere in the
codebase. Not touched by this change; flagged here only so a future reader
doesn't wonder why a fourth copy of this shape was left alone.

## Testing

This repo (unlike the AI Workspace frontend) has normal Go unit test
coverage. For each touched function: a table-driven test covering
`oauth2`/`other` alongside the existing `api-key`/`none` cases. Plus one
round-trip test per resource kind (LlmProvider, LlmProxy, MCP):
create with `policyParams` → get → assert unchanged → update (omitting
`policyParams`) → assert preserved → assert the constructed deployment
payload (`mapModelAuthToAPI`'s output) carries the same `policyParams`.
One test proving `ValidateSecretRefs` catches a raw (non-placeholder)
value inside `policyParams.clientSecret`, confirming the Non-goals claim
about existing coverage rather than leaving it asserted-only.

## Risks

- **Enum widening on a shared, possibly-GA-shipped schema.** Per this
  repo's `db-schema-changes.md` convention for *database* schemas, GA
  products treat shipped shapes as frozen for anything but additive
  changes. This is an OpenAPI/Go-struct change, not a DB schema change
  (confirmed no migration needed), but the same spirit applies: adding an
  enum value and new optional fields is additive and backward compatible
  (existing `api-key`/`none`/`other` clients are unaffected); this is not
  a breaking change to the existing contract.
- **Silent-drop-of-unknown-fields is the root cause and stays true for the
  *next* missing field too.** Out of scope to add strict request
  validation repo-wide here, but worth naming as a systemic gap this bug
  class will recur from until addressed separately.

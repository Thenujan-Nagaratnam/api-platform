# AI Workspace: OAuth2 Upstream Auth (UI) — Design

## Context

`gateway-controller` already fully supports `type: oauth2` upstream auth for
`LlmProvider`/`LlmProxy`/`Mcp` resources (validators, transformers, credential
inheritance, CLI `ap ai-workspace build`, OpenAPI spec, IT coverage). The AI
Workspace portal (`portals/ai-workspace`) never got wired up to it: every
upstream-connection form (LLM Provider connect/create, LLM Proxy
connect/create, MCP/External Servers) only offers `none`/`api-key`/`other`,
and `other` is a dead end (no policy picker, no param form). This spec covers
exposing the existing backend capability through the AI Workspace UI.

Scope is UI-only. No gateway-controller, CLI, or OpenAPI spec changes.

## Goals

- Add `oauth2` as a selectable upstream auth type everywhere `api-key` is
  offered today: LLM Provider (create + edit), LLM Proxy (create + edit),
  MCP/External Servers (create — this surface has no type selector at all
  today).
- Render `oauth2`'s parameters (`tokenEndpoint`/`clientId`/`clientSecret`/
  `grantType`/etc.) from the real `oauth2-generator` policy schema, not a
  hand-maintained second copy of that schema.
- Fix LLM Proxy's auth-inheritance copy logic, which currently drops
  `policyParams` when copying a provider's auth on provider switch.
- Get `other` working as a side effect (same schema-fetch/render pipeline,
  keyed off a user-picked policy name instead of the fixed `oauth2-generator`).

## Non-goals

- Editing `oauth2-generator`'s own schema/behavior.
- A generic "attach any built-in policy as upstream auth for any resource"
  admin UI beyond what's needed for oauth2/other in these three surfaces.
- Seeding oauth2 `policyParams` defaults from provider templates.

## Architecture & data flow

### Resolving the policy schema

`oauth2-generator` is a built-in policy shipped in every gateway image, so it
appears in a connected gateway's self-reported manifest exactly like any
custom policy. Reuse the existing Guardrails pipeline rather than building a
new one:

1. `getGatewayCustomPolicies()` — check the org's already-synced list for a
   policy named `oauth2-generator`.
2. If absent: `getGatewayPolicyManifest(gatewayId)` → find `oauth2-generator`
   in the manifest → `syncGatewayCustomPolicy(gatewayId, 'oauth2-generator',
   version)` to pull its `policyDefinition` into the org's synced list. This
   sync must happen transparently (no manual admin prerequisite) since it's a
   built-in policy the feature depends on — the calling component triggers it
   automatically on first need, not the user.
3. Cache the resolved `PolicyDefinition` (module-level cache or a small
   context) — every surface needs the same definition, no need to refetch per
   form mount.

`gatewayId` resolution: reuse whatever the current organization/gateway
selection context (`AppShellContext`) already exposes; if AI Workspace has no
notion of "the org's gateway" at this call site today, resolve it the same
way the gateway-custom-policy sync flow elsewhere in the app does (open
question — confirm during implementation; see Risks).

### Save/load shape

All three resources already share one wire shape for upstream auth:
`{ type, policyName?, policyVersion?, policyParams? }`. The oauth2/other
branch of the form only ever populates `policyParams` (+ `policyName` for
`other`); `api-key` keeps its existing typed fields.

## Shared component: `UpstreamAuthFields`

One component, mounted by every surface below.

**Type selector**: `none` / `api-key` / `oauth2` / `other`. For `other`, a
second dropdown lists the org's synced custom policies
(`getGatewayCustomPolicies()`) to pick `policyName`.

**Per-type rendering**:
- `none` — nothing.
- `api-key` — unchanged: existing typed fields (header, value, key
  location/prefix). Not migrated to the schema-driven path.
- `oauth2` — fetch `oauth2-generator`'s definition (above), render via
  `<PolicyParameterEditor parameters={definition.parameters}
  existingValues={auth.policyParams} onSubmit={...} />`. Basic/advanced
  split and required-field validation come from the schema's
  `x-wso2-policy-advanced-param`/`required` metadata for free.
- `other` — same, keyed off the user-picked `policyName`'s definition.

**Secret handling**: extract the existing MCP auto-secret logic
(`ExternalServersNew.tsx`'s `createSecret` + `buildSecretPlaceholder` +
`generateSecretHandle`) into a shared helper. On submit, run it over
`policyParams`: any value for a known-sensitive key (`clientSecret`,
`bearerToken`, `password` — the fixed, known-sensitive fields in
`oauth2-generator`'s schema) that isn't already a `{{ secret "..." }}`
placeholder gets auto-wrapped into a newly created secret before the payload
is sent. `PolicyParameterEditor`'s `schemaUtils.isTemplateExpression` already
recognizes the placeholder shape and skips constraint validation on it, so no
changes needed there.

**Masking on read**: like the existing api-key credential field
(`MASKED_CREDENTIAL_VALUE`), a resolved `{{ secret ... }}` value read back
from the API must render masked, not as the literal placeholder string. Pass
a masked display value for the known-sensitive keys when hydrating
`existingValues` into `PolicyParameterEditor`.

## Per-surface integration

**LLM Provider — `ServiceProviderNew.tsx` (create), `ServiceProviderConnectionTab.tsx` (edit).**
Replace the current flat local state (`upstreamAuthType`/
`upstreamAuthHeader`/`upstreamAuthValue`/`valuePrefix`) with
`UpstreamAuthFields`'s draft state, and change the submit payload builder to
include `policyName`/`policyVersion`/`policyParams` for oauth2/other instead
of always building `{type, header, value}`. Provider-template auth defaults
(`template.metadata.auth.type`) remain the seeding mechanism for new
providers; seeding `policyParams` defaults from a template is out of scope.

**LLM Proxy — `LLMProxyNew.tsx` (create), `LLMProxyProviderTab.tsx` (edit).**
Proxies only ever override `api-key`'s credential value; every other type is
inherited from the selected provider via backend `credential_inheritance.go`.
Two changes:
1. Fix `handleProviderChange`'s copy logic (currently rebuilds `nextAuth`
   from only `type`/`header`/`value`, dropping `policyParams`) to copy the
   provider's `auth` object verbatim.
2. When the effective/inherited type is oauth2/other, render
   `UpstreamAuthFields` in read-only mode (`PolicyParameterEditor`'s
   existing `readOnly` prop) with an "inherited from provider" note. Only
   `api-key` keeps its editable per-proxy override field.

**MCP / External Servers — `ExternalServersNew.tsx`.** Net-new: today there's
no type selector, just a single header/value pair. Mount
`UpstreamAuthFields` here too.

## Types

Replace the LLM-only `UpstreamAuth` and the MCP-only `MCPServerUpstreamAuth`
with one shared type:

```ts
export interface UpstreamAuthConfig {
  type: 'none' | 'api-key' | 'oauth2' | 'other' | string;
  header?: string;       // api-key only
  valuePrefix?: string;  // api-key only
  value?: string;        // api-key only, write-only
  policyName?: string;   // oauth2/other
  policyVersion?: string;
  policyParams?: Record<string, unknown>; // oauth2/other
}
```

## Validation

Mirror the backend's rules (`llm_validator.go`/`mcp_validator.go`) at the UI
layer so users get inline feedback before submit, not just a server error:
- `oauth2`/`other` require `policyParams` non-empty.
- `other` requires `policyName`.
- Within `oauth2-generator`'s own schema: `tokenEndpoint`+`clientId`+
  `clientSecret` vs `bearerToken` are mutually exclusive paths — surfaced via
  the schema's existing `anyOf` handling in `PolicyParameterEditor`
  (`validateLevelOneRequiredFields`'s anyOf logic), no new validation code
  needed if the real schema already encodes this as `anyOf`. Confirm during
  implementation; if the schema doesn't encode it, that's a policy-definition
  gap to flag rather than a UI-side workaround.
- Server-side validation remains authoritative; UI validation is best-effort
  UX, not a security boundary.

## Testing

- Unit tests for the new shared helpers (secret auto-wrap, policy-schema
  resolution/cache, the `UpstreamAuthConfig` payload builder) per existing
  conventions in the affected directories.
- Component tests for `UpstreamAuthFields` covering: type switch behavior,
  oauth2 schema fetch + render, secret auto-wrap on submit, masked read-back.
- Update/extend existing tests for `ServiceProviderNew`/`ServiceProviderConnectionTab`/
  `LLMProxyProviderTab`/`ExternalServersNew` for the new type option and
  (for the proxy tab) the inheritance-copy fix.
- Manual verification against a real gateway with `oauth2-generator` in its
  manifest, confirming end-to-end: create provider with oauth2 → deploy →
  proxy inherits it correctly → MCP server with oauth2 also works.

## Risks / open questions

- **Gateway-manifest sync dependency**: the design assumes `oauth2-generator`
  can be auto-synced from a connected gateway's manifest transparently on
  first need. Confirm during implementation that (a) AI Workspace's
  current context has a resolvable `gatewayId` at every one of these three
  form locations, and (b) the auto-sync-if-missing call is safe to fire from
  a form-mount effect (idempotent, cheap) rather than needing an explicit
  user action.
- **`anyOf` in `oauth2-generator`'s schema — RESOLVED (2026-08-25).** The
  schema does *not* use `anyOf` for this: it already encodes the
  tokenEndpoint/clientId/clientSecret-vs-bearerToken exclusivity as a
  top-level `oneOf` with `not` clauses, and the grantType-conditional
  username/password requirement as `allOf`/`if`/`then` — real JSON Schema,
  correctly enforced server-side, but a vocabulary `PolicyParameterEditor`
  didn't parse at all (only `anyOf` was implemented). Fix: extended
  `ParameterSchema`/`schemaUtils.ts`/`PolicyParameterEditor.tsx` with a
  narrow `SchemaCondition`/`SchemaConditional` model (`required`/`const`/
  `not` composition only — not a general JSON Schema engine) covering
  top-level `oneOf` and `allOf`/`if`/`then`, feeding both the submit-button
  gating (`validateLevelOneRequiredFields`) and per-field inline required
  errors (`validateRequiredFields`), plus two new info/error alerts mirroring
  the existing `anyOf` alert's UX. Nested (non-top-level) `oneOf`/`allOf`
  remain unsupported — no current schema needs it.
- **Secret handling / masking on read — DEFERRED (2026-08-25).** The
  "Secret handling" and "Masking on read" sections above describe intended
  behavior that is explicitly **not** built yet: today `clientSecret`/
  `bearerToken`/`password` render as plain unmasked text fields and submit as
  raw plaintext in `policyParams`, with no auto-wrap/masking. User decision:
  leave this for a deliberate future pass rather than bolt it on now. See
  memory `project_oauth2_policyparams_secret_gap_deferred`. Don't treat the
  sections above as already implemented.

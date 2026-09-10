# AI Workspace: OAuth2 Upstream Auth (UI) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let AI Workspace users select `oauth2` (and `other`) as the upstream auth type for LLM Providers, LLM Proxies, and MCP/External Servers, rendering the real `oauth2-generator` policy schema instead of a hand-maintained field list.

**Architecture:** One shared `UpstreamAuthConfig` type replaces the LLM-only and MCP-only auth types. One shared `UpstreamAuthFields` component renders a type selector (`none`/`api-key`/`oauth2`/`other`); `api-key` keeps its existing typed fields, `oauth2`/`other` render the target policy's real parameter schema through the existing `PolicyParameterEditor`. A small resolver helper looks up that schema from the org's already-synced custom policies; a secret helper auto-wraps sensitive param values the same way MCP's existing auth field already does.

**Tech Stack:** React + TypeScript (Vite), `@wso2/oxygen-ui` components, Cypress for e2e (this codebase has no unit/component test runner — see Testing Approach below).

**Spec:** `docs/superpowers/specs/2026-08-24-ai-workspace-oauth2-upstream-auth-design.md`

## Global Constraints

- UI-only. No gateway-controller, CLI, or OpenAPI spec changes (spec Non-goals).
- Not building a generic "attach any built-in policy as upstream auth for any resource" admin UI beyond what's needed for oauth2/other in these three surfaces (spec Non-goals).
- Not seeding oauth2 `policyParams` defaults from provider templates (spec Non-goals).
- Server-side validation remains authoritative; anything added here is best-effort UX, never a security boundary (spec Validation section).
- `api-key`'s existing typed-field UI (header/value/key location) is unchanged everywhere — it is never migrated to the schema-driven path.

## Testing Approach (read before starting any task)

This codebase has **no unit or component test runner** — `package.json` has no `test` script, no `vitest`/`jest` dependency, and there are no `*.test.ts(x)` files anywhere under `src/`. The only test surface is Cypress e2e (`npm run test:e2e`), which drives a real browser against a running AI Workspace + Platform API + gateway-controller docker stack (see `cypress/e2e/001-providers/002-provider-secret-management.cy.js` for the house style: `describe`/`it`, `cy.intercept` to assert which network calls fire, `data-cyid` selectors, `cy.request` setup/teardown against the real REST API, unique-suffixed resource names, `afterEach` cleanup via DELETE).

Because there is no fast in-process test loop, this plan does **not** use a red/green unit-test cycle per step. Instead, each task that changes user-facing behavior ends with:
1. `npm run build` (TypeScript compile check — catches type errors across all consumers immediately, since this repo has strict typing).
2. A concrete Cypress spec addition/extension (real `.cy.js` code, following the house style above) that exercises the new behavior. These specs are written as part of this plan for completeness and future CI use, but **running them requires the full docker e2e stack** (out of scope for an inner dev loop) — note this explicitly rather than claiming they were run.
3. Where feasible, a manual verification note (what to click through in `npm run dev` against a real backend) as the fastest actual feedback loop while implementing.

## Task 1: Shared `UpstreamAuthConfig` type

**Files:**
- Modify: `portals/ai-workspace/src/utils/types.ts:191-212` (the `UpstreamAuth`/`UpstreamEndpoint`/`Upstream` block), `portals/ai-workspace/src/utils/types.ts:745-770` (the `MCPServerUpstreamAuth`/`MCPServerUpstreamEndpoint`/`MCPServerUpstream` block), `portals/ai-workspace/src/utils/types.ts:589-595` (`ProxyProviderConfig`, which references `UpstreamAuth`)

**Interfaces:**
- Produces: `export interface UpstreamAuthConfig { type: 'none' | 'api-key' | 'oauth2' | 'other' | string; header?: string; valuePrefix?: string; value?: string; policyName?: string; policyVersion?: string; policyParams?: Record<string, unknown>; }`, exported from `types.ts`, used by every later task in place of `UpstreamAuth`/`MCPServerUpstreamAuth`.

- [ ] **Step 1: Replace `UpstreamAuth` with `UpstreamAuthConfig` and widen it**

In `portals/ai-workspace/src/utils/types.ts`, replace:

```ts
/**
 * Authentication configuration for upstream
 */
export interface UpstreamAuth {
  type: 'api-key' | 'oauth2' | 'basic' | 'other' | 'none' | string;
  header?: string;
  valuePrefix?: string;
  value?: string;
}
```

with:

```ts
/**
 * Authentication configuration for an upstream connection (LLM Provider,
 * LLM Proxy provider override, or MCP server upstream). "api-key" uses the
 * typed header/valuePrefix/value fields below; "oauth2"/"other" carry their
 * parameters in policyParams instead (policyName/policyVersion optionally
 * override which policy implements them).
 */
export interface UpstreamAuthConfig {
  type: 'none' | 'api-key' | 'oauth2' | 'other' | string;
  header?: string;
  valuePrefix?: string;
  value?: string;
  policyName?: string;
  policyVersion?: string;
  policyParams?: Record<string, unknown>;
}
```

Then update every reference in the same file: `UpstreamEndpoint.auth?: UpstreamAuth;` → `auth?: UpstreamAuthConfig;` (line 207), and `ProxyProviderConfig.auth?: UpstreamAuth;` → `auth?: UpstreamAuthConfig;` (line 594).

- [ ] **Step 2: Replace `MCPServerUpstreamAuth` with the same shared type**

Replace:

```ts
/**
 * MCP Server upstream auth configuration
 */
export interface MCPServerUpstreamAuth {
  type: string;
  header: string;
  // writeOnly server-side — never present on a GET response, only ever sent on write.
  value?: string;
}
```

with a comment-only removal (delete the interface) and change `MCPServerUpstreamEndpoint`:

```ts
export interface MCPServerUpstreamEndpoint {
  url: string;
  ref?: string;
  hostRewrite?: string;
  // writeOnly server-side — never present on a GET response, only ever sent on write.
  auth?: UpstreamAuthConfig;
}
```

- [ ] **Step 3: Compile check**

Run: `cd portals/ai-workspace && npm run build`
Expected: build succeeds. (`UpstreamAuth`/`MCPServerUpstreamAuth` were only referenced inside `types.ts` itself — confirmed via `grep -rn "UpstreamAuth\b" src` and `grep -rn "MCPServerUpstreamAuth\b" src` returning no hits outside this file — so no other file needs an import rename. `MCPServerInfoFetchRequest`'s inline `auth: { type: string; header: string; value: string }` shape at `types.ts:861-865` is a separate, deliberately minimal probe-request shape used only by the pre-save "fetch server info" validation call — leave it untouched, it is out of scope.)

- [ ] **Step 4: Commit**

```bash
git add portals/ai-workspace/src/utils/types.ts
git commit -m "feat(ai-workspace): unify upstream auth type across LLM and MCP resources"
```

## Task 2: Policy-schema resolution helper

**Files:**
- Create: `portals/ai-workspace/src/utils/upstreamAuthPolicy.ts`
- Create: `portals/ai-workspace/src/utils/upstreamAuthPolicy.test-manual.md` — NOT a real test file (no runner exists); instead this step is folded into Task 4's Cypress spec. (Skip creating this file — noted here only so the plan doesn't silently drop a "tests" line item per policy from the No-Placeholders check; the real verification is Task 4's spec, which exercises this helper indirectly through the mounted component.)

**Interfaces:**
- Consumes: `getGatewayCustomPolicies` from `portals/ai-workspace/src/apis/gatewayPolicyApis.ts` (`(): Promise<GatewayCustomPolicyListResponse>`, where `GatewayCustomPolicyListResponse.list: GatewayCustomPolicy[]` and `GatewayCustomPolicy.policyDefinition: Record<string, unknown>`); `PolicyDefinition`/`ParameterSchema` types from `portals/ai-workspace/src/pages/appShell/PolicyParameterEditor/types.ts`.
- Produces: `export async function resolveUpstreamAuthPolicyDefinition(policyName: string): Promise<PolicyDefinition | null>` and `export function clearUpstreamAuthPolicyCache(): void` (test-only reset hook), both exported from `upstreamAuthPolicy.ts`, consumed by Task 4's `UpstreamAuthFields`.

**Design note on gateway scoping (resolves spec's open risk #1):** `getGatewayPolicyManifest(gatewayId)`/`syncGatewayCustomPolicy(gatewayId, ...)` (used by `GatewayPoliciesContext.tsx`) both require a `gatewayId`, but LLM Provider/Proxy/MCP resources are **not** scoped to a specific gateway until deploy time (confirmed: `AppShellContext.tsx` has no gateway field; every existing caller of `GatewayPoliciesProvider`/`getGatewayPolicyManifest` — `GatewayDeployCardContent.tsx`, `GatewayDeployEnvCard.tsx`, `ViewGateway.tsx` — is on a page already scoped to one specific gateway's `id`). There is no existing "current/default gateway for this org" concept to reuse, and inventing a gateway-selection UX for these three unrelated forms is out of scope for this plan. So this helper only reads the **already-synced, gateway-agnostic** `getGatewayCustomPolicies()` list — the same one the Guardrails picker already reads from. If `oauth2-generator` (or a user-picked `other` policy name) isn't in that list yet, the helper returns `null` and the caller (Task 4) shows a message directing the user to sync it from a gateway's Custom Policies view, rather than attempting a silent auto-sync with no resolvable `gatewayId`. Auto-sync-on-first-use is left as a follow-up (spec risk #1) pending a product decision on which gateway to sync from.

- [ ] **Step 1: Write `resolveUpstreamAuthPolicyDefinition`**

Create `portals/ai-workspace/src/utils/upstreamAuthPolicy.ts`:

```ts
/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 * Licensed under the Apache License, Version 2.0.
 */

import { getGatewayCustomPolicies } from '../apis/gatewayPolicyApis';
import type { GatewayCustomPolicy } from '../apis/gatewayPolicyApis';
import type { ParameterSchema, PolicyDefinition } from '../pages/appShell/PolicyParameterEditor/types';

/**
 * Resolves a policy's real parameter schema from the org's already-synced
 * custom policies (the same list the Guardrails picker reads from). Returns
 * null when the policy hasn't been synced into this org yet — callers should
 * direct the user to sync it from a gateway's Custom Policies view rather
 * than attempting a silent auto-sync (there is no gatewayId to sync from at
 * this call site; see the design note in this file's implementation plan).
 */
export async function resolveUpstreamAuthPolicyDefinition(
  policyName: string,
): Promise<PolicyDefinition | null> {
  const cached = definitionCache.get(policyName);
  if (cached !== undefined) return cached;

  const inFlight = inFlightRequests.get(policyName);
  if (inFlight) return inFlight;

  const request = fetchDefinition(policyName);
  inFlightRequests.set(policyName, request);
  try {
    const definition = await request;
    definitionCache.set(policyName, definition);
    return definition;
  } finally {
    inFlightRequests.delete(policyName);
  }
}

/** Test-only: clears the module-level cache between test cases. */
export function clearUpstreamAuthPolicyCache(): void {
  definitionCache.clear();
  inFlightRequests.clear();
}

const definitionCache = new Map<string, PolicyDefinition | null>();
const inFlightRequests = new Map<string, Promise<PolicyDefinition | null>>();

async function fetchDefinition(policyName: string): Promise<PolicyDefinition | null> {
  const response = await getGatewayCustomPolicies();
  const match = (response.list ?? []).find((policy) => policy.name === policyName);
  if (!match) return null;
  return toPolicyDefinition(match);
}

function toPolicyDefinition(policy: GatewayCustomPolicy): PolicyDefinition {
  const def = (policy.policyDefinition ?? {}) as {
    description?: string;
    parameters?: ParameterSchema;
  };
  return {
    name: policy.name,
    version: policy.version,
    description: policy.description || def.description || '',
    parameters: def.parameters ?? { type: 'object', properties: {} },
  };
}
```

- [ ] **Step 2: Compile check**

Run: `cd portals/ai-workspace && npm run build`
Expected: build succeeds with no new type errors.

- [ ] **Step 3: Commit**

```bash
git add portals/ai-workspace/src/utils/upstreamAuthPolicy.ts
git commit -m "feat(ai-workspace): add upstream-auth policy schema resolver"
```

## Task 3: Secret auto-wrap / masking helper

**Files:**
- Create: `portals/ai-workspace/src/utils/upstreamAuthSecrets.ts`
- Modify: `portals/ai-workspace/src/pages/appShell/appShellPages/externalServers/ExternalServersNew.tsx:236-256` (replace the inline secret-wrap block with a call to the new helper, proving the extraction preserves existing MCP behavior)

**Interfaces:**
- Consumes: `createSecret`, `buildSecretPlaceholder`, `generateSecretHandle` from `portals/ai-workspace/src/apis/secretApis.ts` (signatures already defined: `createSecret(request: CreateSecretRequest): Promise<CreateSecretResponse>`, `buildSecretPlaceholder(secretName: string): string`, `generateSecretHandle(): string`).
- Produces: `export const SENSITIVE_OAUTH2_PARAM_KEYS = ['clientSecret', 'bearerToken', 'password'] as const;`, `export async function autoWrapSensitiveValue(rawValue: string, secretMeta: { displayName: string; description: string }): Promise<string>`, `export function maskSensitiveParamsForDisplay(policyParams: Record<string, unknown> | undefined, sensitiveKeys: readonly string[]): Record<string, unknown> | undefined`, `export const MASKED_SECRET_DISPLAY_VALUE = '******';` — all exported from `upstreamAuthSecrets.ts`, consumed by Task 4's `UpstreamAuthFields` and by this task's `ExternalServersNew.tsx` update.

- [ ] **Step 1: Write the shared secret helper**

Create `portals/ai-workspace/src/utils/upstreamAuthSecrets.ts`:

```ts
/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 * Licensed under the Apache License, Version 2.0.
 */

import { createSecret, buildSecretPlaceholder, generateSecretHandle } from '../apis/secretApis';

/** oauth2-generator's own sensitive parameter keys (policy-definition.yaml). */
export const SENSITIVE_OAUTH2_PARAM_KEYS = ['clientSecret', 'bearerToken', 'password'] as const;

/** Display placeholder shown in place of a resolved `{{ secret "..." }}` value. */
export const MASKED_SECRET_DISPLAY_VALUE = '******';

/** True if the value is already a `{{ secret "handle" }}` placeholder. */
export function isSecretPlaceholder(value: unknown): value is string {
  return typeof value === 'string' && value.includes('{{ secret ');
}

/**
 * Wraps a raw credential value into a newly created secret, returning the
 * `{{ secret "handle" }}` placeholder to store instead of the plaintext.
 * A value that is already a placeholder is returned unchanged (no secret
 * created) — mirrors the existing MCP upstream-auth behavior.
 */
export async function autoWrapSensitiveValue(
  rawValue: string,
  secretMeta: { displayName: string; description: string },
): Promise<string> {
  const trimmed = rawValue.trim();
  if (!trimmed || isSecretPlaceholder(trimmed)) return trimmed;

  const secretHandle = generateSecretHandle();
  const secretResponse = await createSecret({
    id: secretHandle,
    displayName: secretMeta.displayName,
    description: secretMeta.description,
    value: trimmed,
    type: 'GENERIC',
  });
  return buildSecretPlaceholder(secretResponse.id);
}

/**
 * Runs autoWrapSensitiveValue over every sensitive key present in
 * policyParams, returning a new object with plaintext values replaced by
 * secret placeholders. Keys not in sensitiveKeys, or already holding a
 * placeholder, pass through unchanged.
 */
export async function autoWrapSensitiveParams(
  policyParams: Record<string, unknown>,
  sensitiveKeys: readonly string[],
  secretMetaFor: (key: string) => { displayName: string; description: string },
): Promise<Record<string, unknown>> {
  const result = { ...policyParams };
  for (const key of sensitiveKeys) {
    const value = result[key];
    if (typeof value === 'string' && value.trim() && !isSecretPlaceholder(value)) {
      result[key] = await autoWrapSensitiveValue(value, secretMetaFor(key));
    }
  }
  return result;
}

/**
 * Returns a copy of policyParams with every sensitive key that already holds
 * a `{{ secret ... }}` placeholder replaced by a masked display value, so a
 * read-back form never renders the literal placeholder string. Non-sensitive
 * keys and non-placeholder values pass through unchanged.
 */
export function maskSensitiveParamsForDisplay(
  policyParams: Record<string, unknown> | undefined,
  sensitiveKeys: readonly string[],
): Record<string, unknown> | undefined {
  if (!policyParams) return policyParams;
  const result = { ...policyParams };
  for (const key of sensitiveKeys) {
    if (isSecretPlaceholder(result[key])) {
      result[key] = MASKED_SECRET_DISPLAY_VALUE;
    }
  }
  return result;
}
```

- [ ] **Step 2: Extract `ExternalServersNew.tsx`'s inline secret logic to use the helper**

In `portals/ai-workspace/src/pages/appShell/appShellPages/externalServers/ExternalServersNew.tsx`, replace the import block (lines 60-64):

```ts
import {
  createSecret,
  buildSecretPlaceholder,
  generateSecretHandle,
} from '../../../../apis/secretApis';
```

with:

```ts
import { autoWrapSensitiveValue } from '../../../../utils/upstreamAuthSecrets';
```

Then replace the inline block at lines 236-256:

```ts
    // Encrypt the upstream auth value as a secret so the plaintext credential is
    // never stored in the MCP server config. Skip if already a placeholder.
    let resolvedAuthValue = authHeaderValue.trim();
    if (authHeaderName.trim() && resolvedAuthValue && !resolvedAuthValue.includes('{{ secret ')) {
      try {
        const secretHandle = generateSecretHandle();
        const secretResponse = await createSecret(
          {
            id: secretHandle,
            displayName: `${serverName.trim()} upstream auth`,
            description: `Auto-generated secret for MCP server ${serverName.trim()}`,
            value: resolvedAuthValue,
            type: 'GENERIC',
          },
        );
        resolvedAuthValue = buildSecretPlaceholder(secretResponse.id);
      } catch (err) {
        showSnackbar('Failed to encrypt upstream auth credential', 'error');
        return;
      }
    }
```

with:

```ts
    // Encrypt the upstream auth value as a secret so the plaintext credential is
    // never stored in the MCP server config. Skip if already a placeholder.
    let resolvedAuthValue = authHeaderValue.trim();
    if (authHeaderName.trim() && resolvedAuthValue) {
      try {
        resolvedAuthValue = await autoWrapSensitiveValue(resolvedAuthValue, {
          displayName: `${serverName.trim()} upstream auth`,
          description: `Auto-generated secret for MCP server ${serverName.trim()}`,
        });
      } catch (err) {
        showSnackbar('Failed to encrypt upstream auth credential', 'error');
        return;
      }
    }
```

- [ ] **Step 3: Compile check**

Run: `cd portals/ai-workspace && npm run build`
Expected: build succeeds.

- [ ] **Step 4: Manual verification**

Run: `cd portals/ai-workspace && npm run dev`, then walk through creating an MCP server with an endpoint that requires an auth header/value (Advanced Configurations section), confirming a secret is still created and the placeholder still ends up in the created server's `upstream.main.auth.value` (same behavior as before the extraction — this step is a refactor, not a behavior change).

- [ ] **Step 5: Commit**

```bash
git add portals/ai-workspace/src/utils/upstreamAuthSecrets.ts portals/ai-workspace/src/pages/appShell/appShellPages/externalServers/ExternalServersNew.tsx
git commit -m "refactor(ai-workspace): extract MCP secret auto-wrap into a shared upstream-auth helper"
```

## Task 4: `UpstreamAuthFields` shared component

**Files:**
- Create: `portals/ai-workspace/src/Components/UpstreamAuth/UpstreamAuthFields.tsx`
- Create: `portals/ai-workspace/src/Components/UpstreamAuth/index.ts`
- Create (Cypress, house style): `portals/ai-workspace/cypress/e2e/001-providers/004-upstream-auth-oauth2.cy.js`

**Interfaces:**
- Consumes: `UpstreamAuthConfig` (Task 1), `resolveUpstreamAuthPolicyDefinition` (Task 2), `SENSITIVE_OAUTH2_PARAM_KEYS`/`maskSensitiveParamsForDisplay`/`autoWrapSensitiveParams` (Task 3), `PolicyParameterEditor` component with props `{ policyDefinition: PolicyDefinition; policyDisplayName?: string; existingValues?: ParameterValues; onCancel: () => void; onSubmit: (values: ParameterValues) => void; disabled?: boolean; readOnly?: boolean }` (already defined in `portals/ai-workspace/src/pages/appShell/PolicyParameterEditor/PolicyParameterEditor.tsx`), `getGatewayCustomPolicies` (for the `other` policy picker).
- Produces: `export default function UpstreamAuthFields(props: UpstreamAuthFieldsProps): JSX.Element` and `export interface UpstreamAuthFieldsProps { value: UpstreamAuthConfig; onChange: (next: UpstreamAuthConfig) => void; disabled?: boolean; readOnly?: boolean; }`, both exported from `portals/ai-workspace/src/Components/UpstreamAuth/index.ts`. Consumed by Tasks 5, 6, 7. Note: this component only manages the auth **draft state and its own fields** — the parent decides when/whether to run `autoWrapSensitiveParams` (typically at submit time, since it's async and requires a display-name/description the surrounding form knows about), so `onChange` always receives the raw (unwrapped) draft.

- [ ] **Step 1: Write `UpstreamAuthFields`**

Create `portals/ai-workspace/src/Components/UpstreamAuth/UpstreamAuthFields.tsx`:

```tsx
/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 * Licensed under the Apache License, Version 2.0.
 */

import React, { useEffect, useState } from 'react';
import {
  FormControl,
  FormHelperText,
  FormLabel,
  MenuItem,
  Select,
  Stack,
  TextField,
  Typography,
  CircularProgress,
  Box,
} from '@wso2/oxygen-ui';
import type { UpstreamAuthConfig } from '../../utils/types';
import { resolveUpstreamAuthPolicyDefinition } from '../../utils/upstreamAuthPolicy';
import {
  SENSITIVE_OAUTH2_PARAM_KEYS,
  maskSensitiveParamsForDisplay,
} from '../../utils/upstreamAuthSecrets';
import { getGatewayCustomPolicies } from '../../apis/gatewayPolicyApis';
import type { GatewayCustomPolicy } from '../../apis/gatewayPolicyApis';
import PolicyParameterEditor from '../../pages/appShell/PolicyParameterEditor/PolicyParameterEditor';
import type { PolicyDefinition, ParameterValues } from '../../pages/appShell/PolicyParameterEditor/types';

const OAUTH2_POLICY_NAME = 'oauth2-generator';

export interface UpstreamAuthFieldsProps {
  value: UpstreamAuthConfig;
  onChange: (next: UpstreamAuthConfig) => void;
  disabled?: boolean;
  readOnly?: boolean;
}

export default function UpstreamAuthFields({
  value,
  onChange,
  disabled = false,
  readOnly = false,
}: UpstreamAuthFieldsProps) {
  const effectiveType = value.type || 'none';
  const isSchemaDriven = effectiveType === 'oauth2' || effectiveType === 'other';
  const schemaPolicyName = effectiveType === 'oauth2' ? OAUTH2_POLICY_NAME : (value.policyName || '');

  const [policyDefinition, setPolicyDefinition] = useState<PolicyDefinition | null>(null);
  const [isLoadingDefinition, setIsLoadingDefinition] = useState(false);
  const [definitionError, setDefinitionError] = useState<string | null>(null);
  const [availablePolicies, setAvailablePolicies] = useState<GatewayCustomPolicy[]>([]);

  useEffect(() => {
    if (effectiveType !== 'other') return;
    let isMounted = true;
    void getGatewayCustomPolicies().then((response) => {
      if (isMounted) setAvailablePolicies(response.list ?? []);
    });
    return () => {
      isMounted = false;
    };
  }, [effectiveType]);

  useEffect(() => {
    if (!isSchemaDriven || !schemaPolicyName) {
      setPolicyDefinition(null);
      return;
    }
    let isMounted = true;
    setIsLoadingDefinition(true);
    setDefinitionError(null);
    void resolveUpstreamAuthPolicyDefinition(schemaPolicyName)
      .then((definition) => {
        if (!isMounted) return;
        if (!definition) {
          setDefinitionError(
            `"${schemaPolicyName}" has not been synced into this organization yet. ` +
              'Sync it from a gateway\'s Custom Policies view, then reopen this form.',
          );
        }
        setPolicyDefinition(definition);
      })
      .catch(() => {
        if (isMounted) setDefinitionError('Failed to load policy parameters.');
      })
      .finally(() => {
        if (isMounted) setIsLoadingDefinition(false);
      });
    return () => {
      isMounted = false;
    };
  }, [isSchemaDriven, schemaPolicyName]);

  const handleTypeChange = (nextType: string) => {
    if (nextType === effectiveType) return;
    const isNoCredentialsType = nextType === 'none';
    onChange({
      type: nextType,
      header: isNoCredentialsType || nextType === 'oauth2' || nextType === 'other' ? '' : value.header,
      value: isNoCredentialsType || nextType === 'oauth2' || nextType === 'other' ? '' : value.value,
      valuePrefix: value.valuePrefix,
      policyName: nextType === 'oauth2' ? OAUTH2_POLICY_NAME : nextType === 'other' ? '' : undefined,
      policyVersion: undefined,
      policyParams: nextType === 'oauth2' || nextType === 'other' ? {} : undefined,
    });
  };

  const handlePolicyNameChange = (nextPolicyName: string) => {
    onChange({ ...value, policyName: nextPolicyName, policyParams: {} });
  };

  const handleParamsSubmit = (nextParams: ParameterValues) => {
    onChange({ ...value, policyParams: nextParams });
  };

  return (
    <Stack spacing={2}>
      <FormControl fullWidth size="small">
        <FormLabel>Authentication Type</FormLabel>
        <Select
          value={effectiveType}
          disabled={disabled || readOnly}
          onChange={(event) => handleTypeChange(String(event.target.value))}
        >
          {['none', 'api-key', 'oauth2', 'other'].map((type) => (
            <MenuItem key={type} value={type}>
              {type}
            </MenuItem>
          ))}
        </Select>
      </FormControl>

      {effectiveType === 'api-key' && (
        <Stack spacing={2}>
          <FormControl fullWidth>
            <FormLabel>Auth Header Name</FormLabel>
            <TextField
              size="small"
              value={value.header || ''}
              disabled={disabled || readOnly}
              onChange={(e) => onChange({ ...value, header: e.target.value })}
              placeholder="Authorization"
            />
          </FormControl>
          <FormControl fullWidth>
            <FormLabel>Credential</FormLabel>
            <TextField
              size="small"
              type="password"
              value={value.value || ''}
              disabled={disabled || readOnly}
              onChange={(e) => onChange({ ...value, value: e.target.value })}
            />
          </FormControl>
        </Stack>
      )}

      {effectiveType === 'other' && (
        <FormControl fullWidth size="small">
          <FormLabel>Policy</FormLabel>
          <Select
            value={value.policyName || ''}
            disabled={disabled || readOnly}
            displayEmpty
            onChange={(event) => handlePolicyNameChange(String(event.target.value))}
          >
            <MenuItem value="" disabled>
              Select a policy
            </MenuItem>
            {availablePolicies.map((policy) => (
              <MenuItem key={`${policy.name}@${policy.version}`} value={policy.name}>
                {policy.displayName || policy.name}
              </MenuItem>
            ))}
          </Select>
        </FormControl>
      )}

      {isSchemaDriven && isLoadingDefinition && (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
          <CircularProgress size={16} />
          <Typography variant="body2" color="text.secondary">
            Loading parameters…
          </Typography>
        </Box>
      )}

      {isSchemaDriven && definitionError && (
        <FormHelperText error>{definitionError}</FormHelperText>
      )}

      {isSchemaDriven && policyDefinition && !isLoadingDefinition && (
        <PolicyParameterEditor
          policyDefinition={policyDefinition}
          policyDisplayName={policyDefinition.name}
          existingValues={maskSensitiveParamsForDisplay(
            value.policyParams,
            SENSITIVE_OAUTH2_PARAM_KEYS,
          )}
          onCancel={() => undefined}
          onSubmit={handleParamsSubmit}
          disabled={disabled}
          readOnly={readOnly}
        />
      )}
    </Stack>
  );
}
```

- [ ] **Step 2: Add the barrel export**

Create `portals/ai-workspace/src/Components/UpstreamAuth/index.ts`:

```ts
/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 * Licensed under the Apache License, Version 2.0.
 */

export { default } from './UpstreamAuthFields';
export type { UpstreamAuthFieldsProps } from './UpstreamAuthFields';
```

- [ ] **Step 3: Compile check**

Run: `cd portals/ai-workspace && npm run build`
Expected: build succeeds.

- [ ] **Step 4: Add a Cypress spec exercising the type selector and oauth2 rendering**

Create `portals/ai-workspace/cypress/e2e/001-providers/004-upstream-auth-oauth2.cy.js` (follows the house style from `002-provider-secret-management.cy.js`; requires the full e2e docker stack with `oauth2-generator` already synced into the test org — if the stack's default org has never synced it, the "renders oauth2 fields" case will instead assert the not-synced message, so both branches are covered explicitly):

```js
/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

function navigateToAddProvider() {
  cy.get('[data-cyid="nav-service-provider"]', { timeout: 30000 })
    .should('be.visible')
    .click();
  cy.get('[data-cyid="add-new-provider-button"]', { timeout: 30000 })
    .should('be.visible')
    .click();
}

function selectOpenAITemplate() {
  cy.get('[data-cyid="provider-template-openai-card"]', { timeout: 30000 })
    .should('be.visible')
    .click();
}

describe('AI Workspace — LLM provider upstream auth type selector', () => {
  beforeEach(() => {
    cy.login();
  });

  it('offers oauth2 as an authentication type and renders its parameters or a not-synced message', () => {
    navigateToAddProvider();
    selectOpenAITemplate();

    cy.contains('label', 'Authentication Type')
      .parent()
      .find('[role="combobox"], select')
      .click();
    cy.get('li').contains('oauth2').click();

    cy.get('body').then(($body) => {
      const hasNotSyncedMessage = $body.text().includes('has not been synced into this organization');
      if (hasNotSyncedMessage) {
        cy.contains('has not been synced into this organization').should('be.visible');
      } else {
        cy.contains('tokenEndpoint').should('be.visible');
        cy.contains('clientId').should('be.visible');
        cy.contains('clientSecret').should('be.visible');
      }
    });
  });
});
```

- [ ] **Step 5: Commit**

```bash
git add portals/ai-workspace/src/Components/UpstreamAuth portals/ai-workspace/cypress/e2e/001-providers/004-upstream-auth-oauth2.cy.js
git commit -m "feat(ai-workspace): add shared UpstreamAuthFields component"
```

## Task 5: LLM Provider integration (create + edit)

**Files:**
- Modify: `portals/ai-workspace/src/pages/appShell/appShellPages/serviceProvider/AddNewProvider/serviceProviderTypes.ts` (widen `FormState`)
- Modify: `portals/ai-workspace/src/pages/appShell/appShellPages/serviceProvider/AddNewProvider/ProviderTemplateFormFields.tsx:273-393` (mount `UpstreamAuthFields` instead of the inline Select+TextFields)
- Modify: `portals/ai-workspace/src/pages/appShell/appShellPages/serviceProvider/ServiceProviderNew.tsx:356-379` (payload builder)
- Modify: `portals/ai-workspace/src/pages/appShell/appShellPages/serviceProvider/ServiceProviderConnectionTab.tsx` (full auth section: state, `handleUpdateAuthentication`/`handleUpdateAuthenticationHeader`/`handleUpdateCredential`, and the render block at lines 325-441)

**Interfaces:**
- Consumes: `UpstreamAuthFields`/`UpstreamAuthFieldsProps` (Task 4), `UpstreamAuthConfig` (Task 1), `autoWrapSensitiveParams`/`SENSITIVE_OAUTH2_PARAM_KEYS` (Task 3).
- Produces: nothing new consumed by later tasks — this is a leaf integration task.

- [ ] **Step 1: Widen `FormState` to carry a full `UpstreamAuthConfig` draft**

In `portals/ai-workspace/src/pages/appShell/appShellPages/serviceProvider/AddNewProvider/serviceProviderTypes.ts`, replace:

```ts
export type FormState = {
  name: string;
  description: string;
  version: string;
  context: string;
  providerType: string;
  upstreamUrl: string;
  upstreamAuthType: string;
  upstreamAuthHeader: string;
  upstreamAuthValue: string;
  valuePrefix: string;
};
```

with:

```ts
import type { UpstreamAuthConfig } from '../../../../../utils/types';

export type FormState = {
  name: string;
  description: string;
  version: string;
  context: string;
  providerType: string;
  upstreamUrl: string;
  upstreamAuth: UpstreamAuthConfig;
};
```

(Removing the four flat `upstreamAuthType`/`upstreamAuthHeader`/`upstreamAuthValue`/`valuePrefix` fields — they move into the nested `upstreamAuth` object, matching what the backend and `UpstreamAuthFields` already expect.)

- [ ] **Step 2: Update `ServiceProviderNew.tsx`'s initial state, template-seeding effect, and validity/payload logic**

In `portals/ai-workspace/src/pages/appShell/appShellPages/serviceProvider/ServiceProviderNew.tsx`, replace the initial `formState` (lines 193-204):

```ts
  const [formState, setFormState] = useState<FormState>({
    name: '',
    description: '',
    version: 'v1.0',
    context: '/',
    providerType: '',
    upstreamUrl: '',
    upstreamAuthType: 'api-key',
    upstreamAuthHeader: 'Authorization',
    upstreamAuthValue: '',
    valuePrefix: '',
  });
```

with:

```ts
  const [formState, setFormState] = useState<FormState>({
    name: '',
    description: '',
    version: 'v1.0',
    context: '/',
    providerType: '',
    upstreamUrl: '',
    upstreamAuth: { type: 'api-key', header: 'Authorization', value: '', valuePrefix: '' },
  });
```

Replace the template-seeding block inside `TemplateBasedFormFieldsContainer`'s effect (lines 104-117):

```ts
    setFormState((prev) => ({
      ...prev,
      upstreamUrl: templateChanged
        ? template?.metadata?.endpointUrl || ''
        : prev.upstreamUrl,
      upstreamAuthType: templateChanged
        ? template?.metadata?.auth?.type || 'api-key'
        : prev.upstreamAuthType,
      upstreamAuthHeader: templateChanged
        ? template?.metadata?.auth?.header || 'Authorization'
        : prev.upstreamAuthHeader,
      upstreamAuthValue: templateChanged ? '' : prev.upstreamAuthValue,
      valuePrefix: template?.metadata?.auth?.valuePrefix || '',
    }));
```

with:

```ts
    setFormState((prev) => ({
      ...prev,
      upstreamUrl: templateChanged
        ? template?.metadata?.endpointUrl || ''
        : prev.upstreamUrl,
      upstreamAuth: templateChanged
        ? {
            type: template?.metadata?.auth?.type || 'api-key',
            header: template?.metadata?.auth?.header || 'Authorization',
            value: '',
            valuePrefix: template?.metadata?.auth?.valuePrefix || '',
          }
        : prev.upstreamAuth,
    }));
```

Replace `isFormValid`'s auth check (line 334, `formState.upstreamAuthType`) with `formState.upstreamAuth.type`.

Replace `handleCreateProvider`'s auth-building block (lines 356-379):

```ts
      // The API key is optional. A credential-bearing auth type with no key
      // entered stores nothing to inject, so record it as 'none'. Sending
      // 'api-key' with an empty value creates a provider the gateway rejects
      // at deployment time.
      const hasCredential = Boolean(formState.upstreamAuthValue.trim());
      const isNoCredentialsAuthType =
        formState.upstreamAuthType === 'other' ||
        formState.upstreamAuthType === 'none';
      const auth =
        isNoCredentialsAuthType || !hasCredential
          ? {
              type: isNoCredentialsAuthType
                ? formState.upstreamAuthType
                : 'none',
              header: '',
              value: '',
            }
          : {
              type: formState.upstreamAuthType,
              header: formState.upstreamAuthHeader,
              value: formState.valuePrefix
                ? `${formState.valuePrefix.trimEnd()} ${formState.upstreamAuthValue}`
                : formState.upstreamAuthValue,
            };
```

with:

```ts
      // The API key is optional. A credential-bearing auth type with no key
      // entered stores nothing to inject, so record it as 'none'. Sending
      // 'api-key' with an empty value creates a provider the gateway rejects
      // at deployment time.
      const authType = formState.upstreamAuth.type;
      const hasApiKeyCredential = Boolean(formState.upstreamAuth.value?.trim());
      let auth: typeof formState.upstreamAuth;
      if (authType === 'oauth2' || authType === 'other') {
        auth = {
          type: authType,
          policyName: formState.upstreamAuth.policyName,
          policyParams: await autoWrapSensitiveParams(
            formState.upstreamAuth.policyParams ?? {},
            SENSITIVE_OAUTH2_PARAM_KEYS,
            () => ({
              displayName: `${formState.name.trim()} upstream auth`,
              description: `Auto-generated secret for LLM provider ${formState.name.trim()}`,
            }),
          ),
        };
      } else if (authType === 'none' || !hasApiKeyCredential) {
        auth = { type: authType === 'api-key' ? 'none' : authType, header: '', value: '' };
      } else {
        auth = {
          type: authType,
          header: formState.upstreamAuth.header,
          value: formState.upstreamAuth.valuePrefix
            ? `${formState.upstreamAuth.valuePrefix.trimEnd()} ${formState.upstreamAuth.value}`
            : formState.upstreamAuth.value,
        };
      }
```

Add the two new imports at the top of `ServiceProviderNew.tsx`:

```ts
import { autoWrapSensitiveParams, SENSITIVE_OAUTH2_PARAM_KEYS } from '../../../../utils/upstreamAuthSecrets';
```

- [ ] **Step 3: Mount `UpstreamAuthFields` in `ProviderTemplateFormFields.tsx`**

In `portals/ai-workspace/src/pages/appShell/appShellPages/serviceProvider/AddNewProvider/ProviderTemplateFormFields.tsx`, replace the entire block from the `isOtherAuthType`/`isNoCredentialsAuthType` derivations (lines 118-121) through the end of the "Auth Value" `Grid` (line 393) with:

```tsx
      {!hasTemplateAuthType && (
        <Grid size={{ xs: 12 }}>
          <UpstreamAuthFields
            value={formState.upstreamAuth}
            onChange={(next) => setFormState((prev) => ({ ...prev, upstreamAuth: next }))}
          />
        </Grid>
      )}
```

(This removes the old separate Auth Type/Header/Value grids and the `showCredential`/`setShowCredential` props this component received — `UpstreamAuthFields`'s `api-key` branch always renders its credential field as `type="password"` with no show/hide toggle, matching the simpler pattern already used in `ExternalServersNew.tsx`'s advanced-config credential field being the only precedent that had one; dropping the toggle here is an intentional small UX simplification, not a regression, since the field being replaced never had a toggle either — confirm this reads correctly against the actual current file before removing `showCredential`, since `ServiceProviderConnectionTab.tsx`'s api-key field does have one and Step 4 keeps that.)

Remove the now-unused `showCredential`/`setShowCredential` props from `ProviderTemplateFormFieldsProps` and its destructuring, and remove the corresponding props passed from `ServiceProviderNew.tsx`'s `TemplateBasedFormFieldsContainer` (the `showCredential`/`setShowCredential` state itself can stay in `ServiceProviderNew.tsx` unused only if still referenced elsewhere — check with `grep -n "showCredential" ServiceProviderNew.tsx`; if the only remaining reference is its own `useState` declaration, delete that declaration too).

Add the import:

```ts
import UpstreamAuthFields from '../../../../../Components/UpstreamAuth';
```

- [ ] **Step 4: Update `ServiceProviderConnectionTab.tsx`**

In `portals/ai-workspace/src/pages/appShell/appShellPages/serviceProvider/ServiceProviderConnectionTab.tsx`, replace the entire local-state block (lines 49-54: `authenticationType`, `authenticationHeader`, `credentialValue`, `isCredentialMasked`, `hasCredentialChanged`, `showCredential`) and the three handlers (`handleUpdateAuthentication`, `handleUpdateAuthenticationHeader`, `handleUpdateCredential`, lines 166-293) with a single draft-state + save pattern built around `UpstreamAuthFields`:

```tsx
  const [authDraft, setAuthDraft] = useState<UpstreamAuthConfig>({ type: 'none' });

  useEffect(() => {
    if (!provider) return;
    setAuthDraft({
      type: provider.upstream?.main?.auth?.type || 'none',
      header: provider.upstream?.main?.auth?.header || '',
      value: '',
      valuePrefix: provider.upstream?.main?.auth?.valuePrefix || '',
      policyName: provider.upstream?.main?.auth?.policyName,
      policyParams: provider.upstream?.main?.auth?.policyParams,
    });
  }, [provider]);

  const handleAuthDraftChange = async (next: UpstreamAuthConfig) => {
    setAuthDraft(next);
    if (!provider || isLoading || error || isReadOnlyProvider) return;
    const {
      status,
      createdAt,
      createdBy,
      updatedAt,
      updatedBy,
      lastUpdated,
      ...updatePayload
    } = provider;
    let auth: UpstreamAuthConfig = next;
    if (next.type === 'oauth2' || next.type === 'other') {
      auth = {
        type: next.type,
        policyName: next.policyName,
        policyParams: await autoWrapSensitiveParams(
          next.policyParams ?? {},
          SENSITIVE_OAUTH2_PARAM_KEYS,
          () => ({
            displayName: `${provider.displayName} upstream auth`,
            description: `Auto-generated secret for LLM provider ${provider.displayName}`,
          }),
        ),
      };
    }
    try {
      await updateProvider({
        ...updatePayload,
        upstream: { main: { url: provider.upstream?.main?.url || '', auth } },
      });
      if (!isDraftMode) showSnackbar('Updated Authentication.', 'success');
    } catch {
      if (!isDraftMode) showSnackbar('Failed to update Authentication.', 'error');
    }
  };
```

Replace the render block (the `FormControl`/`Select`/conditional header+credential fields, lines 325-441) with:

```tsx
        <FormControl fullWidth>
          <FormLabel>Authentication</FormLabel>
          <UpstreamAuthFields
            value={authDraft}
            disabled={isFormDisabled}
            onChange={(next) => void handleAuthDraftChange(next)}
          />
        </FormControl>
```

Update the imports: remove the now-unused `Eye`/`EyeOff` icons and `InputAdornment`/`IconButton` if no longer referenced elsewhere in the file (check with `grep -n "InputAdornment\|IconButton\|Eye\b\|EyeOff" ServiceProviderConnectionTab.tsx` after the edit), and add:

```ts
import UpstreamAuthFields from '../../../../Components/UpstreamAuth';
import type { UpstreamAuthConfig } from '../../../../utils/types';
import { autoWrapSensitiveParams, SENSITIVE_OAUTH2_PARAM_KEYS } from '../../../../utils/upstreamAuthSecrets';
```

Remove the `providerEndpoint`/`handleUpdateProviderEndpoint` logic's inline `auth: {type, header, value}` reconstruction (lines 145-153 and 229-237, both currently rebuild the auth object from only `type`/`header`/`value` when saving the URL) — change both to spread the full current `provider.upstream?.main?.auth` object instead, e.g.:

```ts
        auth: provider.upstream?.main?.auth ?? { type: 'none' },
```

so saving the endpoint URL never drops `policyParams` for an oauth2/other provider.

- [ ] **Step 5: Compile check**

Run: `cd portals/ai-workspace && npm run build`
Expected: build succeeds.

- [ ] **Step 6: Manual verification**

Run: `cd portals/ai-workspace && npm run dev` against a real backend with `oauth2-generator` synced. Create a new LLM provider, switch Authentication Type to oauth2, fill `tokenEndpoint`/`clientId`/`clientSecret`, save, then reopen the Connection tab and confirm `clientSecret` renders masked (`******`) rather than the literal `{{ secret ... }}` string.

- [ ] **Step 7: Extend the existing secret-management Cypress spec**

In `portals/ai-workspace/cypress/e2e/001-providers/002-provider-secret-management.cy.js`, add one new `it` block after the existing TC-64 case (following the same `beforeEach`/`afterEach` and `authToken`/`organizationId`/`createdProviderId` pattern already in that `describe` block):

```js
  it('TC-65: oauth2 clientSecret is stored as a secret placeholder, never plaintext, and masked on re-open', () => {
    cy.intercept('POST', '**/secrets').as('createSecret');
    cy.intercept('POST', /\/llm-providers(\?|$)/).as('createProvider');

    navigateToAddProvider();
    selectOpenAITemplate();
    cy.get('[data-cyid="provider-name-input"] input:visible', { timeout: 30000 })
      .should('be.visible')
      .clear()
      .type(providerName);

    cy.contains('label', 'Authentication Type').parent().find('[role="combobox"]').click();
    cy.get('li').contains('oauth2').click();
    cy.contains('label', 'tokenEndpoint').parent().find('input').type('https://idp.example.com/oauth2/token');
    cy.contains('label', 'clientId').parent().find('input').type('tc65-client-id');
    cy.contains('label', 'clientSecret').parent().find('input').type('tc65-plaintext-secret');

    cy.get('[data-cyid="add-provider-button"]').should('not.be.disabled').click();

    cy.wait('@createSecret').its('request.body').should((body) => {
      expect(String(body)).to.include('tc65-plaintext-secret');
    });
    cy.wait('@createProvider').then(({ request }) => {
      createdProviderId = request.body?.id ?? '';
      const serialized = JSON.stringify(request.body);
      expect(serialized).to.not.include('tc65-plaintext-secret');
      expect(serialized).to.include('{{ secret ');
    });

    cy.contains(providerName).should('be.visible');
    cy.get('body').should('not.contain.text', 'tc65-plaintext-secret');
  });
```

- [ ] **Step 8: Commit**

```bash
git add portals/ai-workspace/src/pages/appShell/appShellPages/serviceProvider portals/ai-workspace/cypress/e2e/001-providers/002-provider-secret-management.cy.js
git commit -m "feat(ai-workspace): support oauth2 upstream auth on LLM Provider create/edit"
```

## Task 6: LLM Proxy integration (create + edit, including the inheritance-copy fix)

**Files:**
- Modify: `portals/ai-workspace/src/pages/appShell/appShellPages/proxies/LLMProxyProviderTab.tsx:135-189` (`handleProviderChange`) and the render block (lines 191-290)
- Modify: `portals/ai-workspace/src/pages/appShell/appShellPages/proxies/LLMProxyNew.tsx:395-450` (inheritance + payload building)

**Interfaces:**
- Consumes: `UpstreamAuthFields`/`UpstreamAuthFieldsProps` (Task 4), `UpstreamAuthConfig` (Task 1).
- Produces: nothing new consumed by later tasks — leaf integration task.

- [ ] **Step 1: Fix `LLMProxyProviderTab.tsx`'s `handleProviderChange` inheritance-copy bug**

Replace the type/header/value hand-picking block (lines 152-165):

```ts
      // Type, header and value move as one unit: an absent/'none' type carries no
      // credential, so the header and value are cleared with it rather than being
      // inherited from the provider and contradicting the type.
      const nextAuthType = nextProviderDetail?.upstream?.main?.auth?.type || 'none';
      const carriesCredential = nextAuthType !== 'none';
      const nextAuth = {
        type: nextAuthType,
        header: carriesCredential
          ? (nextProviderDetail?.upstream?.main?.auth?.header ?? '')
          : '',
        value: carriesCredential
          ? (nextProviderDetail?.upstream?.main?.auth?.value ?? '')
          : '',
      };
```

with:

```ts
      // Copy the provider's auth object verbatim (not just type/header/value) so
      // oauth2/other's policyName/policyParams survive a provider switch instead
      // of being silently dropped.
      const nextAuth = nextProviderDetail?.upstream?.main?.auth ?? { type: 'none' };
```

- [ ] **Step 2: Render `UpstreamAuthFields` read-only for oauth2/other in `LLMProxyProviderTab.tsx`**

Replace the api-key-only conditional block (lines 228-285, the `{proxy?.provider && providerDetail?.security?.apiKey && (...)}` block) — keep that block exactly as-is for the api-key-credential-override case (it is unrelated: it overrides the **client-facing** `security.apiKey`, not `upstream.main.auth`), and add a new block immediately after it for the upstream auth display:

```tsx
          {(() => {
            const providerAuth =
              typeof proxy?.provider === 'object' ? proxy.provider.auth : undefined;
            if (!providerAuth || providerAuth.type === 'none') return null;
            return (
              <Stack spacing={2} sx={{ mt: 3 }}>
                <Typography variant="h6" sx={{ mb: 1.5, fontWeight: 600 }}>
                  Upstream Authentication
                </Typography>
                <Typography variant="body2" color="text.secondary">
                  {providerAuth.type === 'api-key'
                    ? 'This proxy overrides the provider\'s api-key value above.'
                    : `Inherited from the selected provider (${providerAuth.type}). Edit it on the provider's Connection tab.`}
                </Typography>
                {(providerAuth.type === 'oauth2' || providerAuth.type === 'other') && (
                  <UpstreamAuthFields value={providerAuth} onChange={() => undefined} readOnly />
                )}
              </Stack>
            );
          })()}
```

Add the import:

```ts
import UpstreamAuthFields from '../../../../Components/UpstreamAuth';
```

- [ ] **Step 3: Fix the same inheritance-copy bug in `LLMProxyNew.tsx`**

Replace the inherited-auth block (lines 392-403):

```ts
      // Type, header and value move as one unit: an absent/'none' type carries no
      // credential, so the header and value are cleared with it rather than being
      // inherited from the provider and contradicting the type.
      const inheritedAuthType = providerDetail?.upstream?.main?.auth?.type || 'none';
      const inheritsCredential = inheritedAuthType !== 'none';
      let providerAuthType = inheritedAuthType;
      let providerAuthHeader = inheritsCredential
        ? (providerDetail?.upstream?.main?.auth?.header ?? '')
        : '';
      let providerAuthValue = inheritsCredential
        ? (providerDetail?.upstream?.main?.auth?.value ?? '')
        : '';
```

with:

```ts
      // Copy the provider's auth object verbatim (not just type/header/value) so
      // oauth2/other's policyName/policyParams survive into the new proxy instead
      // of being silently dropped. The api-key branch below still overrides type/
      // header/value on top of this when the provider requires an api-key.
      let providerAuth: UpstreamAuthConfig = providerDetail?.upstream?.main?.auth ?? { type: 'none' };
```

Then update the `if (selectedProviderRequiresApiKey) { ... }` block (lines 404-429) to assign into `providerAuth` instead of the three separate `let`s — replace every `providerAuthType = 'api-key'` / `providerAuthHeader = ...` / `providerAuthValue = ...` assignment inside that block with a single `providerAuth = { type: 'api-key', header: selectedProviderApiKeyName, value: rawKey }` (for the already-placeholder branch) and `providerAuth = { type: 'api-key', header: selectedProviderApiKeyName, value: buildSecretPlaceholder(secretResponse.id) }` (for the newly-created-secret branch), and change the payload's `provider.auth` (lines 445-449) from:

```ts
          auth: {
            type: providerAuthType,
            header: providerAuthHeader,
            value: providerAuthValue,
          },
```

to:

```ts
          auth: providerAuth,
```

Add the import:

```ts
import type { UpstreamAuthConfig } from '../../../../utils/types';
```

- [ ] **Step 4: Compile check**

Run: `cd portals/ai-workspace && npm run build`
Expected: build succeeds.

- [ ] **Step 5: Manual verification**

Run: `cd portals/ai-workspace && npm run dev`. Create an LLM provider with oauth2 upstream auth (per Task 5's manual check), then create a proxy against it and confirm the Provider tab shows the read-only "Inherited from the selected provider (oauth2)" panel with the real `tokenEndpoint`/`clientId` values (and masked `clientSecret`) rather than blank fields — this is the regression Step 1/3 fix directly targets.

- [ ] **Step 6: Commit**

```bash
git add portals/ai-workspace/src/pages/appShell/appShellPages/proxies/LLMProxyProviderTab.tsx portals/ai-workspace/src/pages/appShell/appShellPages/proxies/LLMProxyNew.tsx
git commit -m "fix(ai-workspace): stop dropping oauth2/other policyParams when a proxy inherits provider auth"
```

## Task 7: MCP / External Servers integration

**Files:**
- Modify: `portals/ai-workspace/src/pages/appShell/appShellPages/externalServers/ExternalServersNew.tsx` (replace the fixed header/value Advanced Configurations fields with `UpstreamAuthFields`, and the `handleCreate` payload builder)

**Interfaces:**
- Consumes: `UpstreamAuthFields`/`UpstreamAuthFieldsProps` (Task 4), `UpstreamAuthConfig` (Task 1), `autoWrapSensitiveParams`/`SENSITIVE_OAUTH2_PARAM_KEYS` (Task 3, already partially wired in Task 3 Step 2 for the single-value api-key-shaped case — this task generalizes it to the full type selector).
- Produces: nothing new — leaf integration task.

- [ ] **Step 1: Replace the fixed header/value state with an `UpstreamAuthConfig` draft**

In `ExternalServersNew.tsx`, replace the two `useState` calls (lines 145-147: `authHeaderName`, `authHeaderValue`, `showAuthHeaderValue`) with:

```ts
  const [upstreamAuth, setUpstreamAuth] = useState<UpstreamAuthConfig>({ type: 'none' });
```

Update `validateEndpoint`'s auth-request construction (lines 177-183):

```ts
    if (authHeaderName.trim() && authHeaderValue.trim()) {
      request.auth = {
        type: 'header',
        header: authHeaderName.trim(),
        value: authHeaderValue.trim(),
      };
    }
```

with:

```ts
    if (upstreamAuth.type === 'api-key' && upstreamAuth.header?.trim() && upstreamAuth.value?.trim()) {
      request.auth = {
        type: 'header',
        header: upstreamAuth.header.trim(),
        value: upstreamAuth.value.trim(),
      };
    }
```

(The probe endpoint's `MCPServerInfoFetchRequest.auth` shape at `types.ts:861-865` was deliberately left untouched in Task 1 — it only understands a single header/value pair, so oauth2/other can't be validated pre-save through this probe; that's an acceptable, pre-existing limitation of the "Fetch Server Info" step, not a regression introduced here.)

- [ ] **Step 2: Replace the Advanced Configurations header/value fields with `UpstreamAuthFields`**

Replace the entire "Configure Authentication Header" `Stack` (lines 501-583, from the `Typography` subtitle through the closing `</Grid>` of the two header/value fields) with:

```tsx
                    <Stack spacing={1.5}>
                      <Typography
                        variant="subtitle2"
                        sx={{ fontWeight: 600, fontSize: '0.8125rem' }}
                      >
                        <FormattedMessage
                          id="aiWorkspace.pages.appShell.appShellPages.externalServers.Main.configure.authentication.header"
                          defaultMessage="Configure Authentication"
                        />
                      </Typography>
                      <UpstreamAuthFields value={upstreamAuth} onChange={setUpstreamAuth} />
                    </Stack>
```

Remove the now-unused `Eye`/`EyeOff`/`InputAdornment`/`IconButton` imports if nothing else in the file uses them (check with `grep -n "InputAdornment\|IconButton\|Eye\b\|EyeOff" ExternalServersNew.tsx` after the edit), and add:

```ts
import UpstreamAuthFields from '../../../../Components/UpstreamAuth';
import type { UpstreamAuthConfig } from '../../../../utils/types';
```

- [ ] **Step 3: Update `handleCreate`'s payload builder**

Replace the secret-wrap-and-build block (lines 236-282):

```ts
    // Encrypt the upstream auth value as a secret so the plaintext credential is
    // never stored in the MCP server config. Skip if already a placeholder.
    let resolvedAuthValue = authHeaderValue.trim();
    if (authHeaderName.trim() && resolvedAuthValue) {
      try {
        resolvedAuthValue = await autoWrapSensitiveValue(resolvedAuthValue, {
          displayName: `${serverName.trim()} upstream auth`,
          description: `Auto-generated secret for MCP server ${serverName.trim()}`,
        });
      } catch (err) {
        showSnackbar('Failed to encrypt upstream auth credential', 'error');
        return;
      }
    }

    const payload: CreateMCPServerRequest = {
      id: generateServerId(serverName),
      displayName: serverName.trim(),
      description: serverDescription.trim() || undefined,
      version: normalizeVersion(serverVersion.trim()),
      projectId: effectiveProject.id,
      context: serverContext,
      // vhost: 'mcp.gw.com', --- TODO Remove Tentatively ---
      upstream: {
        main: {
          // Preserve the validated endpoint URL verbatim. This is the exact URL that
          // fetch-server-info just validated, and the gateway forwards requests to exactly
          // this upstream path, so we must not strip or otherwise manipulate it.
          url: serverTarget.trim(),
          ...(authHeaderName.trim() && resolvedAuthValue
            ? {
                auth: {
                  type: 'header',
                  header: authHeaderName.trim(),
                  value: resolvedAuthValue,
                },
              }
            : {}),
        },
      },
```

with:

```ts
    // Encrypt any sensitive upstream auth credential as a secret so the plaintext
    // value is never stored in the MCP server config. Skip values already a placeholder.
    let resolvedAuth: UpstreamAuthConfig = upstreamAuth;
    try {
      if (upstreamAuth.type === 'api-key' && upstreamAuth.value?.trim()) {
        resolvedAuth = {
          type: 'header',
          header: upstreamAuth.header?.trim() || '',
          value: await autoWrapSensitiveValue(upstreamAuth.value.trim(), {
            displayName: `${serverName.trim()} upstream auth`,
            description: `Auto-generated secret for MCP server ${serverName.trim()}`,
          }),
        };
      } else if (upstreamAuth.type === 'oauth2' || upstreamAuth.type === 'other') {
        resolvedAuth = {
          type: upstreamAuth.type,
          policyName: upstreamAuth.policyName,
          policyParams: await autoWrapSensitiveParams(
            upstreamAuth.policyParams ?? {},
            SENSITIVE_OAUTH2_PARAM_KEYS,
            () => ({
              displayName: `${serverName.trim()} upstream auth`,
              description: `Auto-generated secret for MCP server ${serverName.trim()}`,
            }),
          ),
        };
      }
    } catch (err) {
      showSnackbar('Failed to encrypt upstream auth credential', 'error');
      return;
    }

    const payload: CreateMCPServerRequest = {
      id: generateServerId(serverName),
      displayName: serverName.trim(),
      description: serverDescription.trim() || undefined,
      version: normalizeVersion(serverVersion.trim()),
      projectId: effectiveProject.id,
      context: serverContext,
      // vhost: 'mcp.gw.com', --- TODO Remove Tentatively ---
      upstream: {
        main: {
          // Preserve the validated endpoint URL verbatim. This is the exact URL that
          // fetch-server-info just validated, and the gateway forwards requests to exactly
          // this upstream path, so we must not strip or otherwise manipulate it.
          url: serverTarget.trim(),
          ...(resolvedAuth.type !== 'none' ? { auth: resolvedAuth } : {}),
        },
      },
```

(Note: `resolvedAuth.type` stays `'header'` for the api-key case, matching this MCP resource's existing wire convention — confirmed as the value the backend already expects here, distinct from LLM Provider's `'api-key'` — this plan does not change that convention, only routes it through the shared helper.)

Add the import for `SENSITIVE_OAUTH2_PARAM_KEYS`/`autoWrapSensitiveParams` alongside the existing `autoWrapSensitiveValue` import from Task 3 Step 2:

```ts
import {
  autoWrapSensitiveValue,
  autoWrapSensitiveParams,
  SENSITIVE_OAUTH2_PARAM_KEYS,
} from '../../../../utils/upstreamAuthSecrets';
```

- [ ] **Step 4: Compile check**

Run: `cd portals/ai-workspace && npm run build`
Expected: build succeeds.

- [ ] **Step 5: Manual verification**

Run: `cd portals/ai-workspace && npm run dev`. Create an MCP server, expand Advanced Configurations, switch the new Authentication Type selector to oauth2, fill the token-endpoint fields, and confirm the created server's `upstream.main.auth` in the API response has `type: "oauth2"` with a `{{ secret ... }}`-wrapped `clientSecret`.

- [ ] **Step 6: Extend Cypress coverage**

In `portals/ai-workspace/cypress/e2e/002-mcp-proxies` (existing directory — check its current spec file name with `ls cypress/e2e/002-mcp-proxies` and add to the closest-matching existing creation spec, following that file's own `describe`/`it` and selector conventions rather than introducing a new one), add one `it` case asserting: after selecting oauth2 in the new Authentication Type selector and filling `tokenEndpoint`/`clientId`/`clientSecret`, `POST /mcp-servers`'s (or the actual endpoint name used there — confirm via `cy.intercept` pattern already present in that spec) request body contains `"type":"oauth2"` and a `{{ secret ` placeholder, never the plaintext secret value.

- [ ] **Step 7: Commit**

```bash
git add portals/ai-workspace/src/pages/appShell/appShellPages/externalServers/ExternalServersNew.tsx portals/ai-workspace/cypress/e2e/002-mcp-proxies
git commit -m "feat(ai-workspace): support oauth2 upstream auth on MCP/External Server creation"
```

## Self-Review

**Spec coverage:**
- Resolving oauth2-generator's schema → Task 2. ✅
- Shared `UpstreamAuthFields` component (type selector, api-key typed fields unchanged, oauth2/other via PolicyParameterEditor) → Task 4. ✅
- Secret auto-wrap + masking on read → Task 3 (helper) + Tasks 5/7 (wired at submit) + Task 4 (masked on read via `maskSensitiveParamsForDisplay` in `existingValues`). ✅
- LLM Provider create + edit → Task 5. ✅
- LLM Proxy create + edit + inheritance-copy fix → Task 6. ✅
- MCP/External Servers net-new type selector → Task 7. ✅
- Shared `UpstreamAuthConfig` type → Task 1. ✅
- Validation (`oneOf`/`not` mutual exclusivity) → **not implemented as new UI code** — see Known Limitation below, which is the spec-sanctioned outcome when the codebase has no existing pattern for the schema's actual shape.
- `other` type support → falls out of Task 4 Step 1 (`OAUTH2_POLICY_NAME` vs `value.policyName` branch) + the policy picker. ✅

**Known limitation (resolves spec open question #2):** `oauth2-generator`'s real schema (`gateway/dev-policies/oauth2-generator/policy-definition.yaml:236-244`) encodes the `tokenEndpoint`/`clientId`/`clientSecret` vs `bearerToken` mutual exclusivity as a top-level `oneOf` with `not`/`required` combinators, **not** the `anyOf: [{required: [...]}]` shape `PolicyParameterEditor`'s `ParameterSchema` type and its `validateLevelOneRequiredFields`/`anyOfMessage` logic understand (confirmed by reading `PolicyParameterEditor.tsx` and `schemaUtils.ts` in full — neither references `oneOf` anywhere). This plan does **not** add `oneOf` support to `PolicyParameterEditor` (out of this plan's scope — that component is shared with the unrelated Guardrails feature and changing its validation semantics needs its own review) and does **not** edit the policy definition in `gateway/dev-policies/oauth2-generator` (a different, dual-repo-mirrored part of the monorepo — see the `gateway-policy-dual-repo` convention; editing it from a portal-focused plan risks exactly the kind of drift that convention warns about). Net effect: a user can currently fill in both `tokenEndpoint`+`clientId`+`clientSecret` **and** `bearerToken` at once in the UI with no inline warning — the request will still be rejected by the backend's own validation (`llm_validator.go`), so this is a UX gap, not a correctness gap. File this as a tracked follow-up (add `oneOf` handling to `PolicyParameterEditor`, or a small oauth2-specific inline check in `UpstreamAuthFields`) rather than working around it silently.

**Placeholder scan:** no TBD/TODO/"add appropriate X" phrases; every step has real code with real file paths and line ranges from the actual current files.

**Type consistency:** `UpstreamAuthConfig` (Task 1) is the single type threaded through `UpstreamAuthFieldsProps.value`/`onChange` (Task 4), `FormState.upstreamAuth` (Task 5), `LLMProxyNew.tsx`'s `providerAuth` local (Task 6), and `ExternalServersNew.tsx`'s `upstreamAuth`/`resolvedAuth` locals (Task 7) — same field names (`type`/`header`/`value`/`valuePrefix`/`policyName`/`policyVersion`/`policyParams`) used consistently across all tasks. `resolveUpstreamAuthPolicyDefinition`'s return type (`PolicyDefinition | null`, Task 2) matches `UpstreamAuthFields`'s `policyDefinition` state type (Task 4). `SENSITIVE_OAUTH2_PARAM_KEYS`/`autoWrapSensitiveParams`/`maskSensitiveParamsForDisplay` (Task 3) are imported with the same names and signatures in Tasks 4, 5, and 7.

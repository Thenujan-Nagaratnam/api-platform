# Platform API: OAuth2 Upstream Auth Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `oauth2` upstream auth (`policyName`/`policyParams`/`policyVersion`) survive the round trip through Platform API — REST request → DB model → GET/list response → the deployment payload handed to gateway-controller — for LlmProvider, LlmProxy, and MCP, which all share one `UpstreamAuth` type.

**Architecture:** One shared `UpstreamAuth` type is defined once in the OpenAPI spec and reused as one Go type at every layer (generated request/response struct → DB-persisted model struct → deployment-payload struct, which is literally the same generated type reused). Widening it once, then fixing the ~8 mapper/helper functions that currently only know about `type`/`header`/`value`, fixes all three resource kinds together. No database migration is needed — the whole provider/proxy/MCP configuration is JSON-serialized into one DB column (confirmed: every field in `internal/model/upstream.go` is tagged `db:"-"`).

**Tech Stack:** Go, `oapi-codegen` v2.5.1 (OpenAPI-to-Go codegen, already wired into this repo's `Makefile`), standard library `testing` (this repo's test convention — no testify/require, confirmed from `internal/service/llm_deployment_test.go`).

**Spec:** `docs/superpowers/specs/2026-08-24-platform-api-oauth2-upstream-auth-design.md`

## Global Constraints

- Do NOT add `oauth2` handling to `basic`/`bearer` — those stay exactly as they are; do not touch their enum values or behavior (spec Non-goals).
- Do NOT modify gateway-controller — it already fully supports this shape (spec Non-goals).
- Do NOT modify the AI Workspace frontend — it already sends the correct shape (spec Non-goals).
- Do NOT add new secret-ref validation code — `ValidateSecretRefs` (`internal/service/secret_service.go:273`) already scans the *entire* marshaled request JSON via `marshalUpstreamForValidation(req)` (`internal/service/llm.go:3368`, called with the whole `*api.LLMProvider`), which is not field-scoped and already covers `policyParams` once the field exists on the struct. Task 8 proves this with a test rather than adding code.
- This is additive and backward-compatible: existing `api-key`/`none`/`other`/`basic`/`bearer` clients are unaffected. No DB migration (confirmed: `internal/model/upstream.go`'s fields are all `db:"-"`, the whole `Configuration` is one JSON-serialized column).
- `internal/dto/upstream.go`'s separate `UpstreamAuth`/`UpstreamConfig`/`UpstreamEndpoint` types are confirmed dead code (zero call sites anywhere — verified via `grep -rln "dto\.UpstreamAuth\b"`). Do not touch them; do not treat them as part of the live data path.

---

## Task 1: OpenAPI schema + codegen regeneration

**Files:**
- Modify: `platform-api/resources/openapi.yaml:6798-6816` (the `UpstreamAuth` component)
- Modify (generated, do not hand-edit beyond running the tool): `platform-api/api/generated.go` (the `UpstreamAuth` struct, currently at line 2555, and the `UpstreamAuthType` enum constants, currently at lines 317-322)

**Interfaces:**
- Produces: `UpstreamAuth.type` enum gains `oauth2`; `UpstreamAuth` gains `PolicyName *string`, `PolicyParams *map[string]interface{}`, `PolicyVersion *string` (exact Go field names/types confirmed in Step 4 below, after real codegen runs — do not assume before verifying). Consumed by every later task.

- [ ] **Step 1: Edit the OpenAPI schema**

In `platform-api/resources/openapi.yaml`, replace the `UpstreamAuth` component (lines 6798-6816):

```yaml
    UpstreamAuth:
      type: object
      description: Authentication configuration for upstream endpoints
      properties:
        type:
          type: string
          description: Authentication type
          enum: [ basic, bearer, api-key, oauth2, other, none ]
          example: api-key
        header:
          type: string
          description: Header name for api-key authentication (e.g., 'Authorization' for bearer/basic, custom header for api-key)
          example: X-API-Key
        value:
          type: string
          writeOnly: true
          format: password
          description: Authentication value (API key, Bearer token, or Base64 encoded credentials for basic auth)
          example: my-api-key-value
        policyName:
          type: string
          description: >
            Name of the policy that implements this upstream auth when type is "oauth2" or
            "other". Optional for "oauth2" (defaults to the built-in oauth2-generator policy).
            Required for "other", since there is no built-in default for a non-built-in auth
            scheme.
          example: oauth2-generator
        policyParams:
          type: object
          description: >
            Parameters passed verbatim to policyName (or the built-in default for "oauth2").
            Required when type is "oauth2" or "other" — there are no typed fields for this
            path, only this bucket (e.g. {tokenEndpoint: ..., clientId: ..., clientSecret: ...}
            for a token-endpoint grant, or {bearerToken: ...} for a directly-supplied
            credential). Reference a stored secret for any sensitive value, e.g.
            {"clientSecret": "{{ secret \"handle\" }}"}.
          additionalProperties: true
          example:
            tokenEndpoint: https://idp.example.com/oauth2/token
            clientId: my-client-id
            clientSecret: "{{ secret \"my-client-secret\" }}"
        policyVersion:
          type: string
          description: >
            Major version of policyName to attach (e.g. "v1"). Optional — defaults to the
            highest version available in the gateway image when omitted.
          pattern: '^v\d+$'
          example: v1
```

- [ ] **Step 2: Regenerate the Go API types**

Run from `platform-api/`:

```bash
make generate
```

This runs the exact pipeline in `Makefile:150-160` (`yq` binding pass, then `oapi-codegen` v2.5.1 against `resources/openapi_with_binding.yaml`). If `yq` is not installed, install it first (`brew install yq` or `go install github.com/mikefarah/yq/v4@latest`) — the Makefile target checks for this and will tell you.

- [ ] **Step 3: Verify the generated types**

```bash
grep -n "type UpstreamAuth struct" -A 20 platform-api/api/generated.go
grep -n "Oauth2 UpstreamAuthType\|OAuth2 UpstreamAuthType" platform-api/api/generated.go
```

Expected: `UpstreamAuth` struct now has `PolicyName *string`, `PolicyParams *map[string]interface{}`, `PolicyVersion *string` fields (following the exact pattern of the pre-existing `Header *string`/`Value *string` fields — pointer types since all three are optional in the schema), and a new enum constant for `oauth2` (verify its exact generated name — likely `Oauth2` based on this codegen's existing `ApiKey`/`Basic`/`Bearer` pattern, but confirm from the actual grep output rather than assuming, since oapi-codegen's exact capitalization rule for a string like "oauth2" containing a digit is worth checking directly). **Record the exact constant name here for later tasks to use** — if it differs from `Oauth2`, every later task in this plan that references `api.Oauth2` must use the real name instead.

- [ ] **Step 4: Confirm the build still compiles**

```bash
cd platform-api && go build ./...
```

Expected: succeeds (no other code references the new fields yet, so nothing should break).

- [ ] **Step 5: Commit**

```bash
git add platform-api/resources/openapi.yaml platform-api/resources/openapi_with_binding.yaml platform-api/api/generated.go
git commit -m "feat(platform-api): add oauth2 and policyParams/policyName/policyVersion to the UpstreamAuth schema"
```

---

## Task 2: Internal DB model widening

**Files:**
- Modify: `platform-api/internal/model/upstream.go`

**Interfaces:**
- Consumes: nothing from Task 1 directly (this is the internal, DB-persisted shape — a separate Go type from `api.UpstreamAuth`, not generated).
- Produces: `model.UpstreamAuth` gains `PolicyName string`, `PolicyParams map[string]interface{}`, `PolicyVersion string` fields (plain, non-pointer — matching this struct's existing style, where `Type`/`Header`/`Value` are all plain `string`, not pointers, since this is not codegen'd and the existing convention here uses zero-value-as-absent rather than pointers). Consumed by every task from here on.

- [ ] **Step 1: Widen the struct**

Replace the full current content of `platform-api/internal/model/upstream.go` (the `UpstreamAuth` struct at the end of the file) with:

```go
type UpstreamAuth struct {
	Type          string                 `json:"type" db:"-"`
	Header        string                 `json:"header,omitempty" db:"-"`
	Value         string                 `json:"value,omitempty" db:"-"`
	PolicyName    string                 `json:"policyName,omitempty" db:"-"`
	PolicyParams  map[string]interface{} `json:"policyParams,omitempty" db:"-"`
	PolicyVersion string                 `json:"policyVersion,omitempty" db:"-"`
}
```

(`UpstreamConfig` and `UpstreamEndpoint` above it are unchanged.)

- [ ] **Step 2: Confirm the build still compiles**

```bash
cd platform-api && go build ./...
```

Expected: succeeds.

- [ ] **Step 3: Commit**

```bash
git add platform-api/internal/model/upstream.go
git commit -m "feat(platform-api): add policyName/policyParams/policyVersion to the internal UpstreamAuth model"
```

---

## Task 3: Type normalization and credential-shape classification

**Files:**
- Modify: `platform-api/internal/service/llm.go:2397-2418` (`normalizeUpstreamAuthType`)
- Modify: `platform-api/internal/service/llm.go:3510-3517` (`isCredentialLessUpstreamAuthType`)
- Test: `platform-api/internal/service/llm_test.go` (new test functions; check this file's existing `import` block and package name — `package service` — before adding)

**Interfaces:**
- Consumes: `api.Oauth2` (or the real constant name recorded in Task 1 Step 3) from `platform-api/api/generated.go`.
- Produces: `normalizeUpstreamAuthType("oauth2")` now returns `"oauth2"` via the enum constant, matching how the other types resolve; a **new** function `isPolicyParamsUpstreamAuthType(authType string) bool` (see rationale in Step 1) returning `true` for `"oauth2"`/`"other"`, consumed by Tasks 4-7.

- [ ] **Step 1: Write the failing tests**

The single most important fact discovered while planning this: `isCredentialLessUpstreamAuthType` currently groups `"other"` together with `"none"` as "carries no credentials, header/value irrelevant" (`internal/service/llm.go:3510-3517`). That grouping was correct before this change — `"other"` never had a way to carry credentials either. It is **not** correct after this change: both `"oauth2"` and `"other"` now carry credentials via `policyParams`, they just don't use `header`/`value` to do it. Renaming or repurposing `isCredentialLessUpstreamAuthType` risks missing one of its 5 call sites (`llm.go:1274`, `llm.go:1879`, `llm.go:3501`, `llm_deployment.go:1939`, plus its own definition) which govern different things (header/value stripping, secret-rotation cleanup skip). The minimal, correct fix: leave `isCredentialLessUpstreamAuthType` exactly as-is (it still correctly answers "does this type use header/value" — `"none"` and `"other"` both answer no, and that's still true), and add a **new**, separate function for "does this type carry policyParams," since that's a different, orthogonal question this codebase has never needed before now.

Add to `platform-api/internal/service/llm_test.go` (check the file's current package/import header first and match it — do not invent a different package name):

```go
func TestNormalizeUpstreamAuthType_Oauth2(t *testing.T) {
	got := normalizeUpstreamAuthType("oauth2")
	if got != "oauth2" {
		t.Fatalf("expected normalizeUpstreamAuthType(\"oauth2\") to return \"oauth2\", got %q", got)
	}
	// Case/separator variants, matching the existing apiKey/api-key/API_KEY handling.
	for _, variant := range []string{"OAuth2", "OAUTH2", "oauth-2", "oauth_2"} {
		if got := normalizeUpstreamAuthType(variant); got != "oauth2" {
			t.Fatalf("expected normalizeUpstreamAuthType(%q) to return \"oauth2\", got %q", variant, got)
		}
	}
}

func TestIsPolicyParamsUpstreamAuthType(t *testing.T) {
	cases := map[string]bool{
		"oauth2":  true,
		"other":   true,
		"api-key": false,
		"none":    false,
		"basic":   false,
		"bearer":  false,
		"":        false,
	}
	for authType, want := range cases {
		if got := isPolicyParamsUpstreamAuthType(authType); got != want {
			t.Errorf("isPolicyParamsUpstreamAuthType(%q) = %v, want %v", authType, got, want)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd platform-api && go test ./internal/service/... -run 'TestNormalizeUpstreamAuthType_Oauth2|TestIsPolicyParamsUpstreamAuthType' -v
```

Expected: `TestNormalizeUpstreamAuthType_Oauth2` FAILs (the "oauth2" case falls through `normalizeUpstreamAuthType`'s `default: return normalized` branch, returning `"oauth2"` unchanged as a bare string rather than through the `api.Oauth2` constant path — actually re-read the failure carefully: since the raw lowercase input already equals what the function should return, this specific assertion may not fail on the exact-match case, but the case-variant checks (`"OAuth2"`, `"oauth-2"`) WILL fail, since the current `canonical` computation strips `-`/`_` and lowercases, so `"oauth-2"` becomes `"oauth2"` — which then does NOT match any `case` in the switch and falls through to `default: return normalized` — returning the *original, unstripped* input (`"oauth-2"`), not the canonical `"oauth2"`. This is the real bug being fixed.). `TestIsPolicyParamsUpstreamAuthType` FAILs to compile (`isPolicyParamsUpstreamAuthType` does not exist yet).

- [ ] **Step 3: Implement**

In `platform-api/internal/service/llm.go`, add a case to the `switch` inside `normalizeUpstreamAuthType` (between the existing `"other"` and `"none"` cases, matching the existing alphabetical-ish ordering isn't required — but keep it readable):

```go
	canonical := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(normalized, "-", ""), "_", ""))
	switch canonical {
	case "apikey":
		return string(api.ApiKey)
	case "basic":
		return string(api.Basic)
	case "bearer":
		return string(api.Bearer)
	case "oauth2":
		return string(api.Oauth2) // use the real constant name recorded in Task 1 Step 3 if it differs
	case "other":
		return string(api.Other)
	case "none":
		return string(api.None)
	default:
		return normalized
	}
```

Add a new function directly below `isCredentialLessUpstreamAuthType` (after line 3517):

```go
// isPolicyParamsUpstreamAuthType reports whether an upstream auth type carries its
// credentials via policyParams ("oauth2" or "other") rather than header/value. This is
// distinct from isCredentialLessUpstreamAuthType, which answers a different question
// (does this type use header/value at all) — "other" answers false to that one and
// true to this one, since it carries credentials, just not via header/value.
func isPolicyParamsUpstreamAuthType(authType string) bool {
	switch normalizeUpstreamAuthType(authType) {
	case string(api.Oauth2), string(api.Other):
		return true
	default:
		return false
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd platform-api && go test ./internal/service/... -run 'TestNormalizeUpstreamAuthType_Oauth2|TestIsPolicyParamsUpstreamAuthType' -v
```

Expected: both PASS.

- [ ] **Step 5: Run the full existing test suite for this package to confirm no regressions**

```bash
cd platform-api && go test ./internal/service/... -run 'UpstreamAuth|NormalizeUpstream|CredentialLess' -v
```

Expected: all PASS, including pre-existing tests for `normalizeUpstreamAuthType`/`isCredentialLessUpstreamAuthType` if any exist (search for them first: `grep -rn "normalizeUpstreamAuthType\|isCredentialLessUpstreamAuthType" internal/service/*_test.go`).

- [ ] **Step 6: Commit**

```bash
git add platform-api/internal/service/llm.go platform-api/internal/service/llm_test.go
git commit -m "feat(platform-api): recognize oauth2 in normalizeUpstreamAuthType, add isPolicyParamsUpstreamAuthType"
```

---

## Task 4: Request↔model mappers for LlmProvider's main/sandbox auth

**Files:**
- Modify: `platform-api/internal/service/llm.go:2332-2345` (`mapUpstreamAuthAPIToModel`)
- Modify: `platform-api/internal/service/llm.go:3492-3505` (`defaultUpstreamAuthToNone`)
- Modify: `platform-api/internal/service/llm.go:2012-2032` (`preserveUpstreamAuthValue`)
- Test: `platform-api/internal/service/llm_test.go`

**Interfaces:**
- Consumes: `isPolicyParamsUpstreamAuthType` (Task 3); `model.UpstreamAuth`'s new fields (Task 2); `api.UpstreamAuth`'s new fields (Task 1).
- Produces: nothing new consumed by later tasks — `mapUpstreamAuthAPIToModel`/`defaultUpstreamAuthToNone`/`preserveUpstreamAuthValue` are called by `LLMProviderService.Create`/`Update` (already-existing call sites, unchanged signatures).

- [ ] **Step 1: Write the failing tests**

Add to `platform-api/internal/service/llm_test.go`:

```go
func TestMapUpstreamAuthAPIToModel_Oauth2CarriesPolicyParams(t *testing.T) {
	oauth2Type := api.Oauth2 // use the real constant name recorded in Task 1 Step 3 if it differs
	policyName := "oauth2-generator"
	params := map[string]interface{}{
		"tokenEndpoint": "https://idp.example.com/oauth2/token",
		"clientId":      "my-client-id",
		"clientSecret":  "{{ secret \"my-handle\" }}",
	}
	in := &api.UpstreamAuth{
		Type:         &oauth2Type,
		PolicyName:   &policyName,
		PolicyParams: &params,
	}

	out := mapUpstreamAuthAPIToModel(in)

	if out.Type != "oauth2" {
		t.Fatalf("expected type oauth2, got %q", out.Type)
	}
	if out.PolicyName != "oauth2-generator" {
		t.Fatalf("expected policyName oauth2-generator, got %q", out.PolicyName)
	}
	if out.PolicyParams["clientId"] != "my-client-id" {
		t.Fatalf("expected clientId to survive mapping, got %+v", out.PolicyParams)
	}
	if out.PolicyParams["clientSecret"] != "{{ secret \"my-handle\" }}" {
		t.Fatalf("expected clientSecret placeholder to survive mapping unresolved, got %+v", out.PolicyParams)
	}
}

func TestDefaultUpstreamAuthToNone_ClearsPolicyParamsWhenSwitchingToNone(t *testing.T) {
	auth := &model.UpstreamAuth{
		Type:         "none",
		PolicyName:   "oauth2-generator",
		PolicyParams: map[string]interface{}{"clientId": "stale"},
	}

	out := defaultUpstreamAuthToNone(auth)

	if out.PolicyName != "" {
		t.Errorf("expected PolicyName cleared when switching to none, got %q", out.PolicyName)
	}
	if out.PolicyParams != nil {
		t.Errorf("expected PolicyParams cleared when switching to none, got %+v", out.PolicyParams)
	}
}

func TestPreserveUpstreamAuthValue_PreservesPolicyParamsWhenUpdateOmitsThem(t *testing.T) {
	existing := &model.UpstreamConfig{
		Main: &model.UpstreamEndpoint{
			Auth: &model.UpstreamAuth{
				Type:         "oauth2",
				PolicyName:   "oauth2-generator",
				PolicyParams: map[string]interface{}{"clientId": "existing-client-id", "clientSecret": "{{ secret \"h1\" }}"},
			},
		},
	}
	updated := &model.UpstreamConfig{
		Main: &model.UpstreamEndpoint{
			Auth: &model.UpstreamAuth{
				Type: "oauth2", // same type, but PolicyParams omitted on this update
			},
		},
	}

	out := preserveUpstreamAuthValue(existing, updated)

	if out.Main.Auth.PolicyParams["clientId"] != "existing-client-id" {
		t.Fatalf("expected stored policyParams to be preserved when update omits them, got %+v", out.Main.Auth.PolicyParams)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd platform-api && go test ./internal/service/... -run 'TestMapUpstreamAuthAPIToModel_Oauth2CarriesPolicyParams|TestDefaultUpstreamAuthToNone_ClearsPolicyParamsWhenSwitchingToNone|TestPreserveUpstreamAuthValue_PreservesPolicyParamsWhenUpdateOmitsThem' -v
```

Expected: all 3 FAIL — `mapUpstreamAuthAPIToModel` doesn't read the new fields yet (`out.PolicyName`/`out.PolicyParams` are zero-value), `defaultUpstreamAuthToNone` doesn't clear them, `preserveUpstreamAuthValue` doesn't preserve them.

- [ ] **Step 3: Implement**

Replace `mapUpstreamAuthAPIToModel` (`internal/service/llm.go:2332-2345`):

```go
func mapUpstreamAuthAPIToModel(in *api.UpstreamAuth) *model.UpstreamAuth {
	if in == nil {
		return nil
	}
	authType := ""
	if in.Type != nil {
		authType = normalizeUpstreamAuthType(string(*in.Type))
	}
	out := &model.UpstreamAuth{
		Type:   authType,
		Header: utils.ValueOrEmpty(in.Header),
		Value:  utils.ValueOrEmpty(in.Value),
	}
	if isPolicyParamsUpstreamAuthType(authType) {
		out.PolicyName = utils.ValueOrEmpty(in.PolicyName)
		out.PolicyVersion = utils.ValueOrEmpty(in.PolicyVersion)
		if in.PolicyParams != nil {
			out.PolicyParams = *in.PolicyParams
		}
	}
	return out
}
```

(The `isPolicyParamsUpstreamAuthType` gate mirrors the existing convention elsewhere in this file of only carrying the fields relevant to a given type — matching how `defaultUpstreamAuthToNone` already strips `Header`/`Value` for credential-less types.)

Replace `defaultUpstreamAuthToNone` (`internal/service/llm.go:3492-3505` — the doc comment above it stays, only the body changes):

```go
func defaultUpstreamAuthToNone(auth *model.UpstreamAuth) *model.UpstreamAuth {
	if auth == nil {
		return &model.UpstreamAuth{Type: string(api.None)}
	}
	if strings.TrimSpace(auth.Type) == "" {
		auth.Type = string(api.None)
	}
	if isCredentialLessUpstreamAuthType(auth.Type) {
		auth.Header = ""
		auth.Value = ""
	}
	if !isPolicyParamsUpstreamAuthType(auth.Type) {
		auth.PolicyName = ""
		auth.PolicyParams = nil
		auth.PolicyVersion = ""
	}
	return auth
}
```

Replace `preserveUpstreamAuthValue` (`internal/service/llm.go:2012-2032`):

```go
func preserveUpstreamAuthValue(existing, updated *model.UpstreamConfig) *model.UpstreamConfig {
	if updated == nil {
		return existing
	}
	if existing == nil {
		return updated
	}
	if updated.Main == nil {
		return existing
	}
	if existing.Main == nil || existing.Main.Auth == nil {
		return updated
	}
	if updated.Main.Auth == nil {
		return updated
	}
	if updated.Main.Auth.Value == "" {
		updated.Main.Auth.Value = existing.Main.Auth.Value
	}
	if len(updated.Main.Auth.PolicyParams) == 0 {
		updated.Main.Auth.PolicyParams = existing.Main.Auth.PolicyParams
		if updated.Main.Auth.PolicyName == "" {
			updated.Main.Auth.PolicyName = existing.Main.Auth.PolicyName
		}
		if updated.Main.Auth.PolicyVersion == "" {
			updated.Main.Auth.PolicyVersion = existing.Main.Auth.PolicyVersion
		}
	}
	return updated
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd platform-api && go test ./internal/service/... -run 'TestMapUpstreamAuthAPIToModel_Oauth2CarriesPolicyParams|TestDefaultUpstreamAuthToNone_ClearsPolicyParamsWhenSwitchingToNone|TestPreserveUpstreamAuthValue_PreservesPolicyParamsWhenUpdateOmitsThem' -v
```

Expected: all PASS.

- [ ] **Step 5: Run the broader package test suite**

```bash
cd platform-api && go test ./internal/service/... -v 2>&1 | tail -100
```

Expected: no new failures versus a baseline run (run the same command on the pre-Task-4 commit if any failure looks suspicious, to confirm it's pre-existing and unrelated).

- [ ] **Step 6: Commit**

```bash
git add platform-api/internal/service/llm.go platform-api/internal/service/llm_test.go
git commit -m "feat(platform-api): thread policyParams through the LlmProvider request/model mappers"
```

---

## Task 5: GET/list response mapping (the DTO redaction path)

**Files:**
- Modify: `platform-api/internal/service/llm.go:2476-2528` (`mapUpstreamConfigToDTO`)
- Test: `platform-api/internal/service/llm_test.go`

**Interfaces:**
- Consumes: `model.UpstreamAuth`'s new fields (Task 2).
- Produces: nothing new consumed by later tasks — `mapUpstreamConfigToDTO` is called from `toProviderAPI` (`internal/service/llm.go:2882`, unchanged call site) to build every GET/list response.

- [ ] **Step 1: Write the failing test**

This is the function the AI Workspace frontend directly depends on: `ServiceProviderConnectionTab.tsx` hydrates its edit form from `provider.upstream.main.auth.policyParams` on every GET — if this function keeps dropping the field, the UI can display a provider's oauth2 config for editing but the initial state loads no oauth2 config. Add to `platform-api/internal/service/llm_test.go`:

```go
func TestMapUpstreamConfigToDTO_IncludesPolicyParamsForOauth2(t *testing.T) {
	cfg := &model.UpstreamConfig{
		Main: &model.UpstreamEndpoint{
			URL: "https://api.anthropic.com",
			Auth: &model.UpstreamAuth{
				Type:       "oauth2",
				PolicyName: "oauth2-generator",
				PolicyParams: map[string]interface{}{
					"tokenEndpoint": "https://idp.example.com/oauth2/token",
					"clientSecret":  "{{ secret \"h1\" }}",
				},
			},
		},
	}

	out := mapUpstreamConfigToDTO(cfg)

	if out.Main.Auth == nil {
		t.Fatal("expected Main.Auth to be present")
	}
	if out.Main.Auth.PolicyName == nil || *out.Main.Auth.PolicyName != "oauth2-generator" {
		t.Fatalf("expected policyName to survive to the DTO, got %+v", out.Main.Auth.PolicyName)
	}
	if out.Main.Auth.PolicyParams == nil {
		t.Fatal("expected policyParams to survive to the DTO")
	}
	if (*out.Main.Auth.PolicyParams)["tokenEndpoint"] != "https://idp.example.com/oauth2/token" {
		t.Fatalf("expected tokenEndpoint to survive to the DTO, got %+v", *out.Main.Auth.PolicyParams)
	}
	// Value stays redacted regardless of type — this is unchanged behavior.
	if out.Main.Auth.Value != nil {
		t.Fatalf("expected Value to remain redacted (nil), got %v", *out.Main.Auth.Value)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd platform-api && go test ./internal/service/... -run TestMapUpstreamConfigToDTO_IncludesPolicyParamsForOauth2 -v
```

Expected: FAIL — `mapUpstreamConfigToDTO` currently only sets `Type`/`Header`/`Value` (redacted to nil) on the returned `api.UpstreamAuth`, so `PolicyName`/`PolicyParams` are nil.

- [ ] **Step 3: Implement**

Replace `mapUpstreamConfigToDTO` (`internal/service/llm.go:2476-2528`) — both the `Main` and `Sandbox` blocks need the same change:

```go
func mapUpstreamConfigToDTO(in *model.UpstreamConfig) api.Upstream {
	main := api.UpstreamDefinition{}
	if in != nil && in.Main != nil {
		if strings.TrimSpace(in.Main.URL) != "" {
			u := in.Main.URL
			main.Url = &u
		}
		if strings.TrimSpace(in.Main.Ref) != "" {
			r := in.Main.Ref
			main.Ref = &r
		}
		if in.Main.Auth != nil {
			main.Auth = redactedUpstreamAuthDTO(in.Main.Auth)
		}
	}
	var sandbox *api.UpstreamDefinition
	if in != nil && in.Sandbox != nil {
		s := api.UpstreamDefinition{}
		if strings.TrimSpace(in.Sandbox.URL) != "" {
			u := in.Sandbox.URL
			s.Url = &u
		}
		if strings.TrimSpace(in.Sandbox.Ref) != "" {
			r := in.Sandbox.Ref
			s.Ref = &r
		}
		if in.Sandbox.Auth != nil {
			s.Auth = redactedUpstreamAuthDTO(in.Sandbox.Auth)
		}
		sandbox = &s
	}
	return api.Upstream{Main: main, Sandbox: sandbox}
}

// redactedUpstreamAuthDTO builds the client-facing auth representation for a GET/list
// response: Value is always redacted (it is writeOnly — an api-key/bearer credential must
// never come back on a read), but PolicyName/PolicyParams are NOT redacted for oauth2/other,
// since any sensitive entry inside PolicyParams is already a {{ secret "..." }} placeholder
// by the time it is stored (see mapUpstreamAuthAPIToModel / autoWrapSensitiveParams on the
// AI Workspace frontend side) — the placeholder itself is safe to return, and the AI
// Workspace edit form depends on reading it back to hydrate its state.
func redactedUpstreamAuthDTO(in *model.UpstreamAuth) *api.UpstreamAuth {
	authType := (*api.UpstreamAuthType)(nil)
	if in.Type != "" {
		t := api.UpstreamAuthType(in.Type)
		authType = &t
	}
	out := &api.UpstreamAuth{
		Type:   authType,
		Header: utils.StringPtrIfNotEmpty(in.Header),
		Value:  nil, // Redact value
	}
	if isPolicyParamsUpstreamAuthType(in.Type) {
		out.PolicyName = utils.StringPtrIfNotEmpty(in.PolicyName)
		out.PolicyVersion = utils.StringPtrIfNotEmpty(in.PolicyVersion)
		if len(in.PolicyParams) > 0 {
			params := in.PolicyParams
			out.PolicyParams = &params
		}
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd platform-api && go test ./internal/service/... -run TestMapUpstreamConfigToDTO_IncludesPolicyParamsForOauth2 -v
```

Expected: PASS.

- [ ] **Step 5: Run the broader package test suite**

```bash
cd platform-api && go test ./internal/service/... -v 2>&1 | tail -100
```

Expected: no new failures.

- [ ] **Step 6: Commit**

```bash
git add platform-api/internal/service/llm.go platform-api/internal/service/llm_test.go
git commit -m "feat(platform-api): include policyParams/policyName in GET/list responses for oauth2/other"
```

---

## Task 6: Deployment-payload mapper (the actual hand-off to gateway-controller)

**Files:**
- Modify: `platform-api/internal/service/llm_deployment.go:1925-1947` (`mapModelAuthToAPI`)
- Test: `platform-api/internal/service/llm_deployment_test.go`

**Interfaces:**
- Consumes: `model.UpstreamAuth`'s new fields (Task 2); `isPolicyParamsUpstreamAuthType` (Task 3).
- Produces: nothing new consumed by later tasks. This single function is called from 3 sites (confirmed via `grep -rn "mapModelAuthToAPI(" internal/service/*.go`): `llm_deployment.go:1109` (LlmProvider deploy), `llm_deployment.go:1878` (LlmProxy deploy), `artifact_import_mcp.go:160` (MCP artifact import) — fixing the one function fixes all three deployment paths without touching their call sites.

- [ ] **Step 1: Write the failing tests**

This is the most consequential function in this plan: it currently early-returns `&api.UpstreamAuth{Type: &t}` for any type `isCredentialLessUpstreamAuthType` calls credential-less — which today includes `"other"`, and which `"oauth2"` would also hit if left unhandled, since it isn't in the recognized-type switch inside `mapModelAuthToAPI` at all yet. This early return is exactly why a correctly-stored oauth2 config in the DB never reaches the deployment payload sent to gateway-controller. Add to `platform-api/internal/service/llm_deployment_test.go` (following this file's existing table style seen in `TestMapModelAuthToAPI_NormalizesApiKeyType`/`TestMapModelAuthToAPI_KeepsApiKeyWhenCredentialPresent`):

```go
func TestMapModelAuthToAPI_Oauth2CarriesPolicyParamsToDeploymentPayload(t *testing.T) {
	auth := &model.UpstreamAuth{
		Type:       "oauth2",
		PolicyName: "oauth2-generator",
		PolicyParams: map[string]interface{}{
			"tokenEndpoint": "https://idp.example.com/oauth2/token",
			"clientId":      "my-client-id",
			"clientSecret":  "{{ secret \"h1\" }}",
		},
	}

	out := mapModelAuthToAPI(auth)

	if out == nil || out.Type == nil || *out.Type != "oauth2" {
		t.Fatalf("expected type oauth2, got %+v", out)
	}
	if out.PolicyName == nil || *out.PolicyName != "oauth2-generator" {
		t.Fatalf("expected policyName oauth2-generator to reach the deployment payload, got %+v", out.PolicyName)
	}
	if out.PolicyParams == nil {
		t.Fatal("expected policyParams to reach the deployment payload")
	}
	if (*out.PolicyParams)["clientId"] != "my-client-id" {
		t.Fatalf("expected clientId to reach the deployment payload, got %+v", *out.PolicyParams)
	}
	// Header/Value must NOT be set for oauth2 — it doesn't use them.
	if out.Header != nil || out.Value != nil {
		t.Fatalf("expected no header/value for oauth2, got header=%v value=%v", out.Header, out.Value)
	}
}

func TestMapModelAuthToAPI_OtherStillOmitsPolicyParamsWhenAbsent(t *testing.T) {
	// "other" with no stored PolicyParams (e.g. never configured) must not panic or
	// produce a non-nil-but-empty PolicyParams pointer.
	auth := &model.UpstreamAuth{Type: "other"}

	out := mapModelAuthToAPI(auth)

	if out == nil || out.Type == nil || *out.Type != "other" {
		t.Fatalf("expected type other, got %+v", out)
	}
	if out.PolicyParams != nil {
		t.Fatalf("expected nil policyParams when none stored, got %+v", *out.PolicyParams)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd platform-api && go test ./internal/service/... -run 'TestMapModelAuthToAPI_Oauth2CarriesPolicyParamsToDeploymentPayload|TestMapModelAuthToAPI_OtherStillOmitsPolicyParamsWhenAbsent' -v
```

Expected: `TestMapModelAuthToAPI_Oauth2CarriesPolicyParamsToDeploymentPayload` FAILs — today `mapModelAuthToAPI` returns `Type` only for `"oauth2"` (falls into the `isCredentialLessUpstreamAuthType` early-return path indirectly, since `"oauth2"` isn't recognized by `normalizeUpstreamAuthType` before Task 3 lands — with Task 3 in place, `"oauth2"` normalizes correctly but `isCredentialLessUpstreamAuthType("oauth2")` still returns `false` today since oauth2 isn't in that switch either — so it would currently fall through to the final `return &api.UpstreamAuth{Type: &t, Header: ..., Value: ...}` branch with both Header/Value empty-but-present pointers, and PolicyParams nil regardless). `TestMapModelAuthToAPI_OtherStillOmitsPolicyParamsWhenAbsent` should already PASS with no changes (documenting existing-and-still-correct behavior) — if it fails, that's new information to resolve during implementation, not a sign the test is wrong.

- [ ] **Step 3: Implement**

Replace `mapModelAuthToAPI` (`internal/service/llm_deployment.go:1925-1947`):

```go
// mapModelAuthToAPI converts a stored model.UpstreamAuth into the api.UpstreamAuth shape
// sent to gateway-controller as part of a deployment payload. The gateway accepts an
// explicit type of "api-key", "oauth2", "other", or "none"; absent/empty auth defaults to
// "none"; "none" carries only the type; "api-key" (and legacy basic/bearer) carry header
// and value; "oauth2" and "other" carry policyName/policyParams/policyVersion instead.
func mapModelAuthToAPI(auth *model.UpstreamAuth) *api.UpstreamAuth {
	if auth == nil {
		t := api.None
		return &api.UpstreamAuth{Type: &t}
	}
	authType := string(api.None)
	if normalized := normalizeUpstreamAuthType(auth.Type); normalized != "" {
		authType = normalized
	}
	t := api.UpstreamAuthType(authType)
	if isPolicyParamsUpstreamAuthType(authType) {
		out := &api.UpstreamAuth{
			Type:       &t,
			PolicyName: utils.StringPtrIfNotEmpty(auth.PolicyName),
		}
		if v := auth.PolicyVersion; v != "" {
			out.PolicyVersion = &v
		}
		if len(auth.PolicyParams) > 0 {
			params := auth.PolicyParams
			out.PolicyParams = &params
		}
		return out
	}
	if isCredentialLessUpstreamAuthType(authType) {
		return &api.UpstreamAuth{Type: &t}
	}
	return &api.UpstreamAuth{
		Type:   &t,
		Header: utils.StringPtrIfNotEmpty(auth.Header),
		Value:  utils.StringPtrIfNotEmpty(auth.Value),
	}
}
```

(The `isPolicyParamsUpstreamAuthType` branch is checked *before* `isCredentialLessUpstreamAuthType`, since `"other"` currently returns `true` from the latter and would otherwise still hit the old type-only early return — ordering here is load-bearing, not stylistic.)

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd platform-api && go test ./internal/service/... -run 'TestMapModelAuthToAPI_Oauth2CarriesPolicyParamsToDeploymentPayload|TestMapModelAuthToAPI_OtherStillOmitsPolicyParamsWhenAbsent|TestMapModelAuthToAPI_NormalizesApiKeyType|TestMapModelAuthToAPI_KeepsApiKeyWhenCredentialPresent' -v
```

Expected: all 4 PASS (the last two are the pre-existing tests in this file — confirm they still pass unchanged).

- [ ] **Step 5: Run the broader package test suite**

```bash
cd platform-api && go test ./internal/service/... -v 2>&1 | tail -100
```

Expected: no new failures.

- [ ] **Step 6: Commit**

```bash
git add platform-api/internal/service/llm_deployment.go platform-api/internal/service/llm_deployment_test.go
git commit -m "feat(platform-api): carry policyParams through to the gateway-controller deployment payload"
```

---

## Task 7: MCP's own preserve-on-update function

**Files:**
- Modify: `platform-api/internal/service/mcp.go:843-863` (`preserveMCPUpstreamAuthValue`)
- Test: `platform-api/internal/service/mcp_test.go`

**Interfaces:**
- Consumes: `model.UpstreamAuth`'s new fields (Task 2).
- Produces: nothing new consumed by later tasks. `preserveMCPUpstreamAuthValue` is called from `MCPProxyService.Update` (`internal/service/mcp.go`, unchanged call site) — MCP has its own separate copy of this preserve logic (not the shared `preserveUpstreamAuthValue` fixed in Task 4), so it needs its own fix.

- [ ] **Step 1: Write the failing test**

Check `platform-api/internal/service/mcp_test.go`'s existing package/import header before adding (this repo's MCP tests already construct `model.UpstreamAuth{Header: ..., Value: ...}` literals per `mcp_test.go:227`, confirmed earlier — match that style):

```go
func TestPreserveMCPUpstreamAuthValue_PreservesPolicyParamsWhenUpdateOmitsThem(t *testing.T) {
	existing := &model.UpstreamConfig{
		Main: &model.UpstreamEndpoint{
			Auth: &model.UpstreamAuth{
				Type:         "oauth2",
				PolicyName:   "oauth2-generator",
				PolicyParams: map[string]interface{}{"clientId": "existing-client-id"},
			},
		},
	}
	updated := &model.UpstreamConfig{
		Main: &model.UpstreamEndpoint{
			Auth: &model.UpstreamAuth{
				Type: "oauth2",
			},
		},
	}

	out := preserveMCPUpstreamAuthValue(existing, updated)

	if out.Main.Auth.PolicyParams["clientId"] != "existing-client-id" {
		t.Fatalf("expected stored policyParams to be preserved, got %+v", out.Main.Auth.PolicyParams)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd platform-api && go test ./internal/service/... -run TestPreserveMCPUpstreamAuthValue_PreservesPolicyParamsWhenUpdateOmitsThem -v
```

Expected: FAIL — `preserveMCPUpstreamAuthValue` currently only preserves `.Value`.

- [ ] **Step 3: Implement**

Replace `preserveMCPUpstreamAuthValue` (`internal/service/mcp.go:843-863`):

```go
func preserveMCPUpstreamAuthValue(existing, updated *model.UpstreamConfig) *model.UpstreamConfig {
	if updated == nil {
		return existing
	}
	if existing == nil {
		return updated
	}
	if updated.Main == nil {
		return updated
	}
	if existing.Main == nil || existing.Main.Auth == nil {
		return updated
	}
	if updated.Main.Auth == nil {
		return updated
	}
	if updated.Main.Auth.Value == "" {
		updated.Main.Auth.Value = existing.Main.Auth.Value
	}
	if len(updated.Main.Auth.PolicyParams) == 0 {
		updated.Main.Auth.PolicyParams = existing.Main.Auth.PolicyParams
		if updated.Main.Auth.PolicyName == "" {
			updated.Main.Auth.PolicyName = existing.Main.Auth.PolicyName
		}
		if updated.Main.Auth.PolicyVersion == "" {
			updated.Main.Auth.PolicyVersion = existing.Main.Auth.PolicyVersion
		}
	}
	return updated
}
```

(Same shape as Task 4's fix to `preserveUpstreamAuthValue` — MCP maintains a separate copy of this logic rather than sharing the function, matching this codebase's existing convention of not sharing it; do not attempt to unify these into one shared function as part of this plan — that is a larger refactor with its own review surface, out of scope here.)

- [ ] **Step 4: Run test to verify it passes**

```bash
cd platform-api && go test ./internal/service/... -run TestPreserveMCPUpstreamAuthValue_PreservesPolicyParamsWhenUpdateOmitsThem -v
```

Expected: PASS.

- [ ] **Step 5: Run the broader package test suite**

```bash
cd platform-api && go test ./internal/service/... -v 2>&1 | tail -100
```

Expected: no new failures.

- [ ] **Step 6: Commit**

```bash
git add platform-api/internal/service/mcp.go platform-api/internal/service/mcp_test.go
git commit -m "feat(platform-api): preserve policyParams on MCP upstream auth updates"
```

---

## Task 8: Secret-ref validation coverage proof + end-to-end round-trip tests

**Files:**
- Test: `platform-api/internal/service/llm_secret_validation_test.go` (existing file — check its current content/conventions before adding)
- Test: `platform-api/internal/service/llm_deployment_test.go` (round-trip additions)

**Interfaces:**
- Consumes: everything from Tasks 1-7.
- Produces: nothing — this is the final proof/coverage task, no later task depends on it.

- [ ] **Step 1: Write the failing test proving secret-ref validation already covers policyParams**

Read `platform-api/internal/service/llm_secret_validation_test.go` in full first to match its exact existing test setup pattern (how it constructs a `SecretService`, what `repo.Exists` mock/fake it uses) — do not invent a different setup style. Using that same pattern, add a test asserting: a request with `policyParams.clientSecret` set to a raw (non-placeholder) string is rejected, and one with a proper `{{ secret "handle" }}` placeholder for an existing secret passes — proving `ValidateSecretRefs`/`marshalUpstreamForValidation` already catches this via the whole-request JSON scan without any new code (per this plan's Global Constraints). The exact test body depends on the fixtures/mocks that file already sets up — mirror its nearest existing test for a `value` field, adapted to `policyParams.clientSecret`.

- [ ] **Step 2: Run the test to verify it fails (or passes) as expected**

```bash
cd platform-api && go test ./internal/service/... -run TestValidateSecretRefs -v
```

If the new test PASSES immediately with no code changes: this confirms the Global Constraints claim and there is nothing to implement — proceed to Step 3 documenting that outcome. If it FAILS: this is new information contradicting the spec's Non-goals claim (the whole-JSON scan does NOT already cover `policyParams` for some reason) — stop, do not silently add validation code to make it pass; report this as a finding, since it means the spec's stated Non-goal was wrong and needs the controller/human to decide the right fix rather than the plan being extended ad hoc mid-task.

- [ ] **Step 3: Write round-trip tests per resource kind**

Add to `platform-api/internal/service/llm_deployment_test.go`, following its existing style (the two tests already read in this plan's research, e.g. `TestGenerateLLMProviderDeploymentYAML_OtherAuthEmitsTypeOnly` — check that test's fixture-setup pattern, likely a fake/mock repo, and mirror it):

```go
func TestOauth2UpstreamAuth_SurvivesToDeploymentPayload(t *testing.T) {
	stored := &model.UpstreamAuth{
		Type:       "oauth2",
		PolicyName: "oauth2-generator",
		PolicyParams: map[string]interface{}{
			"tokenEndpoint": "https://idp.example.com/oauth2/token",
			"clientId":      "my-client-id",
			"clientSecret":  "{{ secret \"h1\" }}",
		},
	}

	deployed := mapModelAuthToAPI(stored)

	if deployed.PolicyParams == nil || (*deployed.PolicyParams)["tokenEndpoint"] != "https://idp.example.com/oauth2/token" {
		t.Fatalf("expected oauth2 config to survive model -> deployment-payload mapping, got %+v", deployed)
	}
}
```

This single function-level test stands in for the full HTTP round trip (request → handler → service → repo → deployment) since Tasks 4-6 already unit-test every individual mapping step in isolation; a true end-to-end test would need a running Postgres + full handler wiring, which is disproportionate for this plan — if this repo has an existing integration-test harness that spins up a real DB (check for one via `grep -rln "sqlmock\|testcontainers\|_integration_test.go" internal/service/`), extend it instead of this narrower unit test; otherwise, this unit-level chain is the right-sized proof.

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd platform-api && go test ./internal/service/... -run 'TestValidateSecretRefs|TestOauth2UpstreamAuth_SurvivesToDeploymentPayload' -v
```

Expected: all PASS.

- [ ] **Step 5: Run the full test suite one final time**

```bash
cd platform-api && go build ./... && go test ./... 2>&1 | tail -150
```

Expected: build succeeds, no test failures anywhere in the module (not just `internal/service`).

- [ ] **Step 6: Commit**

```bash
git add platform-api/internal/service/llm_secret_validation_test.go platform-api/internal/service/llm_deployment_test.go
git commit -m "test(platform-api): prove secret-ref validation covers policyParams, add oauth2 round-trip coverage"
```

---

## Self-Review

**Spec coverage:**
- OpenAPI schema + `oauth2` enum + `policyName`/`policyParams`/`policyVersion` → Task 1. ✅
- `api/generated.go` regeneration → Task 1. ✅
- `model.UpstreamAuth` widening, no DB migration → Task 2. ✅
- `mapUpstreamAPIToModel`/`toProviderAPI` (request/DB mapper) → Task 4 (`mapUpstreamAuthAPIToModel`, the actual per-auth mapper `mapUpstreamAPIToModel` calls) + Task 5 (the response half, `mapUpstreamConfigToDTO`, which is what `toProviderAPI` actually calls — not `mapUpstreamAuthModelToAPI`, which research found has no live call site in the response path; noted as an observation, not touched, since it's out of the request/response path this plan traces). ✅
- `preserveUpstreamAuthValue` → Task 4. ✅
- `mapModelAuthToAPI` (deployment payload) → Task 6, covering all 3 real call sites (LlmProvider, LlmProxy, MCP artifact import) via the one shared function. ✅
- MCP's own call sites → Task 7 (`preserveMCPUpstreamAuthValue`) + Task 6 covers MCP's deployment path already via the shared `mapModelAuthToAPI`. ✅
- Secret-ref validation coverage (proving the Non-goal, not adding code) → Task 8. ✅
- `internal/dto/upstream.go` dead code → explicitly called out in Global Constraints as not-touched, not a task.
- `defaultUpstreamAuthToNone`/`isCredentialLessUpstreamAuthType`/`mainUpstreamAuthType`/`mainUpstreamAuthValue`/`normalizeUpstreamAuthType` audit → `normalizeUpstreamAuthType` fixed in Task 3 (real bug found: case-variant inputs like `"oauth-2"` fell through un-normalized); `defaultUpstreamAuthToNone` fixed in Task 4; `isCredentialLessUpstreamAuthType` deliberately left unchanged with a new sibling function added instead (Task 3), since changing its existing return value for `"other"` would silently alter its other call sites' behavior (`llm.go:1274`, `:1879`, `:3501`) in ways not audited by this plan; `mainUpstreamAuthType`/`mainUpstreamAuthValue` read `.Type`/`.Value` only and need no change (verified during research — they're nil-safe field accessors, not switches over known types).

**Placeholder scan:** No "TBD"/"TODO"/"add appropriate X" phrases. Every step has real, current-codebase-derived code. Task 8 Step 1 is deliberately not-fully-scripted (says "mirror its nearest existing test... adapted to policyParams.clientSecret" rather than inline code) because the exact fixture/mock setup in `llm_secret_validation_test.go` wasn't read in full during planning (only referenced by name) — this is flagged explicitly as a read-first instruction rather than a placeholder, and is the one place an implementer must do their own file read before writing code, not invent syntax.

**Type consistency:** `isPolicyParamsUpstreamAuthType` (introduced Task 3) is used with the identical signature and name in Tasks 4, 5, 6. `model.UpstreamAuth`'s new field names (`PolicyName`/`PolicyParams`/`PolicyVersion`, all Task 2) match `api.UpstreamAuth`'s new field names (Task 1) exactly, matching the existing `Header`/`Value` naming symmetry between the two types. The one place a name is *not* yet 100% certain is `api.Oauth2` (Task 1's generated constant) — every later task that references it is explicitly flagged to double check against Task 1 Step 3's real grep output rather than assume, since it's the one identifier this plan cannot know for certain before the actual codegen tool runs.

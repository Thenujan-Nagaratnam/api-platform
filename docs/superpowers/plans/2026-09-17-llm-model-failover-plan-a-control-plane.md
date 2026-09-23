# LLM Model Failover — Plan A: Config Schema & Control-Plane Translation

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Given an `LlmProxy` with a `resilience.failover` block, gateway-controller produces correct Envoy xDS (aggregate clusters, retry policy, host rewrite, attempt-count header) and correct policy-engine-facing xDS route metadata — with zero output change for any `LlmProxy`/`RestApi`/`McpProxy`/`LlmProvider` that doesn't set `failover`.

**Architecture:** `resilience.failover` is a new schema, scoped to `LlmProxy` only via a new `LLMResilience` wrapper (not the shared `Resilience` schema every other kind uses). A new enrichment pass in `LLMTransformer.Transform` resolves the LLM-public `{model, provider}` shorthand into the existing generic `RouteUpstream`/`UpstreamCluster` model (the same internal shape `additionalProviders` already resolves into), so the xDS translator's new aggregate-cluster/retry logic operates on plain cluster keys and never needs to know what a "model" is.

**Tech Stack:** Go, `oapi-codegen` v2.5.1, `go-control-plane` v1.37.0 (`envoy` package tree).

**Spec:** `docs/superpowers/specs/2026-09-17-llm-model-failover-design.md`

## Global Constraints

- `resilience.failover` is settable on `LlmProxy` only — never leak it onto the shared `Resilience` schema (`RestApi`, `McpProxy`, `LlmProvider`, and `Operation`-level resilience must stay untouched, still typed `*api.Resilience`).
- `LlmProxy` resilience has no operation-level override today (confirmed: `LLMProxyConfigData.resilience`'s own OpenAPI description says "Supported at the API level only") — `failover` inherits that same API-level-only scope, no new operation-level field.
- Any `LlmProxy` (or other kind) with no `failover` block set must produce byte-identical xDS output to before this change. Every new code path in this plan is gated on `rdcRoute.Upstream.Failover != nil` (or the route-config-struct equivalent) — never unconditional.
- `suspendDuration` lives once per `failover` block (not per-target, not per-fallback) — v1 scope, per spec §2.
- Suspension *enforcement* (actually skipping a suspended target) is Plan C's job, not this plan's — this plan only carries `suspendDuration` through to the wire so Plan C has it to read.

---

## File Structure

| File | Responsibility |
|---|---|
| `gateway-controller/api/management-openapi.yaml` | Modify: add `LLMResilience`, `LLMFailoverConfig`, `LLMFailoverTarget` schemas; repoint `LLMProxyConfigData.resilience` at `LLMResilience`. |
| `gateway-controller/pkg/api/management/generated.go` | Regenerated (not hand-edited) — produces `LLMResilience`, `LLMFailoverConfig`, `LLMFailoverTarget` Go types and changes `LLMProxyConfigData.Resilience`'s type. |
| `gateway-controller/pkg/config/llm_validator.go` | Modify: adapt the two `*api.Resilience`-typed call sites to the new type; add failover validation (model/provider correctness, duplicate-model detection). |
| `gateway-controller/pkg/utils/llm_transformer.go` | Modify: adapt `applyResilienceToTrafficRoutes` call site to the new type. |
| `gateway-controller/pkg/models/runtime_deploy_config.go` | Modify: add `RouteFailover`/`RouteFailoverTarget`/`RouteFailoverEntry` types and a `Failover *RouteFailover` field on `RouteUpstream` — the generic, LLM-agnostic internal shape every downstream task consumes. |
| `gateway-controller/pkg/transform/llm.go` | Modify: new enrichment step in `LLMTransformer.Transform`, calling a new `applyFailoverToRoutes` function that resolves the LLM-public `{model, provider}` shorthand into the generic `RouteFailover` shape. |
| `gateway-controller/pkg/xds/failover_cluster.go` | Create: builds one `envoy.clusters.aggregate` cluster per `RouteFailoverTarget`, with the upstream ext_proc filter attached to the aggregate itself (not its members) — mirrors `upstream_policy_filter.go`'s pattern. |
| `gateway-controller/pkg/xds/translator.go` | Modify: call the new aggregate-cluster builder from the cluster-building loop; attach `RetryPolicy`/force `HostRewriteSpecifier` on a failover route's `RouteAction`; set `IncludeRequestAttemptCount` on any `VirtualHost` containing a failover route. |
| `gateway-controller/pkg/policyxds/snapshot.go` | Modify: emit a new `failover_targets` key in the policy-engine-facing route-config struct, alongside the existing `default_upstream` key. |

---

### Task 1: OpenAPI schema — `LLMResilience`/`LLMFailoverConfig`/`LLMFailoverTarget`

**Files:**
- Modify: `gateway-controller/api/management-openapi.yaml`
- Modify: `gateway-controller/pkg/config/llm_validator.go:860` (compile-fix only, not new validation — that's Task 2)
- Modify: `gateway-controller/pkg/config/llm_validator_test.go:2005` (`validProxyWithResilience` helper + its 4 call sites in `TestValidateLLMProxy_Resilience`)
- Modify: `gateway-controller/pkg/utils/llm_transformer.go:410` (compile-fix only)
- Test: existing suites, no new test file — this task's "test" is the build going green again

**Interfaces:**
- Produces: `api.LLMResilience{Timeout *string, IdleTimeout *string, Failover *api.LLMFailoverConfig}`, `api.LLMFailoverConfig{Targets []api.LLMFailoverTargetEntry, SuspendDuration *int}`, `api.LLMFailoverTargetEntry{Target api.LLMFailoverTarget, Fallbacks []api.LLMFailoverTarget}`, `api.LLMFailoverTarget{Model string, Provider *string}` — every later task in this plan and in Plan B/C/D reads these exact type/field names.
- Produces: `toBaseResilience(r *api.LLMResilience) *api.Resilience` — a small adapter, defined in `llm_validator.go`, used by both compile-fix call sites.

- [ ] **Step 1: Add the three new schemas to the OpenAPI spec**

Insert into `gateway-controller/api/management-openapi.yaml`, alphabetically near the existing `Resilience` schema (search for `    Resilience:` — there is exactly one definition, at line 3071 as of this plan):

```yaml
    LLMFailoverTarget:
      type: object
      required:
        - model
      description: >
        One attempt slot in a failover chain: which model to send, and
        optionally which provider to send it to. Omitting provider means the
        LlmProxy's primary provider.
      properties:
        model:
          type: string
          minLength: 1
          description: Model name to send for this attempt.
          example: gpt-4o
        provider:
          type: string
          description: >
            Must match an additionalProviders[].as (or .id when as is
            omitted), or be omitted to mean the LlmProxy's primary provider.
          minLength: 1
          example: anthropic-upstream

    LLMFailoverTargetEntry:
      type: object
      required:
        - target
        - fallbacks
      description: >
        One client-requested model's own failover chain: its primary attempt
        plus an ordered list of fallbacks tried in order if the primary (or
        an earlier fallback) fails within the same request.
      properties:
        target:
          $ref: '#/components/schemas/LLMFailoverTarget'
        fallbacks:
          type: array
          minItems: 1
          items:
            $ref: '#/components/schemas/LLMFailoverTarget'

    LLMFailoverConfig:
      type: object
      required:
        - targets
      description: >
        Declares genuine mid-request failover: if the request's target model
        fails, Envoy itself retries the SAME request against the next
        fallback, re-signed and re-transformed for that fallback's provider.
      properties:
        targets:
          type: array
          minItems: 1
          description: >
            One entry per client-requestable model this proxy protects with
            failover. A request whose model matches no entry here is
            unaffected by this block and uses the proxy's normal primary
            provider.
          items:
            $ref: '#/components/schemas/LLMFailoverTargetEntry'
        suspendDuration:
          type: integer
          minimum: 0
          default: 0
          description: >
            Seconds a target is skipped for new requests after it fails.
            0 disables suspension tracking entirely (a request can still
            fail over mid-request; it just won't be pre-emptively skipped
            on the next one).
          example: 900

    LLMResilience:
      type: object
      description: >
        LlmProxy-only resilience: the same timeout/idleTimeout every other
        kind's Resilience carries, plus failover. Not used by RestApi,
        McpProxy, or LlmProvider — those keep the shared Resilience schema.
      properties:
        timeout:
          type: string
          description: Maximum time for the entire route (request to upstream response). "0s" disables the timeout.
          pattern: '^\d+(\.\d+)?(ms|s|m|h)$'
          example: 15s
        idleTimeout:
          type: string
          description: Per-route stream idle timeout (overrides the listener stream idle timeout for this route). "0s" disables the timeout.
          pattern: '^\d+(\.\d+)?(ms|s|m|h)$'
          example: 0s
        failover:
          $ref: '#/components/schemas/LLMFailoverConfig'
```

Then change the ONE `resilience` field inside `LLMProxyConfigData` (search for `LLMProxyConfigData:` — the properties block containing `deploymentState`/`resilience` with the comment "Applies to all routes generated for this LLM Proxy") from:

```yaml
        resilience:
          $ref: '#/components/schemas/Resilience'
```

to:

```yaml
        resilience:
          $ref: '#/components/schemas/LLMResilience'
```

Do **not** touch the other four `resilience: $ref: '#/components/schemas/Resilience'` occurrences (`APIConfigData`, `Operation`, `MCPProxyConfigData`, `LLMProviderConfigData`) — those stay on the shared schema.

- [ ] **Step 2: Regenerate**

```bash
cd gateway-controller && make generate-server-code
```

- [ ] **Step 3: Confirm the new types exist and the old ones are untouched**

```bash
grep -n "^type LLMResilience struct\|^type LLMFailoverConfig struct\|^type LLMFailoverTarget struct\|^type LLMFailoverTargetEntry struct" gateway-controller/pkg/api/management/generated.go
```
Expected: all four found. Also confirm `Resilience *Resilience` still appears at 4 locations (not 5) via `grep -n "Resilience \*Resilience" gateway-controller/pkg/api/management/generated.go`.

- [ ] **Step 4: Build and observe the expected compile failures**

```bash
cd gateway-controller && go build ./... 2>&1
```
Expected: FAIL at `pkg/config/llm_validator.go:860` and `pkg/utils/llm_transformer.go:410` — both pass `spec.Resilience` (now `*api.LLMResilience`) to a function expecting `*api.Resilience`.

- [ ] **Step 5: Add the adapter and fix both call sites**

In `gateway-controller/pkg/config/llm_validator.go`, add near `validateProxyData` (the function containing the line-860 call):

```go
// toBaseResilience adapts LlmProxy's LLMResilience down to the shared
// Resilience shape so the one existing timeout/idleTimeout validator serves
// every kind. nil in, nil out.
func toBaseResilience(r *api.LLMResilience) *api.Resilience {
	if r == nil {
		return nil
	}
	return &api.Resilience{Timeout: r.Timeout, IdleTimeout: r.IdleTimeout}
}
```

Change line 860 from:
```go
	errors = append(errors, validateResilienceTimeouts("spec.resilience", spec.Resilience)...)
```
to:
```go
	errors = append(errors, validateResilienceTimeouts("spec.resilience", toBaseResilience(spec.Resilience))...)
```

In `gateway-controller/pkg/utils/llm_transformer.go`, change line 410 from:
```go
	applyResilienceToTrafficRoutes(ops, proxy.Spec.Resilience, nil)
```
to:
```go
	applyResilienceToTrafficRoutes(ops, config.ToBaseResilience(proxy.Spec.Resilience), nil)
```
This calls a package-qualified sibling of the same adapter — `llm_transformer.go` is in package `utils`, not `config`, so it cannot call the unexported `toBaseResilience` directly. Export it: rename `toBaseResilience` to `ToBaseResilience` in `llm_validator.go` (Step 5's definition above), and update the `llm_validator.go:860` call site to use the exported name too. Add the import `"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"` to `llm_transformer.go`'s import block if not already present (check first — `llm_validator.go`'s package is `config`, confirm via its `package config` declaration).

- [ ] **Step 6: Build again — expect success**

```bash
cd gateway-controller && go build ./...
```
Expected: PASS, no output.

- [ ] **Step 7: Fix the existing resilience test helper and its call sites**

In `gateway-controller/pkg/config/llm_validator_test.go`, change `validProxyWithResilience`'s signature (line 2005) from `func validProxyWithResilience(r *api.Resilience) api.LLMProxyConfiguration` to `func validProxyWithResilience(r *api.LLMResilience) api.LLMProxyConfiguration` — the body is unchanged (it just assigns `r` to `Spec.Resilience`, which is fine once the parameter's type matches).

In `TestValidateLLMProxy_Resilience` (same file, ~line 2163), change all four `&api.Resilience{...}` literals to `&api.LLMResilience{...}` — the fields (`Timeout`, `IdleTimeout`) are unchanged, only the type name changes:
```go
	t.Run("valid timeout", func(t *testing.T) {
		errs := validator.Validate(validProxyWithResilience(&api.LLMResilience{Timeout: stringPtr("75s")}))
		assert.Empty(t, errs)
	})

	t.Run("nil resilience is fine", func(t *testing.T) {
		errs := validator.Validate(validProxyWithResilience(nil))
		assert.Empty(t, errs)
	})

	t.Run("malformed timeout is rejected", func(t *testing.T) {
		errs := validator.Validate(validProxyWithResilience(&api.LLMResilience{Timeout: stringPtr("fast")}))
		assertHasFieldError(t, errs, "spec.resilience.timeout")
	})

	t.Run("negative idleTimeout is rejected", func(t *testing.T) {
		errs := validator.Validate(validProxyWithResilience(&api.LLMResilience{IdleTimeout: stringPtr("-1s")}))
		assertHasFieldError(t, errs, "spec.resilience.idleTimeout")
	})
```

- [ ] **Step 8: Run the full existing suite for both touched packages**

```bash
cd gateway-controller && go test ./pkg/config/... ./pkg/utils/... ./pkg/api/...
```
Expected: PASS, no failures, no behavior change (this task only changed types, not logic).

- [ ] **Step 9: Commit**

```bash
git add gateway-controller/api/management-openapi.yaml gateway-controller/pkg/api/management/generated.go gateway-controller/pkg/config/llm_validator.go gateway-controller/pkg/config/llm_validator_test.go gateway-controller/pkg/utils/llm_transformer.go
git commit -m "feat(llm-failover): add LLMResilience/LLMFailoverConfig schema, scoped to LlmProxy only"
```

---

### Task 2: Validation — target/fallback correctness

**Files:**
- Modify: `gateway-controller/pkg/config/llm_validator.go` (`validateProxyData`, ~line 850, right before the existing `validateResilienceTimeouts` call)
- Test: `gateway-controller/pkg/config/llm_validator_test.go` (new tests, alongside `TestValidateLLMProxy_Resilience`)

**Interfaces:**
- Consumes: `api.LLMFailoverConfig`/`api.LLMFailoverTargetEntry`/`api.LLMFailoverTarget` from Task 1. The `seen` map of valid upstream names already built earlier in `validateProxyData` for `additionalProviders` validation (contains `spec.Provider.Id` and every `additionalProviders[].as`-or-`.id`).
- Produces: `(v *LLMValidator) validateLLMFailover(fieldPrefix string, failover *api.LLMFailoverConfig, validUpstreamNames map[string]bool) []ValidationError` — Plan B/C don't call this directly, but its error-message field-path conventions (`spec.resilience.failover.targets[i].target.provider`) are what a deploy-time integration test in a later plan will assert against.

- [ ] **Step 1: Write the failing tests**

Add to `gateway-controller/pkg/config/llm_validator_test.go`:

```go
func validProxyWithFailover(failover *api.LLMFailoverConfig, additional *[]api.LLMProxyAdditionalProvider) api.LLMProxyConfiguration {
	return api.LLMProxyConfiguration{
		ApiVersion: api.LLMProxyConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProxyConfigurationKindLlmProxy,
		Metadata:   api.Metadata{Name: "openai-proxy"},
		Spec: api.LLMProxyConfigData{
			DisplayName:         "my-proxy",
			Version:              "v1.0",
			Provider:             api.LLMProxyProvider{Id: "openai-provider"},
			AdditionalProviders:  additional,
			Resilience:           &api.LLMResilience{Failover: failover},
		},
	}
}

func TestValidateLLMProxy_Failover(t *testing.T) {
	validator := NewLLMValidator()
	anthropic := &[]api.LLMProxyAdditionalProvider{{Id: "anthropic-provider", As: stringPtr("anthropic-upstream")}}

	t.Run("valid failover to a named additional provider", func(t *testing.T) {
		errs := validator.Validate(validProxyWithFailover(&api.LLMFailoverConfig{
			Targets: []api.LLMFailoverTargetEntry{{
				Target:    api.LLMFailoverTarget{Model: "gpt-4o"},
				Fallbacks: []api.LLMFailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: stringPtr("anthropic-upstream")}},
			}},
		}, anthropic))
		assert.Empty(t, errs)
	})

	t.Run("valid failover to the primary provider (no provider field)", func(t *testing.T) {
		errs := validator.Validate(validProxyWithFailover(&api.LLMFailoverConfig{
			Targets: []api.LLMFailoverTargetEntry{{
				Target:    api.LLMFailoverTarget{Model: "gpt-4o"},
				Fallbacks: []api.LLMFailoverTarget{{Model: "gpt-4o-mini"}},
			}},
		}, nil))
		assert.Empty(t, errs)
	})

	t.Run("empty target model is rejected", func(t *testing.T) {
		errs := validator.Validate(validProxyWithFailover(&api.LLMFailoverConfig{
			Targets: []api.LLMFailoverTargetEntry{{
				Target:    api.LLMFailoverTarget{Model: ""},
				Fallbacks: []api.LLMFailoverTarget{{Model: "gpt-4o-mini"}},
			}},
		}, nil))
		assertHasFieldError(t, errs, "spec.resilience.failover.targets[0].target.model")
	})

	t.Run("empty fallbacks is rejected", func(t *testing.T) {
		errs := validator.Validate(validProxyWithFailover(&api.LLMFailoverConfig{
			Targets: []api.LLMFailoverTargetEntry{{
				Target:    api.LLMFailoverTarget{Model: "gpt-4o"},
				Fallbacks: []api.LLMFailoverTarget{},
			}},
		}, nil))
		assertHasFieldError(t, errs, "spec.resilience.failover.targets[0].fallbacks")
	})

	t.Run("unknown provider reference is rejected", func(t *testing.T) {
		errs := validator.Validate(validProxyWithFailover(&api.LLMFailoverConfig{
			Targets: []api.LLMFailoverTargetEntry{{
				Target:    api.LLMFailoverTarget{Model: "gpt-4o"},
				Fallbacks: []api.LLMFailoverTarget{{Model: "claude-sonnet", Provider: stringPtr("nonexistent-upstream")}},
			}},
		}, anthropic))
		assertHasFieldError(t, errs, "spec.resilience.failover.targets[0].fallbacks[0].provider")
	})

	t.Run("duplicate target model within one failover block is rejected", func(t *testing.T) {
		errs := validator.Validate(validProxyWithFailover(&api.LLMFailoverConfig{
			Targets: []api.LLMFailoverTargetEntry{
				{Target: api.LLMFailoverTarget{Model: "gpt-4o"}, Fallbacks: []api.LLMFailoverTarget{{Model: "gpt-4o-mini"}}},
				{Target: api.LLMFailoverTarget{Model: "gpt-4o"}, Fallbacks: []api.LLMFailoverTarget{{Model: "gpt-3.5"}}},
			},
		}, nil))
		assertHasFieldError(t, errs, "spec.resilience.failover.targets[1].target.model")
	})

	t.Run("negative suspendDuration is rejected", func(t *testing.T) {
		errs := validator.Validate(validProxyWithFailover(&api.LLMFailoverConfig{
			Targets:         []api.LLMFailoverTargetEntry{{Target: api.LLMFailoverTarget{Model: "gpt-4o"}, Fallbacks: []api.LLMFailoverTarget{{Model: "gpt-4o-mini"}}}},
			SuspendDuration: intPtr(-1),
		}, nil))
		assertHasFieldError(t, errs, "spec.resilience.failover.suspendDuration")
	})
}
```

Check whether an `intPtr` helper already exists in this test file (`grep -n "func intPtr" gateway-controller/pkg/config/*_test.go`); if not, add `func intPtr(i int) *int { return &i }` beside the existing `stringPtr` helper.

- [ ] **Step 2: Run to verify it fails**

```bash
cd gateway-controller && go test ./pkg/config/... -run TestValidateLLMProxy_Failover -v
```
Expected: FAIL — `validateProxyData` doesn't read `.Failover` at all yet, so every sub-test asserting a rejection finds no errors (the two "valid" sub-tests will actually pass already, which is fine and expected; every sub-test expecting `assertHasFieldError` fails).

- [ ] **Step 3: Implement**

In `gateway-controller/pkg/config/llm_validator.go`, inside `validateProxyData`, immediately before the existing `errors = append(errors, validateResilienceTimeouts(...)...)` line, insert:

```go
	if spec.Resilience != nil && spec.Resilience.Failover != nil {
		validUpstreamNames := map[string]bool{spec.Provider.Id: true}
		if spec.AdditionalProviders != nil {
			for _, ap := range *spec.AdditionalProviders {
				name := ap.Id
				if ap.As != nil && *ap.As != "" {
					name = *ap.As
				}
				validUpstreamNames[name] = true
			}
		}
		errors = append(errors, v.validateLLMFailover("spec.resilience.failover", spec.Resilience.Failover, validUpstreamNames)...)
	}
```

Then add the new functions (near `validateLLMProxyTransformer`):

```go
func (v *LLMValidator) validateLLMFailover(fieldPrefix string, failover *api.LLMFailoverConfig, validUpstreamNames map[string]bool) []ValidationError {
	var errors []ValidationError

	if failover.SuspendDuration != nil && *failover.SuspendDuration < 0 {
		errors = append(errors, ValidationError{
			Field:   fieldPrefix + ".suspendDuration",
			Message: fieldPrefix + ".suspendDuration must be >= 0",
		})
	}

	seenModels := map[string]bool{}
	for i, entry := range failover.Targets {
		entryPrefix := fmt.Sprintf("%s.targets[%d]", fieldPrefix, i)

		// The "is required" check for entry.Target.Model itself lives in
		// validateLLMFailoverTarget below (called on the next line) — this
		// loop only adds the duplicate-detection check that's specific to
		// the target position, not shared with fallbacks.
		if entry.Target.Model != "" && seenModels[entry.Target.Model] {
			errors = append(errors, ValidationError{
				Field:   entryPrefix + ".target.model",
				Message: fmt.Sprintf("duplicate target model %q in resilience.failover.targets", entry.Target.Model),
			})
		}
		seenModels[entry.Target.Model] = true

		errors = append(errors, v.validateLLMFailoverTarget(entryPrefix+".target", entry.Target, validUpstreamNames)...)

		if len(entry.Fallbacks) == 0 {
			errors = append(errors, ValidationError{
				Field:   entryPrefix + ".fallbacks",
				Message: entryPrefix + ".fallbacks must have at least one entry",
			})
		}
		for j, fb := range entry.Fallbacks {
			errors = append(errors, v.validateLLMFailoverTarget(fmt.Sprintf("%s.fallbacks[%d]", entryPrefix, j), fb, validUpstreamNames)...)
		}
	}

	return errors
}

func (v *LLMValidator) validateLLMFailoverTarget(fieldPrefix string, target api.LLMFailoverTarget, validUpstreamNames map[string]bool) []ValidationError {
	var errors []ValidationError
	if strings.TrimSpace(target.Model) == "" {
		errors = append(errors, ValidationError{
			Field:   fieldPrefix + ".model",
			Message: fieldPrefix + ".model is required",
		})
	}
	if target.Provider != nil && strings.TrimSpace(*target.Provider) != "" {
		if !validUpstreamNames[*target.Provider] {
			errors = append(errors, ValidationError{
				Field:   fieldPrefix + ".provider",
				Message: fmt.Sprintf("%s.provider %q does not match the primary provider or any additionalProviders[].as/id", fieldPrefix, *target.Provider),
			})
		}
	}
	return errors
}
```

- [ ] **Step 4: Run to verify it passes**

```bash
cd gateway-controller && go test ./pkg/config/... -run TestValidateLLMProxy_Failover -v
```
Expected: PASS, all sub-tests green.

- [ ] **Step 5: Run the full config package suite (regression check)**

```bash
cd gateway-controller && go test ./pkg/config/...
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add gateway-controller/pkg/config/llm_validator.go gateway-controller/pkg/config/llm_validator_test.go
git commit -m "feat(llm-failover): validate resilience.failover targets/fallbacks/provider references"
```

---

### Task 3: Internal models + LLM mapper enrichment

**Files:**
- Modify: `gateway-controller/pkg/models/runtime_deploy_config.go` (`RouteUpstream` struct, ~line 141)
- Modify: `gateway-controller/pkg/transform/llm.go` (`Transform` method, ~line 94-108; new function `applyFailoverToRoutes`)
- Test: `gateway-controller/pkg/transform/llm_failover_test.go` (new file)

**Interfaces:**
- Consumes: `api.LLMFailoverConfig`/`api.LLMFailoverTargetEntry`/`api.LLMFailoverTarget` (Task 1); `models.RuntimeDeployConfig.UpstreamClusters map[string]*models.UpstreamCluster` and `models.Route.Upstream.{ClusterKey, Default, UseClusterHeader, DefaultCluster}` (existing, verified in `pkg/models/runtime_deploy_config.go` and `pkg/transform/restapi.go`).
- Produces: `models.RouteUpstream.Failover *models.RouteFailover`; `models.RouteFailover{SuspendDurationSeconds int, Targets []models.RouteFailoverTarget}`; `models.RouteFailoverTarget{Model string, Target models.RouteFailoverEntry, Fallbacks []models.RouteFailoverEntry}`; `models.RouteFailoverEntry{Model string, ClusterKey string, Upstream policyenginev1.UpstreamInfo}`. Task 4/5/6 read these exact field names.

- [ ] **Step 1: Add the new model types**

In `gateway-controller/pkg/models/runtime_deploy_config.go`, add after the `RouteUpstream` struct (after its closing `}` at line 151):

```go
// RouteFailover declares, for one route, the ordered failover chains a
// client-requested model can match against. Populated only when the source
// LlmProxy has a resilience.failover block; nil otherwise. See
// docs/superpowers/specs/2026-09-17-llm-model-failover-design.md.
type RouteFailover struct {
	SuspendDurationSeconds int
	Targets                []RouteFailoverTarget
}

// RouteFailoverTarget is one client-requested model's own failover chain.
type RouteFailoverTarget struct {
	Model     string
	Target    RouteFailoverEntry
	Fallbacks []RouteFailoverEntry
}

// RouteFailoverEntry is a single attempt slot: which model to send (may
// differ from RouteFailoverTarget.Model for a fallback using a cheaper
// model), which real cluster to dial, and that cluster's resolved upstream
// info.
type RouteFailoverEntry struct {
	Model      string
	ClusterKey string
	Upstream   policyenginev1.UpstreamInfo
}
```

Then add the field to `RouteUpstream` (insert before its closing `}`):
```go
	// Failover is this route's failover configuration (nil unless the source
	// LlmProxy declared resilience.failover). See RouteFailover.
	Failover *RouteFailover
```

Confirm `policyenginev1` is already imported in this file (it's used by `RouteUpstream.Default *policyenginev1.UpstreamInfo` two lines above — it is).

- [ ] **Step 2: Write the failing test for the enrichment function**

Create `gateway-controller/pkg/transform/llm_failover_test.go`:

```go
package transform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	policyenginev1 "github.com/wso2/api-platform/sdk/core/policyengine"
)

func TestApplyFailoverToRoutes_ResolvesPrimaryAndNamedProvider(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"upstream_main_openai_com_443": {
				Name:     "", // main slot cluster
				BasePath: "/",
				Endpoints: []models.Endpoint{{Host: "openai.com", Port: 443}},
				TLS:      &models.UpstreamTLS{Enabled: true},
			},
			"upstream_anthropic-upstream_anthropic_com_443": {
				Name:     "anthropic-upstream",
				BasePath: "/",
				Endpoints: []models.Endpoint{{Host: "anthropic.com", Port: 443}},
				TLS:      &models.UpstreamTLS{Enabled: true},
			},
		},
		Routes: map[string]*models.Route{
			"POST|/chat/completions|main": {
				Upstream: models.RouteUpstream{
					ClusterKey: "upstream_main_openai_com_443",
					Default: &policyenginev1.UpstreamInfo{
						ClusterName: "upstream_main_openai_com_443",
						URL:         "https://openai.com",
						BasePath:    "/",
					},
				},
			},
		},
	}

	failover := &management.LLMFailoverConfig{
		Targets: []management.LLMFailoverTargetEntry{{
			Target: management.LLMFailoverTarget{Model: "gpt-4o"},
			Fallbacks: []management.LLMFailoverTarget{{
				Model:    "claude-sonnet-4-5-20250929",
				Provider: strPtr("anthropic-upstream"),
			}},
		}},
	}

	err := applyFailoverToRoutes(rdc, failover)
	require.NoError(t, err)

	route := rdc.Routes["POST|/chat/completions|main"]
	require.NotNil(t, route.Upstream.Failover)
	require.Len(t, route.Upstream.Failover.Targets, 1)

	target := route.Upstream.Failover.Targets[0]
	assert.Equal(t, "gpt-4o", target.Model)
	assert.Equal(t, "upstream_main_openai_com_443", target.Target.ClusterKey)
	assert.Equal(t, "https://openai.com", target.Target.Upstream.URL)

	require.Len(t, target.Fallbacks, 1)
	assert.Equal(t, "claude-sonnet-4-5-20250929", target.Fallbacks[0].Model)
	assert.Equal(t, "upstream_anthropic-upstream_anthropic_com_443", target.Fallbacks[0].ClusterKey)
	assert.Equal(t, "https://anthropic.com", target.Fallbacks[0].Upstream.URL)

	assert.True(t, route.Upstream.UseClusterHeader, "a failover route must use cluster_header dynamic routing")
	assert.Equal(t, "upstream_main_openai_com_443", route.Upstream.DefaultCluster, "no-match requests must still fall back to the plain primary cluster")
}

func TestApplyFailoverToRoutes_UnknownProviderIsAnError(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{},
		Routes: map[string]*models.Route{
			"POST|/chat/completions|main": {
				Upstream: models.RouteUpstream{
					ClusterKey: "upstream_main_openai_com_443",
					Default:    &policyenginev1.UpstreamInfo{ClusterName: "upstream_main_openai_com_443", URL: "https://openai.com"},
				},
			},
		},
	}
	failover := &management.LLMFailoverConfig{
		Targets: []management.LLMFailoverTargetEntry{{
			Target:    management.LLMFailoverTarget{Model: "gpt-4o"},
			Fallbacks: []management.LLMFailoverTarget{{Model: "x", Provider: strPtr("nonexistent")}},
		}},
	}

	err := applyFailoverToRoutes(rdc, failover)
	assert.Error(t, err)
}

func TestApplyFailoverToRoutes_NilFailoverIsNoOp(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		Routes: map[string]*models.Route{
			"r": {Upstream: models.RouteUpstream{ClusterKey: "c"}},
		},
	}
	require.NoError(t, applyFailoverToRoutes(rdc, nil))
	assert.Nil(t, rdc.Routes["r"].Upstream.Failover)
	assert.False(t, rdc.Routes["r"].Upstream.UseClusterHeader)
}

func strPtr(s string) *string { return &s }
```

Check the actual import path/package name for the generated API types before using `management.LLMFailoverConfig` above — confirm via `head -5 gateway-controller/pkg/api/management/generated.go` (Task 1 already established this package is `management`, consistent with `oapi-codegen.yaml`'s `package: management`). If existing test files in `pkg/transform` import it under a different alias (check `head -20 gateway-controller/pkg/transform/restapi_test.go` for the existing convention — it uses `api` as the alias for this same package based on earlier reads in this session), use that alias (`api.LLMFailoverConfig` etc.) instead of `management.` for consistency with the rest of the package's tests, and drop the explicit import line accordingly (reuse whatever `restapi_test.go` already imports as `api`).

- [ ] **Step 2b: Run to verify it fails**

```bash
cd gateway-controller && go test ./pkg/transform/... -run TestApplyFailoverToRoutes -v
```
Expected: FAIL with `undefined: applyFailoverToRoutes`.

- [ ] **Step 3: Implement**

Add to `gateway-controller/pkg/transform/llm.go`:

```go
// applyFailoverToRoutes resolves failover (the LLM-public {model, provider}
// shorthand) into models.RouteFailover on every route in rdc, using
// rdc.UpstreamClusters (already built by RestAPITransformer) to translate a
// named provider into a real cluster key + upstream info. A target/fallback
// with no provider uses the route's OWN already-resolved primary upstream
// (route.Upstream.ClusterKey / .Default) directly — never a name lookup —
// because the primary/sandbox slot clusters are stored with an empty Name
// (see models.UpstreamCluster.Name's doc comment), which is not a usable
// lookup key on its own.
func applyFailoverToRoutes(rdc *models.RuntimeDeployConfig, failover *api.LLMFailoverConfig) error {
	if failover == nil || len(failover.Targets) == 0 {
		return nil
	}

	suspendSeconds := 0
	if failover.SuspendDuration != nil {
		suspendSeconds = *failover.SuspendDuration
	}

	for routeKey, r := range rdc.Routes {
		targets := make([]models.RouteFailoverTarget, 0, len(failover.Targets))
		for _, entry := range failover.Targets {
			targetEntry, err := resolveFailoverEntry(rdc, r, entry.Target)
			if err != nil {
				return fmt.Errorf("route %q: resolving failover target %q: %w", routeKey, entry.Target.Model, err)
			}
			fallbacks := make([]models.RouteFailoverEntry, 0, len(entry.Fallbacks))
			for _, fb := range entry.Fallbacks {
				fbEntry, err := resolveFailoverEntry(rdc, r, fb)
				if err != nil {
					return fmt.Errorf("route %q: resolving failover fallback %q: %w", routeKey, fb.Model, err)
				}
				fallbacks = append(fallbacks, fbEntry)
			}
			targets = append(targets, models.RouteFailoverTarget{
				Model:     entry.Target.Model,
				Target:    targetEntry,
				Fallbacks: fallbacks,
			})
		}

		r.Upstream.Failover = &models.RouteFailover{
			SuspendDurationSeconds: suspendSeconds,
			Targets:                targets,
		}
		if !r.Upstream.UseClusterHeader {
			r.Upstream.UseClusterHeader = true
			r.Upstream.DefaultCluster = r.Upstream.ClusterKey
		}
	}
	return nil
}

// resolveFailoverEntry resolves one {model, provider} shorthand into a real
// cluster reference. provider == nil means the route's own primary upstream.
func resolveFailoverEntry(rdc *models.RuntimeDeployConfig, r *models.Route, t api.LLMFailoverTarget) (models.RouteFailoverEntry, error) {
	if t.Provider == nil || strings.TrimSpace(*t.Provider) == "" {
		if r.Upstream.Default == nil {
			return models.RouteFailoverEntry{}, fmt.Errorf("route has no default upstream to use as the primary failover target")
		}
		return models.RouteFailoverEntry{
			Model:      t.Model,
			ClusterKey: r.Upstream.ClusterKey,
			Upstream:   *r.Upstream.Default,
		}, nil
	}

	providerName := strings.TrimSpace(*t.Provider)
	for key, uc := range rdc.UpstreamClusters {
		if uc.Name != providerName {
			continue
		}
		if len(uc.Endpoints) == 0 {
			return models.RouteFailoverEntry{}, fmt.Errorf("provider %q has no endpoints", providerName)
		}
		scheme := "http"
		if uc.TLS != nil && uc.TLS.Enabled {
			scheme = "https"
		}
		return models.RouteFailoverEntry{
			Model:      t.Model,
			ClusterKey: key,
			Upstream: policyenginev1.UpstreamInfo{
				ClusterName: key,
				URL:         fmt.Sprintf("%s://%s:%d", scheme, uc.Endpoints[0].Host, uc.Endpoints[0].Port),
				BasePath:    uc.BasePath,
			},
		}, nil
	}
	return models.RouteFailoverEntry{}, fmt.Errorf("provider %q not found among configured upstreams", providerName)
}
```

Add `"strings"` and `policyenginev1 "github.com/wso2/api-platform/sdk/core/policyengine"` to `llm.go`'s imports if not already present (check first — `fmt` is almost certainly already imported given the existing error-wrapping in this file; `strings` and `policyenginev1` likely are not).

Then wire it into `Transform`, right after Step 4 (the existing `rdc.Metadata.LLM = llmMeta` block, ~line 105) and before `return rdc, nil`:

```go
	// Step 5: Resolve resilience.failover (LlmProxy-only) into the generic
	// RouteFailover shape every route carries. No-op for any other kind or
	// any LlmProxy with no failover block.
	if proxy, ok := cfg.SourceConfiguration.(api.LLMProxyConfiguration); ok && proxy.Spec.Resilience != nil {
		if err := applyFailoverToRoutes(rdc, proxy.Spec.Resilience.Failover); err != nil {
			return nil, fmt.Errorf("resolving resilience.failover: %w", err)
		}
	}
```

- [ ] **Step 4: Run to verify it passes**

```bash
cd gateway-controller && go test ./pkg/transform/... -run TestApplyFailoverToRoutes -v
```
Expected: PASS, all three tests green.

- [ ] **Step 5: Run the full transform package suite (regression check)**

```bash
cd gateway-controller && go test ./pkg/transform/...
```
Expected: PASS — confirms existing `LLMTransformer.Transform`/`RestAPITransformer.Transform` behavior for non-failover configs is unchanged (Step 5's new block is a no-op whenever `Failover` is nil, which is every existing test fixture).

- [ ] **Step 6: Commit**

```bash
git add gateway-controller/pkg/models/runtime_deploy_config.go gateway-controller/pkg/transform/llm.go gateway-controller/pkg/transform/llm_failover_test.go
git commit -m "feat(llm-failover): resolve resilience.failover into the generic RouteFailover model"
```

---

### Task 4: xDS translator — aggregate cluster construction

**Files:**
- Create: `gateway-controller/pkg/xds/failover_cluster.go`
- Create: `gateway-controller/pkg/xds/failover_cluster_test.go`
- Modify: `gateway-controller/pkg/xds/translator.go` (cluster-building loop, ~line 252-286 as read in this session — the `for clusterName, uc := range rdc.UpstreamClusters` loop)

**Interfaces:**
- Consumes: `models.RouteFailover`/`RouteFailoverTarget`/`RouteFailoverEntry` (Task 3); `constants.UpstreamExtProcFilterName`/`UpstreamCodecFilterName`/`HttpProtocolOptionsTypedConfigKey`/`ExtProcRequestAttributeRouteName`/`ExtProcRequestAttributeClusterName`/`UpstreamPolicyEngineClusterName` (all already defined in `gateway-controller/pkg/constants/constants.go` from this session's earlier work); `attachUpstreamPolicyFilter` (already defined in `pkg/xds/upstream_policy_filter.go` from this session's earlier work — reused as-is, not reimplemented).
- Produces: `aggregateClusterName(routeKey string, targetIndex int) string` and `buildFailoverAggregateClusters(rf *models.RouteFailover, routeKey string) ([]*cluster.Cluster, error)` — Task 5 calls the latter; Task 6 calls the former (to know what name to put on the xDS-metadata wire).

- [ ] **Step 1: Write the failing test**

Create `gateway-controller/pkg/xds/failover_cluster_test.go`:

```go
package xds

import (
	"testing"

	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	policyenginev1 "github.com/wso2/api-platform/sdk/core/policyengine"
)

func TestBuildFailoverAggregateClusters_OneAggregatePerTarget(t *testing.T) {
	rf := &models.RouteFailover{
		Targets: []models.RouteFailoverTarget{
			{
				Model:  "gpt-4o",
				Target: models.RouteFailoverEntry{ClusterKey: "upstream_main_openai_443", Upstream: policyenginev1.UpstreamInfo{ClusterName: "upstream_main_openai_443"}},
				Fallbacks: []models.RouteFailoverEntry{
					{ClusterKey: "upstream_anthropic_443", Upstream: policyenginev1.UpstreamInfo{ClusterName: "upstream_anthropic_443"}},
				},
			},
			{
				Model:  "gpt-4o-mini",
				Target: models.RouteFailoverEntry{ClusterKey: "upstream_main_openai_443", Upstream: policyenginev1.UpstreamInfo{ClusterName: "upstream_main_openai_443"}},
				Fallbacks: []models.RouteFailoverEntry{
					{ClusterKey: "upstream_anthropic_haiku_443", Upstream: policyenginev1.UpstreamInfo{ClusterName: "upstream_anthropic_haiku_443"}},
				},
			},
		},
	}

	clusters, err := buildFailoverAggregateClusters(rf, "POST|/chat/completions|main")
	require.NoError(t, err)
	require.Len(t, clusters, 2, "one aggregate cluster per targets[] entry")

	names := map[string]bool{}
	for i, c := range clusters {
		names[c.Name] = true
		assert.Equal(t, aggregateClusterName("POST|/chat/completions|main", i), c.Name)

		anyOpts, ok := c.TypedExtensionProtocolOptions[constants.HttpProtocolOptionsTypedConfigKey]
		require.True(t, ok, "upstream ext_proc filter must be attached to the aggregate cluster itself")
		var opts httpv3.HttpProtocolOptions
		require.NoError(t, anyOpts.UnmarshalTo(&opts))
		assert.NoError(t, opts.ValidateAll(), "must pass Envoy's own proto validation, same class of bug this session already found once")
		require.Len(t, opts.HttpFilters, 2)
		assert.Equal(t, constants.UpstreamExtProcFilterName, opts.HttpFilters[0].Name)
		assert.Equal(t, constants.UpstreamCodecFilterName, opts.HttpFilters[1].Name)
	}
	assert.Len(t, names, 2, "aggregate cluster names must be unique per target entry")
}

func TestBuildFailoverAggregateClusters_MemberOrderMatchesPriority(t *testing.T) {
	rf := &models.RouteFailover{
		Targets: []models.RouteFailoverTarget{{
			Model:  "gpt-4o",
			Target: models.RouteFailoverEntry{ClusterKey: "primary-cluster"},
			Fallbacks: []models.RouteFailoverEntry{
				{ClusterKey: "fallback-1"},
				{ClusterKey: "fallback-2"},
			},
		}},
	}

	clusters, err := buildFailoverAggregateClusters(rf, "route-key")
	require.NoError(t, err)
	require.Len(t, clusters, 1)

	var aggCfg aggregateConfigForTest
	require.NoError(t, unmarshalAggregateConfig(t, clusters[0], &aggCfg))
	assert.Equal(t, []string{"primary-cluster", "fallback-1", "fallback-2"}, aggCfg.Clusters)
}
```

`aggregateConfigForTest`/`unmarshalAggregateConfig` are small test-only helpers — add them to the same test file:

```go
type aggregateConfigForTest struct {
	Clusters []string
}

func unmarshalAggregateConfig(t *testing.T, c *cluster.Cluster, out *aggregateConfigForTest) error {
	t.Helper()
	ct, ok := c.ClusterDiscoveryType.(*cluster.Cluster_ClusterType)
	require.True(t, ok, "expected a custom cluster type (aggregate)")
	var agg aggregatev3.ClusterConfig
	if err := ct.ClusterType.TypedConfig.UnmarshalTo(&agg); err != nil {
		return err
	}
	out.Clusters = agg.Clusters
	return nil
}
```

Add the two missing imports (`cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"` and `aggregatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/aggregate/v3"`) to this test file's import block.

- [ ] **Step 2: Run to verify it fails**

```bash
cd gateway-controller && go test ./pkg/xds/... -run TestBuildFailoverAggregateClusters -v
```
Expected: FAIL with `undefined: buildFailoverAggregateClusters` / `undefined: aggregateClusterName`.

- [ ] **Step 3: Implement**

Create `gateway-controller/pkg/xds/failover_cluster.go`:

```go
/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 */

package xds

import (
	"fmt"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	aggregatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/aggregate/v3"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// aggregateClusterName deterministically names the aggregate cluster for the
// targetIndex'th entry of routeKey's failover block. Stable across redeploys
// (routeKey + index never change for the same declared target unless the
// operator reorders resilience.failover.targets, which is an intentional
// config change, not a spurious xDS re-version).
func aggregateClusterName(routeKey string, targetIndex int) string {
	return fmt.Sprintf("failover_agg_%s_%d", sanitizeClusterNameComponent(routeKey), targetIndex)
}

// buildFailoverAggregateClusters builds one envoy.clusters.aggregate cluster
// per rf.Targets entry, members in priority order (target, then fallbacks in
// order — aggregate cluster priority is assigned by list position). The
// upstream ext_proc filter is attached to the AGGREGATE cluster itself, not
// its members — confirmed live this session that attaching to the real
// members never fires when reached through an aggregate.
func buildFailoverAggregateClusters(rf *models.RouteFailover, routeKey string) ([]*cluster.Cluster, error) {
	clusters := make([]*cluster.Cluster, 0, len(rf.Targets))
	for i, target := range rf.Targets {
		memberNames := make([]string, 0, len(target.Fallbacks)+1)
		memberNames = append(memberNames, target.Target.ClusterKey)
		for _, fb := range target.Fallbacks {
			memberNames = append(memberNames, fb.ClusterKey)
		}

		aggConfigAny, err := anypb.New(&aggregatev3.ClusterConfig{Clusters: memberNames})
		if err != nil {
			return nil, fmt.Errorf("failed to marshal aggregate cluster config for %q target %d: %w", routeKey, i, err)
		}

		aggCluster := &cluster.Cluster{
			Name:     aggregateClusterName(routeKey, i),
			LbPolicy: cluster.Cluster_CLUSTER_PROVIDED,
			ClusterDiscoveryType: &cluster.Cluster_ClusterType{
				ClusterType: &cluster.Cluster_CustomClusterType{
					Name:        "envoy.clusters.aggregate",
					TypedConfig: aggConfigAny,
				},
			},
		}
		if err := attachUpstreamPolicyFilter(aggCluster, constants.UpstreamPolicyEngineClusterName); err != nil {
			return nil, fmt.Errorf("failed to attach upstream policy filter to aggregate cluster %q: %w", aggCluster.Name, err)
		}
		clusters = append(clusters, aggCluster)
	}
	return clusters, nil
}
```

Check whether `sanitizeClusterNameComponent` (or an equivalently-purposed existing helper that strips characters unsafe in an Envoy cluster name — pipes, slashes, asterisks all appear in a routeKey like `POST|/chat/completions|main`) already exists in `pkg/xds/translator.go` via `grep -n "func sanitize" gateway-controller/pkg/xds/translator.go`. If `sanitizeClusterName` exists (this session's earlier reading found `sanitizeEnvoyClusterName` in `restapi.go`, a different, narrower helper for host/scheme — check translator.go specifically for a route-key-oriented one), reuse it. If none fits, add this small helper in `failover_cluster.go`:

```go
// sanitizeClusterNameComponent strips characters Envoy cluster names can't
// safely contain (a route key looks like "POST|/chat/completions|main").
func sanitizeClusterNameComponent(s string) string {
	replacer := strings.NewReplacer("|", "_", "/", "_", "*", "_", " ", "_")
	return replacer.Replace(s)
}
```
(add `"strings"` to the import block if using this fallback).

- [ ] **Step 4: Run to verify it passes**

```bash
cd gateway-controller && go test ./pkg/xds/... -run TestBuildFailoverAggregateClusters -v
```
Expected: PASS, both tests green — including the `opts.ValidateAll()` assertion, which only passes because `attachUpstreamPolicyFilter` already carries this session's earlier `UpstreamProtocolOptions` fix.

- [ ] **Step 5: Wire into the translator's cluster-building loop**

In `gateway-controller/pkg/xds/translator.go`, inside `translateRuntimeConfig`, after the existing `for clusterName, uc := range rdc.UpstreamClusters { ... }` loop finishes (it appends every real cluster to `clusters`), add:

```go
	// Build one aggregate cluster per resilience.failover targets[] entry,
	// deduped by route key + entry index (multiple operations of the same
	// LlmProxy share the identical failover block, so this avoids emitting
	// duplicate aggregate clusters with colliding names).
	seenAggregates := map[string]bool{}
	for routeKey, rdcRoute := range rdc.Routes {
		if rdcRoute.Upstream.Failover == nil {
			continue
		}
		aggClusters, err := buildFailoverAggregateClusters(rdcRoute.Upstream.Failover, routeKey)
		if err != nil {
			return nil, nil, fmt.Errorf("route %q: %w", routeKey, err)
		}
		for _, c := range aggClusters {
			if seenAggregates[c.Name] {
				continue
			}
			seenAggregates[c.Name] = true
			clusters = append(clusters, c)
		}
	}
```

Place this block right before the existing `// Build routes from Routes map` comment (the loop this session already read earlier at ~line 288-293), so it runs after all real clusters exist but before route construction needs to reference an aggregate cluster's name.

- [ ] **Step 6: Run the full xds package suite (regression check)**

```bash
cd gateway-controller && go test ./pkg/xds/...
```
Expected: PASS — every existing fixture has no `Upstream.Failover`, so `buildFailoverAggregateClusters` is never called for them (loop body's `continue` on line 1 skips every one), meaning `clusters`'s existing contents are unchanged.

- [ ] **Step 7: Commit**

```bash
git add gateway-controller/pkg/xds/failover_cluster.go gateway-controller/pkg/xds/failover_cluster_test.go gateway-controller/pkg/xds/translator.go
git commit -m "feat(llm-failover): build one envoy.clusters.aggregate per failover target, filter attached to the aggregate"
```

---

### Task 5: xDS translator — retry policy, host rewrite, attempt-count header

**Files:**
- Modify: `gateway-controller/pkg/xds/translator.go` (route-action construction, ~line 347-368 as read this session; virtual-host construction, ~line 869-882 as read this session)
- Test: `gateway-controller/pkg/xds/upstream_policy_filter_wiring_test.go` (extend — this file already exists from this session's earlier work and already constructs a full `rdc`/calls `translateRuntimeConfig`, matching exactly what this task's test needs)

**Interfaces:**
- Consumes: `aggregateClusterName` (Task 4); `models.RouteFailover` (Task 3); `route.RetryPolicy`/`route.RetryPolicy_RetryPriority`/`route.RetryPolicy_RetryPriority_TypedConfig`/`route.RouteAction_AutoHostRewrite` (go-control-plane, verified field names in this plan's research); `previous_prioritiesv3.PreviousPrioritiesConfig` (go-control-plane extension package `envoy/extensions/retry/priority/previous_priorities/v3`).
- Produces: nothing new for later tasks — this task's output is pure Envoy `RouteAction`/`VirtualHost` shape, consumed only by Envoy itself.

- [ ] **Step 1: Write the failing test**

Add to `gateway-controller/pkg/xds/upstream_policy_filter_wiring_test.go` (check its existing test function names first via `grep -n "^func Test" gateway-controller/pkg/xds/upstream_policy_filter_wiring_test.go` and follow its exact `rdc`-construction pattern — it already builds a full `*models.RuntimeDeployConfig` with `UpstreamClusters`/`Routes` by hand and calls `(&Translator{...}).translateRuntimeConfig(rdc)`, reuse that scaffolding rather than duplicating it):

```go
func TestTranslateRuntimeConfig_FailoverRouteGetsRetryPolicyAndHostRewrite(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"primary-cluster": {BasePath: "/", Endpoints: []models.Endpoint{{Host: "openai.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
			"fallback-cluster": {BasePath: "/", Endpoints: []models.Endpoint{{Host: "anthropic.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
		},
		Routes: map[string]*models.Route{
			"POST|/chat/completions|main": {
				Method: "POST",
				Path:   "/chat/completions",
				Vhost:  "main",
				Upstream: models.RouteUpstream{
					ClusterKey:       "primary-cluster",
					UseClusterHeader: true,
					DefaultCluster:   "primary-cluster",
					Failover: &models.RouteFailover{
						Targets: []models.RouteFailoverTarget{{
							Model:     "gpt-4o",
							Target:    models.RouteFailoverEntry{ClusterKey: "primary-cluster"},
							Fallbacks: []models.RouteFailoverEntry{{ClusterKey: "fallback-cluster"}},
						}},
					},
				},
			},
		},
	}

	translator := newTestTranslator(t) // reuse this file's existing translator-construction helper
	_, routes, err := translator.translateRuntimeConfig(rdc)
	require.NoError(t, err)
	require.Len(t, routes, 1)

	action := routes[0].GetRoute()
	require.NotNil(t, action)
	require.NotNil(t, action.RetryPolicy)
	assert.Equal(t, "5xx", action.RetryPolicy.RetryOn)
	require.NotNil(t, action.RetryPolicy.RetryPriority)
	assert.Equal(t, "envoy.retry_priorities.previous_priorities", action.RetryPolicy.RetryPriority.Name)

	_, isAutoRewrite := action.HostRewriteSpecifier.(*route.RouteAction_AutoHostRewrite)
	assert.True(t, isAutoRewrite, "a failover route must auto-rewrite Host, or per-attempt backend resolution can't tell attempts apart")
}
```

Check `newTestTranslator` (or whatever this file's actual translator-construction helper is named — inspect the file for its existing tests' setup) and use its real name; if no such shared helper exists yet, inspect how `TestTranslateRuntimeConfig_AttachesUpstreamFilterOnlyWhereNeeded` (this session's earlier test in the same file) constructs its `*Translator` and copy that exact construction inline instead of inventing a helper that doesn't exist.

- [ ] **Step 2: Run to verify it fails**

```bash
cd gateway-controller && go test ./pkg/xds/... -run TestTranslateRuntimeConfig_FailoverRouteGetsRetryPolicyAndHostRewrite -v
```
Expected: FAIL — `action.RetryPolicy` is nil (translator doesn't set it yet), and `HostRewriteSpecifier` is nil too (`AutoHostRewrite` is a separate bool this test's fixture doesn't set, and even if it did, that path already exists — the failing assertion that actually matters here is `RetryPolicy`).

- [ ] **Step 3: Implement**

In `gateway-controller/pkg/xds/translator.go`, right after the existing host-rewrite block (~line 366, immediately after `if rdcRoute.AutoHostRewrite { ... }`), add:

```go
	// Failover routes always auto-rewrite Host (needed for per-attempt
	// backend resolution to tell attempts apart, see the design spec) and
	// carry a retry policy that escalates through the aggregate cluster's
	// priority levels on a 5xx.
	if rdcRoute.Upstream.Failover != nil {
		routeAction.Route.HostRewriteSpecifier = &route.RouteAction_AutoHostRewrite{
			AutoHostRewrite: &wrapperspb.BoolValue{Value: true},
		}
		retryPriorityAny, err := anypb.New(&previous_prioritiesv3.PreviousPrioritiesConfig{UpdateFrequency: 1})
		if err != nil {
			return nil, nil, fmt.Errorf("route %q: failed to marshal retry_priority config: %w", routeKey, err)
		}
		routeAction.Route.RetryPolicy = &route.RetryPolicy{
			RetryOn: "5xx",
			RetryPriority: &route.RetryPolicy_RetryPriority{
				Name:       "envoy.retry_priorities.previous_priorities",
				ConfigType: &route.RetryPolicy_RetryPriority_TypedConfig{TypedConfig: retryPriorityAny},
			},
		}
	}
```

Add the import `previous_prioritiesv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/retry/priority/previous_priorities/v3"` to `translator.go`'s import block (check first whether `wrapperspb`/`anypb`/`route` are already imported under those exact aliases — this session's earlier reading of this same function already showed `wrapperspb.BoolValue` in use two lines above, so `wrapperspb` is already imported; `route` and `anypb` are almost certainly already imported given the surrounding code in this same function uses both).

Then, in the virtual-host construction block (~line 869), change:
```go
		virtualHost := &route.VirtualHost{
			Name:    vhost,
			Domains: t.getVHostDomains(vhost),
			Routes:  routes,
```
to:
```go
		vhostNeedsAttemptCount := false
		for _, r := range routes {
			if r.GetRoute().GetRetryPolicy().GetRetryPriority() != nil {
				vhostNeedsAttemptCount = true
				break
			}
		}
		virtualHost := &route.VirtualHost{
			Name:                       vhost,
			Domains:                    t.getVHostDomains(vhost),
			Routes:                     routes,
			IncludeRequestAttemptCount: vhostNeedsAttemptCount,
```
(keeping the existing `RequestHeadersToRemove: []string{envoyOriginalPathHeader},` field on the line after — this is an ADD, not a replace, of one new field plus the small detection loop above it).

- [ ] **Step 4: Run to verify it passes**

```bash
cd gateway-controller && go test ./pkg/xds/... -run TestTranslateRuntimeConfig_FailoverRouteGetsRetryPolicyAndHostRewrite -v
```
Expected: PASS.

- [ ] **Step 5: Run the full xds package suite (regression check)**

```bash
cd gateway-controller && go test ./pkg/xds/...
```
Expected: PASS — `vhostNeedsAttemptCount` stays `false` for every existing fixture (none has a route with a non-nil `RetryPriority`), so `IncludeRequestAttemptCount` stays the Go zero-value `false`, identical to the field being absent before this change.

- [ ] **Step 6: Commit**

```bash
git add gateway-controller/pkg/xds/translator.go gateway-controller/pkg/xds/upstream_policy_filter_wiring_test.go
git commit -m "feat(llm-failover): attach retry_policy/auto_host_rewrite to failover routes, attempt-count header on their vhost"
```

---

### Task 6: Policy-engine-facing xDS metadata sync

**Files:**
- Modify: `gateway-controller/pkg/policyxds/snapshot.go` (route-config-struct builder, ~line 432-441 as read this session — where `data["default_upstream"]` is set)
- Test: `gateway-controller/pkg/policyxds/snapshot_test.go` (extend — check this file exists first via `ls gateway-controller/pkg/policyxds/*_test.go`; if the route-struct-building function has no dedicated test file, check `snapshot.go`'s own tests are in a file with a different name and use that instead)

**Interfaces:**
- Consumes: `models.RouteFailover`/`RouteFailoverTarget`/`RouteFailoverEntry` (Task 3); `aggregateClusterName` (Task 4, so the emitted wire data's cluster names match exactly what Envoy will report via `xds.cluster_name`).
- Produces: a new `failover_targets` key in the route-config wire struct, shaped as a list of `{aggregate_cluster: string, model: string, chain: [{model: string, cluster_name: string, url: string, base_path: string}]}` objects — Plan B's policy-engine-side xDS handler (`gateway-runtime/policy-engine/internal/xdsclient/handler.go`) parses this exact shape; that handler doesn't exist yet, so this shape is the CONTRACT Plan B is written against, not something to leave ambiguous. `chain[0]` is always the target itself, `chain[1:]` are the fallbacks in order — this is the same list `x-envoy-attempt-count - 1` indexes into per the design spec §6.

- [ ] **Step 1: Write the failing test**

First inspect the existing test for the function containing the `default_upstream` line, to copy its exact fixture-construction style:
```bash
grep -n "default_upstream" gateway-controller/pkg/policyxds/*_test.go
```
Read whichever test that surfaces, and add a new test in the SAME file, following its exact pattern for constructing a `*models.RuntimeDeployConfig`/`*models.Route` and calling whatever function builds the route-config struct (this session's earlier reading found it inline inside a larger function in `snapshot.go` around line 380-443 — confirm the enclosing function's name via `sed -n '350,445p' gateway-controller/pkg/policyxds/snapshot.go` and use ITS real name, not a guessed one, when writing the test call).

```go
func TestBuildRouteConfigStruct_EmitsFailoverTargets(t *testing.T) {
	route := &models.Route{
		Upstream: models.RouteUpstream{
			ClusterKey: "primary-cluster",
			Failover: &models.RouteFailover{
				SuspendDurationSeconds: 900,
				Targets: []models.RouteFailoverTarget{{
					Model:  "gpt-4o",
					Target: models.RouteFailoverEntry{Model: "gpt-4o", ClusterKey: "primary-cluster", Upstream: policyenginev1.UpstreamInfo{ClusterName: "primary-cluster", URL: "https://openai.com"}},
					Fallbacks: []models.RouteFailoverEntry{{
						Model:      "claude-sonnet-4-5-20250929",
						ClusterKey: "fallback-cluster",
						Upstream:   policyenginev1.UpstreamInfo{ClusterName: "fallback-cluster", URL: "https://anthropic.com"},
					}},
				}},
			},
		},
	}

	// Call whatever function this session finds actually builds the struct
	// (its real name, confirmed from reading snapshot.go directly — do not
	// guess a name here).
	data, err := buildRouteConfigData("POST|/chat/completions|main", route, /* whatever other params that real function signature requires */)
	require.NoError(t, err)

	raw, ok := data["failover_targets"]
	require.True(t, ok)
	targets, ok := raw.([]interface{})
	require.True(t, ok)
	require.Len(t, targets, 1)

	entry := targets[0].(map[string]interface{})
	assert.Equal(t, aggregateClusterName("POST|/chat/completions|main", 0), entry["aggregate_cluster"])
	assert.Equal(t, "gpt-4o", entry["model"])

	chain, ok := entry["chain"].([]interface{})
	require.True(t, ok)
	require.Len(t, chain, 2, "target + 1 fallback")
	first := chain[0].(map[string]interface{})
	assert.Equal(t, "primary-cluster", first["cluster_name"])
	second := chain[1].(map[string]interface{})
	assert.Equal(t, "fallback-cluster", second["cluster_name"])
}
```

**This step requires reading `gateway-controller/pkg/policyxds/snapshot.go` lines 350-445 directly before writing the final version of this test** — the function name, its exact parameter list, and the exact surrounding variable named `data` (confirmed to exist from this session's earlier reading, but its declaring function's signature was not fully captured) must come from that read, not from this plan. Do that read as the literal first action of this step, before finalizing the test body above.

- [ ] **Step 2: Run to verify it fails**

```bash
cd gateway-controller && go test ./pkg/policyxds/... -run TestBuildRouteConfigStruct_EmitsFailoverTargets -v
```
Expected: FAIL — `data["failover_targets"]` doesn't exist (`ok` is `false`).

- [ ] **Step 3: Implement**

In the same function, immediately after the existing:
```go
	if route.Upstream.Default != nil {
		data["default_upstream"] = route.Upstream.Default.ToMap()
	}
```
add:
```go
	if route.Upstream.Failover != nil {
		targets := make([]interface{}, 0, len(route.Upstream.Failover.Targets))
		for i, target := range route.Upstream.Failover.Targets {
			chain := make([]interface{}, 0, len(target.Fallbacks)+1)
			chain = append(chain, failoverEntryToMap(target.Target))
			for _, fb := range target.Fallbacks {
				chain = append(chain, failoverEntryToMap(fb))
			}
			targets = append(targets, map[string]interface{}{
				"aggregate_cluster": aggregateClusterName(routeKey, i),
				"model":             target.Model,
				"chain":             chain,
			})
		}
		data["failover_targets"] = targets
	}
```

Add the small helper (in the same file, or in `failover_cluster.go` from Task 4 if `policyxds` can import `xds` — check for an import cycle first: does `pkg/xds` already import `pkg/policyxds`, or vice versa, via `grep -rn "gateway-controller/pkg/policyxds\"" gateway-controller/pkg/xds/*.go` and `grep -rn "gateway-controller/pkg/xds\"" gateway-controller/pkg/policyxds/*.go`; if `policyxds` already imports `xds` for something else, `aggregateClusterName` is reachable as `xds.AggregateClusterName` once exported — export it, i.e. rename to `AggregateClusterName` in Task 4 and update Task 4's and Task 5's call sites to the exported name too, rather than duplicating the naming logic here):

```go
func failoverEntryToMap(e models.RouteFailoverEntry) map[string]interface{} {
	return map[string]interface{}{
		"model":        e.Model,
		"cluster_name": e.Upstream.ClusterName,
		"url":          e.Upstream.URL,
		"base_path":    e.Upstream.BasePath,
	}
}
```

Confirm `routeKey` is already an in-scope variable name in this function (this session's earlier reading of the surrounding code showed `routeKey, _ := data["route_key"].(string)` a few lines above the `default_upstream` block, inside the SAME function — reuse that exact variable, do not reintroduce it).

- [ ] **Step 4: Run to verify it passes**

```bash
cd gateway-controller && go test ./pkg/policyxds/... -run TestBuildRouteConfigStruct_EmitsFailoverTargets -v
```
Expected: PASS.

- [ ] **Step 5: Run the full policyxds package suite (regression check)**

```bash
cd gateway-controller && go test ./pkg/policyxds/...
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add gateway-controller/pkg/policyxds/snapshot.go gateway-controller/pkg/policyxds/snapshot_test.go gateway-controller/pkg/xds/failover_cluster.go
git commit -m "feat(llm-failover): sync failover_targets route metadata to the policy engine over xDS"
```

---

### Task 7: End-to-end regression + full-shape assertion

**Files:**
- Test: `gateway-controller/pkg/xds/failover_e2e_test.go` (new file)

**Interfaces:**
- Consumes: everything from Tasks 1-6. This task adds no new production code — it is the safety net proving the whole chain (OpenAPI → validation → mapper enrichment → aggregate clusters → retry policy → xDS metadata) holds together end-to-end, and that a config with NO failover block is provably unaffected.

- [ ] **Step 1: Write the regression test (no-failover byte-identical output)**

```go
func TestLLMTransform_NoFailoverBlock_OutputUnchangedFromBeforeThisFeature(t *testing.T) {
	// Build the exact same fixture Task 5's test uses, but with
	// Upstream.Failover left nil (the pre-this-feature shape). Run it
	// through translateRuntimeConfig and assert:
	//   - no aggregate cluster appears in the returned clusters
	//   - the route's RetryPolicy is nil
	//   - the route's HostRewriteSpecifier reflects only AutoHostRewrite
	//     (the pre-existing bool field), never RetryPolicy-driven rewrite
	//   - the containing VirtualHost's IncludeRequestAttemptCount is false
	// This is the concrete proof of this plan's Global Constraint: zero
	// output change for any route that doesn't opt in.
}
```

Fill in the actual assertions by re-using Task 5's exact fixture with `Failover: nil` removed entirely from the `RouteUpstream` literal, mirroring `TestTranslateRuntimeConfig_AttachesUpstreamFilterOnlyWhereNeeded`'s existing "only where needed" structure from this session's earlier work (that test already proves the analogous property for the upstream ext_proc filter — this test proves the same property for aggregate clusters/retry policy).

- [ ] **Step 2: Write the full end-to-end shape test**

```go
func TestLLMTransform_FailoverBlock_FullShapeEndToEnd(t *testing.T) {
	// Construct an api.LLMProxyConfiguration with:
	//   - primary provider "openai-provider"
	//   - one additionalProvider "anthropic-provider" (as: "anthropic-upstream")
	//   - resilience.failover with one target {model: gpt-4o} and one
	//     fallback {model: claude-sonnet-4-5-20250929, provider: anthropic-upstream}
	// Run it through LLMTransformer.Transform, then through
	// (&Translator{...}).translateRuntimeConfig(rdc), and assert:
	//   - exactly one aggregate cluster exists, named per aggregateClusterName
	//   - its members are [the resolved openai cluster key, the resolved
	//     anthropic cluster key], in that order
	//   - the chat/completions route's RetryPolicy.RetryOn == "5xx"
	//   - the route's HostRewriteSpecifier is AutoHostRewrite
	//   - the containing VirtualHost's IncludeRequestAttemptCount is true
	// This is the test that would have caught this plan's Task 1 schema
	// mistake (LLMResilience shared with RestApi) had it existed before —
	// treat it as the plan's own regression guard, not just a feature demo.
}
```

Fill in the actual test body by composing `TestRestAPITransformer_DefaultUpstreamClusterNameReferencesRealCluster`'s `api.RestAPI`-construction style (this session's earlier work, in `restapi_test.go`) adapted to `api.LLMProxyConfiguration`, following `TestLLMProviderTransformer_TransformProxy_AdditionalProviderAuthIsConditional`'s DB/template scaffolding (`gateway-controller/pkg/utils/llm_transformer_multiprovider_test.go`, read in this session) for the additionalProviders setup, then feeding the resulting `rdc` into a `Translator` constructed the same way Task 5's test does.

- [ ] **Step 3: Run both, verify they pass**

```bash
cd gateway-controller && go test ./pkg/xds/... -run TestLLMTransform -v
```
Expected: PASS.

- [ ] **Step 4: Run the ENTIRE gateway-controller test suite**

```bash
cd gateway-controller && go test ./...
```
Expected: PASS, zero failures anywhere — the final proof this plan introduced no regression across the whole module.

- [ ] **Step 5: Commit**

```bash
git add gateway-controller/pkg/xds/failover_e2e_test.go
git commit -m "test(llm-failover): end-to-end regression + full-shape assertion for resilience.failover"
```

---

## Self-Review

**Spec coverage:** §3 (config surface) → Task 1. §4 (control-plane translation: aggregate cluster, retry_priority, auto_host_rewrite, attempt-count, filter-on-aggregate) → Tasks 4-5. The "one aggregate per targets[] entry" requirement from the brainstorming conversation → Task 4. Validation of `provider` references and deploy-time rejection → Task 2. §4's xDS-metadata-sync half → Task 6. §9's testing strategy (no-op-when-unattached assertion, `ValidateAll()` proto check) → Tasks 4 and 7.

**Deferred to later plans, explicitly not in this plan:** §5 (downstream target selection, suspension pre-emption), §6 (upstream attempt-count resolution, transformer dispatch), §7 (transformer `OnUpstreamRequestBody`/`OnUpstreamResponseBody`, suspension recording) — all Plan B/C/D, per the sequencing decision.

**Placeholder scan:** Task 6 and Task 7 both contain an instruction to read real source before finalizing a test body, rather than a filled-in final version — this is a deliberate, flagged exception to "no placeholders," not an oversight: this plan's own research (documented in the conversation this plan was produced from) confirmed the SHAPE of `snapshot.go`'s route-struct builder and `translateRuntimeConfig`'s test scaffolding exist and behave as described, but did not capture their exact enclosing function signatures/helper names character-for-character. Every other task's code is complete and directly runnable as written.

**Type consistency:** `models.RouteFailover`/`RouteFailoverTarget`/`RouteFailoverEntry` (Task 3) are used with identical field names in Tasks 4, 5, 6, 7. `aggregateClusterName` (Task 4) is called with identical `(routeKey string, targetIndex int)` signature in Tasks 6 and 7 — Task 6 flags the export-rename (`AggregateClusterName`) needed if an import-cycle check requires it; if no cycle exists, keep the lowercase name and Task 6's helper lives in `policyxds` calling into `xds` normally.

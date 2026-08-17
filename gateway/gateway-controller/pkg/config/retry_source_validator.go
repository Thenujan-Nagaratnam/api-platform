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

package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// LookupRetryMetadata resolves ONE policy reference (name plus a possibly
// major-only version, the form a models.PolicyChain carries) against the
// loaded policy definitions and returns whichever retry metadata that
// policy's own policy-definition.yaml declared. Both nil means "this policy
// participates in no retry mechanism", which is the case for almost every
// policy — and for any name/version that doesn't resolve at all, since an
// unresolvable reference is already rejected far earlier by PolicyValidator
// and must not turn into a translation-time error here.
//
// This is the single shared primitive BOTH registration-time validation
// (ValidateRetrySourcesForOperations, via NewRetrySourceResolver) and
// translation-time discovery (xds.(*Translator).resolveRetryDeclarations)
// call, so the two can never diverge on their reading of the same policy
// chain — a config that passes validation must build, and vice versa. It
// reads cached YAML metadata only; gateway-controller never imports or
// instantiates any policy implementation package.
//
// The second return value is the policy's own parameters JSON-schema
// (def.Parameters), needed by callers that go on to call
// ParseRetryTriggerParams — that generic parser reads a policy's AS-DEPLOYED
// params map, which never received schema-declared defaults for an omitted
// field (gateway-controller's own coerceParamsBySchema only coerces types
// for keys already present; it never materializes a missing key), so the
// schema itself must travel alongside the metadata for that fallback to work.
//
// The third return value is the policy's raw x-wso2-retry-conditions block
// (def.RetryConditions), resolved generically by config.ParseRetryConditions.
func LookupRetryMetadata(
	definitions map[string]models.PolicyDefinition,
	latestVersions map[string]string,
	name, version string,
) (*models.RetrySourceMetadata, *map[string]interface{}, *map[string]interface{}) {
	if len(definitions) == 0 {
		return nil, nil, nil
	}
	// A policy chain stores the MAJOR-only version ("v1"), not the full
	// "v1.0.0" the definitions map is keyed by, so resolve before keying.
	resolved, err := ResolvePolicyVersion(definitions, latestVersions, name, version)
	if err != nil {
		return nil, nil, nil
	}
	def, ok := definitions[name+"|"+resolved]
	if !ok {
		return nil, nil, nil
	}
	return def.RetrySource, def.Parameters, def.RetryConditions
}

// RetrySourceResolver resolves an operation's effective policy list (API-level
// entries first, then operation-level — the same precedence
// transform.RestAPITransformer.buildPolicyChain applies) into every
// RetrySourceDeclaration those policies declare, plus the total count for the
// exclusivity check.
type RetrySourceResolver func(apiPolicies, opPolicies *[]api.Policy) ([]*policy.RetrySourceDeclaration, int, error)

// NewRetrySourceResolver builds the RetrySourceResolver every registration-time
// deploy path passes to ValidateRetrySourcesForOperations. Passing nil/empty
// definitions yields a resolver that finds nothing, which is correct for the
// handful of test/bootstrap constructions that have no policy registry wired —
// there is nothing to discover without policy definitions.
func NewRetrySourceResolver(
	definitions map[string]models.PolicyDefinition,
	latestVersions map[string]string,
) RetrySourceResolver {
	return func(apiPolicies, opPolicies *[]api.Policy) ([]*policy.RetrySourceDeclaration, int, error) {
		var decls []*policy.RetrySourceDeclaration
		count := 0
		visit := func(p api.Policy) error {
			source, _, _ := LookupRetryMetadata(definitions, latestVersions, p.Name, p.Version)
			if source == nil {
				return nil
			}
			count++
			var params map[string]interface{}
			if p.Params != nil {
				params = *p.Params
			}
			decl, err := ParseRetrySourceParams(params, source.GroupKeyField)
			if err != nil {
				return fmt.Errorf("policy %q: %w", p.Name, err)
			}
			decls = append(decls, decl)
			return nil
		}
		for _, list := range []*[]api.Policy{apiPolicies, opPolicies} {
			if list == nil {
				continue
			}
			for _, p := range *list {
				if err := visit(p); err != nil {
					return nil, 0, err
				}
			}
		}
		return decls, count, nil
	}
}

// ValidateRetrySourceTargetsHaveNoBasePath rejects any RetryGroup with 2+
// OrderedTargets (i.e. one that will actually get an aggregate cluster
// built — see xds/translator.go) where any target resolves to a backend
// with a non-empty BasePath. An empty UpstreamDefinitionName means "the
// API's main upstream" and is checked against mainBasePath.
//
// This is a real, confirmed correctness gap, not a hypothetical: Envoy's
// aggregate-cluster + retry_priority mechanism only ever varies WHICH
// CLUSTER gets dialed on a retry — it has no hook to vary the REQUEST PATH
// per member. An upstream that relies on a base path to reach the right
// destination (most notably an LlmProxy additionalProviders-derived alias,
// or an LlmProxy's own main upstream — both always resolve to the
// identical loopback address and are distinguished ONLY by path) silently
// misroutes on retry otherwise. Generalized from
// ValidateModelFailoverAggregateMembersHaveNoBasePath — same logic,
// operating on any policy's declared RetrySourceDeclaration.Groups instead
// of one policy's own ModelFailoverParams.Targets.
func ValidateRetrySourceTargetsHaveNoBasePath(decl *policy.RetrySourceDeclaration, basePathByUpstreamDef map[string]string, mainBasePath string) error {
	if decl == nil {
		return nil
	}
	resolve := func(upstreamDef string) string {
		if upstreamDef == "" {
			return mainBasePath
		}
		return basePathByUpstreamDef[upstreamDef]
	}
	describe := func(upstreamDef string) string {
		if upstreamDef == "" {
			return "the API's main upstream"
		}
		return fmt.Sprintf("upstreamDefinition %q", upstreamDef)
	}
	for _, group := range decl.Groups {
		if len(group.OrderedTargets) < 2 {
			continue // no aggregate cluster is ever built for this group - not at risk
		}
		for _, target := range group.OrderedTargets {
			if bp := resolve(target.UpstreamDefinitionName); bp != "" && bp != "/" {
				return fmt.Errorf("retry-source group %q resolves target %s, which has a non-empty basePath (%q), and will be used as an aggregate-cluster member — Envoy's native retry cannot vary the request path per member, so a basePath-dependent upstream (e.g. an LlmProxy additionalProviders alias, or an LlmProxy's own main upstream) would silently misroute on retry. Give this target no fallbacks, or point every member of this group at a plain, no-basePath upstream", group.Key, describe(target.UpstreamDefinitionName), bp)
			}
		}
	}
	return nil
}

// ValidateAtMostOneRetrySourcePerRoute rejects a route where retry
// ownership is ambiguous: more than one policy declaring
// x-wso2-retry-source, OR exactly one such policy combined with
// resilience.retry also configured. Both are real conflicts — Envoy has
// exactly one RouteAction.RetryPolicy (and one RetryPriority extension
// slot) per route, so two independent owners can never be reconciled.
// retrySourceCount is the number of policies in this route's chain whose
// policy-definition.yaml declares x-wso2-retry-source (computed by the
// caller via the declarative metadata lookup — see
// xds/translator.go's resolveRetryDeclarations, Task 6 Step 6a — never a
// Go type assertion, since gateway-controller never instantiates policy
// Go code) — this function only enforces the counting rule, generic to
// whichever policies happen to be present. Generalized from
// ValidateModelFailoverPolicy.
func ValidateAtMostOneRetrySourcePerRoute(retrySourceCount int, retry *api.Retry) error {
	if retrySourceCount == 0 {
		return nil
	}
	if retrySourceCount > 1 {
		return fmt.Errorf("this route has %d policies each declaring retry-source ownership — at most one is allowed per route, since Envoy has a single RouteAction.RetryPolicy slot", retrySourceCount)
	}
	if retry != nil {
		return fmt.Errorf("a retry-source policy on this route cannot be combined with resilience.retry — both would drive RouteAction.RetryPolicy")
	}
	return nil
}

// ValidateRetrySourceUpstreamReferences rejects a declaration referencing a
// NAMED upstreamDefinition not actually declared on the same API. An empty
// UpstreamDefinitionName ("" — the API's main upstream) always resolves and is
// never checked. Without this, a typo'd upstreamDefinition deploys
// "successfully" and only fails at request time with an opaque Envoy "no such
// cluster" error, because the kernel's UpstreamName resolver builds the target
// cluster name unconditionally and never checks it exists. Generalized from
// ValidateModelFailoverUpstreamReferences — same logic, operating on any
// policy's declared Groups rather than one policy's own params struct.
func ValidateRetrySourceUpstreamReferences(decl *policy.RetrySourceDeclaration, declaredUpstreamDefs map[string]bool) error {
	if decl == nil {
		return nil
	}
	for _, group := range decl.Groups {
		for _, target := range group.OrderedTargets {
			if target.UpstreamDefinitionName != "" && !declaredUpstreamDefs[target.UpstreamDefinitionName] {
				return fmt.Errorf("retry-source group %q references upstreamDefinition %q, which is not declared in this API's upstreamDefinitions", group.Key, target.UpstreamDefinitionName)
			}
		}
	}
	return nil
}

// ValidateRetrySourcesForOperations runs the generic retry-source registration
// checks against every operation in spec, for whichever policies in that
// operation's chain declare x-wso2-retry-source in their policy-definition.yaml
// — generalized from ValidateModelFailoverForOperations, which only ever looked
// for a policy literally named "model-failover".
//
// resolveDeclarations is caller-supplied (build it with NewRetrySourceResolver)
// so this validator and translation-time discovery share one implementation:
// both funnel through LookupRetryMetadata + ParseRetrySourceParams, so a config
// can never pass validation and then fail translation on a divergent reading of
// the same policy chain. A nil resolver makes this a no-op — the only callers
// that pass nil are ones constructed without a policy registry, where there is
// nothing to discover.
//
// This is the single transform-independent entry point deliberately factored out
// so it can be called from BOTH pkg/transform.RestAPITransformer.Transform (the
// async xDS/runtime-config build path) AND every synchronous pre-persist deploy
// path (LlmProvider, LlmProxy, and plain RestAPI registration) — closing a
// confirmed-live gap where a bad config was accepted with an HTTP 201
// (registration only ever ran the async transform, which fails off the request
// thread and leaves a persisted-but-broken route returning a 500 with no
// client-visible error at registration time). Every caller MUST run it before
// persisting, not only before building routes.
func ValidateRetrySourcesForOperations(spec *api.APIConfigData, resolveDeclarations RetrySourceResolver) error {
	if spec == nil || resolveDeclarations == nil {
		return nil
	}

	declaredUpstreamDefs := make(map[string]bool)
	basePathByUpstreamDef := make(map[string]string)
	if spec.UpstreamDefinitions != nil {
		for _, def := range *spec.UpstreamDefinitions {
			declaredUpstreamDefs[def.Name] = true
			if def.BasePath != nil {
				basePathByUpstreamDef[def.Name] = *def.BasePath
			}
		}
	}
	mainBasePath := mainUpstreamBasePath(spec.Upstream.Main, spec.UpstreamDefinitions)
	apiRetry := effectiveResilienceRetry(spec.Resilience)

	for _, op := range spec.Operations {
		decls, count, err := resolveDeclarations(spec.Policies, op.Policies)
		if err != nil {
			return fmt.Errorf("operation %s %s: %w", op.EffectiveMethod(), op.EffectivePath(), err)
		}
		if count == 0 {
			continue
		}
		effectiveRetry := effectiveResilienceRetry(op.Resilience)
		if effectiveRetry == nil {
			effectiveRetry = apiRetry
		}
		if err := ValidateAtMostOneRetrySourcePerRoute(count, effectiveRetry); err != nil {
			return fmt.Errorf("operation %s %s: %w", op.EffectiveMethod(), op.EffectivePath(), err)
		}
		for _, decl := range decls {
			if err := ValidateRetrySourceUpstreamReferences(decl, declaredUpstreamDefs); err != nil {
				return fmt.Errorf("operation %s %s: %w", op.EffectiveMethod(), op.EffectivePath(), err)
			}
			if err := ValidateRetrySourceTargetsHaveNoBasePath(decl, basePathByUpstreamDef, mainBasePath); err != nil {
				return fmt.Errorf("operation %s %s: %w", op.EffectiveMethod(), op.EffectivePath(), err)
			}
		}
	}
	return nil
}

// ParseRetrySourceParams generically parses a policy's params into a
// RetrySourceDeclaration, for ANY policy whose policy-definition.yaml
// declares x-wso2-retry-source — driven entirely by the fixed structural
// shape (targets: [{<groupKeyField>: string, upstreamDefinition: string,
// fallbacks: [{upstreamDefinition: string}]}], statusCodes: [int]) and the
// caller-supplied groupKeyField (from that policy's own
// models.RetrySourceMetadata.GroupKeyField). gateway-controller never
// executes policy Go code to produce this — see the design's Design
// Revision 2 for why. Fields in each targets[]/fallbacks[] entry other
// than groupKeyField/upstreamDefinition (e.g. model-failover's own
// fallbacks[].model, used to rewrite the request body — a concern entirely
// internal to that policy's own runtime code) are ignored here; this
// parser only extracts what gateway-controller itself needs to build
// Envoy config.
func ParseRetrySourceParams(params map[string]interface{}, groupKeyField string) (*policy.RetrySourceDeclaration, error) {
	rawTargets, ok := params["targets"].([]interface{})
	if !ok || len(rawTargets) == 0 {
		return nil, fmt.Errorf("retry-source policy requires a non-empty 'targets' list")
	}

	groups := make([]policy.RetryGroup, 0, len(rawTargets))
	for i, raw := range rawTargets {
		t, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("retry-source policy: targets[%d] is not an object", i)
		}
		key, _ := t[groupKeyField].(string)
		if key == "" {
			return nil, fmt.Errorf("retry-source policy: targets[%d].%s is required", i, groupKeyField)
		}
		upstreamDef, _ := t["upstreamDefinition"].(string)
		orderedTargets := []policy.RetryTarget{{UpstreamDefinitionName: upstreamDef}}

		rawFallbacks, _ := t["fallbacks"].([]interface{})
		for j, rawFb := range rawFallbacks {
			fb, ok := rawFb.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("retry-source policy: targets[%d].fallbacks[%d] is not an object", i, j)
			}
			fbUpstreamDef, _ := fb["upstreamDefinition"].(string)
			orderedTargets = append(orderedTargets, policy.RetryTarget{UpstreamDefinitionName: fbUpstreamDef})
		}

		groups = append(groups, policy.RetryGroup{Key: key, OrderedTargets: orderedTargets})
	}

	rawStatusCodes, ok := params["statusCodes"].([]interface{})
	if !ok || len(rawStatusCodes) == 0 {
		return nil, fmt.Errorf("retry-source policy requires a non-empty 'statusCodes' list")
	}
	statusCodes := make([]int, 0, len(rawStatusCodes))
	for i, raw := range rawStatusCodes {
		code, ok := raw.(int)
		if !ok {
			if f, ok := raw.(float64); ok { // YAML/JSON numeric decode may hand back float64
				code = int(f)
			} else {
				return nil, fmt.Errorf("retry-source policy: statusCodes[%d] must be an integer, got %T", i, raw)
			}
		}
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("retry-source policy: statusCodes[%d] value %d is not a valid HTTP status code", i, code)
		}
		statusCodes = append(statusCodes, code)
	}

	decl := &policy.RetrySourceDeclaration{Groups: groups}

	// requestTimeout is part of the same fixed structural shape: it bounds ONE
	// attempt (Envoy's RetryPolicy.PerTryTimeout), not the whole route. Optional —
	// absent means the route's existing timeout applies to every attempt.
	if raw, ok := params["requestTimeout"].(string); ok && raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("retry-source policy: invalid requestTimeout %q: %w", raw, err)
		}
		decl.PerAttemptTimeout = &d
	}

	return decl, nil
}

// ParseRetryTriggerParams generically parses a policy's params into a
// RetryTriggerDeclaration, for ANY policy whose policy-definition.yaml
// declares x-wso2-retry-trigger. statusCodesField and minAttempts come
// from that policy's own models.RetryTriggerMetadata. An empty named field
// is not an error — it means this policy contributes no trigger conditions
// for the current config (e.g. oauth2-generator's tokenPurgeStatusCodes
// explicitly set to []), which the caller (Task 6) treats as "nothing to
// add", not a failure.
//
// schema is the policy's own parameters JSON-schema (models.PolicyDefinition
// .Parameters, threaded through by LookupRetryMetadata's third return value).
// It matters only when the field is entirely ABSENT from params — as opposed
// to explicitly set to [] — since gateway-controller's schema-coercion step
// (coerceParamsBySchema) never materializes a schema-declared default into
// an omitted key; it only coerces the type of keys already present. Without
// this fallback, a deployment that omits tokenPurgeStatusCodes to rely on
// its schema default ([401]) would silently stop contributing that code to
// route-level retry, changing today's behavior. May be nil (e.g. in tests),
// in which case an absent field behaves like an explicitly empty one.
func ParseRetryTriggerParams(params map[string]interface{}, statusCodesField string, minAttempts int, schema *map[string]interface{}) (*policy.RetryConditions, error) {
	raw, present := params[statusCodesField]
	if !present {
		raw = retryTriggerSchemaDefault(schema, statusCodesField)
	}
	rawStatusCodes, ok := raw.([]interface{})
	if !ok {
		return &policy.RetryConditions{}, nil
	}
	statusCodes := make([]int, 0, len(rawStatusCodes))
	for i, raw := range rawStatusCodes {
		code, ok := raw.(int)
		if !ok {
			if f, ok := raw.(float64); ok {
				code = int(f)
			} else {
				return nil, fmt.Errorf("retry-trigger policy: %s[%d] must be an integer, got %T", statusCodesField, i, raw)
			}
		}
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("retry-trigger policy: %s[%d] value %d is not a valid HTTP status code", statusCodesField, i, code)
		}
		statusCodes = append(statusCodes, code)
	}
	if len(statusCodes) == 0 {
		return &policy.RetryConditions{}, nil
	}
	minAttemptsCopy := minAttempts
	return &policy.RetryConditions{StatusCodes: statusCodes, MinAttempts: &minAttemptsCopy}, nil
}

// retryTriggerSchemaDefault reads properties.<field>.default from a policy's
// JSON-schema parameters (models.PolicyDefinition.Parameters), returning nil
// if schema is nil or the path is absent/malformed at any level.
func retryTriggerSchemaDefault(schema *map[string]interface{}, field string) interface{} {
	if schema == nil {
		return nil
	}
	properties, ok := (*schema)["properties"].(map[string]interface{})
	if !ok {
		return nil
	}
	propSchema, ok := properties[field].(map[string]interface{})
	if !ok {
		return nil
	}
	return propSchema["default"]
}

// mainUpstreamBasePath derives the API's main upstream's effective base path, mirroring
// pkg/transform.RestAPITransformer's own resolveUpstreamURL (a direct URL carries its base
// path in the URL's own path component; a ref takes it from the referenced
// UpstreamDefinition's BasePath field instead) — so ValidateRetrySourceTargetsHaveNoBasePath
// can treat "defaults to main" exactly like a named upstreamDefinition reference, with no
// special-casing the caller needs to know about. Returns "" on any unresolvable shape
// (missing URL/ref, malformed URL, dangling ref) — those are all real problems, but they're
// caught elsewhere (schema validation, the main upstream's own cluster-build step); this
// function only needs a best-effort basePath for the aggregate-cluster safety check.
func mainUpstreamBasePath(main api.Upstream, upstreamDefinitions *[]api.UpstreamDefinition) string {
	if main.Url != nil && strings.TrimSpace(*main.Url) != "" {
		u, err := url.Parse(strings.TrimSpace(*main.Url))
		if err != nil {
			return ""
		}
		return u.Path
	}
	if main.Ref != nil && strings.TrimSpace(*main.Ref) != "" && upstreamDefinitions != nil {
		refName := strings.TrimSpace(*main.Ref)
		for _, def := range *upstreamDefinitions {
			if def.Name == refName && def.BasePath != nil {
				return *def.BasePath
			}
		}
	}
	return ""
}

// effectiveResilienceRetry extracts the Retry field from an optional api.Resilience, without
// needing the full timeout-resolution machinery in pkg/xds (which pkg/config cannot import —
// pkg/xds already imports pkg/config for retry-source/retry-trigger parsing).
// ValidateAtMostOneRetrySourcePerRoute only needs to know whether a retry policy is configured
// at all, not its resolved values.
func effectiveResilienceRetry(r *api.Resilience) *api.Retry {
	if r == nil {
		return nil
	}
	return r.Retry
}

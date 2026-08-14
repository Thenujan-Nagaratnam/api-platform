/*
 * Copyright (c) 2025, WSO2 LLC. (https://www.wso2.com).
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
)

// ModelFailoverFallback is one fallback entry within a target group's chain.
// UpstreamDefinition is OPTIONAL: empty means "the API's own main upstream" (spec.upstream),
// the same backend used when no model-failover is configured at all. Most APIs have exactly
// one upstream, so most fallbacks need nothing here — only a fallback that must reach a
// genuinely different backend (a different upstreamDefinition, e.g. a real cross-provider
// target) needs to name one explicitly.
type ModelFailoverFallback struct {
	Model              string // injected into the request body's "model" field for this attempt
	UpstreamDefinition string // "" means the API's main upstream; otherwise must match a declared UpstreamDefinition.Name
}

// ModelFailoverTargetGroup is one independently-selectable target: Model is the value
// a client's own request.body.model must equal to select this whole group, and
// Fallbacks is that group's own ordered failover chain — entirely independent of every
// other group's chain (own suspend state, own retry budget). A group with zero
// Fallbacks is legal: it's just "route this one model name, no failover," useful when
// only some of an API's supported models actually need a fallback chain.
type ModelFailoverTargetGroup struct {
	Model              string // the client-requested model name that selects this group
	UpstreamDefinition string // "" means the API's main upstream; otherwise must match a declared UpstreamDefinition.Name
	Fallbacks          []ModelFailoverFallback
}

// ModelFailoverParams is the parsed, validated shape of the model-failover policy's
// policyParams. RequestTimeout/SuspendDuration are nil when unset (SuspendDuration nil
// means "no suspend tracking at all"). StatusCodes/RequestTimeout are necessarily
// route-wide, not per-group: they drive the single RouteAction.RetryPolicy Envoy
// attaches to the whole route (Envoy has no per-cluster retry policy), which every
// group's resolved cluster is retried under identically, regardless of which target
// matched.
type ModelFailoverParams struct {
	Targets         []ModelFailoverTargetGroup
	StatusCodes     []int
	RequestTimeout  *time.Duration
	SuspendDuration *time.Duration
}

// ParseModelFailoverParams parses and validates model-failover's policyParams map (as
// stored in models.Policy.Params). This only validates the shape of policyParams itself —
// callers must separately call ValidateModelFailoverUpstreamReferences against the same
// API's declared upstreamDefinitions, since a dangling reference here would otherwise only
// surface as a confusing "no such cluster" failure from Envoy at request time, not a clean
// rejection at registration time (the kernel's UpstreamName resolver builds the target
// cluster name unconditionally and never checks it actually exists).
func ParseModelFailoverParams(params map[string]interface{}) (*ModelFailoverParams, error) {
	rawTargets, ok := params["targets"].([]interface{})
	if !ok || len(rawTargets) == 0 {
		return nil, fmt.Errorf("model-failover requires a non-empty 'targets' list")
	}

	targets := make([]ModelFailoverTargetGroup, 0, len(rawTargets))
	seenModels := make(map[string]bool, len(rawTargets))
	for i, raw := range rawTargets {
		t, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("model-failover: targets[%d] is not an object", i)
		}
		model, _ := t["model"].(string)
		if model == "" {
			return nil, fmt.Errorf("model-failover: targets[%d].model is required", i)
		}
		if seenModels[model] {
			return nil, fmt.Errorf("model-failover: targets[%d].model %q is declared more than once — each target's model must be unique, it's the dispatch key matched against the client's own request.model", i, model)
		}
		seenModels[model] = true
		// upstreamDefinition is optional: absent/empty means "this API's own main upstream".
		upstreamDef, _ := t["upstreamDefinition"].(string)

		rawFallbacks, _ := t["fallbacks"].([]interface{})
		fallbacks := make([]ModelFailoverFallback, 0, len(rawFallbacks))
		for j, rawFb := range rawFallbacks {
			fb, ok := rawFb.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("model-failover: targets[%d].fallbacks[%d] is not an object", i, j)
			}
			fbModel, _ := fb["model"].(string)
			if fbModel == "" {
				return nil, fmt.Errorf("model-failover: targets[%d].fallbacks[%d].model is required", i, j)
			}
			// upstreamDefinition is optional here too: absent/empty means main upstream.
			fbUpstreamDef, _ := fb["upstreamDefinition"].(string)
			fallbacks = append(fallbacks, ModelFailoverFallback{Model: fbModel, UpstreamDefinition: fbUpstreamDef})
		}

		targets = append(targets, ModelFailoverTargetGroup{Model: model, UpstreamDefinition: upstreamDef, Fallbacks: fallbacks})
	}

	rawStatusCodes, ok := params["statusCodes"].([]interface{})
	if !ok || len(rawStatusCodes) == 0 {
		return nil, fmt.Errorf("model-failover requires a non-empty 'statusCodes' list")
	}
	statusCodes := make([]int, 0, len(rawStatusCodes))
	for i, raw := range rawStatusCodes {
		code, ok := raw.(int)
		if !ok {
			if f, ok := raw.(float64); ok { // YAML/JSON numeric decode may hand back float64
				code = int(f)
			} else {
				return nil, fmt.Errorf("model-failover: statusCodes[%d] must be an integer, got %T", i, raw)
			}
		}
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("model-failover: statusCodes[%d] value %d is not a valid HTTP status code", i, code)
		}
		statusCodes = append(statusCodes, code)
	}

	mf := &ModelFailoverParams{Targets: targets, StatusCodes: statusCodes}

	if raw, ok := params["requestTimeout"].(string); ok && raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("model-failover: invalid requestTimeout %q: %w", raw, err)
		}
		mf.RequestTimeout = &d
	}
	if raw, ok := params["suspendDuration"].(string); ok && raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("model-failover: invalid suspendDuration %q: %w", raw, err)
		}
		mf.SuspendDuration = &d
	}

	return mf, nil
}

// MaxFallbackChainLength returns the longest fallback chain across every target group —
// the route-wide RetryPolicy.NumRetries this policy needs, since Envoy has one retry
// budget per route, shared by whichever group's cluster a given request actually
// resolves to. A group with a shorter (or zero) chain simply never exhausts its share
// of that shared budget; it does not get a shorter one of its own.
func (mf *ModelFailoverParams) MaxFallbackChainLength() int {
	max := 0
	for _, t := range mf.Targets {
		if len(t.Fallbacks) > max {
			max = len(t.Fallbacks)
		}
	}
	return max
}

// ValidateModelFailoverUpstreamReferences rejects a model-failover config that references
// a NAMED UpstreamDefinition not actually declared on the same API. An empty/omitted
// UpstreamDefinition ("" — main upstream) always resolves and is never checked here; the
// API's main upstream always exists by definition. Without this check on named references, a
// typo'd upstreamDefinition name deploys "successfully" and only fails at request time with
// an opaque Envoy "no such cluster" error — the kernel's UpstreamName resolver builds the
// target cluster name unconditionally and never checks it exists (see ParseModelFailoverParams's
// own doc comment).
func ValidateModelFailoverUpstreamReferences(mf *ModelFailoverParams, declaredUpstreamDefs map[string]bool) error {
	if mf == nil {
		return nil
	}
	for i, t := range mf.Targets {
		if t.UpstreamDefinition != "" && !declaredUpstreamDefs[t.UpstreamDefinition] {
			return fmt.Errorf("model-failover: targets[%d].upstreamDefinition %q is not declared in this API's upstreamDefinitions", i, t.UpstreamDefinition)
		}
		for j, fb := range t.Fallbacks {
			if fb.UpstreamDefinition != "" && !declaredUpstreamDefs[fb.UpstreamDefinition] {
				return fmt.Errorf("model-failover: targets[%d].fallbacks[%d].upstreamDefinition %q is not declared in this API's upstreamDefinitions", i, j, fb.UpstreamDefinition)
			}
		}
	}
	return nil
}

// ValidateModelFailoverAggregateMembersHaveNoBasePath rejects a target group (one with at
// least one fallback, i.e. one that will actually get an aggregate cluster built — see
// xds/translator.go) where the group's own upstreamDefinition, or any of its fallbacks',
// resolves to a backend with a non-empty BasePath. An empty UpstreamDefinition ("") means
// "the API's main upstream" and is checked against mainBasePath — the SAME risk applies
// there too: an LlmProxy's own main upstream is ALWAYS loopback-routed with the provider's
// context baked into its URL (see llm_transformer.go's transformProxy), structurally
// identical to an additionalProviders alias, just represented as a URL path instead of a
// separate BasePath field. So defaulting to main is exactly as restricted as naming a
// BasePath-carrying upstreamDefinition explicitly — this function treats them identically,
// with no separate concept the caller needs to know about.
//
// This is a real, confirmed correctness gap, not a hypothetical: Envoy's aggregate-cluster +
// retry_priority mechanism only ever varies WHICH CLUSTER gets dialed on a retry — it has no
// hook to vary the REQUEST PATH per member. A plain external upstream with no base path
// (the common case for LlmProvider — every upstreamDefinition in this codebase's own working
// e2e coverage has none, and a plain spec.upstream.url main has none either) is unaffected,
// since "no rewrite" and "the trivial root rewrite" are the same thing. But an upstream that
// DOES rely on a base path to reach the right destination — most notably an LlmProxy's
// additionalProviders-derived alias, or an LlmProxy's own main upstream, both of which always
// resolve to the identical loopback address (127.0.0.1:<gateway's own listener port>) and are
// distinguished ONLY by path — silently breaks: confirmed live, a retry that should land on a
// different provider instead loops back to the SAME one, while the per-attempt body rewrite
// still (incorrectly) labels the response as having come from the target it never actually
// reached. That's not a clean failure, it's a silent mislabeling, which is exactly the class
// of bug worth a hard rejection at registration time rather than a runtime surprise.
// Suspend-driven skip-ahead (a separate, later request) is NOT affected by this — it
// redirects directly to a single upstream via the ordinary, already-working
// resolveUpstreamRedirect path, never through an aggregate cluster at all.
func ValidateModelFailoverAggregateMembersHaveNoBasePath(mf *ModelFailoverParams, basePathByUpstreamDef map[string]string, mainBasePath string) error {
	if mf == nil {
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
	for i, t := range mf.Targets {
		if len(t.Fallbacks) == 0 {
			continue // no aggregate cluster is ever built for this group - not at risk
		}
		if bp := resolve(t.UpstreamDefinition); bp != "" && bp != "/" {
			return fmt.Errorf("model-failover: targets[%d] resolves to %s, which has a non-empty basePath (%q), and this target has fallbacks, so it will be used as an aggregate-cluster member — Envoy's native retry cannot vary the request path per member, so a basePath-dependent upstream (e.g. an LlmProxy additionalProviders alias, or an LlmProxy's own main upstream) would silently misroute on retry. Use suspend-driven skip-ahead instead (give this target zero fallbacks, or move basePath-dependent targets to their own zero-fallback groups) — or point every member of this chain at a plain, no-basePath upstream", i, describe(t.UpstreamDefinition), bp)
		}
		for j, fb := range t.Fallbacks {
			if bp := resolve(fb.UpstreamDefinition); bp != "" && bp != "/" {
				return fmt.Errorf("model-failover: targets[%d].fallbacks[%d] resolves to %s, which has a non-empty basePath (%q), and would be used as an aggregate-cluster member — Envoy's native retry cannot vary the request path per member, so a basePath-dependent upstream (e.g. an LlmProxy additionalProviders alias, or an LlmProxy's own main upstream) would silently misroute on retry", i, j, describe(fb.UpstreamDefinition), bp)
			}
		}
	}
	return nil
}

// ValidateModelFailoverPolicy rejects a route that configures both model-failover and
// resilience.retry — both drive RouteAction.RetryPolicy, and letting both configure it
// independently would mean whichever one translates last silently wins.
func ValidateModelFailoverPolicy(mf *ModelFailoverParams, retry *api.Retry) error {
	if mf == nil {
		return nil
	}
	if retry != nil {
		return fmt.Errorf("model-failover cannot be combined with resilience.retry on the same route/operation — they both drive RouteAction.RetryPolicy")
	}
	return nil
}

// ValidateModelFailoverForOperations runs all three model-failover registration-time checks
// (ValidateModelFailoverPolicy, ValidateModelFailoverUpstreamReferences,
// ValidateModelFailoverAggregateMembersHaveNoBasePath) against every operation in spec.
//
// This is the single transform-independent entry point deliberately factored out so it can
// be called from BOTH pkg/transform.RestAPITransformer.Transform (the async xDS/runtime-
// config build path) AND every synchronous pre-persist deploy path (LlmProvider, LlmProxy,
// and plain RestAPI registration) — closing a confirmed-live gap where a bad model-failover
// config was accepted with an HTTP 201 (registration only ever ran the async transform,
// which fails off the request thread and leaves a persisted-but-broken route returning a
// 500 with no client-visible error at registration time). Every caller of this function
// MUST run it before persisting, not only before building routes.
//
// spec is the resolved api.APIConfigData — for an LlmProxy/LlmProvider registration this is
// the post-transform api.RestAPI.Spec (UpstreamDefinitions already carries any
// additionalProviders-synthesized aliases with their BasePath set); for a plain RestAPI
// registration it's the caller-supplied spec directly.
func ValidateModelFailoverForOperations(spec *api.APIConfigData) error {
	if spec == nil {
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
		policy := firstModelFailoverPolicy(spec.Policies, op.Policies)
		if policy == nil {
			continue
		}
		var params map[string]interface{}
		if policy.Params != nil {
			params = *policy.Params
		}
		mf, err := ParseModelFailoverParams(params)
		if err != nil {
			return fmt.Errorf("operation %s %s: %w", op.EffectiveMethod(), op.EffectivePath(), err)
		}

		effectiveRetry := effectiveResilienceRetry(op.Resilience)
		if effectiveRetry == nil {
			effectiveRetry = apiRetry
		}
		if err := ValidateModelFailoverPolicy(mf, effectiveRetry); err != nil {
			return fmt.Errorf("operation %s %s: %w", op.EffectiveMethod(), op.EffectivePath(), err)
		}
		if err := ValidateModelFailoverUpstreamReferences(mf, declaredUpstreamDefs); err != nil {
			return fmt.Errorf("operation %s %s: %w", op.EffectiveMethod(), op.EffectivePath(), err)
		}
		if err := ValidateModelFailoverAggregateMembersHaveNoBasePath(mf, basePathByUpstreamDef, mainBasePath); err != nil {
			return fmt.Errorf("operation %s %s: %w", op.EffectiveMethod(), op.EffectivePath(), err)
		}
	}
	return nil
}

// mainUpstreamBasePath derives the API's main upstream's effective base path, mirroring
// pkg/transform.RestAPITransformer's own resolveUpstreamURL (a direct URL carries its base
// path in the URL's own path component; a ref takes it from the referenced
// UpstreamDefinition's BasePath field instead) — so ValidateModelFailoverAggregateMembersHaveNoBasePath
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
// pkg/xds already imports pkg/config for ParseModelFailoverParams). ValidateModelFailoverPolicy
// only needs to know whether a retry policy is configured at all, not its resolved values.
func effectiveResilienceRetry(r *api.Resilience) *api.Retry {
	if r == nil {
		return nil
	}
	return r.Retry
}

// firstModelFailoverPolicy returns the first "model-failover" policy found across the
// API-level policies followed by the operation-level policies — the exact precedence
// pkg/transform.RestAPITransformer.buildPolicyChain + parseModelFailoverPolicyParams apply
// (API-level entries evaluated first), so this reaches the identical verdict without needing
// that transformer's policy-version-resolution state (policyDefinitions/latestVersions),
// which is irrelevant to model-failover's own params.
func firstModelFailoverPolicy(apiPolicies, opPolicies *[]api.Policy) *api.Policy {
	if apiPolicies != nil {
		for i := range *apiPolicies {
			if (*apiPolicies)[i].Name == "model-failover" {
				return &(*apiPolicies)[i]
			}
		}
	}
	if opPolicies != nil {
		for i := range *opPolicies {
			if (*opPolicies)[i].Name == "model-failover" {
				return &(*opPolicies)[i]
			}
		}
	}
	return nil
}

package utils

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/storage"
	"gopkg.in/yaml.v3"
)

// modelFailoverPolicyName is the policy whose operationPolicies: attachment
// declares an LlmProxy's failover chains. Its presence is the one and only
// trigger for failover attachment/xDS generation. Kept in lockstep with
// pkg/transform's constant of the same name (the two packages cannot share one:
// pkg/transform imports pkg/utils).
const modelFailoverPolicyName = "model-failover"

type LLMProviderTransformer struct {
	store                 *storage.ConfigStore
	db                    storage.Storage
	routerConfig          *config.RouterConfig
	policyVersionResolver PolicyVersionResolver
}

// pathMethodKey represents a unique path+method combination
type pathMethodKey struct {
	path   string
	method string
}

type llmPolicyAttachment struct {
	policy    api.OperationPolicy
	pathEntry api.OperationPolicyPath
	// upstream marks an attachment that came from upstreamPolicies: the policy
	// engine runs it in the upstream-attempt phase instead of the downstream one.
	upstream bool
}

func NewLLMProviderTransformer(store *storage.ConfigStore, db storage.Storage, routerConfig *config.RouterConfig, policyVersionResolver PolicyVersionResolver) *LLMProviderTransformer {
	if db == nil {
		panic("LLMProviderTransformer requires non-nil storage")
	}

	return &LLMProviderTransformer{
		store:                 store,
		db:                    db,
		routerConfig:          routerConfig,
		policyVersionResolver: policyVersionResolver,
	}
}

// HydrateLLMConfig populates cfg.Configuration with a derived RestAPI for LlmProvider and
// LlmProxy kinds. These are stored with only SourceConfiguration set (Configuration is nil
// by design) and must be hydrated before policy derivation or xDS snapshot generation.
// For other kinds (RestApi, WebSubApi, Mcp) the function is a no-op.
func HydrateLLMConfig(cfg *models.StoredConfig, store *storage.ConfigStore, db storage.Storage, routerConfig *config.RouterConfig, policyDefinitions map[string]models.PolicyDefinition) error {
	if cfg == nil {
		return nil
	}
	if _, ok := cfg.Configuration.(api.RestAPI); ok {
		return nil
	}

	transformer := NewLLMProviderTransformer(store, db, routerConfig, NewLoadedPolicyVersionResolver(policyDefinitions))

	var restAPI api.RestAPI
	switch source := cfg.SourceConfiguration.(type) {
	case api.LLMProviderConfiguration:
		if _, err := transformer.Transform(&source, &restAPI); err != nil {
			return err
		}
	case api.LLMProxyConfiguration:
		if _, err := transformer.Transform(&source, &restAPI); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported LLM source configuration type: %T", cfg.SourceConfiguration)
	}

	cfg.Configuration = restAPI
	return nil
}

func (t *LLMProviderTransformer) Transform(input any, output *api.RestAPI) (*api.RestAPI, error) {
	switch v := input.(type) {
	case *api.LLMProviderConfiguration:
		return t.transformProvider(v, output)
	case *api.LLMProxyConfiguration:
		return t.transformProxy(v, output)
	default:
		return nil, fmt.Errorf("invalid input type: expected *api.LLMProviderConfiguration or *api.LLMProxyConfiguration")
	}
}

func (t *LLMProviderTransformer) resolvePolicyVersion(name string) (string, error) {
	if t.policyVersionResolver == nil {
		return "", &PolicyDefinitionMissingError{PolicyName: name}
	}
	return t.policyVersionResolver.Resolve(name)
}

// resolvePolicyVersionOverride errors if an optional caller-requested override
// doesn't match the one version of name actually loaded in this gateway image.
func (t *LLMProviderTransformer) resolvePolicyVersionOverride(name string, override *string) (string, error) {
	resolved, err := t.resolvePolicyVersion(name)
	if err != nil {
		return "", err
	}
	if override == nil {
		return resolved, nil
	}
	trimmed := strings.TrimSpace(*override)
	if trimmed == "" || trimmed == resolved {
		return resolved, nil
	}
	return "", fmt.Errorf("policy '%s' version '%s' was requested, but this gateway build only has '%s' loaded", name, trimmed, resolved)
}

func (t *LLMProviderTransformer) getTemplateByHandle(handle string) (*models.StoredLLMProviderTemplate, error) {
	return t.db.GetLLMProviderTemplateByHandle(handle)
}

func (t *LLMProviderTransformer) transformProxy(proxy *api.LLMProxyConfiguration,
	output *api.RestAPI) (*api.RestAPI, error) {

	// Step 1: Retrieve and validate provider reference
	provider, err := t.db.GetConfigByKindAndHandle(string(api.LLMProviderConfigurationKindLlmProvider), proxy.Spec.Provider.Id)
	if err != nil {
		return nil, fmt.Errorf("failed to look up provider '%s': %w", proxy.Spec.Provider.Id, err)
	}
	if provider == nil {
		return nil, fmt.Errorf("failed to retrieve provider by id '%s'", proxy.Spec.Provider.Id)
	}

	// Step 1.5: Get provider's template and extract template params
	providerConfig, ok := provider.SourceConfiguration.(api.LLMProviderConfiguration)
	if !ok {
		return nil, fmt.Errorf("provider source configuration is not LLMProviderConfiguration")
	}

	tmpl, err := t.getTemplateByHandle(providerConfig.Spec.Template)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve template '%s' from provider: %w", providerConfig.Spec.Template, err)
	}

	// Step 2: Configure API metadata and basic spec
	output.Kind = api.RestAPIKindRestApi
	output.ApiVersion = api.RestAPIApiVersionGatewayApiPlatformWso2Comv1
	output.Metadata = proxy.Metadata

	spec := api.APIConfigData{}
	spec.DisplayName = proxy.Spec.DisplayName
	spec.Version = proxy.Spec.Version
	spec.Context = constants.BASE_PATH
	if proxy.Spec.Context != nil {
		spec.Context = *proxy.Spec.Context
	}

	// Step 3: Map the referenced local provider as an upstream in the transformed API config
	// Always use HTTP for internal loopback routing (proxy to provider) since:
	// 1. Traffic stays on localhost and never leaves the machine
	// 2. TLS adds unnecessary overhead for internal routing
	// 3. Self-signed listener certificates can cause TLS verification failures
	providerContext, err := provider.GetContext()
	if err != nil {
		return nil, fmt.Errorf("failed to get provider context: %w", err)
	}
	upstream := fmt.Sprintf("%s://%s:%d%s",
		constants.SchemeHTTP, constants.LocalhostIP, t.routerConfig.ListenerPort, providerContext)
	spec.Upstream.Main = api.Upstream{
		Url: &upstream,
	}

	// valuePrefix of each additional provider's downstream api-key-auth, keyed by
	// provider id, captured while resolving them below and reused when attaching the
	// per-provider loopback upstream auth (Step 3.5).
	additionalValuePrefixByID := map[string]string{}

	// Step 3.1: Resolve additional providers (multi-provider proxies). Each is
	// exposed as a named UpstreamDefinition so policies can route to it via
	// the loopback context. The primary provider above remains the default.
	if proxy.Spec.AdditionalProviders != nil && len(*proxy.Spec.AdditionalProviders) > 0 {
		seen := map[string]bool{proxy.Spec.Provider.Id: true}
		var defs []api.UpstreamDefinition
		for _, ap := range *proxy.Spec.AdditionalProviders {
			if ap.Id == "" {
				return nil, fmt.Errorf("additionalProviders entry must have a non-empty id")
			}
			name := ap.Id
			if ap.As != nil && *ap.As != "" {
				name = *ap.As
			}
			if seen[name] {
				return nil, fmt.Errorf("duplicate upstream name '%s' in additionalProviders (must be unique within the proxy and not collide with the primary provider id)", name)
			}
			seen[name] = true

			addCfg, err := t.db.GetConfigByKindAndHandle(string(api.LLMProviderConfigurationKindLlmProvider), ap.Id)
			if err != nil {
				return nil, fmt.Errorf("failed to look up additional provider '%s': %w", ap.Id, err)
			}
			if addCfg == nil {
				return nil, fmt.Errorf("additional provider '%s' not found", ap.Id)
			}
			addCtx, err := addCfg.GetContext()
			if err != nil {
				return nil, fmt.Errorf("failed to get context for additional provider '%s': %w", ap.Id, err)
			}
			addProviderConfig, ok := addCfg.SourceConfiguration.(api.LLMProviderConfiguration)
			if !ok {
				return nil, fmt.Errorf("additional provider '%s' source configuration is not LLMProviderConfiguration", ap.Id)
			}
			additionalValuePrefixByID[ap.Id] = apiKeyAuthValuePrefix(addProviderConfig.Spec.GlobalPolicies)
			// UpstreamDefinition URLs are host[:port] only. Keep the provider's
			// loopback context in BasePath so the router rewrites requests to the
			// additional provider route instead of dropping the context.
			normalizedAddCtx := strings.TrimRight(addCtx, "/")
			if normalizedAddCtx != "" && !strings.HasPrefix(normalizedAddCtx, "/") {
				normalizedAddCtx = "/" + normalizedAddCtx
			}
			addURL := fmt.Sprintf("%s://%s:%d",
				constants.SchemeHTTP, constants.LocalhostIP, t.routerConfig.ListenerPort)
			def := api.UpstreamDefinition{
				Name: name,
				Upstreams: []struct {
					Url    string `json:"url" yaml:"url"`
					Weight *int   `json:"weight,omitempty" yaml:"weight,omitempty"`
				}{{Url: addURL}},
			}
			if normalizedAddCtx != "" && normalizedAddCtx != constants.BASE_PATH {
				def.BasePath = &normalizedAddCtx
			}
			defs = append(defs, def)
		}
		spec.UpstreamDefinitions = &defs
	}

	// If provider has vhost configured add a host adding policy
	if providerConfig.Spec.Vhost != nil && *providerConfig.Spec.Vhost != "" {
		providerVhost := *providerConfig.Spec.Vhost
		// Add host header adding policy at API level
		hParams, err := GetHostAdditionPolicyParams(providerVhost)
		if err != nil {
			return nil, fmt.Errorf("failed to build host addition policy params: %w", err)
		}
		policyVersion, err := t.resolvePolicyVersion(constants.PROXY_HOST__HEADER_POLICY_NAME)
		if err != nil {
			return nil, err
		}

		hh := api.Policy{
			Name:    constants.PROXY_HOST__HEADER_POLICY_NAME,
			Version: policyVersion, Params: &hParams}
		spec.Policies = &[]api.Policy{hh}

		// Update spec upstream hostRewrite to Manual
		hostRewrite := api.Manual
		spec.Upstream.Main.HostRewrite = &hostRewrite
	}

	// Set proxy-specific vhost if provided
	if proxy.Spec.Vhost != nil {
		spec.Vhosts = &struct {
			Main    string  `json:"main" yaml:"main"`
			Sandbox *string `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
		}{
			Main: *proxy.Spec.Vhost,
		}
	}

	// Step 3.4b: determine, before building any downstream provider-scoped
	// policy, which providers a model-failover chain references (if one is
	// attached at all). Moved ahead of Step 3.5 so it can be consulted there:
	// a provider a failover chain references must NOT also get a downstream
	// attachment (see Step 3.5's skip below for why).
	opLevelPolicies := collectOperationLevelLLMPolicies(proxy.Spec.OperationPolicies, proxy.Spec.Policies)
	upstreamPolicies := derefOperationPolicies(proxy.Spec.UpstreamPolicies)
	modelFailoverAttachments := operationPoliciesNamed(opLevelPolicies, modelFailoverPolicyName)
	var failoverReferencedProviders []string
	failoverReferencedProviderSet := map[string]bool{}
	if len(modelFailoverAttachments) > 0 {
		var err error
		failoverReferencedProviders, err = modelFailoverReferencedProviders(modelFailoverAttachments, proxy.Spec.Provider.Id)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", modelFailoverPolicyName, err)
		}
		for _, id := range failoverReferencedProviders {
			failoverReferencedProviderSet[id] = true
		}
	}

	// Step 3.5: Apply proxy-level provider auth for proxy->provider loopback upstream
	// and inline translators declared per additional provider. Both are attached as
	// conditional policies so they run only when their provider is selected downstream
	// (the model-round-robin/single-provider path - ExecutionCondition is never
	// evaluated for the upstream-attempt phase, see below) — EXCEPT for a provider a
	// model-failover chain references: that provider gets ONLY the upstream-attempt
	// instance Step 3.6 synthesizes (see the skip below for why).
	var upstreamAuthPolicies []api.Policy
	var transformerPolicies []api.Policy
	// providerAuthPolicyByID/providerTransformerPolicyByID capture the built
	// provider-scoped policies by provider identity, so Step 3.6 can re-attach
	// the exact same content as an upstream-attempt instance instead of
	// constructing it a second, divergent way. Populated unconditionally — even
	// for a provider skipped below — since Step 3.6 always needs the content.
	providerAuthPolicyByID := map[string]api.Policy{}
	providerTransformerPolicyByID := map[string]api.Policy{}
	if proxy.Spec.Provider.Auth != nil {
		pol, err := t.proxyUpstreamAuthPolicy(proxy.Spec.Provider.Auth, apiKeyAuthValuePrefix(providerConfig.Spec.GlobalPolicies), proxy.Spec.Provider.Id, "provider.auth")
		if err != nil {
			return nil, err
		}
		// "other"/"none" auth yield no policy - nothing to attach.
		if pol != nil {
			condition := selectedProviderExecutionCondition(proxy.Spec.Provider.Id, true)
			pol.ExecutionCondition = &condition
			providerAuthPolicyByID[proxy.Spec.Provider.Id] = *pol
			// A provider a model-failover chain references gets its credential
			// attached ONLY via Step 3.6's upstream-attempt instance, which
			// already covers every attempt including the first (Envoy's
			// upstream ext_proc phase runs on attempt 1 too, not just
			// retries). Attaching it here as well would inject it
			// unconditionally, before model-failover's own routing decision
			// even runs — and if a later attempt resolves to a DIFFERENT
			// provider whose credential uses a different header name, nothing
			// removes this one: it would leak alongside the correct one.
			if !failoverReferencedProviderSet[proxy.Spec.Provider.Id] {
				upstreamAuthPolicies = append(upstreamAuthPolicies, *pol)
			}
		}
	}
	if proxy.Spec.AdditionalProviders != nil {
		for _, ap := range *proxy.Spec.AdditionalProviders {
			name := ap.Id
			if ap.As != nil && *ap.As != "" {
				name = *ap.As
			}

			if ap.Auth != nil {
				pol, err := t.proxyUpstreamAuthPolicy(ap.Auth, additionalValuePrefixByID[ap.Id], name, fmt.Sprintf("additionalProviders[%s].auth", name))
				if err != nil {
					return nil, err
				}
				// "other"/"none" auth yield no policy - nothing to attach.
				if pol != nil {
					condition := selectedProviderExecutionCondition(name, false)
					pol.ExecutionCondition = &condition
					providerAuthPolicyByID[name] = *pol
					// See the primary-provider skip above for why.
					if !failoverReferencedProviderSet[name] {
						upstreamAuthPolicies = append(upstreamAuthPolicies, *pol)
					}
				}
			}

			if ap.Transformer != nil {
				pol, err := t.proxyTransformerPolicy(ap.Transformer, name, fmt.Sprintf("additionalProviders[%s].transformer", name))
				if err != nil {
					return nil, err
				}
				providerTransformerPolicyByID[name] = *pol
				if !failoverReferencedProviderSet[name] {
					transformerPolicies = append(transformerPolicies, *pol)
				}
			}
		}
	}

	// Step 3.6: model-failover policy attachments. A model-failover entry under
	// operationPolicies: is the one and only way a proxy declares failover
	// chains. When one is present the controller synthesizes two further
	// attachments:
	//
	//  a) a second, unconditioned instance of model-failover itself under
	//     upstreamPolicies:, carrying the exact same params — it is what resolves
	//     each attempt's chain position and seeds selected_provider/selected_model
	//     into the shared request metadata during the upstream-attempt phase.
	//  b) one upstream-attempt instance of every provider-scoped
	//     credential/transform policy, per provider the chain references, each
	//     gated by the same selectedProviderExecutionCondition CEL expression
	//     that already gates its downstream counterpart — except a referenced
	//     provider now has NO downstream counterpart (Step 3.5 skipped it), so
	//     this is that provider's ONLY credential/transform attachment on this
	//     route. The policies themselves need no code changes — only the
	//     attachment point and the condition differ (design doc §8).
	//
	// (a) is attached by appending to upstreamPolicies below, so it flows through
	// the ordinary Phase 2 attachment loop and therefore lands ahead of every
	// provider-scoped attachment appended in Phase 3 — the ordering the metadata
	// hand-off requires (design doc §9).
	//
	// opLevelPolicies/upstreamPolicies/modelFailoverAttachments/
	// failoverReferencedProviders were all resolved in Step 3.4b above, before
	// Step 3.5 needed to consult failoverReferencedProviderSet.
	var modelFailoverProviderPolicies []api.Policy
	if len(modelFailoverAttachments) > 0 {
		if len(operationPoliciesNamed(upstreamPolicies, modelFailoverPolicyName)) == 0 {
			// Copy rather than append in place: derefOperationPolicies hands back
			// the caller's own slice, whose spare capacity is not ours to write.
			combined := make([]api.OperationPolicy, 0, len(upstreamPolicies)+len(modelFailoverAttachments))
			combined = append(combined, upstreamPolicies...)
			combined = append(combined, modelFailoverAttachments...)
			upstreamPolicies = combined
		}

		for _, providerID := range failoverReferencedProviders {
			condition := selectedProviderExecutionCondition(providerID, false)
			// Translators must run before upstream auth so the request is
			// rewritten into the selected provider's shape before its key is
			// added — the same order Phase 3 applies downstream.
			for _, base := range providerScopedPoliciesFor(providerID, providerTransformerPolicyByID, providerAuthPolicyByID) {
				instance := base
				instance.Params = copyPolicyParams(base.Params)
				instance.Upstream = upstreamFlag(true)
				instance.ExecutionCondition = &condition
				modelFailoverProviderPolicies = append(modelFailoverProviderPolicies, instance)
			}
		}
	}

	// Step 4: Build operations (AllowAll mode without exceptions)
	// This follows the same pattern as transformProvider AllowAll mode but simplified
	var ops []api.Operation

	// Phase 1: Create Catch-All Base Operations
	// In proxy mode, we always allow all requests (no access control)
	operationRegistry := make(map[pathMethodKey]*api.Operation)
	for _, method := range constants.WILDCARD_HTTP_METHODS {
		op := &api.Operation{
			Path:   api.Ptr(constants.BASE_PATH + constants.WILD_CARD),
			Method: api.Ptr(api.OperationMethod(method)),
		}
		operationRegistry[pathMethodKey{path: op.EffectivePath(), method: method}] = op
	}

	// Phase 2: Process User-Defined Policies (operationPolicies + deprecated policies).
	// opLevelPolicies/upstreamPolicies were resolved in Step 3.6 above, which may have
	// appended the synthesized model-failover upstream attachment to the latter.
	if len(opLevelPolicies) > 0 || len(upstreamPolicies) > 0 {
		registerExplicitLLMPolicyOperations(operationRegistry, append(append([]api.OperationPolicy{}, opLevelPolicies...), upstreamPolicies...), nil)

		for _, attachment := range orderedLLMPolicyAttachments(opLevelPolicies, upstreamPolicies) {
			policyMethods := expandLLMPolicyMethods(attachment.pathEntry.Methods)

			for _, policyMethod := range policyMethods {
				attachedPolicyPaths := make(map[string]bool)
				methodOperations := getOperationsForMethod(operationRegistry, policyMethod)

				for _, op := range methodOperations {
					// Use pathsMatch to determine if policy applies to this operation
					if pathsMatch(op.EffectivePath(), attachment.pathEntry.Path) {
						for _, targetPath := range expandPolicyTargetPaths(op.EffectivePath(), &tmpl.Configuration.Spec) {
							if attachedPolicyPaths[targetPath] {
								continue
							}
							if moreSpecificPolicyAttachmentCovers(targetPath, policyMethod, attachment) {
								continue
							}
							targetKey := pathMethodKey{path: targetPath, method: policyMethod}
							targetOp, exists := operationRegistry[targetKey]
							if !exists {
								targetOp = &api.Operation{
									Path:   api.Ptr(targetPath),
									Method: api.Ptr(api.OperationMethod(policyMethod)),
								}
								operationRegistry[targetKey] = targetOp
							}

							templateParams, err := buildTemplateParams(tmpl, targetPath)
							if err != nil {
								return nil, fmt.Errorf("failed to build template params: %w", err)
							}
							pol := api.Policy{
								Name:               attachment.policy.Name,
								Version:            attachment.policy.Version,
								ExecutionCondition: attachment.policy.ExecutionCondition,
								Upstream:           upstreamFlag(attachment.upstream),
								Params:             mergeParams(attachment.pathEntry.Params, templateParams),
							}
							appendOperationPolicy(targetOp, pol)
							attachedPolicyPaths[targetPath] = true
						}
					}
				}
			}
		}
	}

	// Phase 3: Sort and Finalize Operations
	for _, op := range operationRegistry {
		ops = append(ops, *op)
	}
	ops = sortOperationsBySpecificity(ops)
	// Translators must run before upstream auth so the request is rewritten into
	// the selected provider's shape before the upstream key is added.
	if len(transformerPolicies) > 0 {
		for i := range ops {
			for _, transformerPolicy := range transformerPolicies {
				appendOperationPolicy(&ops[i], transformerPolicy)
			}
		}
	}
	if len(upstreamAuthPolicies) > 0 {
		for i := range ops {
			for _, upstreamAuthPolicy := range upstreamAuthPolicies {
				appendOperationPolicy(&ops[i], upstreamAuthPolicy)
			}
		}
	}
	// The provider-scoped upstream-attempt instances the model-failover
	// attachment synthesized (Step 3.6b). Appended after the downstream
	// attachments above and after Phase 2 — which already emitted
	// model-failover's own upstream instance — so the metadata that instance
	// seeds exists before any of these conditions is evaluated.
	if len(modelFailoverProviderPolicies) > 0 {
		for i := range ops {
			for _, modelFailoverProviderPolicy := range modelFailoverProviderPolicies {
				appendOperationPolicy(&ops[i], modelFailoverProviderPolicy)
			}
		}
	}
	// Phase 4: Attach loopback marker policy to all operations so the gateway can identify
	loopbackMarkerPolicy, err := t.proxyInternalLoopbackMarkerPolicy()
	if err != nil {
		return nil, err
	}
	for i := range ops {
		appendOperationPolicy(&ops[i], *loopbackMarkerPolicy)
	}
	// A proxy is always allow-all with no access control, so there are no deny routes:
	// attach API-level resilience to all generated routes.
	applyResilienceToTrafficRoutes(ops, config.ToBaseResilience(proxy.Spec.Resilience), nil)
	spec.Operations = ops

	// Global (api-level) policies: route into the derived RestAPI's spec.Policies so they are
	// applied across ALL operations as one shared scope, evaluated before operation-level policies.
	// Append because the proxy may already hold an api-level host-header policy (see Step 3).
	if proxy.Spec.GlobalPolicies != nil && len(*proxy.Spec.GlobalPolicies) > 0 {
		gp := make([]api.Policy, len(*proxy.Spec.GlobalPolicies))
		copy(gp, *proxy.Spec.GlobalPolicies)
		if spec.Policies == nil {
			spec.Policies = &gp
		} else {
			merged := append(*spec.Policies, gp...)
			spec.Policies = &merged
		}
	}

	output.Spec = spec
	return output, nil
}

func (t *LLMProviderTransformer) transformProvider(provider *api.LLMProviderConfiguration,
	output *api.RestAPI) (*api.RestAPI, error) {
	// @TODO: Step 1) Configure token based rate-limiting policy based on template configs
	// Retrieve and validate template
	tmpl, err := t.getTemplateByHandle(provider.Spec.Template)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve template '%s': %w", provider.Spec.Template, err)
	}

	output.Kind = api.RestAPIKindRestApi
	output.ApiVersion = api.RestAPIApiVersionGatewayApiPlatformWso2Comv1
	output.Metadata = provider.Metadata

	spec := api.APIConfigData{}
	spec.DisplayName = provider.Spec.DisplayName
	spec.Version = provider.Spec.Version
	spec.Context = constants.BASE_PATH
	if provider.Spec.Context != nil {
		spec.Context = *provider.Spec.Context
	}

	// Step 2) Upstreams: map provider upstream (direct url or upstreamDefinition ref) and vhost
	// to the API main upstream. When a ref is used, carry the upstreamDefinitions through so the
	// per-upstream connect timeout resolves the same way it does for RestApi.
	if provider.Spec.Upstream.Ref != nil && strings.TrimSpace(*provider.Spec.Upstream.Ref) != "" {
		spec.Upstream.Main = api.Upstream{
			Ref: provider.Spec.Upstream.Ref,
		}
	} else {
		spec.Upstream.Main = api.Upstream{
			Url: provider.Spec.Upstream.Url,
		}
	}
	spec.UpstreamDefinitions = provider.Spec.UpstreamDefinitions
	if provider.Spec.Vhost != nil {
		spec.Vhosts = &struct {
			Main    string  `json:"main" yaml:"main"`
			Sandbox *string `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
		}{
			Main: *provider.Spec.Vhost,
		}
	}

	// Step 3) Map upstream auth to corresponding api policy
	upstream := provider.Spec.Upstream
	var upstreamAuthPolicy *api.Policy
	if upstream.Auth != nil {
		auth := upstream.Auth
		switch auth.Type {
		case api.LLMProviderConfigDataUpstreamAuthTypeApiKey:
			pol, err := buildUpstreamAuthPolicy(string(auth.Type), "upstream.auth",
				auth.PolicyName, auth.PolicyVersion, auth.PolicyParams,
				constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME,
				func() (map[string]interface{}, error) {
					if auth.Header == nil || *auth.Header == "" {
						return nil, fmt.Errorf("upstream.auth.header is required")
					}
					if auth.Value == nil || *auth.Value == "" {
						return nil, fmt.Errorf("upstream.auth.value is required")
					}
					return GetUpstreamAuthApikeyPolicyParams(*auth.Header, *auth.Value)
				},
				t.resolvePolicyVersionOverride,
			)
			if err != nil {
				return nil, err
			}
			upstreamAuthPolicy = pol
		case api.LLMProviderConfigDataUpstreamAuthTypeOauth2:
			// No typed-field fallback for oauth2 - policyParams is always required.
			pol, err := buildUpstreamAuthPolicy(string(auth.Type), "upstream.auth",
				auth.PolicyName, auth.PolicyVersion, auth.PolicyParams,
				constants.UPSTREAM_AUTH_OAUTH2_POLICY_NAME, nil, t.resolvePolicyVersionOverride)
			if err != nil {
				return nil, err
			}
			upstreamAuthPolicy = pol
		case api.LLMProviderConfigDataUpstreamAuthTypeOther:
			// No default policy name (policyName is required) and no typed-field
			// fallback (policyParams is always required).
			pol, err := buildUpstreamAuthPolicy(string(auth.Type), "upstream.auth",
				auth.PolicyName, auth.PolicyVersion, auth.PolicyParams,
				"", nil, t.resolvePolicyVersionOverride)
			if err != nil {
				return nil, err
			}
			upstreamAuthPolicy = pol
		case api.LLMProviderConfigDataUpstreamAuthTypeNone:
			// No auth policy attached; auth (if any) is handled by user-attached
			// policies elsewhere in the chain.
		default:
			return nil, fmt.Errorf("unsupported upstream auth type: %s", auth.Type)
		}
	}

	// Step 4) Apply access control
	mode := provider.Spec.AccessControl.Mode
	var exceptions []api.RouteException
	if provider.Spec.AccessControl.Exceptions != nil {
		exceptions = *provider.Spec.AccessControl.Exceptions
	}

	var ops []api.Operation

	// denyOpKeys tracks the routes created purely to deny traffic (the access-control
	// exception routes, which carry the 404 respond policy). API-level resilience is NOT
	// attached to these (they never reach an upstream). Populated in allow_all mode; empty
	// in deny_all (where every created route is an allowed/forwarding route).
	denyOpKeys := make(map[pathMethodKey]bool)

	switch mode {
	case api.AllowAll:
		var denyPolicyVersion string
		if len(exceptions) > 0 {
			var err error
			denyPolicyVersion, err = t.resolvePolicyVersion(constants.ACCESS_CONTROL_DENY_POLICY_NAME)
			if err != nil {
				return nil, err
			}
		}

		// Phase 1: Create Catch-All Base Operations
		operationRegistry := make(map[pathMethodKey]*api.Operation)
		for _, method := range constants.WILDCARD_HTTP_METHODS {
			op := &api.Operation{
				Path:   api.Ptr(constants.BASE_PATH + constants.WILD_CARD),
				Method: api.Ptr(api.OperationMethod(method)),
			}
			operationRegistry[pathMethodKey{path: op.EffectivePath(), method: method}] = op
		}

		// Phase 2: Normalize and Process Access Control Exceptions (Deny List)
		deniedPathMethods := make(map[pathMethodKey]bool)

		for _, ex := range exceptions {
			var methods []string
			// Expand wildcard methods
			if len(ex.Methods) == 1 && string(ex.Methods[0]) == "*" {
				methods = constants.WILDCARD_HTTP_METHODS
			} else {
				methods = make([]string, len(ex.Methods))
				for i, m := range ex.Methods {
					methods[i] = string(m)
				}
			}

			for _, method := range methods {
				key := pathMethodKey{path: ex.Path, method: method}
				deniedPathMethods[key] = true
				denyOpKeys[key] = true

				// Check if operation exists
				if _, exists := operationRegistry[key]; !exists {
					// Create operation for this specific denied path
					op := &api.Operation{
						Path:   api.Ptr(ex.Path),
						Method: api.Ptr(api.OperationMethod(method)),
					}
					operationRegistry[key] = op
				}

				// Attach deny policy to this operation
				var policyParams map[string]interface{}
				if err := yaml.Unmarshal([]byte(constants.ACCESS_CONTROL_DENY_POLICY_PARAMS), &policyParams); err != nil {
					return nil, err
				}
				denyPolicy := api.Policy{
					Name:    constants.ACCESS_CONTROL_DENY_POLICY_NAME,
					Version: denyPolicyVersion,
					Params:  &policyParams,
				}
				op := operationRegistry[key]
				if op.Policies == nil {
					op.Policies = &[]api.Policy{denyPolicy}
				} else {
					existing := *op.Policies
					existing = append(existing, denyPolicy)
					op.Policies = &existing
				}
			}
		}

		// Phase 3: Process User-Defined Policies (operationPolicies + deprecated policies)
		opLevelPolicies := collectOperationLevelLLMPolicies(provider.Spec.OperationPolicies, provider.Spec.Policies)
		upstreamPolicies := derefOperationPolicies(provider.Spec.UpstreamPolicies)
		if len(opLevelPolicies) > 0 || len(upstreamPolicies) > 0 {
			registerExplicitLLMPolicyOperations(operationRegistry, append(append([]api.OperationPolicy{}, opLevelPolicies...), upstreamPolicies...), func(path, method string) bool {
				return !isDeniedByException(path, method, deniedPathMethods)
			})

			for _, attachment := range orderedLLMPolicyAttachments(opLevelPolicies, upstreamPolicies) {
				policyMethods := expandLLMPolicyMethods(attachment.pathEntry.Methods)

				for _, policyMethod := range policyMethods {
					attachedPolicyPaths := make(map[string]bool)
					methodOperations := getOperationsForMethod(operationRegistry, policyMethod)

					// CRITICAL: Skip if this path+method is denied by exception
					if isDeniedByException(attachment.pathEntry.Path, policyMethod, deniedPathMethods) {
						continue // Exception deny policy takes precedence
					}

					for _, op := range methodOperations {
						// Skip if this operation has deny policy (from exceptions)
						if denyPolicyVersion != "" && hasDenyPolicy(op, denyPolicyVersion) {
							continue
						}

						if pathsMatch(op.EffectivePath(), attachment.pathEntry.Path) {
							for _, targetPath := range expandPolicyTargetPaths(op.EffectivePath(), &tmpl.Configuration.Spec) {
								if attachedPolicyPaths[targetPath] {
									continue
								}
								if moreSpecificPolicyAttachmentCovers(targetPath, policyMethod, attachment) {
									continue
								}
								targetKey := pathMethodKey{path: targetPath, method: policyMethod}
								targetOp, exists := operationRegistry[targetKey]
								if !exists {
									targetOp = &api.Operation{
										Path:   api.Ptr(targetPath),
										Method: api.Ptr(api.OperationMethod(policyMethod)),
									}
									operationRegistry[targetKey] = targetOp
								}

								if denyAppliesToTarget(targetPath, policyMethod, denyPolicyVersion, operationRegistry) {
									continue
								}

								templateParams, err := buildTemplateParams(tmpl, targetPath)
								if err != nil {
									return nil, fmt.Errorf("failed to build template params: %w", err)
								}
								pol := api.Policy{
									Name:               attachment.policy.Name,
									Version:            attachment.policy.Version,
									ExecutionCondition: attachment.policy.ExecutionCondition,
									Upstream:           upstreamFlag(attachment.upstream),
									Params:             mergeParams(attachment.pathEntry.Params, templateParams),
								}
								appendOperationPolicy(targetOp, pol)
								attachedPolicyPaths[targetPath] = true
							}
						}
					}
				}
			}
		}

		// Phase 4: Sort and Finalize
		for _, op := range operationRegistry {
			ops = append(ops, *op)
		}

	case api.DenyAll:
		// Phase 1: Normalize Access Control - expand wildcard methods
		normalizedExceptions := make(map[pathMethodKey]bool)

		for _, ex := range exceptions {
			var methods []string
			// Expand wildcard methods
			if len(ex.Methods) == 1 && string(ex.Methods[0]) == "*" {
				methods = constants.WILDCARD_HTTP_METHODS
			} else {
				methods = make([]string, len(ex.Methods))
				for i, m := range ex.Methods {
					methods[i] = string(m)
				}
			}

			for _, method := range methods {
				normalizedExceptions[pathMethodKey{path: ex.Path, method: method}] = true
			}
		}

		// Phase 2: Build Operation Registry - create base operations
		operationRegistry := make(map[pathMethodKey]*api.Operation)
		for key := range normalizedExceptions {
			op := &api.Operation{
				Path:   api.Ptr(key.path),
				Method: api.Ptr(api.OperationMethod(key.method)),
			}
			operationRegistry[key] = op
		}

		// Phase 3: Process Policies with Dynamic Operation Creation (operationPolicies + deprecated policies)
		opLevelPolicies := collectOperationLevelLLMPolicies(provider.Spec.OperationPolicies, provider.Spec.Policies)
		upstreamPolicies := derefOperationPolicies(provider.Spec.UpstreamPolicies)
		if len(opLevelPolicies) > 0 || len(upstreamPolicies) > 0 {
			registerExplicitLLMPolicyOperations(operationRegistry, append(append([]api.OperationPolicy{}, opLevelPolicies...), upstreamPolicies...), func(path, method string) bool {
				return isAllowedByAccessControl(path, method, normalizedExceptions)
			})

			for _, attachment := range orderedLLMPolicyAttachments(opLevelPolicies, upstreamPolicies) {
				policyMethods := expandLLMPolicyMethods(attachment.pathEntry.Methods)

				for _, policyMethod := range policyMethods {
					attachedPolicyPaths := make(map[string]bool)
					methodOperations := getOperationsForMethod(operationRegistry, policyMethod)

					for _, op := range methodOperations {
						if pathsMatch(op.EffectivePath(), attachment.pathEntry.Path) {
							for _, targetPath := range expandPolicyTargetPaths(op.EffectivePath(), &tmpl.Configuration.Spec) {
								if attachedPolicyPaths[targetPath] {
									continue
								}
								if moreSpecificPolicyAttachmentCovers(targetPath, policyMethod, attachment) {
									continue
								}
								targetKey := pathMethodKey{path: targetPath, method: policyMethod}
								targetOp, exists := operationRegistry[targetKey]
								if !exists {
									targetOp = &api.Operation{
										Path:   api.Ptr(targetPath),
										Method: api.Ptr(api.OperationMethod(policyMethod)),
									}
									operationRegistry[targetKey] = targetOp
								}

								templateParams, err := buildTemplateParams(tmpl, targetPath)
								if err != nil {
									return nil, fmt.Errorf("failed to build template params: %w", err)
								}
								pol := api.Policy{
									Name:               attachment.policy.Name,
									Version:            attachment.policy.Version,
									ExecutionCondition: attachment.policy.ExecutionCondition,
									Upstream:           upstreamFlag(attachment.upstream),
									Params:             mergeParams(attachment.pathEntry.Params, templateParams),
								}
								appendOperationPolicy(targetOp, pol)
								attachedPolicyPaths[targetPath] = true
							}
						}
					}
				}
			}
		}

		// Phase 4: Sort and Finalize - convert map to sorted slice
		for _, op := range operationRegistry {
			ops = append(ops, *op)
		}

	default:
		return nil, fmt.Errorf("unsupported access control mode: %s", mode)
	}

	ops = sortOperationsBySpecificity(ops)
	if upstreamAuthPolicy != nil {
		for i := range ops {
			if ops[i].Policies == nil {
				ops[i].Policies = &[]api.Policy{*upstreamAuthPolicy}
			} else {
				existing := *ops[i].Policies
				existing = append(existing, *upstreamAuthPolicy)
				ops[i].Policies = &existing
			}
		}
	}
	// Attach API-level resilience to every traffic-forwarding route, skipping the deny routes.
	applyResilienceToTrafficRoutes(ops, provider.Spec.Resilience, denyOpKeys)
	spec.Operations = ops

	// Global (api-level) policies: route into the derived RestAPI's spec.Policies so they are
	// applied across ALL operations as one shared scope, evaluated before operation-level policies.
	if provider.Spec.GlobalPolicies != nil && len(*provider.Spec.GlobalPolicies) > 0 {
		gp := make([]api.Policy, len(*provider.Spec.GlobalPolicies))
		copy(gp, *provider.Spec.GlobalPolicies)
		if spec.Policies == nil {
			spec.Policies = &gp
		} else {
			merged := append(*spec.Policies, gp...)
			spec.Policies = &merged
		}
	}

	output.Spec = spec
	return output, nil
}

// applyResilienceToTrafficRoutes attaches the API-level resilience block to every operation that
// forwards traffic upstream, skipping access-control deny routes (which only return a canned 404
// and never reach an upstream, so a timeout on them is meaningless). denyKeys identifies the deny
// routes by path+method; it is empty for deny_all and proxy (no deny routes exist there), so in
// those cases the block is applied to all routes. A nil resilience block is a no-op.
//
// Resilience is attached at the operation level (not the derived API level) on purpose: that keeps
// the deny routes on the gateway's global default timeout instead of inheriting the API-level value
// via the translator's per-field fallback.
func applyResilienceToTrafficRoutes(ops []api.Operation, resilience *api.Resilience, denyKeys map[pathMethodKey]bool) {
	if resilience == nil {
		return
	}
	for i := range ops {
		if denyKeys[pathMethodKey{path: ops[i].EffectivePath(), method: ops[i].EffectiveMethod()}] {
			continue
		}
		ops[i].Resilience = resilience
	}
}

// GetUpstreamAuthApikeyPolicyParams builds the set-headers policy params for the given
// header and value.
func GetUpstreamAuthApikeyPolicyParams(header, value string) (map[string]interface{}, error) {
	return map[string]interface{}{
		"request": map[string]interface{}{
			"headers": []interface{}{
				map[string]interface{}{
					"name":  header,
					"value": value,
				},
			},
		},
	}, nil
}

// apiKeyAuthValuePrefix returns the valuePrefix configured on a provider's downstream
// api-key-auth global policy (empty when absent). A proxy loops back into the provider's
// own context, so the credential it injects on that hop must carry the same prefix the
// provider's api-key-auth expects, otherwise the loopback request is rejected with 401.
func apiKeyAuthValuePrefix(globalPolicies *[]api.Policy) string {
	if globalPolicies == nil {
		return ""
	}
	for _, p := range *globalPolicies {
		if p.Name != constants.API_KEY_AUTH_POLICY_NAME || p.Params == nil {
			continue
		}
		if v, ok := (*p.Params)["valuePrefix"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// proxyUpstreamAuthPolicy builds the api.Policy for an LlmProxy
// provider/additionalProviders auth config. valuePrefix is the provider's own
// api-key-auth value prefix, applied the same way to the loopback credential.
// providerID is this provider's own identity (primary's Id, or an
// additionalProviders[].as/.id). It is the value the caller pairs with
// selectedProviderExecutionCondition, the CEL gate that decides whether this
// attachment runs on a given attempt: that gate compares providerID against
// the 'selected_provider' key model-failover seeds into the attempt's
// SharedContext.Metadata. For oauth2 it is additionally injected into the
// policy's own params (see below), so oauth2-generator can read back which
// provider the attempt resolved to.
func (t *LLMProviderTransformer) proxyUpstreamAuthPolicy(auth *api.LLMUpstreamAuth, valuePrefix, providerID, field string) (*api.Policy, error) {
	if auth == nil {
		return nil, nil
	}
	switch auth.Type {
	case api.LLMUpstreamAuthTypeApiKey:
		return buildUpstreamAuthPolicy(string(auth.Type), field,
			auth.PolicyName, auth.PolicyVersion, auth.PolicyParams,
			constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME,
			func() (map[string]interface{}, error) {
				if auth.Header == nil || *auth.Header == "" {
					return nil, fmt.Errorf("%s.header is required", field)
				}
				if auth.Value == nil || *auth.Value == "" {
					return nil, fmt.Errorf("%s.value is required", field)
				}
				// Loopback re-enters the provider's own api-key-auth, so match
				// its valuePrefix stripping.
				value := *auth.Value
				if valuePrefix != "" {
					value = valuePrefix + " " + value
				}
				return GetUpstreamAuthApikeyPolicyParams(*auth.Header, value)
			},
			t.resolvePolicyVersionOverride,
		)
	case api.LLMUpstreamAuthTypeOauth2:
		// No typed-field fallback for oauth2 - policyParams is always required.
		pol, err := buildUpstreamAuthPolicy(string(auth.Type), field,
			auth.PolicyName, auth.PolicyVersion, auth.PolicyParams,
			constants.UPSTREAM_AUTH_OAUTH2_POLICY_NAME, nil, t.resolvePolicyVersionOverride)
		if err != nil || pol == nil {
			return pol, err
		}
		// Injected unconditionally, mirroring proxyTransformerPolicy's own
		// providerId injection below - harmless when the proxy declares no
		// failover chains, since oauth2-generator's OnUpstreamRequestBody (the
		// only consumer of this param) is never invoked unless a request
		// actually reaches the upstream ext_proc phase on a cluster a failover
		// chain owns (its aggregate, or a member's own cluster on the
		// suspended-primary bypass).
		(*pol.Params)["providerId"] = providerID
		return pol, nil
	case api.LLMUpstreamAuthTypeOther:
		// No default policy name (policyName is required) and no typed-field
		// fallback (policyParams is always required).
		return buildUpstreamAuthPolicy(string(auth.Type), field,
			auth.PolicyName, auth.PolicyVersion, auth.PolicyParams,
			"", nil, t.resolvePolicyVersionOverride)
	case api.LLMUpstreamAuthTypeNone:
		// No auth policy attached; auth (if any) is handled by user-attached
		// policies elsewhere.
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported upstream auth type: %s", auth.Type)
	}
}

// proxyInternalLoopbackMarkerPolicy builds an unconditional set-headers policy that stamps the
// internal loopback marker header on the proxy's loopback forward to its provider. The analytics
// system policy reads it on the provider hop so the duplicate provider analytics event is dropped
// from Moesif. It carries no ExecutionCondition, so it applies for the primary and every
// additional provider; set-headers overwrites any client-supplied value.
func (t *LLMProviderTransformer) proxyInternalLoopbackMarkerPolicy() (*api.Policy, error) {
	params, err := GetUpstreamAuthApikeyPolicyParams(constants.InternalLoopbackHeader, "1")
	if err != nil {
		return nil, fmt.Errorf("failed to build internal loopback marker params: %w", err)
	}
	policyVersion, err := t.resolvePolicyVersion(constants.SET_HEADERS_POLICY_NAME)
	if err != nil {
		return nil, err
	}
	return &api.Policy{
		Name:    constants.SET_HEADERS_POLICY_NAME,
		Version: policyVersion,
		Params:  &params,
	}, nil
}

// proxyTransformerPolicy builds a translator policy for an additional provider's
// inline transformer. The provider's upstream name is passed to the translator
// as its "providerId" param so it targets the correct upstream, and gates
// execution so the translator runs only when this provider is selected.
func (t *LLMProviderTransformer) proxyTransformerPolicy(transformer *api.LLMProxyTransformer, name, field string) (*api.Policy, error) {
	if transformer == nil {
		return nil, nil
	}
	if transformer.Type == "" {
		return nil, fmt.Errorf("%s.type is required", field)
	}
	if transformer.Version == "" {
		return nil, fmt.Errorf("%s.version is required", field)
	}

	params := map[string]interface{}{}
	if transformer.Params != nil {
		for k, v := range *transformer.Params {
			params[k] = v
		}
	}
	params["providerId"] = name

	condition := selectedProviderExecutionCondition(name, false)
	return &api.Policy{
		Name:               transformer.Type,
		Version:            transformer.Version,
		Params:             &params,
		ExecutionCondition: &condition,
	}, nil
}

// operationPoliciesNamed returns every attachment of the named policy, in
// declaration order.
func operationPoliciesNamed(policies []api.OperationPolicy, name string) []api.OperationPolicy {
	var out []api.OperationPolicy
	for _, p := range policies {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

// providerScopedPoliciesFor returns the provider-scoped policies attached for
// providerID, translator first and credential second — the same relative order
// the downstream Phase 3 attachment applies, so the request is rewritten into
// the selected provider's shape before its key is added.
func providerScopedPoliciesFor(providerID string, transformers, auths map[string]api.Policy) []api.Policy {
	var out []api.Policy
	if pol, ok := transformers[providerID]; ok {
		out = append(out, pol)
	}
	if pol, ok := auths[providerID]; ok {
		out = append(out, pol)
	}
	return out
}

// copyPolicyParams deep-enough-copies a policy's params map so an
// upstream-attempt instance built from a downstream one cannot alias (and later
// mutate) the other's map.
func copyPolicyParams(params *map[string]interface{}) *map[string]interface{} {
	if params == nil {
		return nil
	}
	out := make(map[string]interface{}, len(*params))
	for k, v := range *params {
		out[k] = v
	}
	return &out
}

// modelFailoverChainMember mirrors one {model, provider} slot of the
// model-failover policy's own params shape. Only `provider` is read here — this
// package needs nothing else off a chain member, and pkg/transform owns the full
// decode (its modelFailoverTarget/modelFailoverParams) for the resolution pass.
// Keep the JSON tags in lockstep with that shape.
type modelFailoverChainMember struct {
	Provider string `json:"provider,omitempty"`
}

// modelFailoverChainParams is the subset of a model-failover attachment's params
// this package decodes: the declared chains, so their provider references can be
// collected. Keys the policy carries but this traversal doesn't need (model,
// suspendDuration, aggregateCluster) are simply ignored by the decode.
type modelFailoverChainParams struct {
	Targets []struct {
		Target    modelFailoverChainMember   `json:"target"`
		Fallbacks []modelFailoverChainMember `json:"fallbacks"`
	} `json:"targets"`
}

// modelFailoverReferencedProviders returns every provider identity referenced
// anywhere in the failover chains declared by the given model-failover
// attachments, deduplicated and in first-seen order. A member with no `provider`
// means the LlmProxy's primary provider — the same default pkg/transform's
// resolveFailoverEntry applies when it resolves the very same params.
func modelFailoverReferencedProviders(attachments []api.OperationPolicy, primaryProviderID string) ([]string, error) {
	seen := map[string]bool{}
	var ordered []string
	add := func(member modelFailoverChainMember) {
		id := strings.TrimSpace(member.Provider)
		if id == "" {
			id = primaryProviderID
		}
		if seen[id] {
			return
		}
		seen[id] = true
		ordered = append(ordered, id)
	}
	for _, attachment := range attachments {
		for _, pathEntry := range attachment.Paths {
			if pathEntry.Params == nil {
				continue
			}
			blob, err := json.Marshal(pathEntry.Params)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal params for path %q: %w", pathEntry.Path, err)
			}
			var params modelFailoverChainParams
			if err := json.Unmarshal(blob, &params); err != nil {
				return nil, fmt.Errorf("failed to parse params for path %q: %w", pathEntry.Path, err)
			}
			for _, entry := range params.Targets {
				add(entry.Target)
				for _, fb := range entry.Fallbacks {
					add(fb)
				}
			}
		}
	}
	return ordered, nil
}

func selectedProviderExecutionCondition(providerName string, includeDefault bool) string {
	selectedExpr := fmt.Sprintf("request.Metadata['selected_provider'] == '%s'", providerName)
	if includeDefault {
		return fmt.Sprintf("!('selected_provider' in request.Metadata) || %s", selectedExpr)
	}
	return fmt.Sprintf("'selected_provider' in request.Metadata && %s", selectedExpr)
}

// GetHostAdditionPolicyParams builds the host-rewrite policy params. Constructed
// structurally (not via YAML interpolation) so a host value containing a quote or newline
// cannot break or inject the params.
func GetHostAdditionPolicyParams(value string) (map[string]interface{}, error) {
	return map[string]interface{}{
		"host": value,
	}, nil
}

// buildTemplateParams extracts template parameters from the LLM provider template for the given resource path
func buildTemplateParams(template *models.StoredLLMProviderTemplate, resourcePath string) (map[string]interface{}, error) {
	if template == nil {
		return nil, fmt.Errorf("template is nil")
	}

	templateParams := make(map[string]interface{})

	spec := template.Configuration.Spec
	applyExtractionFieldsFromBaseSpec(templateParams, &spec)

	selectedMapping := selectTemplateResourceMapping(spec.ResourceMappings, resourcePath)
	if selectedMapping != nil {
		applyExtractionFieldsFromMapping(templateParams, selectedMapping)
	}

	return templateParams, nil
}

func applyExtractionFieldsFromBaseSpec(templateParams map[string]interface{}, spec *api.LLMProviderTemplateData) {
	if spec == nil {
		return
	}
	setExtractionParam(templateParams, "requestModel", spec.RequestModel)
	setExtractionParam(templateParams, "responseModel", spec.ResponseModel)
	setExtractionParam(templateParams, "promptTokens", spec.PromptTokens)
	setExtractionParam(templateParams, "completionTokens", spec.CompletionTokens)
	setExtractionParam(templateParams, "totalTokens", spec.TotalTokens)
	setExtractionParam(templateParams, "remainingTokens", spec.RemainingTokens)
}

func applyExtractionFieldsFromMapping(templateParams map[string]interface{}, mapping *api.LLMProviderTemplateResourceMapping) {
	if mapping == nil {
		return
	}
	setExtractionParam(templateParams, "requestModel", mapping.RequestModel)
	setExtractionParam(templateParams, "responseModel", mapping.ResponseModel)
	setExtractionParam(templateParams, "promptTokens", mapping.PromptTokens)
	setExtractionParam(templateParams, "completionTokens", mapping.CompletionTokens)
	setExtractionParam(templateParams, "totalTokens", mapping.TotalTokens)
	setExtractionParam(templateParams, "remainingTokens", mapping.RemainingTokens)
}

func setExtractionParam(templateParams map[string]interface{}, key string, identifier *api.ExtractionIdentifier) {
	if identifier == nil {
		return
	}
	templateParams[key] = map[string]interface{}{
		"location":   identifier.Location,
		"identifier": identifier.Identifier,
	}
}

func selectTemplateResourceMapping(mappings *api.LLMProviderTemplateResourceMappings,
	resourcePath string) *api.LLMProviderTemplateResourceMapping {
	if mappings == nil {
		return nil
	}

	var selected *api.LLMProviderTemplateResourceMapping
	if mappings.Resources == nil {
		return selected
	}

	for i := range *mappings.Resources {
		candidate := &(*mappings.Resources)[i]
		candidateResource := candidate.Resource
		if !pathsMatch(resourcePath, candidateResource) {
			continue
		}

		if selected == nil {
			selected = candidate
			continue
		}

		selectedResource := selected.Resource
		if shouldPreferTemplateResourceMapping(candidateResource, selectedResource) {
			selected = candidate
		}
	}

	return selected
}

func shouldPreferTemplateResourceMapping(candidatePath, selectedPath string) bool {
	candidateHasWildcard := strings.Contains(candidatePath, "*")
	selectedHasWildcard := strings.Contains(selectedPath, "*")

	if !candidateHasWildcard && selectedHasWildcard {
		return true
	}
	if candidateHasWildcard && !selectedHasWildcard {
		return false
	}

	return len(candidatePath) > len(selectedPath)
}

// mergeParams merges base parameters with additional parameters (deep copy to avoid mutation)
func mergeParams(base map[string]interface{}, extra map[string]interface{}) *map[string]interface{} {
	merged := make(map[string]interface{}, len(base)+len(extra))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return &merged
}

func appendOperationPolicy(op *api.Operation, pol api.Policy) {
	if op.Policies == nil {
		op.Policies = &[]api.Policy{pol}
		return
	}
	existing := *op.Policies
	existing = append(existing, pol)
	op.Policies = &existing
}

// legacyToOperationPolicy converts a deprecated LLMPolicy into an OperationPolicy.
// This is the SOLE remaining reference to the legacy LLMPolicy type; remove it (along
// with the LLMPolicy/LLMPolicyPath types) once the deprecated `policies` field is dropped.
func legacyToOperationPolicy(p api.LLMPolicy) api.OperationPolicy {
	op := api.OperationPolicy{Name: p.Name, Version: p.Version}
	for _, pe := range p.Paths {
		methods := make([]api.OperationPolicyPathMethods, 0, len(pe.Methods))
		for _, m := range pe.Methods {
			methods = append(methods, api.OperationPolicyPathMethods(m))
		}
		op.Paths = append(op.Paths, api.OperationPolicyPath{Path: pe.Path, Methods: methods, Params: pe.Params})
	}
	return op
}

// collectOperationLevelLLMPolicies merges the operation-level policies with the deprecated
// `policies` list (converted to operation policies), operationPolicies first. The deprecated
// list is treated identically to operationPolicies; setting both is discouraged.
func collectOperationLevelLLMPolicies(operationPolicies *[]api.OperationPolicy, deprecated *[]api.LLMPolicy) []api.OperationPolicy {
	var out []api.OperationPolicy
	if operationPolicies != nil {
		out = append(out, *operationPolicies...)
	}
	if deprecated != nil {
		for _, p := range *deprecated {
			out = append(out, legacyToOperationPolicy(p))
		}
	}
	return out
}

func orderedLLMPolicyAttachments(policies, upstreamPolicies []api.OperationPolicy) []llmPolicyAttachment {
	attachments := make([]llmPolicyAttachment, 0)
	for _, llmPol := range policies {
		for _, pathEntry := range llmPol.Paths {
			attachments = append(attachments, llmPolicyAttachment{policy: llmPol, pathEntry: pathEntry})
		}
	}
	for _, llmPol := range upstreamPolicies {
		for _, pathEntry := range llmPol.Paths {
			attachments = append(attachments, llmPolicyAttachment{policy: llmPol, pathEntry: pathEntry, upstream: true})
		}
	}

	sort.SliceStable(attachments, func(i, j int) bool {
		return shouldAttachPathBefore(attachments[i].pathEntry.Path, attachments[j].pathEntry.Path)
	})

	return attachments
}

func derefOperationPolicies(p *[]api.OperationPolicy) []api.OperationPolicy {
	if p == nil {
		return nil
	}
	return *p
}

func upstreamFlag(upstream bool) *bool {
	if !upstream {
		return nil
	}
	return &upstream
}

func shouldAttachPathBefore(leftPath, rightPath string) bool {
	leftHasWildcard := strings.Contains(leftPath, constants.WILD_CARD)
	rightHasWildcard := strings.Contains(rightPath, constants.WILD_CARD)

	if leftHasWildcard != rightHasWildcard {
		return leftHasWildcard
	}

	if len(leftPath) != len(rightPath) {
		return len(leftPath) < len(rightPath)
	}

	return leftPath < rightPath
}

// isMoreSpecificPath reports whether policy path a is strictly more specific than b.
// A concrete (non-wildcard) path is more specific than a wildcard one; among paths with
// the same wildcard-ness, the longer path is more specific.
func isMoreSpecificPath(a, b string) bool {
	aWildcard := strings.Contains(a, constants.WILD_CARD)
	bWildcard := strings.Contains(b, constants.WILD_CARD)
	if aWildcard != bWildcard {
		return !aWildcard
	}
	return len(a) > len(b)
}

// methodsInclude reports whether method is present in methods.
func methodsInclude(methods []string, method string) bool {
	for _, m := range methods {
		if m == method {
			return true
		}
	}
	return false
}

// moreSpecificPolicyAttachmentCovers reports whether another path entry WITHIN THE SAME policy
// block covers targetPath for the given HTTP method and is strictly more specific than the
// current entry. When true, the current (less specific) entry must not be layered onto
// targetPath, so the most specific match wins within that block. Specificity is compared by
// path first (see isMoreSpecificPath) and then, for entries on an equally specific path, by
// method (the narrower method set wins). Resolution is scoped to a single block, so separate
// policy blocks - even ones with the same name - each contribute their most specific match and
// all layer onto the route.
func moreSpecificPolicyAttachmentCovers(targetPath, method string, current llmPolicyAttachment) bool {
	for i := range current.policy.Paths {
		other := current.policy.Paths[i]
		if !pathsMatch(targetPath, other.Path) {
			continue
		}
		if !methodsInclude(expandLLMPolicyMethods(other.Methods), method) {
			continue
		}
		if isMoreSpecificAttachment(other, current.pathEntry) {
			return true
		}
	}
	return false
}

// isMoreSpecificAttachment reports whether path entry a is strictly more specific than b.
// Path specificity dominates (see isMoreSpecificPath); for entries on an equally specific
// path, the narrower HTTP method set is more specific.
func isMoreSpecificAttachment(a, b api.OperationPolicyPath) bool {
	if isMoreSpecificPath(a.Path, b.Path) {
		return true
	}
	if isMoreSpecificPath(b.Path, a.Path) {
		return false
	}
	return isStrictMethodSubset(a.Methods, b.Methods)
}

// isStrictMethodSubset reports whether the methods covered by a are a strict subset of the
// methods covered by b ('*' expands to the full supported method set). This makes e.g.
// [POST] more specific than [GET, POST], which in turn is more specific than '*'.
func isStrictMethodSubset(a, b []api.OperationPolicyPathMethods) bool {
	aSet := methodSet(a)
	bSet := methodSet(b)
	if len(aSet) == 0 || len(aSet) >= len(bSet) {
		return false
	}
	for m := range aSet {
		if !bSet[m] {
			return false
		}
	}
	return true
}

// methodSet returns the set of concrete HTTP methods an entry covers, with '*' expanded.
func methodSet(methods []api.OperationPolicyPathMethods) map[string]bool {
	set := make(map[string]bool)
	for _, m := range expandLLMPolicyMethods(methods) {
		set[m] = true
	}
	return set
}

// ensureOperation checks if an operation for the given path+method exists in the registry, and creates it if not
func ensureOperation(operationRegistry map[pathMethodKey]*api.Operation, path, method string) *api.Operation {
	key := pathMethodKey{path: path, method: method}
	if op, exists := operationRegistry[key]; exists {
		return op
	}

	op := &api.Operation{
		Path:   api.Ptr(path),
		Method: api.Ptr(api.OperationMethod(method)),
	}
	operationRegistry[key] = op
	return op
}

// expandLLMPolicyMethods takes the methods defined in an LLM policy and expands them to actual HTTP methods if wildcard is used
func expandLLMPolicyMethods(methods []api.OperationPolicyPathMethods) []string {
	if len(methods) == 1 && string(methods[0]) == constants.WILD_CARD {
		return append([]string(nil), constants.WILDCARD_HTTP_METHODS...)
	}

	expanded := make([]string, len(methods))
	for i, m := range methods {
		expanded[i] = string(m)
	}
	return expanded
}

// registerExplicitLLMPolicyOperations iterates through the explicitly defined policies in the LLM policy and ensures that operations
// exist for their paths and methods in the operation registry. The shouldRegister callback allows conditional registration based on
// path and method (e.g., to skip paths/methods denied by access control exceptions).
func registerExplicitLLMPolicyOperations(operationRegistry map[pathMethodKey]*api.Operation, policies []api.OperationPolicy,
	shouldRegister func(path, method string) bool) {
	for _, llmPol := range policies {
		for _, pathEntry := range llmPol.Paths {
			for _, method := range expandLLMPolicyMethods(pathEntry.Methods) {
				if shouldRegister != nil && !shouldRegister(pathEntry.Path, method) {
					continue
				}
				ensureOperation(operationRegistry, pathEntry.Path, method)
			}
		}
	}
}

func getOperationsForMethod(operationRegistry map[pathMethodKey]*api.Operation, method string) []*api.Operation {
	ops := make([]*api.Operation, 0)
	for key, op := range operationRegistry {
		if key.method == method {
			ops = append(ops, op)
		}
	}
	return ops
}

func expandPolicyTargetPaths(opPath string, templateSpec *api.LLMProviderTemplateData) []string {
	if !strings.Contains(opPath, constants.WILD_CARD) {
		return []string{opPath}
	}
	if templateSpec == nil || templateSpec.ResourceMappings == nil || templateSpec.ResourceMappings.Resources == nil {
		return []string{opPath}
	}

	seen := make(map[string]bool)
	expanded := make([]string, 0)
	seen[opPath] = true
	for i := range *templateSpec.ResourceMappings.Resources {
		resource := (*templateSpec.ResourceMappings.Resources)[i].Resource
		candidatePath := resource
		if !pathsMatch(candidatePath, opPath) {
			continue
		}

		selected := selectTemplateResourceMapping(templateSpec.ResourceMappings, candidatePath)
		if selected == nil {
			continue
		}

		selectedPath := selected.Resource
		if !pathsMatch(selectedPath, opPath) || seen[selectedPath] {
			continue
		}
		seen[selectedPath] = true
		expanded = append(expanded, selectedPath)
	}

	if len(expanded) == 0 {
		return []string{opPath}
	}
	expanded = append(expanded, opPath)
	return expanded
}

// isDeniedByException checks if a policy path+method is denied by access control exceptions in AllowAll mode
func isDeniedByException(policyPath, policyMethod string, deniedPathMethods map[pathMethodKey]bool) bool {
	// Check exact match first
	key := pathMethodKey{path: policyPath, method: policyMethod}
	if deniedPathMethods[key] {
		return true
	}

	// Check if any denied wildcard exception covers this policy path+method
	for deniedKey := range deniedPathMethods {
		if deniedKey.method != policyMethod {
			continue
		}

		// Check if deniedPath is wildcard that covers policyPath
		// Example: deniedPath="chat/*" covers policyPath="chat/completions"
		if strings.Contains(deniedKey.path, "*") {
			prefix := deniedKey.path[:strings.LastIndex(deniedKey.path, "*")]
			if strings.HasPrefix(policyPath, prefix) {
				return true
			}
		}
	}

	return false
}

// hasDenyPolicy checks if an operation has a deny policy attached (from access control exceptions)
func hasDenyPolicy(op *api.Operation, denyPolicyVersion string) bool {
	if op.Policies == nil {
		return false
	}
	if denyPolicyVersion == "" {
		return false
	}
	for _, pol := range *op.Policies {
		if pol.Name == constants.ACCESS_CONTROL_DENY_POLICY_NAME &&
			pol.Version == denyPolicyVersion {
			return true
		}
	}
	return false
}

// denyAppliesToTarget checks if a deny policy applies to a target path for a specific method.
// It considers both exact deny paths and wildcard deny paths present in the operation registry.
func denyAppliesToTarget(targetPath, policyMethod, denyPolicyVersion string,
	operationRegistry map[pathMethodKey]*api.Operation) bool {
	if denyPolicyVersion == "" {
		return false
	}

	for key, op := range operationRegistry {
		if key.method != policyMethod {
			continue
		}
		if !hasDenyPolicy(op, denyPolicyVersion) {
			continue
		}
		if pathsMatch(targetPath, key.path) {
			return true
		}
	}

	return false
}

// isAllowedByAccessControl checks if a policy path+method is allowed by access control exceptions
func isAllowedByAccessControl(policyPath, policyMethod string, normalizedExceptions map[pathMethodKey]bool) bool {
	// Check each exception to see if it allows this policy path+method
	for key := range normalizedExceptions {
		if key.method != policyMethod {
			continue
		}

		// Case 1: Exact match
		if key.path == policyPath {
			return true
		}

		// Case 2: Exception path is wildcard that covers policy path
		// Example: exceptionPath="chat/*" covers policyPath="chat/completions"
		if strings.Contains(key.path, "*") {
			prefix := key.path[:strings.LastIndex(key.path, "*")]
			if strings.HasPrefix(policyPath, prefix) {
				return true
			}
		}
	}

	return false
}

// pathsMatch checks if an operation path matches a policy path for policy attachment
func pathsMatch(opPath, policyPath string) bool {
	// Case 0: policyPath is root (covers any operation path)
	if policyPath == constants.BASE_PATH+constants.WILD_CARD {
		return true
	}
	// Case 1: Exact match (including same wildcard)
	if opPath == policyPath {
		return true
	}

	// Case 2: Policy has wildcard and operation path starts with policy prefix
	// Example: policyPath="chat/*", opPath="chat/completions" or "chat/completions/stream"
	if strings.Contains(policyPath, "*") {
		prefix := policyPath[:strings.LastIndex(policyPath, "*")]
		if strings.HasPrefix(opPath, prefix) {
			return true
		}
	}

	// Note: We do NOT match if operation is wildcard and policy is specific
	// (e.g., opPath="chat/*", policyPath="chat/completions")
	// This would incorrectly apply specific policies to catch-all routes

	return false
}

// sortOperationsBySpecificity sorts operations with most specific paths first
func sortOperationsBySpecificity(ops []api.Operation) []api.Operation {
	// Sort by:
	// 1. Non-wildcard paths before wildcard paths
	// 2. Longer paths before shorter paths
	// 3. Path string lexicographically
	// 4. Method alphabetically
	sorted := make([]api.Operation, len(ops))
	copy(sorted, ops)

	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if shouldSwap(sorted[i], sorted[j]) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	return sorted
}

// shouldSwap determines if two operations should be swapped in sorting
func shouldSwap(op1, op2 api.Operation) bool {
	path1HasWildcard := strings.Contains(op1.EffectivePath(), "*")
	path2HasWildcard := strings.Contains(op2.EffectivePath(), "*")

	// Non-wildcard paths come before wildcard paths
	if !path1HasWildcard && path2HasWildcard {
		return false
	}
	if path1HasWildcard && !path2HasWildcard {
		return true
	}

	// Longer paths come before shorter paths
	if len(op1.EffectivePath()) != len(op2.EffectivePath()) {
		return len(op1.EffectivePath()) < len(op2.EffectivePath())
	}

	// Lexicographic comparison for paths
	if op1.EffectivePath() != op2.EffectivePath() {
		return op1.EffectivePath() > op2.EffectivePath()
	}

	// Method alphabetically
	return op1.EffectiveMethod() > op2.EffectiveMethod()
}

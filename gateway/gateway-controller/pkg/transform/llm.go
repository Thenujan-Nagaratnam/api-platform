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

package transform

import (
	"fmt"
	"strings"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/storage"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/utils"
	policyenginev1 "github.com/wso2/api-platform/sdk/core/policyengine"
)

// LLMTransformer transforms LLM Provider or LLM Proxy StoredConfig into RuntimeDeployConfig.
// It first uses the existing LLMProviderTransformer to produce a RestAPI, then runs
// RestAPITransformer on the result, and finally enriches the metadata with LLM-specific fields.
type LLMTransformer struct {
	llmTransformer  *utils.LLMProviderTransformer
	restTransformer *RestAPITransformer
	store           *storage.ConfigStore
}

// NewLLMTransformer creates a new LLMTransformer.
func NewLLMTransformer(
	store *storage.ConfigStore,
	db storage.Storage,
	routerConfig *config.RouterConfig,
	systemConfig *config.Config,
	policyDefinitions map[string]models.PolicyDefinition,
	policyVersionResolver utils.PolicyVersionResolver,
) *LLMTransformer {
	return &LLMTransformer{
		llmTransformer:  utils.NewLLMProviderTransformer(store, db, routerConfig, policyVersionResolver),
		restTransformer: NewRestAPITransformer(routerConfig, systemConfig, policyDefinitions),
		store:           store,
	}
}

// Transform converts a StoredConfig (LLM Provider or LLM Proxy) into RuntimeDeployConfig.
func (t *LLMTransformer) Transform(cfg *models.StoredConfig) (*models.RuntimeDeployConfig, error) {
	// Step 1: Obtain the RestAPI representation.
	// If cfg.Configuration is already a RestAPI (e.g. after hydration + policy resolution),
	// use it directly so that resolved policy state is preserved.
	// Otherwise, re-derive from SourceConfiguration.
	var restAPI api.RestAPI
	if existing, ok := cfg.Configuration.(api.RestAPI); ok {
		restAPI = existing
	} else {
		var err error
		switch sc := cfg.SourceConfiguration.(type) {
		case api.LLMProviderConfiguration:
			_, err = t.llmTransformer.Transform(&sc, &restAPI)
		case api.LLMProxyConfiguration:
			_, err = t.llmTransformer.Transform(&sc, &restAPI)
		default:
			return nil, fmt.Errorf("unsupported LLM source configuration type: %T", cfg.SourceConfiguration)
		}
		if err != nil {
			return nil, fmt.Errorf("LLM transformation failed: %w", err)
		}
	}

	// Step 2: Build a temporary StoredConfig with the RestAPI result
	tempCfg := &models.StoredConfig{
		UUID:                cfg.UUID,
		Kind:                cfg.Kind,
		Handle:              cfg.Handle,
		DisplayName:         cfg.DisplayName,
		Version:             cfg.Version,
		Configuration:       restAPI,
		SourceConfiguration: cfg.SourceConfiguration,
		DesiredState:        cfg.DesiredState,
		CreatedAt:           cfg.CreatedAt,
		UpdatedAt:           cfg.UpdatedAt,
	}

	// Step 3: Use RestAPITransformer to build RuntimeDeployConfig
	rdc, err := t.restTransformer.Transform(tempCfg)
	if err != nil {
		return nil, fmt.Errorf("RestAPI transformation for LLM failed: %w", err)
	}

	// Step 4: Enrich metadata with LLM-specific fields
	rdc.Metadata.Kind = cfg.Kind // Restore original kind (LlmProvider/LlmProxy)
	llmMeta := t.extractLLMMetadata(cfg)
	if llmMeta != nil {
		rdc.Metadata.LLM = llmMeta
	}
	rdc.SensitiveValues = cfg.SensitiveValues

	// Step 5: Resolve resilience.failover (LlmProxy-only) into the generic
	// RouteFailover shape every route carries. No-op for any other kind or
	// any LlmProxy with no failover block.
	if proxy, ok := cfg.SourceConfiguration.(api.LLMProxyConfiguration); ok && proxy.Spec.Resilience != nil {
		if err := applyFailoverToRoutes(rdc, proxy.Spec.Resilience.Failover, proxy.Spec.Provider.Id); err != nil {
			return nil, fmt.Errorf("resolving resilience.failover: %w", err)
		}
	}

	return rdc, nil
}

// extractLLMMetadata extracts LLM-specific metadata from the source configuration.
func (t *LLMTransformer) extractLLMMetadata(cfg *models.StoredConfig) *models.LLMMetadata {
	meta := &models.LLMMetadata{}

	switch sc := cfg.SourceConfiguration.(type) {
	case api.LLMProviderConfiguration:
		meta.TemplateHandle = sc.Spec.Template
		meta.ProviderName = sc.Metadata.Name

	case api.LLMProxyConfiguration:
		// Get provider name and template handle from referenced provider
		providerCfg, err := t.store.GetByKindAndHandle(string(api.LLMProviderConfigurationKindLlmProvider), sc.Spec.Provider.Id)
		if err != nil || providerCfg == nil {
			return meta
		}
		if provSrc, ok := providerCfg.SourceConfiguration.(api.LLMProviderConfiguration); ok {
			meta.TemplateHandle = provSrc.Spec.Template
			meta.ProviderName = provSrc.Metadata.Name
		}
	}

	if meta.TemplateHandle == "" && meta.ProviderName == "" {
		return nil
	}
	return meta
}

// applyFailoverToRoutes resolves failover (the LLM-public {model, provider}
// shorthand) into models.RouteFailover on every route in rdc, using
// rdc.UpstreamClusters (already built by RestAPITransformer) to translate a
// named provider into a real cluster key + upstream info. A target/fallback
// with no provider uses the route's OWN already-resolved primary upstream
// (route.Upstream.ClusterKey / .Default) directly — never a name lookup —
// because the primary/sandbox slot clusters are stored with an empty Name
// (see models.UpstreamCluster.Name's doc comment), which is not a usable
// lookup key on its own. primaryProviderID is the proxy's own spec.provider.id,
// needed so an explicit `provider: <primary's own id>` also takes this same
// primary path instead of falling through to the (failing) named-cluster scan.
//
// Known v1 scope limitation, not a bug: this applies to every route the LlmProxy
// owns, including operations with no client-supplied model to match against
// (e.g. a "/models" listing endpoint) — such a route still gets retry-on-5xx and
// forced host-rewrite it never had before. Properly scoping this to only
// model-bearing operations needs per-operation request-shape awareness that
// belongs in a later plan's downstream target-selection work (which already has
// to parse the client-requested model out of the request body), not here.
func applyFailoverToRoutes(rdc *models.RuntimeDeployConfig, failover *api.LLMFailoverConfig, primaryProviderID string) error {
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
			targetEntry, err := resolveFailoverEntry(rdc, r, entry.Target, primaryProviderID)
			if err != nil {
				return fmt.Errorf("route %q: resolving failover target %q: %w", routeKey, entry.Target.Model, err)
			}
			fallbacks := make([]models.RouteFailoverEntry, 0, len(entry.Fallbacks))
			for _, fb := range entry.Fallbacks {
				fbEntry, err := resolveFailoverEntry(rdc, r, fb, primaryProviderID)
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
// cluster reference. provider == nil/empty, or provider == the proxy's own
// primaryProviderID, both mean the route's own primary upstream — a validated
// config can legally spell out `provider: <primary's own id>` explicitly
// (llm_validator.go seeds it into validUpstreamNames), and that must resolve
// exactly like omitting the field, not fall through to the named-cluster scan
// below, whose clusters are keyed by additionalProviders[].as/id and would
// never contain the primary (its cluster is stored with an empty Name — see
// models.UpstreamCluster.Name's doc comment).
func resolveFailoverEntry(rdc *models.RuntimeDeployConfig, r *models.Route, t api.LLMFailoverTarget, primaryProviderID string) (models.RouteFailoverEntry, error) {
	providerName := ""
	if t.Provider != nil {
		providerName = strings.TrimSpace(*t.Provider)
	}
	if providerName == "" || providerName == primaryProviderID {
		if r.Upstream.Default == nil {
			return models.RouteFailoverEntry{}, fmt.Errorf("route has no default upstream to use as the primary failover target")
		}
		return models.RouteFailoverEntry{
			Model:      t.Model,
			ClusterKey: r.Upstream.ClusterKey,
			Upstream:   *r.Upstream.Default,
			Provider:   primaryProviderID,
		}, nil
	}

	for key, uc := range rdc.UpstreamClusters {
		if uc.Name != providerName {
			continue
		}
		if len(uc.Endpoints) == 0 {
			return models.RouteFailoverEntry{}, fmt.Errorf("provider %q has no endpoints", providerName)
		}
		scheme := "http"
		defaultPort := 80
		if uc.TLS != nil && uc.TLS.Enabled {
			scheme = "https"
			defaultPort = 443
		}
		host := uc.Endpoints[0].Host
		hostPort := host
		if uc.Endpoints[0].Port != defaultPort {
			hostPort = fmt.Sprintf("%s:%d", host, uc.Endpoints[0].Port)
		}
		// NOTE: this re-derives the URL from uc.Endpoints[0] with its own default-port
		// omission logic, rather than reusing the URL restapi.go's addUpstreamCluster
		// already computed for this same cluster (upstreamClusterResult.URL) — that
		// value isn't persisted on models.UpstreamCluster, only returned transiently.
		// The two can disagree in spelling for a non-default port explicitly written
		// into the source URL (e.g. "https://host:443" vs "https://host"), though both
		// name the identical backend. Not fixed here: plumbing the original URL onto
		// UpstreamCluster is more invasive than this fix warrants (see Fix 5 in the
		// final-review fix report) — the wire consumer must not rely on exact string
		// equality between this URL and a same-host default_upstream.url elsewhere.
		return models.RouteFailoverEntry{
			Model:      t.Model,
			ClusterKey: key,
			Upstream: policyenginev1.UpstreamInfo{
				ClusterName: key,
				URL:         fmt.Sprintf("%s://%s", scheme, hostPort),
				BasePath:    uc.BasePath,
			},
			Provider: providerName,
		}, nil
	}
	return models.RouteFailoverEntry{}, fmt.Errorf("provider %q not found among configured upstreams", providerName)
}

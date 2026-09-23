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

	// Step 5: Resolve failover (LlmProxy-only) into the generic RouteFailover
	// shape every route carries. A model-failover policy attachment is the one
	// and only source: it resolves exactly the routes the policy is attached to.
	// No-op for any other kind, and for any LlmProxy without the attachment.
	if proxy, ok := cfg.SourceConfiguration.(api.LLMProxyConfiguration); ok {
		if err := applyModelFailoverPolicyToRoutes(rdc, llmProxyProviderIdentities(&proxy), proxy.Spec.Provider.Id); err != nil {
			return nil, fmt.Errorf("resolving %s policy: %w", modelFailoverPolicyName, err)
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

// resolveFailoverEntry resolves one chain member (as authored in a
// model-failover policy attachment's params) into a real cluster reference,
// using rdc.UpstreamClusters (already built by RestAPITransformer) to
// translate a named provider/upstream into a real cluster key + upstream
// info. This is two independent questions: identity always comes from
// t.Provider (defaulting to the primary), but the dial target defaults to
// that same name only when t.UpstreamDefinition is empty — an explicit
// UpstreamDefinition always wins, letting a member reuse one provider's
// credentials against a differently-named upstream (see modelFailoverTarget's
// own doc comment).
//
// An empty provider, or a provider equal to the proxy's own primaryProviderID,
// both mean the route's OWN already-resolved primary upstream
// (route.Upstream.ClusterKey / .Default) — never a name lookup. A config can
// legally spell out `provider: <primary's own id>` explicitly, and that must
// resolve exactly like omitting the field rather than falling through to the
// named-cluster scan below, whose clusters are keyed by
// additionalProviders[].as/id and would never contain the primary (its cluster
// is stored with an empty Name — see models.UpstreamCluster.Name's doc comment).
func resolveFailoverEntry(rdc *models.RuntimeDeployConfig, r *models.Route, t modelFailoverTarget, primaryProviderID string) (models.RouteFailoverEntry, error) {
	providerName := strings.TrimSpace(t.Provider)
	if providerName == "" {
		providerName = primaryProviderID
	}

	dialTarget := strings.TrimSpace(t.UpstreamDefinition)
	if dialTarget == "" {
		dialTarget = providerName
	}

	if dialTarget == primaryProviderID {
		if r.Upstream.Default == nil {
			return models.RouteFailoverEntry{}, fmt.Errorf("route has no default upstream to use as the primary failover target")
		}
		return models.RouteFailoverEntry{
			Model:      t.Model,
			ClusterKey: r.Upstream.ClusterKey,
			Upstream:   *r.Upstream.Default,
			Provider:   providerName,
		}, nil
	}

	for key, uc := range rdc.UpstreamClusters {
		if uc.Name != dialTarget {
			continue
		}
		if len(uc.Endpoints) == 0 {
			return models.RouteFailoverEntry{}, fmt.Errorf("upstream %q has no endpoints", dialTarget)
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
		// UpstreamCluster is more invasive than this warrants — the wire consumer must
		// not rely on exact string equality between this URL and a same-host
		// default_upstream.url elsewhere.
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
	return models.RouteFailoverEntry{}, fmt.Errorf("upstream %q not found among configured upstreams", dialTarget)
}

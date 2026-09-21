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
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/metrics"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/storage"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/utils"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/xds"
)

// modelFailoverE2EHarness builds a real SQLite-backed store/template/provider
// set, so a model-failover attachment is resolved end-to-end through the same
// LLMProviderTransformer -> RestAPITransformer chain a real deploy runs.
func modelFailoverE2EHarness(t *testing.T) (*LLMTransformer, *config.RouterConfig) {
	t.Helper()

	store := storage.NewConfigStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := newTestSQLiteStorageForFailoverE2E(t, logger)

	require.NoError(t, db.SaveLLMProviderTemplate(&models.StoredLLMProviderTemplate{
		UUID: "model-failover-attachment-template",
		Configuration: api.LLMProviderTemplate{
			ApiVersion: api.LLMProviderTemplateApiVersionGatewayApiPlatformWso2Comv1,
			Kind:       api.LLMProviderTemplateKindLlmProviderTemplate,
			Metadata:   api.Metadata{Name: "openai"},
			Spec:       api.LLMProviderTemplateData{DisplayName: "openai"},
		},
	}))

	saveProvider := func(name, context string) {
		require.NoError(t, db.SaveConfig(&models.StoredConfig{
			UUID:        name + "-uuid",
			Kind:        string(api.LLMProviderConfigurationKindLlmProvider),
			Handle:      name,
			DisplayName: name,
			Version:     "v1.0",
			SourceConfiguration: api.LLMProviderConfiguration{
				ApiVersion: api.LLMProviderConfigurationApiVersionGatewayApiPlatformWso2Comv1,
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: name},
				Spec: api.LLMProviderConfigData{
					DisplayName:   name,
					Version:       "v1.0",
					Context:       ptrStr(context),
					Template:      "openai",
					Upstream:      api.LLMProviderConfigData_Upstream{Url: ptrStr("https://example.com")},
					AccessControl: api.LLMAccessControl{Mode: api.AllowAll},
				},
			},
			DesiredState: models.StateDeployed,
		}))
	}
	saveProvider("openai-provider", "/openai-provider")
	saveProvider("anthropic-provider", "/anthropic-provider")

	routerCfg := testRouterCfg()
	routerCfg.ListenerPort = 8080

	policyVersionResolver := utils.NewStaticPolicyVersionResolver(map[string]string{
		constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME:          "v1",
		constants.UPSTREAM_AUTH_FAILOVER_APIKEY_POLICY_NAME: "v1",
	})
	policyDefinitions := map[string]models.PolicyDefinition{
		"set-headers":                {Name: constants.SET_HEADERS_POLICY_NAME, Version: "v1.0.0"},
		"llm-upstream-provider-auth": {Name: constants.UPSTREAM_AUTH_FAILOVER_APIKEY_POLICY_NAME, Version: "v1.0.0"},
		modelFailoverPolicyName:      {Name: modelFailoverPolicyName, Version: "v0.0.0"},
	}

	return NewLLMTransformer(store, db, routerCfg, &config.Config{}, policyDefinitions, policyVersionResolver), routerCfg
}

func modelFailoverProxyConfig() api.LLMProxyConfiguration {
	return api.LLMProxyConfiguration{
		ApiVersion: api.LLMProxyConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProxyConfigurationKindLlmProxy,
		Metadata:   api.Metadata{Name: "model-failover-proxy"},
		Spec: api.LLMProxyConfigData{
			DisplayName: "model-failover-proxy",
			Version:     "v1.0",
			Provider: api.LLMProxyProvider{
				Id: "openai-provider",
				Auth: &api.LLMUpstreamAuth{
					Type:   api.LLMUpstreamAuthTypeApiKey,
					Header: ptrStr("Authorization"),
					Value:  ptrStr("Bearer primary"),
				},
			},
			AdditionalProviders: &[]api.LLMProxyAdditionalProvider{{
				Id: "anthropic-provider",
				As: ptrStr("anthropic-upstream"),
				Auth: &api.LLMUpstreamAuth{
					Type:   api.LLMUpstreamAuthTypeApiKey,
					Header: ptrStr("X-Provider-Key"),
					Value:  ptrStr("anthropic-loopback"),
				},
			}},
			OperationPolicies: &[]api.OperationPolicy{{
				Name:    modelFailoverPolicyName,
				Version: "v0",
				Paths: []api.OperationPolicyPath{{
					Path:    "/chat/completions",
					Methods: []api.OperationPolicyPathMethods{"POST"},
					Params: map[string]interface{}{
						"targets": []interface{}{
							map[string]interface{}{
								"target": map[string]interface{}{"model": "gpt-4o"},
								"fallbacks": []interface{}{
									map[string]interface{}{
										"model":    "claude-sonnet-4-5-20250929",
										"provider": "anthropic-upstream",
									},
								},
							},
						},
						"suspendDuration": 900,
					},
				}},
			}},
		},
	}
}

func modelFailoverStoredConfig(proxy api.LLMProxyConfiguration) *models.StoredConfig {
	return &models.StoredConfig{
		UUID:                "model-failover-proxy-uuid",
		Kind:                string(api.LLMProxyConfigurationKindLlmProxy),
		Handle:              "model-failover-proxy",
		DisplayName:         "model-failover-proxy",
		Version:             "v1.0",
		SourceConfiguration: proxy,
		DesiredState:        models.StateDeployed,
	}
}

func chainInstancesNamed(t *testing.T, chain *models.PolicyChain, name string) []models.Policy {
	t.Helper()
	require.NotNil(t, chain)
	var out []models.Policy
	for _, p := range chain.Policies {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

// TestLLMTransformer_ModelFailoverPolicy_ResolvesFailoverAndInjectsAggregateCluster
// is the end-to-end proof that a model-failover policy attachment drives the
// full failover resolution: the route gains a resolved RouteFailover
// (which is what triggers the unchanged aggregate
// cluster / retry_policy xDS generation), and both the downstream and the
// upstream instance of the policy get the controller-assigned aggregate cluster
// name injected into their params — the single source of truth the policy
// string-compares against at request time (design §3/§5).
func TestLLMTransformer_ModelFailoverPolicy_ResolvesFailoverAndInjectsAggregateCluster(t *testing.T) {
	llmTransformer, routerCfg := modelFailoverE2EHarness(t)

	rdc, err := llmTransformer.Transform(modelFailoverStoredConfig(modelFailoverProxyConfig()))
	require.NoError(t, err)

	routeKey := "POST|/chat/completions|" + routerCfg.VHosts.Main.Default
	chatRoute, ok := rdc.Routes[routeKey]
	require.True(t, ok, "expected route %q in the transformed RuntimeDeployConfig", routeKey)

	require.NotNil(t, chatRoute.Upstream.Failover,
		"a model-failover attachment must resolve into the route's RouteFailover")
	require.Len(t, chatRoute.Upstream.Failover.Targets, 1)
	assert.Equal(t, 900, chatRoute.Upstream.Failover.SuspendDurationSeconds)
	assert.Equal(t, []string{"5xx"}, chatRoute.Upstream.Failover.RetryOn)

	target := chatRoute.Upstream.Failover.Targets[0]
	assert.Equal(t, "gpt-4o", target.Model)
	assert.Equal(t, chatRoute.Upstream.ClusterKey, target.Target.ClusterKey,
		"omitting provider on the target must resolve to the proxy's own primary upstream")
	require.Len(t, target.Fallbacks, 1)

	var anthropicClusterKey string
	for key, uc := range rdc.UpstreamClusters {
		if uc.Name == "anthropic-upstream" {
			anthropicClusterKey = key
		}
	}
	require.NotEmpty(t, anthropicClusterKey)
	assert.Equal(t, anthropicClusterKey, target.Fallbacks[0].ClusterKey,
		"the fallback's provider must resolve to the additional provider's own cluster")

	assert.True(t, chatRoute.Upstream.UseClusterHeader)
	assert.Equal(t, chatRoute.Upstream.ClusterKey, chatRoute.Upstream.DefaultCluster)

	// Both attachments of the policy carry the expanded params.
	chain := rdc.PolicyChains[rdc.EffectiveCanonicalChainKey(routeKey, chatRoute)]
	instances := chainInstancesNamed(t, chain, modelFailoverPolicyName)
	require.Len(t, instances, 2, "expected a downstream and an upstream model-failover instance")

	var sawDownstream, sawUpstream bool
	for _, instance := range instances {
		targets, ok := instance.Params["targets"].([]interface{})
		require.True(t, ok, "expanded params must still carry a targets list")
		require.Len(t, targets, 1)
		entry, ok := targets[0].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, xds.AggregateClusterName(routeKey, 0), entry["aggregateCluster"],
			"the controller must inject the aggregate cluster name it assigned this entry")
		if instance.Upstream {
			sawUpstream = true
		} else {
			sawDownstream = true
		}
	}
	assert.True(t, sawDownstream, "model-failover must stay attached downstream")
	assert.True(t, sawUpstream, "model-failover must also be attached in the upstream-attempt phase")

	// A route the policy was not attached to (the wildcard catch-all) must not
	// have gained a failover chain.
	for key, r := range rdc.Routes {
		if key == routeKey {
			continue
		}
		assert.Nil(t, r.Upstream.Failover, "route %q must not gain a failover chain", key)
	}
}

// TestLLMTransformer_ModelFailoverPolicy_UnknownProviderRejected proves the
// validation Task 7 put in parseModelFailoverParams actually runs on this path,
// so a bad provider reference fails the deploy rather than producing an
// aggregate cluster with a dangling member.
func TestLLMTransformer_ModelFailoverPolicy_UnknownProviderRejected(t *testing.T) {
	llmTransformer, _ := modelFailoverE2EHarness(t)

	proxy := modelFailoverProxyConfig()
	params := (*proxy.Spec.OperationPolicies)[0].Paths[0].Params
	params["targets"] = []interface{}{
		map[string]interface{}{
			"target": map[string]interface{}{"model": "gpt-4o"},
			"fallbacks": []interface{}{
				map[string]interface{}{"model": "claude", "provider": "not-a-configured-provider"},
			},
		},
	}

	_, err := llmTransformer.Transform(modelFailoverStoredConfig(proxy))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-configured-provider")
}

func newTestSQLiteStorageForFailoverE2E(t *testing.T, logger *slog.Logger) storage.Storage {
	t.Helper()

	metrics.Init()

	dbPath := filepath.Join(t.TempDir(), "llm_failover_e2e.db")
	db, err := storage.NewStorage(storage.BackendConfig{
		Type:       "sqlite",
		SQLitePath: dbPath,
	}, logger)
	if err != nil {
		t.Fatalf("failed to create sqlite storage: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	return db
}

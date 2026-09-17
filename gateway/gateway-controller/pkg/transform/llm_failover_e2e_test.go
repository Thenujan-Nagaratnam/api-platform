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
)

// TestLLMTransformer_FailoverBlock_FullShapeEndToEnd runs a real LlmProxy —
// primary provider "openai-provider", one additionalProvider
// "anthropic-provider" (as: "anthropic-upstream"), and a resilience.failover
// block targeting "gpt-4o" with a "claude-sonnet-4-5-20250929" fallback on
// "anthropic-upstream" — through the REAL LLMTransformer.Transform chain
// (LLMProviderTransformer -> RestAPITransformer -> applyFailoverToRoutes),
// with providers/template resolved from a real SQLite-backed store exactly
// as they would be at deploy time.
//
// This is the half of Task 7's "full shape" test that proves provider/model
// resolution end-to-end — the class of bug this plan's own Task 1 self-review
// flagged (LLMResilience accidentally sharing schema with RestApi's
// Resilience) would have surfaced here, since it would either fail this
// transform outright or resolve the fallback to the wrong/primary cluster.
// The xDS-translation half of the "full shape" test (aggregate cluster
// identity/members/order, RetryPolicy, HostRewriteSpecifier,
// VirtualHost.IncludeRequestAttemptCount) lives in
// pkg/xds/failover_e2e_test.go instead of here — see that file's doc comment
// for why this had to split into two packages (a real, verified Go import
// cycle: pkg/transform and pkg/utils already import pkg/xds in production
// code, so a pkg/xds test file cannot import pkg/transform to reach this
// same real transform chain). Approved deviation from the task brief, which
// named a single file/chain; recorded here and in the task report.
func TestLLMTransformer_FailoverBlock_FullShapeEndToEnd(t *testing.T) {
	store := storage.NewConfigStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := newTestSQLiteStorageForFailoverE2E(t, logger)

	template := &models.StoredLLMProviderTemplate{
		UUID: "llm-failover-e2e-template",
		Configuration: api.LLMProviderTemplate{
			ApiVersion: api.LLMProviderTemplateApiVersionGatewayApiPlatformWso2Comv1,
			Kind:       api.LLMProviderTemplateKindLlmProviderTemplate,
			Metadata:   api.Metadata{Name: "openai"},
			Spec:       api.LLMProviderTemplateData{DisplayName: "openai"},
		},
	}
	require.NoError(t, db.SaveLLMProviderTemplate(template))

	saveProvider := func(name, context string) {
		providerSourceConfig := api.LLMProviderConfiguration{
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
		}
		require.NoError(t, db.SaveConfig(&models.StoredConfig{
			UUID:                name + "-uuid",
			Kind:                string(api.LLMProviderConfigurationKindLlmProvider),
			Handle:              name,
			DisplayName:         name,
			Version:             "v1.0",
			SourceConfiguration: providerSourceConfig,
			DesiredState:        models.StateDeployed,
		}))
	}
	saveProvider("openai-provider", "/openai-provider")
	saveProvider("anthropic-provider", "/anthropic-provider")

	routerCfg := testRouterCfg()
	routerCfg.ListenerPort = 8080

	policyVersionResolver := utils.NewStaticPolicyVersionResolver(map[string]string{
		constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME: "v1",
	})
	// Minimal definitions so buildPolicyChain's operation-level version resolution
	// (set-headers for the upstream-auth/loopback-marker policies, llm-header-router
	// for the policy that registers the "/chat/completions" operation below) succeeds
	// instead of just logging and dropping the policy — keeps this test's output clean
	// and exercises the same resolution path a real deploy goes through.
	policyDefinitions := map[string]models.PolicyDefinition{
		"set-headers":       {Name: constants.SET_HEADERS_POLICY_NAME, Version: "v1.0.0"},
		"llm-header-router": {Name: "llm-header-router", Version: "v1.0.0"},
	}

	llmTransformer := NewLLMTransformer(store, db, routerCfg, &config.Config{}, policyDefinitions, policyVersionResolver)

	proxy := api.LLMProxyConfiguration{
		ApiVersion: api.LLMProxyConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProxyConfigurationKindLlmProxy,
		Metadata:   api.Metadata{Name: "failover-e2e-proxy"},
		Spec: api.LLMProxyConfigData{
			DisplayName: "failover-e2e-proxy",
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
			// A "/chat/completions" POST operation only gets registered in the
			// generated RestAPI when something targets that path — a bare proxy with
			// no Policies only produces the wildcard "/*" catch-all operations (see
			// llm_transformer.go's operationRegistry Phase 1/2). Mirrors the
			// llm-header-router attachment TestLLMProviderTransformer_TransformProxy_
			// AdditionalProviderAuthIsConditional (llm_transformer_multiprovider_test.go)
			// uses for the same reason.
			Policies: &[]api.LLMPolicy{{
				Name:    "llm-header-router",
				Version: "v1",
				Paths: []api.LLMPolicyPath{{
					Path:    "/chat/completions",
					Methods: []api.LLMPolicyPathMethods{"POST"},
					Params: map[string]interface{}{
						"defaultProvider": "openai-provider",
					},
				}},
			}},
			Resilience: &api.LLMResilience{
				Failover: &api.LLMFailoverConfig{
					Targets: []api.LLMFailoverTargetEntry{{
						Target: api.LLMFailoverTarget{Model: "gpt-4o"},
						Fallbacks: []api.LLMFailoverTarget{{
							Model:    "claude-sonnet-4-5-20250929",
							Provider: ptrStr("anthropic-upstream"),
						}},
					}},
				},
			},
		},
	}

	cfg := &models.StoredConfig{
		UUID:                "failover-e2e-proxy-uuid",
		Kind:                string(api.LLMProxyConfigurationKindLlmProxy),
		Handle:              "failover-e2e-proxy",
		DisplayName:         "failover-e2e-proxy",
		Version:             "v1.0",
		SourceConfiguration: proxy,
		DesiredState:        models.StateDeployed,
	}

	rdc, err := llmTransformer.Transform(cfg)
	require.NoError(t, err)

	// The route's vhost segment comes from routerCfg.VHosts.Main.Default, not a
	// fixed literal, so it's derived rather than hardcoded here.
	routeKey := "POST|/chat/completions|" + routerCfg.VHosts.Main.Default
	chatRoute, ok := rdc.Routes[routeKey]
	require.True(t, ok, "expected route %q in the transformed RuntimeDeployConfig", routeKey)
	require.NotNil(t, chatRoute.Upstream.Failover)
	require.Len(t, chatRoute.Upstream.Failover.Targets, 1)

	target := chatRoute.Upstream.Failover.Targets[0]
	assert.Equal(t, "gpt-4o", target.Model)
	assert.Equal(t, chatRoute.Upstream.ClusterKey, target.Target.ClusterKey,
		"omitting provider on the target must resolve to the proxy's own primary (openai) upstream")

	require.Len(t, target.Fallbacks, 1)
	fallback := target.Fallbacks[0]
	assert.Equal(t, "claude-sonnet-4-5-20250929", fallback.Model)

	var anthropicClusterKey string
	for key, uc := range rdc.UpstreamClusters {
		if uc.Name == "anthropic-upstream" {
			anthropicClusterKey = key
		}
	}
	require.NotEmpty(t, anthropicClusterKey,
		`expected an UpstreamCluster named "anthropic-upstream" (the additionalProviders[].as alias)`)
	assert.Equal(t, anthropicClusterKey, fallback.ClusterKey,
		"the fallback's provider: anthropic-upstream must resolve to the additionalProvider's own cluster, not the primary")
	assert.NotEqual(t, target.Target.ClusterKey, fallback.ClusterKey,
		"target and fallback must resolve to genuinely distinct clusters")

	assert.True(t, chatRoute.Upstream.UseClusterHeader, "a failover route must use cluster_header dynamic routing")
	assert.Equal(t, target.Target.ClusterKey, chatRoute.Upstream.DefaultCluster,
		"no-match requests must still fall back to the plain primary cluster")
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

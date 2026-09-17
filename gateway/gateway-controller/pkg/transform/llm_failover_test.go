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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	policyenginev1 "github.com/wso2/api-platform/sdk/core/policyengine"
)

func TestApplyFailoverToRoutes_ResolvesPrimaryAndNamedProvider(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"upstream_main_openai_com_443": {
				Name:      "", // main slot cluster
				BasePath:  "/",
				Endpoints: []models.Endpoint{{Host: "openai.com", Port: 443}},
				TLS:       &models.UpstreamTLS{Enabled: true},
			},
			"upstream_anthropic-upstream_anthropic_com_443": {
				Name:      "anthropic-upstream",
				BasePath:  "/",
				Endpoints: []models.Endpoint{{Host: "anthropic.com", Port: 443}},
				TLS:       &models.UpstreamTLS{Enabled: true},
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

	failover := &api.LLMFailoverConfig{
		Targets: []api.LLMFailoverTargetEntry{{
			Target: api.LLMFailoverTarget{Model: "gpt-4o"},
			Fallbacks: []api.LLMFailoverTarget{{
				Model:    "claude-sonnet-4-5-20250929",
				Provider: strPtr("anthropic-upstream"),
			}},
		}},
	}

	err := applyFailoverToRoutes(rdc, failover, "openai-provider")
	require.NoError(t, err)

	route := rdc.Routes["POST|/chat/completions|main"]
	require.NotNil(t, route.Upstream.Failover)
	require.Len(t, route.Upstream.Failover.Targets, 1)

	target := route.Upstream.Failover.Targets[0]
	assert.Equal(t, "gpt-4o", target.Model)
	assert.Equal(t, "upstream_main_openai_com_443", target.Target.ClusterKey)
	assert.Equal(t, "https://openai.com", target.Target.Upstream.URL)
	assert.Equal(t, "openai-provider", target.Target.Provider, "the primary target entry must carry the primary provider's id")

	require.Len(t, target.Fallbacks, 1)
	assert.Equal(t, "claude-sonnet-4-5-20250929", target.Fallbacks[0].Model)
	assert.Equal(t, "upstream_anthropic-upstream_anthropic_com_443", target.Fallbacks[0].ClusterKey)
	assert.Equal(t, "https://anthropic.com", target.Fallbacks[0].Upstream.URL)
	assert.Equal(t, "anthropic-upstream", target.Fallbacks[0].Provider, "a named-provider fallback must carry the resolved provider name")

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
	failover := &api.LLMFailoverConfig{
		Targets: []api.LLMFailoverTargetEntry{{
			Target:    api.LLMFailoverTarget{Model: "gpt-4o"},
			Fallbacks: []api.LLMFailoverTarget{{Model: "x", Provider: strPtr("nonexistent")}},
		}},
	}

	err := applyFailoverToRoutes(rdc, failover, "openai-provider")
	assert.Error(t, err)
}

func TestApplyFailoverToRoutes_NilFailoverIsNoOp(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		Routes: map[string]*models.Route{
			"r": {Upstream: models.RouteUpstream{ClusterKey: "c"}},
		},
	}
	require.NoError(t, applyFailoverToRoutes(rdc, nil, "openai-provider"))
	assert.Nil(t, rdc.Routes["r"].Upstream.Failover)
	assert.False(t, rdc.Routes["r"].Upstream.UseClusterHeader)
}

// TestResolveFailoverEntry_AllThreeWaysOfNamingThePrimaryProvider covers the
// three ways a validated failover target can refer to the proxy's own primary
// provider — omitting `provider`, and explicitly naming it either as the
// primary's own id or (bundled here per the fix report) as an additionalProvider's
// bare .Id with no .As set — and confirms all resolve to a correct entry rather
// than a translate-time error. Regression test for the fix that made an explicit
// `provider: <primary's own id>` (accepted by llm_validator.go's
// validUpstreamNames, since it seeds the primary's own id) fall through to the
// named-cluster scan instead of the primary path, where it could never be found
// (the primary's own cluster is stored with an empty Name).
func TestResolveFailoverEntry_AllThreeWaysOfNamingThePrimaryProvider(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"upstream_main_openai_com_443": {
				Name:      "", // primary/main slot cluster — empty Name, not a usable lookup key
				BasePath:  "/",
				Endpoints: []models.Endpoint{{Host: "openai.com", Port: 443}},
				TLS:       &models.UpstreamTLS{Enabled: true},
			},
			"upstream_cohere-provider_cohere_com_443": {
				Name:      "cohere-provider", // additionalProvider with no .As — bare .Id is the lookup key
				BasePath:  "/",
				Endpoints: []models.Endpoint{{Host: "cohere.com", Port: 443}},
				TLS:       &models.UpstreamTLS{Enabled: true},
			},
		},
	}
	r := &models.Route{
		Upstream: models.RouteUpstream{
			ClusterKey: "upstream_main_openai_com_443",
			Default: &policyenginev1.UpstreamInfo{
				ClusterName: "upstream_main_openai_com_443",
				URL:         "https://openai.com",
				BasePath:    "/",
			},
		},
	}
	const primaryProviderID = "openai-provider"

	t.Run("provider omitted", func(t *testing.T) {
		entry, err := resolveFailoverEntry(rdc, r, api.LLMFailoverTarget{Model: "gpt-4o"}, primaryProviderID)
		require.NoError(t, err)
		assert.Equal(t, "upstream_main_openai_com_443", entry.ClusterKey)
		assert.Equal(t, primaryProviderID, entry.Provider)
	})

	t.Run("provider explicitly names the primary's own id", func(t *testing.T) {
		entry, err := resolveFailoverEntry(rdc, r, api.LLMFailoverTarget{Model: "gpt-4o", Provider: strPtr(primaryProviderID)}, primaryProviderID)
		require.NoError(t, err)
		assert.Equal(t, "upstream_main_openai_com_443", entry.ClusterKey)
		assert.Equal(t, primaryProviderID, entry.Provider)
	})

	t.Run("provider names an additionalProvider's bare id with no as set", func(t *testing.T) {
		entry, err := resolveFailoverEntry(rdc, r, api.LLMFailoverTarget{Model: "command-r", Provider: strPtr("cohere-provider")}, primaryProviderID)
		require.NoError(t, err)
		assert.Equal(t, "upstream_cohere-provider_cohere_com_443", entry.ClusterKey)
		assert.Equal(t, "cohere-provider", entry.Provider)
	})
}

func strPtr(s string) *string { return &s }

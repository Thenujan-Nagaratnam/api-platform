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

package xds

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

func TestTranslateRuntimeConfig_AttachesUpstreamFilterOnlyWhereNeeded(t *testing.T) {
	translator := createTestTranslator()
	rdc := &models.RuntimeDeployConfig{
		Metadata: models.Metadata{UUID: "u", Kind: "LlmProvider"},
		Routes: map[string]*models.Route{
			"route-needs-it": {Upstream: models.RouteUpstream{ClusterKey: "bedrock-fallback"}},
			"route-plain":    {Upstream: models.RouteUpstream{ClusterKey: "openai-main"}},
		},
		PolicyChains: map[string]*models.PolicyChain{
			"route-needs-it": {Policies: []models.Policy{{Name: "aws-authentication"}}},
			"route-plain":    {Policies: []models.Policy{{Name: "prompt-decorator"}}},
		},
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"bedrock-fallback": {BasePath: "/", Endpoints: []models.Endpoint{{Host: "bedrock.example.com", Port: 443}}},
			"openai-main":      {BasePath: "/", Endpoints: []models.Endpoint{{Host: "api.openai.com", Port: 443}}},
		},
	}

	_, clusters, err := translator.translateRuntimeConfig(rdc)
	require.NoError(t, err)

	byName := map[string]bool{} // name -> has upstream filter
	for _, c := range clusters {
		_, has := c.GetTypedExtensionProtocolOptions()[constants.HttpProtocolOptionsTypedConfigKey]
		byName[c.GetName()] = has
	}

	assert.True(t, byName["bedrock-fallback"], "cluster whose route has an upstream-phase policy must get the filter")
	assert.False(t, byName["openai-main"], "cluster whose route has no upstream-phase policy must stay filter-free (zero cost)")
}

func TestTranslateRuntimeConfig_FailoverRouteGetsRetryPolicyAndHostRewrite(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"primary-cluster":  {BasePath: "/", Endpoints: []models.Endpoint{{Host: "openai.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
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

	translator := createTestTranslator()
	routes, _, err := translator.translateRuntimeConfig(rdc)
	require.NoError(t, err)
	require.Len(t, routes, 1)

	action := routes[0].GetRoute()
	require.NotNil(t, action)
	require.NotNil(t, action.RetryPolicy)
	assert.Equal(t, "5xx", action.RetryPolicy.RetryOn)
	require.NotNil(t, action.RetryPolicy.RetryPriority)
	assert.Equal(t, "envoy.retry_priorities.previous_priorities", action.RetryPolicy.RetryPriority.Name)
	require.NotNil(t, action.RetryPolicy.NumRetries, "num_retries must be set or Envoy defaults to 1, capping escalation at priority 1 regardless of chain depth")
	assert.Equal(t, uint32(1), action.RetryPolicy.NumRetries.GetValue())

	_, isAutoRewrite := action.HostRewriteSpecifier.(*route.RouteAction_AutoHostRewrite)
	assert.True(t, isAutoRewrite, "a failover route must auto-rewrite Host, or per-attempt backend resolution can't tell attempts apart")
}

func failoverTestRDC(failover *models.RouteFailover) *models.RuntimeDeployConfig {
	return &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"primary-cluster":  {BasePath: "/", Endpoints: []models.Endpoint{{Host: "openai.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
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
					Failover:         failover,
				},
			},
		},
	}
}

func translateSingleFailoverRoute(t *testing.T, failover *models.RouteFailover) *route.RouteAction {
	t.Helper()
	translator := createTestTranslator()
	routes, _, err := translator.translateRuntimeConfig(failoverTestRDC(failover))
	require.NoError(t, err)
	require.Len(t, routes, 1)
	action := routes[0].GetRoute()
	require.NotNil(t, action)
	require.NotNil(t, action.RetryPolicy)
	return action
}

func TestTranslateRuntimeConfig_FailoverRoute_CustomRetryOn(t *testing.T) {
	action := translateSingleFailoverRoute(t, &models.RouteFailover{
		Targets: []models.RouteFailoverTarget{{
			Model:     "gpt-4o",
			Target:    models.RouteFailoverEntry{ClusterKey: "primary-cluster"},
			Fallbacks: []models.RouteFailoverEntry{{ClusterKey: "fallback-cluster"}},
		}},
		RetryOn: []string{"reset", "connect-failure", "gateway-error"},
	})

	assert.Equal(t, "reset,connect-failure,gateway-error", action.RetryPolicy.RetryOn)
}

func TestTranslateRuntimeConfig_FailoverRoute_RetriableStatusCodes(t *testing.T) {
	action := translateSingleFailoverRoute(t, &models.RouteFailover{
		Targets: []models.RouteFailoverTarget{{
			Model:     "gpt-4o",
			Target:    models.RouteFailoverEntry{ClusterKey: "primary-cluster"},
			Fallbacks: []models.RouteFailoverEntry{{ClusterKey: "fallback-cluster"}},
		}},
		RetryOn:              []string{"retriable-status-codes"},
		RetriableStatusCodes: []uint32{409, 425},
	})

	assert.Equal(t, "retriable-status-codes", action.RetryPolicy.RetryOn)
	assert.Equal(t, []uint32{409, 425}, action.RetryPolicy.RetriableStatusCodes)
}

func TestTranslateRuntimeConfig_FailoverRoute_RetriableHeaders(t *testing.T) {
	action := translateSingleFailoverRoute(t, &models.RouteFailover{
		Targets: []models.RouteFailoverTarget{{
			Model:     "gpt-4o",
			Target:    models.RouteFailoverEntry{ClusterKey: "primary-cluster"},
			Fallbacks: []models.RouteFailoverEntry{{ClusterKey: "fallback-cluster"}},
		}},
		RetryOn:          []string{"retriable-headers"},
		RetriableHeaders: []string{"X-Should-Retry"},
	})

	require.Len(t, action.RetryPolicy.RetriableHeaders, 1)
	hm := action.RetryPolicy.RetriableHeaders[0]
	assert.Equal(t, "x-should-retry", hm.Name, "header names must be lowercased for Envoy header matching")
	presentMatch, ok := hm.HeaderMatchSpecifier.(*route.HeaderMatcher_PresentMatch)
	require.True(t, ok, "expected a presence-match specifier, not a value match")
	assert.True(t, presentMatch.PresentMatch)
}

// TestTranslateRuntimeConfig_FailoverRoute_EmptyRetryOnDefaultsTo5xx proves the
// translator's own defensive default (not just applyFailoverToRoutes' one) —
// a RouteFailover built any other way, with RetryOn left nil, must never
// produce an empty retry_on (which Envoy treats as "never retry").
func TestTranslateRuntimeConfig_FailoverRoute_EmptyRetryOnDefaultsTo5xx(t *testing.T) {
	action := translateSingleFailoverRoute(t, &models.RouteFailover{
		Targets: []models.RouteFailoverTarget{{
			Model:     "gpt-4o",
			Target:    models.RouteFailoverEntry{ClusterKey: "primary-cluster"},
			Fallbacks: []models.RouteFailoverEntry{{ClusterKey: "fallback-cluster"}},
		}},
	})

	assert.Equal(t, "5xx", action.RetryPolicy.RetryOn)
}

// TestTranslateRuntimeConfig_FailoverRetryPolicyNumRetriesMatchesDeepestChain asserts
// NumRetries is set to the deepest fallback chain among the route's targets — Envoy
// defaults num_retries to 1, so without this, a target with 2+ fallbacks could only
// ever escalate from priority 0 to priority 1 and fallbacks[1:] would be unreachable.
func TestTranslateRuntimeConfig_FailoverRetryPolicyNumRetriesMatchesDeepestChain(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"primary-cluster":   {BasePath: "/", Endpoints: []models.Endpoint{{Host: "openai.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
			"fallback-cluster1": {BasePath: "/", Endpoints: []models.Endpoint{{Host: "anthropic.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
			"fallback-cluster2": {BasePath: "/", Endpoints: []models.Endpoint{{Host: "cohere.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
			"fallback-cluster3": {BasePath: "/", Endpoints: []models.Endpoint{{Host: "mistral.com", Port: 443}}, TLS: &models.UpstreamTLS{Enabled: true}},
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
						Targets: []models.RouteFailoverTarget{
							{
								Model:  "gpt-4o",
								Target: models.RouteFailoverEntry{ClusterKey: "primary-cluster"},
								Fallbacks: []models.RouteFailoverEntry{
									{ClusterKey: "fallback-cluster1"},
									{ClusterKey: "fallback-cluster2"},
								},
							},
							{
								Model:  "gpt-4o-mini",
								Target: models.RouteFailoverEntry{ClusterKey: "primary-cluster"},
								Fallbacks: []models.RouteFailoverEntry{
									{ClusterKey: "fallback-cluster1"},
									{ClusterKey: "fallback-cluster2"},
									{ClusterKey: "fallback-cluster3"},
								},
							},
						},
					},
				},
			},
		},
	}

	translator := createTestTranslator()
	routes, _, err := translator.translateRuntimeConfig(rdc)
	require.NoError(t, err)
	require.Len(t, routes, 1)

	action := routes[0].GetRoute()
	require.NotNil(t, action)
	require.NotNil(t, action.RetryPolicy)
	require.NotNil(t, action.RetryPolicy.NumRetries)
	// Deepest chain across both targets: 3 fallbacks (second target) — the max, not
	// either target's own individual depth, since RetryPolicy is one object per route.
	assert.Equal(t, uint32(3), action.RetryPolicy.NumRetries.GetValue())
}

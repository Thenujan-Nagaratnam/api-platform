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

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

func minimalRouteRDC(upstream models.RouteUpstream) *models.RuntimeDeployConfig {
	return &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"main-cluster": {BasePath: "/", Endpoints: []models.Endpoint{{Host: "backend.example.com", Port: 443}}},
		},
		Routes: map[string]*models.Route{
			"POST|/chat/completions|main": {
				Method:   "POST",
				Path:     "/chat/completions",
				Vhost:    "main",
				Upstream: upstream,
			},
		},
	}
}

// TestTranslateRuntimeConfig_NoRetrySources_NoRetryPolicy guards spec
// FR-004/SC-002: a route with no failover chain and no policy contributing
// a Retry declaration must get no RetryPolicy at all, exactly as before this
// feature existed (quickstart.md case 1).
func TestTranslateRuntimeConfig_NoRetrySources_NoRetryPolicy(t *testing.T) {
	rdc := minimalRouteRDC(models.RouteUpstream{ClusterKey: "main-cluster"})

	translator := createTestTranslator()
	routes, _, err := translator.translateRuntimeConfig(rdc)
	require.NoError(t, err)
	require.Len(t, routes, 1)

	assert.Nil(t, routes[0].GetRoute().RetryPolicy)
}

// TestTranslateRuntimeConfig_SingleContribution_NoFailover_GeneratesRetryPolicy
// is User Story 1's core acceptance case (quickstart.md case 2): one
// contributing policy, no failover chain, zero extra configuration.
func TestTranslateRuntimeConfig_SingleContribution_NoFailover_GeneratesRetryPolicy(t *testing.T) {
	rdc := minimalRouteRDC(models.RouteUpstream{
		ClusterKey: "main-cluster",
		RetryContributions: []models.UpstreamRetryContribution{
			{PolicyName: "oauth2-generator", StatusCodes: []int{401}, MaxAttempts: 2},
		},
	})

	translator := createTestTranslator()
	routes, _, err := translator.translateRuntimeConfig(rdc)
	require.NoError(t, err)
	require.Len(t, routes, 1)

	retryPolicy := routes[0].GetRoute().RetryPolicy
	require.NotNil(t, retryPolicy)
	assert.Contains(t, retryPolicy.RetryOn, "retriable-status-codes")
	assert.Equal(t, []uint32{401}, retryPolicy.RetriableStatusCodes)
	assert.Equal(t, uint32(1), retryPolicy.NumRetries.GetValue())
}

// TestTranslateRuntimeConfig_ContributionAndFailover_BothCoverageKept is User
// Story 2's core acceptance case (quickstart.md case 3; spec FR-007).
func TestTranslateRuntimeConfig_ContributionAndFailover_BothCoverageKept(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"primary-cluster":  {BasePath: "/", Endpoints: []models.Endpoint{{Host: "openai.com", Port: 443}}},
			"fallback-cluster": {BasePath: "/", Endpoints: []models.Endpoint{{Host: "anthropic.com", Port: 443}}},
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
						RetryOn: []string{"5xx", "connect-failure", "reset", "reset-before-request"},
						Targets: []models.RouteFailoverTarget{{
							Model:     "gpt-4o",
							Target:    models.RouteFailoverEntry{ClusterKey: "primary-cluster"},
							Fallbacks: []models.RouteFailoverEntry{{ClusterKey: "fallback-cluster"}},
						}},
					},
					RetryContributions: []models.UpstreamRetryContribution{
						{PolicyName: "oauth2-generator", StatusCodes: []int{401}, MaxAttempts: 2},
					},
				},
			},
		},
	}

	translator := createTestTranslator()
	routes, _, err := translator.translateRuntimeConfig(rdc)
	require.NoError(t, err)
	require.Len(t, routes, 1)

	retryPolicy := routes[0].GetRoute().RetryPolicy
	require.NotNil(t, retryPolicy)
	assert.Equal(t, []uint32{401}, retryPolicy.RetriableStatusCodes, "the contribution's own code must not be dropped")
	// failover's own derived attempts (1 fallback -> 2 total attempts) ties
	// the contribution's MaxAttempts (2) -> NumRetries stays 1 either way,
	// but RetryOn must still carry failover's transport conditions.
	assert.Contains(t, retryPolicy.RetryOn, "connect-failure")
	assert.Contains(t, retryPolicy.RetryOn, "retriable-status-codes")
}

// TestTranslateRuntimeConfig_FailoverOnlyRoute_Unaffected is the explicit
// regression guard spec Acceptance Scenario US2.2 requires: a route with
// only a failover chain, zero contributions, must be unaffected by this
// feature (same assertions as the pre-existing
// TestTranslateRuntimeConfig_FailoverRouteGetsRetryPolicyAndHostRewrite,
// re-stated here for traceability to this feature's own spec).
func TestTranslateRuntimeConfig_FailoverOnlyRoute_Unaffected(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"primary-cluster":  {BasePath: "/", Endpoints: []models.Endpoint{{Host: "openai.com", Port: 443}}},
			"fallback-cluster": {BasePath: "/", Endpoints: []models.Endpoint{{Host: "anthropic.com", Port: 443}}},
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

	retryPolicy := routes[0].GetRoute().RetryPolicy
	require.NotNil(t, retryPolicy)
	assert.Equal(t, "5xx,connect-failure,reset,reset-before-request", retryPolicy.RetryOn)
	assert.Equal(t, uint32(1), retryPolicy.NumRetries.GetValue())
}

// TestTranslateRuntimeConfig_TwoContributions_UnionHonored is User Story 3's
// core acceptance case (quickstart.md case 4).
func TestTranslateRuntimeConfig_TwoContributions_UnionHonored(t *testing.T) {
	rdc := minimalRouteRDC(models.RouteUpstream{
		ClusterKey: "main-cluster",
		RetryContributions: []models.UpstreamRetryContribution{
			{PolicyName: "policy-a", StatusCodes: []int{401}, MaxAttempts: 2},
			{PolicyName: "policy-b", StatusCodes: []int{429}, MaxAttempts: 3},
		},
	})

	translator := createTestTranslator()
	routes, _, err := translator.translateRuntimeConfig(rdc)
	require.NoError(t, err)
	require.Len(t, routes, 1)

	retryPolicy := routes[0].GetRoute().RetryPolicy
	require.NotNil(t, retryPolicy)
	assert.ElementsMatch(t, []uint32{401, 429}, retryPolicy.RetriableStatusCodes)
	assert.Equal(t, uint32(2), retryPolicy.NumRetries.GetValue()) // max(2,3) - 1
}

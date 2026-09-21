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

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/xds"
	policyenginev1 "github.com/wso2/api-platform/sdk/core/policyengine"
)

func TestParseModelFailoverParams_ValidatesProviderReference(t *testing.T) {
	params := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"target": map[string]interface{}{"model": "gpt-4o"},
				"fallbacks": []interface{}{
					map[string]interface{}{"model": "claude-sonnet-4-5-20250929", "provider": "unregistered-provider"},
				},
			},
		},
	}

	_, err := parseModelFailoverParams(params, []string{"anthropic-upstream"}, "openai-primary")

	require.Error(t, err)
}

func TestParseModelFailoverParams_ValidAndEmptyTargets(t *testing.T) {
	ok := map[string]interface{}{
		"suspendDuration": 900,
		"targets": []interface{}{
			map[string]interface{}{
				"target":    map[string]interface{}{"model": "gpt-4o", "provider": "openai-primary"},
				"fallbacks": []interface{}{map[string]interface{}{"model": "c", "provider": "anthropic-upstream"}},
			},
		},
	}
	p, err := parseModelFailoverParams(ok, []string{"anthropic-upstream"}, "openai-primary")
	require.NoError(t, err)
	assert.Equal(t, 900, p.SuspendDuration)
	require.Len(t, p.Targets, 1)
	assert.Equal(t, "anthropic-upstream", p.Targets[0].Fallbacks[0].Provider)

	_, err = parseModelFailoverParams(map[string]interface{}{"targets": []interface{}{}}, nil, "p")
	require.Error(t, err)
}

func failoverTestRDC() (*models.RuntimeDeployConfig, *models.Route) {
	route := &models.Route{
		Upstream: models.RouteUpstream{
			ClusterKey: "upstream_main_openai_com_443",
			Default: &policyenginev1.UpstreamInfo{
				ClusterName: "upstream_main_openai_com_443",
				URL:         "https://openai.com",
				BasePath:    "/",
			},
		},
	}
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"upstream_anthropic-upstream_anthropic_com_443": {
				Name:      "anthropic-upstream",
				BasePath:  "/",
				Endpoints: []models.Endpoint{{Host: "anthropic.com", Port: 443}},
				TLS:       &models.UpstreamTLS{Enabled: true},
			},
		},
		Routes: map[string]*models.Route{"POST|/chat/completions|main": route},
	}
	return rdc, route
}

func TestBuildRouteFailoverFromPolicy_InjectsAggregateClusterName(t *testing.T) {
	rdc, route := failoverTestRDC()
	params := &modelFailoverParams{
		Targets: []modelFailoverTargetEntry{
			{
				Target:    modelFailoverTarget{Model: "gpt-4o"},
				Fallbacks: []modelFailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
			},
			{Target: modelFailoverTarget{Model: "gpt-4o-mini"}},
		},
		SuspendDuration: 900,
	}
	routeKey := "POST|/chat/completions|main"

	rf, expanded, err := buildRouteFailoverFromPolicy(rdc, route, params, routeKey, "openai-primary")

	require.NoError(t, err)
	assert.Equal(t, 900, rf.SuspendDurationSeconds)
	assert.Equal(t, []string{"5xx"}, rf.RetryOn)
	require.Len(t, rf.Targets, 2)
	assert.Equal(t, "upstream_main_openai_com_443", rf.Targets[0].Target.ClusterKey)
	require.Len(t, rf.Targets[0].Fallbacks, 1)
	assert.Equal(t, "upstream_anthropic-upstream_anthropic_com_443", rf.Targets[0].Fallbacks[0].ClusterKey)

	require.Len(t, expanded.Targets, 2)
	assert.Equal(t, xds.AggregateClusterName(routeKey, 0), expanded.Targets[0].AggregateCluster)
	assert.Equal(t, xds.AggregateClusterName(routeKey, 1), expanded.Targets[1].AggregateCluster)
	assert.Empty(t, params.Targets[0].AggregateCluster, "input params must not be mutated")
	assert.Equal(t, "openai-primary", expanded.PrimaryProvider)
	assert.Equal(t, "", expanded.Targets[0].Target.Provider, "member providers stay verbatim")
	assert.Equal(t, "anthropic-upstream", expanded.Targets[0].Fallbacks[0].Provider)
}

func TestBuildRouteFailoverFromPolicy_UnknownProviderIsAnError(t *testing.T) {
	rdc, route := failoverTestRDC()
	params := &modelFailoverParams{Targets: []modelFailoverTargetEntry{{
		Target:    modelFailoverTarget{Model: "gpt-4o"},
		Fallbacks: []modelFailoverTarget{{Model: "x", Provider: "nonexistent"}},
	}}}

	_, _, err := buildRouteFailoverFromPolicy(rdc, route, params, "k", "openai-primary")
	require.Error(t, err)
}

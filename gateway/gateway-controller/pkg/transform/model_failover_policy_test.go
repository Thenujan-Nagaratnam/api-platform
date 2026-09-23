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
				modelFailoverTarget: modelFailoverTarget{Model: "gpt-4o"},
				Fallbacks:           []modelFailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
			},
			{modelFailoverTarget: modelFailoverTarget{Model: "gpt-4o-mini"}},
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
	assert.Equal(t, "", expanded.Targets[0].Provider, "member providers stay verbatim")
	assert.Equal(t, "anthropic-upstream", expanded.Targets[0].Fallbacks[0].Provider)
}

// TestBuildRouteFailoverFromPolicy_InjectsMemberBasePathsAndOperationPath pins
// the two fields the policy needs to correct a retry's :path: every chain
// member is a loopback upstream on the same host:port, so auto_host_rewrite
// leaves :authority identical across attempts and only the base path inside
// :path distinguishes one provider's route from another's. The policy can
// compute neither itself — the controller hands both down.
func TestBuildRouteFailoverFromPolicy_InjectsMemberBasePathsAndOperationPath(t *testing.T) {
	rdc, route := failoverTestRDC()
	route.OperationPath = "/chat/completions"
	route.Upstream.Default.BasePath = "/openai-provider"
	rdc.UpstreamClusters["upstream_anthropic-upstream_anthropic_com_443"].BasePath = "/anthropic-provider"

	params := &modelFailoverParams{
		Targets: []modelFailoverTargetEntry{{
			modelFailoverTarget: modelFailoverTarget{Model: "gpt-4o"},
			Fallbacks:           []modelFailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
		}},
	}

	_, expanded, err := buildRouteFailoverFromPolicy(rdc, route, params, "POST|/chat/completions|main", "openai-primary")

	require.NoError(t, err)
	assert.Equal(t, "/chat/completions", expanded.OperationPath)
	assert.Equal(t, "/openai-provider", expanded.Targets[0].BasePath)
	assert.Equal(t, "/anthropic-provider", expanded.Targets[0].Fallbacks[0].BasePath)
	assert.Empty(t, params.Targets[0].BasePath, "input params must not be mutated")
	assert.Empty(t, params.Targets[0].Fallbacks[0].BasePath, "input params must not be mutated")
}

// An author-supplied basePath/operationPath is never trusted: the controller
// resolves both, so whatever was written in the attachment is overwritten.
func TestBuildRouteFailoverFromPolicy_OverwritesAuthoredBasePathAndOperationPath(t *testing.T) {
	rdc, route := failoverTestRDC()
	route.OperationPath = "/chat/completions"
	route.Upstream.Default.BasePath = "/openai-provider"

	params := &modelFailoverParams{
		OperationPath: "/attacker-supplied",
		Targets: []modelFailoverTargetEntry{{
			modelFailoverTarget: modelFailoverTarget{Model: "gpt-4o", BasePath: "/attacker-supplied"},
		}},
	}

	_, expanded, err := buildRouteFailoverFromPolicy(rdc, route, params, "POST|/chat/completions|main", "openai-primary")

	require.NoError(t, err)
	assert.Equal(t, "/chat/completions", expanded.OperationPath)
	assert.Equal(t, "/openai-provider", expanded.Targets[0].BasePath)
}

// TestBuildRouteFailoverFromPolicy_InjectsMemberClusterNames pins the field
// that lets the policy recognise a suspended-primary bypass: that dispatch goes
// straight onto a fallback's own Envoy cluster rather than the chain's
// aggregate, so the policy's upstream phase sees that cluster's name and has
// nothing else to match it against.
func TestBuildRouteFailoverFromPolicy_InjectsMemberClusterNames(t *testing.T) {
	rdc, route := failoverTestRDC()

	params := &modelFailoverParams{
		Targets: []modelFailoverTargetEntry{{
			modelFailoverTarget: modelFailoverTarget{Model: "gpt-4o"},
			Fallbacks:           []modelFailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
		}},
	}

	_, expanded, err := buildRouteFailoverFromPolicy(rdc, route, params, "POST|/chat/completions|main", "openai-primary")

	require.NoError(t, err)
	assert.Equal(t, "upstream_main_openai_com_443", expanded.Targets[0].ClusterName)
	assert.Equal(t, "upstream_anthropic-upstream_anthropic_com_443", expanded.Targets[0].Fallbacks[0].ClusterName)
	assert.Empty(t, params.Targets[0].Fallbacks[0].ClusterName, "input params must not be mutated")
}

// An author-supplied clusterName is never trusted — it would otherwise let an
// attachment claim an arbitrary cluster as a chain member.
func TestBuildRouteFailoverFromPolicy_OverwritesAuthoredClusterName(t *testing.T) {
	rdc, route := failoverTestRDC()

	params := &modelFailoverParams{
		Targets: []modelFailoverTargetEntry{{
			modelFailoverTarget: modelFailoverTarget{Model: "gpt-4o", ClusterName: "attacker-cluster"},
			Fallbacks:           []modelFailoverTarget{{Model: "c", Provider: "anthropic-upstream", ClusterName: "attacker-cluster"}},
		}},
	}

	_, expanded, err := buildRouteFailoverFromPolicy(rdc, route, params, "POST|/chat/completions|main", "openai-primary")

	require.NoError(t, err)
	assert.Equal(t, "upstream_main_openai_com_443", expanded.Targets[0].ClusterName)
	assert.Equal(t, "upstream_anthropic-upstream_anthropic_com_443", expanded.Targets[0].Fallbacks[0].ClusterName)
}

func TestBuildRouteFailoverFromPolicy_UnknownProviderIsAnError(t *testing.T) {
	rdc, route := failoverTestRDC()
	params := &modelFailoverParams{Targets: []modelFailoverTargetEntry{{
		modelFailoverTarget: modelFailoverTarget{Model: "gpt-4o"},
		Fallbacks:           []modelFailoverTarget{{Model: "x", Provider: "nonexistent"}},
	}}}

	_, _, err := buildRouteFailoverFromPolicy(rdc, route, params, "k", "openai-primary")
	require.Error(t, err)
}

// ─── Configurable statusCodes ────────────────────────────────────────────────

func TestBuildRouteFailoverFromPolicy_OmittedStatusCodesDefaultsToPlain5xx(t *testing.T) {
	rdc, route := failoverTestRDC()
	params := &modelFailoverParams{
		Targets: []modelFailoverTargetEntry{{modelFailoverTarget: modelFailoverTarget{Model: "gpt-4o"}}},
	}

	rf, expanded, err := buildRouteFailoverFromPolicy(rdc, route, params, "POST|/chat/completions|main", "openai-primary")

	require.NoError(t, err)
	assert.Equal(t, []string{"5xx"}, rf.RetryOn)
	assert.Empty(t, rf.RetriableStatusCodes)
	assert.Empty(t, expanded.StatusCodes)
}

// statusCodes REPLACES the "any 5xx" default rather than extending it — see
// modelFailoverParams.StatusCodes's own doc comment.
func TestBuildRouteFailoverFromPolicy_StatusCodesReplaceDefaultRetryOn(t *testing.T) {
	rdc, route := failoverTestRDC()
	params := &modelFailoverParams{
		Targets:     []modelFailoverTargetEntry{{modelFailoverTarget: modelFailoverTarget{Model: "gpt-4o"}}},
		StatusCodes: []int{500, 502, 429},
	}

	rf, expanded, err := buildRouteFailoverFromPolicy(rdc, route, params, "POST|/chat/completions|main", "openai-primary")

	require.NoError(t, err)
	assert.Equal(t, []string{"retriable-status-codes"}, rf.RetryOn, "must not also carry plain 5xx")
	assert.Equal(t, []int{500, 502, 429}, rf.RetriableStatusCodes)
	assert.Equal(t, []int{500, 502, 429}, expanded.StatusCodes, "carried through to the policy's own runtime params")
}

// ─── parseModelFailoverParams: statusCodes validation ────────────────────────

func TestParseModelFailoverParams_RejectsOutOfRangeStatusCode(t *testing.T) {
	raw := map[string]interface{}{
		"targets":     []interface{}{map[string]interface{}{"model": "gpt-4o"}},
		"statusCodes": []interface{}{float64(999)},
	}

	_, err := parseModelFailoverParams(raw, nil, "openai-primary")
	require.Error(t, err)
}

func TestParseModelFailoverParams_AcceptsValidStatusCodes(t *testing.T) {
	raw := map[string]interface{}{
		"targets":     []interface{}{map[string]interface{}{"model": "gpt-4o"}},
		"statusCodes": []interface{}{float64(500), float64(429)},
	}

	params, err := parseModelFailoverParams(raw, nil, "openai-primary")
	require.NoError(t, err)
	assert.Equal(t, []int{500, 429}, params.StatusCodes)
}

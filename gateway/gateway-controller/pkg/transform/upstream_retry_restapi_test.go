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
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// TestRestAPITransformer_RetryContributionPopulatedFromAttachedPolicy exercises
// T009's hook end-to-end: a policy definition declaring Retry, attached to a
// plain route (no model-failover chain), should populate that route's
// Upstream.RetryContributions — this is the same restapi.go code path
// LLMTransformer.Transform delegates to (pkg/transform/llm.go Step 3), so
// this test also stands in for T010's LLM/MCP coverage: there is no separate
// policy-chain-building logic in llm.go to hook into independently.
func TestRestAPITransformer_RetryContributionPopulatedFromAttachedPolicy(t *testing.T) {
	defs := map[string]models.PolicyDefinition{
		"oauth2-generator|v1.0.0": {
			Name:    "oauth2-generator",
			Version: "v1.0.0",
			Retry:   &models.Retry{StatusCodes: []int{401}, MaxAttempts: 2},
		},
	}

	transformer := NewRestAPITransformer(testRouterCfg(), &config.Config{}, defs)
	cfg := makeRestAPIStoredConfig(
		[]api.Policy{{Name: "oauth2-generator", Version: "v1"}},
		nil,
	)

	rdc, err := transformer.Transform(cfg)
	require.NoError(t, err)
	require.NotNil(t, rdc)

	routeKey := "GET|/test/hello|main.local"
	route, ok := rdc.Routes[routeKey]
	require.True(t, ok)
	require.Len(t, route.Upstream.RetryContributions, 1)
	contribution := route.Upstream.RetryContributions[0]
	assert.Equal(t, "oauth2-generator", contribution.PolicyName)
	assert.Equal(t, []int{401}, contribution.StatusCodes)
	assert.Equal(t, 2, contribution.MaxAttempts)
}

// TestRestAPITransformer_RetryContributionAbsentWhenPolicyHasNoRetry guards
// spec FR-004/SC-002: a policy with no Retry declaration must leave
// RetryContributions untouched, exactly as before this feature existed.
func TestRestAPITransformer_RetryContributionAbsentWhenPolicyHasNoRetry(t *testing.T) {
	defs := map[string]models.PolicyDefinition{
		"header-mutate|v1.0.0": {Name: "header-mutate", Version: "v1.0.0"},
	}

	transformer := NewRestAPITransformer(testRouterCfg(), &config.Config{}, defs)
	cfg := makeRestAPIStoredConfig(
		[]api.Policy{{Name: "header-mutate", Version: "v1"}},
		nil,
	)

	rdc, err := transformer.Transform(cfg)
	require.NoError(t, err)
	require.NotNil(t, rdc)

	routeKey := "GET|/test/hello|main.local"
	route, ok := rdc.Routes[routeKey]
	require.True(t, ok)
	assert.Empty(t, route.Upstream.RetryContributions)
}

// TestRestAPITransformer_RetryContributionSkippedWhenEnabledByParamFalse
// confirms a per-attachment EnabledByParam value of false suppresses the
// contribution entirely, not just its fields.
func TestRestAPITransformer_RetryContributionSkippedWhenEnabledByParamFalse(t *testing.T) {
	defs := map[string]models.PolicyDefinition{
		"rate-aware|v1.0.0": {
			Name:    "rate-aware",
			Version: "v1.0.0",
			Retry: &models.Retry{
				EnabledByParam: "autoRetry",
				StatusCodes:    []int{429},
				MaxAttempts:    2,
			},
		},
	}

	transformer := NewRestAPITransformer(testRouterCfg(), &config.Config{}, defs)
	params := map[string]interface{}{"autoRetry": false}
	cfg := makeRestAPIStoredConfig(
		[]api.Policy{{Name: "rate-aware", Version: "v1", Params: &params}},
		nil,
	)

	rdc, err := transformer.Transform(cfg)
	require.NoError(t, err)
	require.NotNil(t, rdc)

	routeKey := "GET|/test/hello|main.local"
	route, ok := rdc.Routes[routeKey]
	require.True(t, ok)
	assert.Empty(t, route.Upstream.RetryContributions)
}

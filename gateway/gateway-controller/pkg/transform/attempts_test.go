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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/attempts"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/failover"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// A retrying test policy, a per-attempt signer and a per-request guardrail,
// known to the gateway only through their definitions.
func retryDefs() map[string]models.PolicyDefinition {
	two := 2
	return map[string]models.PolicyDefinition{
		"retry-on-status|v1.0.0": {Name: "retry-on-status", Version: "v1.0.0", RetryBehavior: &models.RetryBehavior{
			Runs:     models.RunsOnEveryAttempt,
			CanRetry: &models.CanRetry{EnabledByParam: "enabled", MaxAttempts: &two, PerAttemptTimeout: "5s"},
		}},
		"sign|v1.0.0":  {Name: "sign", Version: "v1.0.0", RetryBehavior: &models.RetryBehavior{Runs: models.RunsOnEveryAttempt}},
		"guard|v1.0.0": {Name: "guard", Version: "v1.0.0"},
	}
}

func retryRestAPI(retryParams map[string]interface{}, sandbox bool) *models.StoredConfig {
	post := api.OperationMethod("POST")
	get := api.OperationMethod("GET")
	apiPolicies := []api.Policy{{Name: "guard", Version: "v1"}}
	opPolicies := []api.Policy{
		{Name: "retry-on-status", Version: "v1", Params: &retryParams},
		{Name: "sign", Version: "v1"},
	}
	spec := api.APIConfigData{
		DisplayName: "retry-api",
		Context:     "/retry",
		Version:     "v1.0",
		Policies:    &apiPolicies,
		Operations: []api.Operation{
			{Method: &post, Path: api.Ptr("/orders"), Policies: &opPolicies},
			{Method: &get, Path: api.Ptr("/health")},
		},
	}
	spec.Upstream.Main = api.Upstream{Url: ptrStr("http://backend:8080")}
	if sandbox {
		spec.Upstream.Sandbox = &api.Upstream{Url: ptrStr("http://sandbox-backend:8080")}
	}
	return &models.StoredConfig{
		UUID:          "retry-api-uuid",
		Kind:          "RestApi",
		Configuration: api.RestAPI{Kind: api.RestAPIKindRestApi, Metadata: api.Metadata{Name: "retry-api"}, Spec: spec},
	}
}

func transformRetry(t *testing.T, params map[string]interface{}, sandbox bool) *models.RuntimeDeployConfig {
	t.Helper()
	rdc, err := NewRestAPITransformer(testRouterCfg(), &config.Config{}, retryDefs()).Transform(retryRestAPI(params, sandbox))
	require.NoError(t, err)
	return rdc
}

func splitRoutes(t *testing.T, rdc *models.RuntimeDeployConfig) (frontKey, attemptKey string) {
	t.Helper()
	for k, r := range rdc.Routes {
		if r.Failover == nil || r.Vhost != "main.local" {
			continue
		}
		switch r.Failover.Role {
		case string(failover.RoleFront):
			frontKey = k
		case string(failover.RoleDispatch):
			attemptKey = k
		}
	}
	require.NotEmpty(t, frontKey)
	require.NotEmpty(t, attemptKey)
	return frontKey, attemptKey
}

func chainNames(c *models.PolicyChain) []string {
	var out []string
	for _, p := range c.Policies {
		out = append(out, p.Name)
	}
	return out
}

func TestAttemptSplitFromDefinitionsOnly(t *testing.T) {
	rdc := transformRetry(t, map[string]interface{}{"enabled": true}, false)
	frontKey, attemptKey := splitRoutes(t, rdc)
	front, attempt := rdc.Routes[frontKey], rdc.Routes[attemptKey]

	assert.Equal(t, 1, front.Failover.NumRetries, "maxAttempts 2 = one retry")
	assert.Equal(t, 5*time.Second, front.Failover.PerTryTimeout)
	assert.Equal(t, 12*time.Second, front.Failover.RouteTimeout)
	assert.Equal(t, "retriable-headers", front.Failover.RetryOn)
	assert.Equal(t, front.Failover.ChainID, attempt.Failover.ChainID)
	assert.True(t, attempt.Failover.SameUpstream, "the per-attempt route forwards to the operation's own upstream")
	require.Len(t, attempt.MatchHeaders, 1)
	assert.Equal(t, failover.HeaderChain, attempt.MatchHeaders[0].Name)
	assert.Equal(t, front.Upstream.ClusterKey, attempt.Upstream.ClusterKey)

	assert.Equal(t, []string{attempts.SystemPolicyName, "guard"}, chainNames(rdc.PolicyChains[frontKey]), "per-request policies stay in front")
	assert.Equal(t, []string{attempts.SystemPolicyName, "retry-on-status", "sign"}, chainNames(rdc.PolicyChains[attemptKey]), "per-attempt policies move to the per-attempt route")

	fp := rdc.PolicyChains[frontKey].Policies[0].Params
	assert.Equal(t, attempts.RoleFront, fp[attempts.ParamRole])
	assert.Equal(t, front.Failover.ChainID, fp[attempts.ParamScope])
	assert.Equal(t, map[string]interface{}{"retry-on-status": 2}, fp[attempts.ParamAllowances])
	assert.Equal(t, "17s", fp[attempts.ParamTTL])
	ap := rdc.PolicyChains[attemptKey].Policies[0].Params
	assert.Equal(t, attempts.RoleAttempt, ap[attempts.ParamRole])
	assert.Equal(t, failover.HopSecret(), ap[attempts.ParamHopSecret])
}

func TestNoRetryNoSplitAndIdenticalConfig(t *testing.T) {
	on := transformRetry(t, map[string]interface{}{"enabled": false}, false)
	for _, r := range on.Routes {
		assert.Nil(t, r.Failover, "a retrier switched off changes nothing")
	}
	assert.Len(t, on.Routes, 2)
}

func TestSandboxAttemptRouteUsesTheSandboxUpstream(t *testing.T) {
	rdc := transformRetry(t, map[string]interface{}{"enabled": true}, true)
	var sbFront, sbAttempt *models.Route
	for _, r := range rdc.Routes {
		if r.Vhost != "sandbox.local" || r.Failover == nil {
			continue
		}
		if r.Failover.Role == string(failover.RoleFront) {
			sbFront = r
		} else {
			sbAttempt = r
		}
	}
	require.NotNil(t, sbFront)
	require.NotNil(t, sbAttempt)
	assert.Equal(t, sbFront.Upstream.ClusterKey, sbAttempt.Upstream.ClusterKey)
	assert.NotEqual(t, sbFront.Failover.ChainID, "", "each vhost's route has its own scope token")
}

func TestRetryDeclarationErrorsFailTheDeploy(t *testing.T) {
	_, err := NewRestAPITransformer(testRouterCfg(), &config.Config{}, retryDefs()).Transform(retryRestAPI(map[string]interface{}{"enabled": "yes"}, false))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "POST /orders")
}

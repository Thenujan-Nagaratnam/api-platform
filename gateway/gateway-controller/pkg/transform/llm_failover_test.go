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
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/failover"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// failoverRestAPI mirrors the shape the LlmProxy transformer emits for one
// model-failover operation: an API-level guardrail, a front operation and a
// dispatch operation matched on the chain header.
func failoverRestAPI(params map[string]interface{}) *models.StoredConfig {
	front := map[string]interface{}{failover.ParamRole: "front", failover.ParamChainID: "tok"}
	dispatch := map[string]interface{}{failover.ParamRole: "dispatch", failover.ParamChainID: "tok", failover.ParamHopSecret: "s"}
	for k, v := range params {
		front[k] = v
		dispatch[k] = v
	}
	post := api.OperationMethod("POST")
	headers := []api.OperationHeaderMatch{{Name: failover.HeaderChain, Value: "tok"}}
	apiPolicies := []api.Policy{{Name: "word-count-guardrail", Version: "v1"}}
	frontPolicies := []api.Policy{{Name: failover.PolicyName, Version: "v0", Params: &front}}
	dispatchPolicies := []api.Policy{
		{Name: failover.PolicyName, Version: "v0", Params: &dispatch},
		{Name: "set-headers", Version: "v1"},
	}
	defs := []api.UpstreamDefinition{{Name: "failover-tok-t0", Upstreams: []struct {
		Url    string `json:"url" yaml:"url"`
		Weight *int   `json:"weight,omitempty" yaml:"weight,omitempty"`
	}{{Url: "http://127.0.0.1:8080"}}}}
	spec := api.APIConfigData{
		DisplayName: "mf-proxy",
		Context:     "/mf-proxy",
		Version:     "v1.0",
		Policies:    &apiPolicies,
		Operations: []api.Operation{
			{Method: &post, Path: api.Ptr("/chat/completions"), Policies: &frontPolicies},
			{Match: &api.OperationMatch{Method: post, Path: api.OperationPathMatch{Value: "/chat/completions"}, Headers: &headers}, Policies: &dispatchPolicies},
		},
		UpstreamDefinitions: &defs,
	}
	spec.Upstream.Main = api.Upstream{Url: ptrStr("http://127.0.0.1:8080/openai-a")}
	return &models.StoredConfig{
		UUID:          "mf-proxy",
		Kind:          "LlmProxy",
		Configuration: api.RestAPI{Kind: api.RestAPIKindRestApi, Metadata: api.Metadata{Name: "mf-proxy"}, Spec: spec},
	}
}

func transformFailover(t *testing.T, params map[string]interface{}) *models.RuntimeDeployConfig {
	t.Helper()
	defs := map[string]models.PolicyDefinition{
		failover.PolicyName + "|v0.1.0": {Name: failover.PolicyName, Version: "v0.1.0"},
		"word-count-guardrail|v1.0.0":   {Name: "word-count-guardrail", Version: "v1.0.0"},
		"set-headers|v1.0.0":            {Name: "set-headers", Version: "v1.0.0"},
	}
	rdc, err := NewRestAPITransformer(testRouterCfg(), &config.Config{}, defs).Transform(failoverRestAPI(params))
	require.NoError(t, err)
	require.NoError(t, applyFailoverRoutes(rdc))
	return rdc
}

func failoverRoutes(t *testing.T, rdc *models.RuntimeDeployConfig) (frontKey, dispatchKey string) {
	t.Helper()
	for k, r := range rdc.Routes {
		if r.Failover == nil {
			continue
		}
		switch r.Failover.Role {
		case string(failover.RoleFront):
			frontKey = k
		case string(failover.RoleDispatch):
			dispatchKey = k
		}
	}
	require.NotEmpty(t, frontKey, "front route")
	require.NotEmpty(t, dispatchKey, "dispatch route")
	return frontKey, dispatchKey
}

func TestApplyFailoverRoutes_FrontRouteRetrySettings(t *testing.T) {
	rdc := transformFailover(t, map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{"provider": "a", "model": "m1"},
			map[string]interface{}{"provider": "b", "model": "m2"},
			map[string]interface{}{"provider": "c", "model": "m3"},
		},
		"perAttemptTimeout": "5s",
	})
	frontKey, _ := failoverRoutes(t, rdc)
	fo := rdc.Routes[frontKey].Failover
	assert.Equal(t, "tok", fo.ChainID)
	assert.Equal(t, 2, fo.NumRetries, "num_retries = targets - 1")
	assert.Equal(t, 5*time.Second, fo.PerTryTimeout)
	assert.Equal(t, 17*time.Second, fo.RouteTimeout, "3 x 5s + 2s margin")
	assert.Equal(t, "retriable-headers,connect-failure,reset", fo.RetryOn)
}

func TestApplyFailoverRoutes_TimeoutDisabledDropsReset(t *testing.T) {
	rdc := transformFailover(t, map[string]interface{}{
		"targets":    []interface{}{map[string]interface{}{"provider": "a", "model": "m1"}},
		"failoverOn": map[string]interface{}{"timeout": false},
	})
	frontKey, _ := failoverRoutes(t, rdc)
	assert.Equal(t, "retriable-headers,connect-failure", rdc.Routes[frontKey].Failover.RetryOn)
	assert.Equal(t, 0, rdc.Routes[frontKey].Failover.NumRetries)
}

func TestApplyFailoverRoutes_DispatchChainDropsAPILevelPolicies(t *testing.T) {
	rdc := transformFailover(t, map[string]interface{}{
		"targets": []interface{}{map[string]interface{}{"provider": "a", "model": "m1"}},
	})
	frontKey, dispatchKey := failoverRoutes(t, rdc)

	names := func(key string) []string {
		var out []string
		for _, p := range rdc.PolicyChains[rdc.EffectiveCanonicalChainKey(key, rdc.Routes[key])].Policies {
			out = append(out, p.Name)
		}
		return out
	}
	assert.Equal(t, []string{"word-count-guardrail", failover.PolicyName}, names(frontKey),
		"the front route runs the API-level guardrail once per client request")
	assert.Equal(t, []string{failover.PolicyName, "set-headers"}, names(dispatchKey),
		"the dispatch route runs only per-attempt policies")
	assert.Nil(t, rdc.Routes[frontKey].MatchHeaders)
	require.Len(t, rdc.Routes[dispatchKey].MatchHeaders, 1)
	assert.Equal(t, failover.HeaderChain, rdc.Routes[dispatchKey].MatchHeaders[0].Name)
}

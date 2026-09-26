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

package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/failover"
)

func failoverProxy(opPolicies []api.OperationPolicy, global []api.Policy) *api.LLMProxyConfiguration {
	spec := api.LLMProxyConfigData{
		DisplayName:         "mf-proxy",
		Version:             "v1.0",
		Provider:            &api.LLMProxyProvider{Id: "openai-a"},
		AdditionalProviders: &[]api.LLMProxyAdditionalProvider{{Id: "openai-b"}, {Id: "anthropic", As: strPtr("claude")}},
	}
	if opPolicies != nil {
		spec.OperationPolicies = &opPolicies
	}
	if global != nil {
		spec.GlobalPolicies = &global
	}
	return &api.LLMProxyConfiguration{
		ApiVersion: api.LLMProxyConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProxyConfigurationKindLlmProxy,
		Metadata:   api.Metadata{Name: "mf-proxy"},
		Spec:       spec,
	}
}

func failoverOp(params map[string]interface{}) api.OperationPolicy {
	return api.OperationPolicy{
		Name:    failover.PolicyName,
		Version: "v0",
		Paths: []api.OperationPolicyPath{{
			Path: "/chat/completions", Methods: []api.OperationPolicyPathMethods{"POST"}, Params: params,
		}},
	}
}

func targets(providers ...string) []interface{} {
	out := make([]interface{}, len(providers))
	for i, p := range providers {
		out[i] = map[string]interface{}{"provider": p, "model": "m" + p}
	}
	return out
}

func failoverErrors(t *testing.T, proxy *api.LLMProxyConfiguration) []ValidationError {
	t.Helper()
	var out []ValidationError
	for _, e := range NewLLMValidator().Validate(proxy) {
		if strings.Contains(e.Field, "Policies") || strings.Contains(e.Field, "policies") || strings.Contains(e.Message, failover.PolicyName) {
			out = append(out, e)
		}
	}
	return out
}

func TestValidateModelFailover_Accepts(t *testing.T) {
	proxy := failoverProxy([]api.OperationPolicy{failoverOp(map[string]interface{}{
		// Targets may name a provider by id or by alias.
		"targets":           targets("openai-a", "openai-b", "claude"),
		"perAttemptTimeout": "10s",
		"failoverOn":        map[string]interface{}{"statusCodes": []interface{}{float64(429), float64(529)}, "timeout": false},
	})}, nil)
	assert.Empty(t, failoverErrors(t, proxy))
}

func TestValidateModelFailover_Rejects(t *testing.T) {
	cases := map[string]struct {
		params map[string]interface{}
		field  string
	}{
		"unattached provider": {map[string]interface{}{"targets": targets("openai-a", "gemini")}, ".targets[1].provider"},
		"internal key":        {map[string]interface{}{"targets": targets("openai-a"), "_role": "front"}, ".params"},
		"status 404":          {map[string]interface{}{"targets": targets("openai-a"), "failoverOn": map[string]interface{}{"statusCodes": []interface{}{float64(404)}}}, ".params"},
		"zero timeout":        {map[string]interface{}{"targets": targets("openai-a"), "perAttemptTimeout": "0s"}, ".params"},
		"threshold zero":      {map[string]interface{}{"targets": targets("openai-a"), "suspendAfterConsecutiveFailures": float64(0)}, ".params"},
		"no targets":          {map[string]interface{}{}, ".params"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			errs := failoverErrors(t, failoverProxy([]api.OperationPolicy{failoverOp(tc.params)}, nil))
			if assert.NotEmpty(t, errs) {
				assert.True(t, strings.HasPrefix(errs[0].Field, "spec.operationPolicies[0].paths[0]"), errs[0].Field)
				assert.True(t, strings.HasSuffix(errs[0].Field, tc.field), errs[0].Field)
			}
		})
	}
}

func TestValidateModelFailover_RejectsProviderSelectingPolicy(t *testing.T) {
	router := api.OperationPolicy{Name: "llm-header-router", Version: "v0", Paths: []api.OperationPolicyPath{{
		Path: "/chat/completions", Methods: []api.OperationPolicyPathMethods{"POST"}, Params: map[string]interface{}{},
	}}}
	errs := failoverErrors(t, failoverProxy([]api.OperationPolicy{
		failoverOp(map[string]interface{}{"targets": targets("openai-a")}), router,
	}, nil))
	if assert.Len(t, errs, 1) {
		assert.Equal(t, "spec.operationPolicies[1].paths[0]", errs[0].Field)
	}
}

func TestValidateModelFailover_GlobalAtMostOnce(t *testing.T) {
	p := map[string]interface{}{"targets": targets("openai-a")}
	pol := api.Policy{Name: failover.PolicyName, Version: "v0", Params: &p}
	errs := failoverErrors(t, failoverProxy(nil, []api.Policy{pol, pol}))
	if assert.NotEmpty(t, errs) {
		assert.Equal(t, "spec.globalPolicies", errs[0].Field)
	}
}

func TestValidateModelFailover_NoFailoverNoChecks(t *testing.T) {
	router := api.OperationPolicy{Name: "llm-header-router", Version: "v0", Paths: []api.OperationPolicyPath{{
		Path: "/chat/completions", Methods: []api.OperationPolicyPathMethods{"POST"}, Params: map[string]interface{}{},
	}}}
	assert.Empty(t, validateModelFailover(&failoverProxy([]api.OperationPolicy{router}, nil).Spec, nil),
		"a provider-selecting policy on its own is fine")
}

func strPtr(s string) *string { return &s }

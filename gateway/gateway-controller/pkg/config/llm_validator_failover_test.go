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

// chains is one chain for model "primary", falling back to "m<provider>" on
// each named provider in order.
func chains(providers ...string) []interface{} {
	fallbacks := make([]interface{}, len(providers))
	for i, p := range providers {
		fallbacks[i] = map[string]interface{}{"provider": p, "model": "m" + p}
	}
	return []interface{}{map[string]interface{}{"primary": map[string]interface{}{"model": "primary"}, "fallbacks": fallbacks}}
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
		// Fallbacks may name a provider by id or by alias.
		"chains":            chains("openai-a", "openai-b", "claude"),
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
		"unattached provider": {map[string]interface{}{"chains": chains("openai-a", "gemini")}, ".chains[0].fallbacks[1].provider"},
		"internal key":        {map[string]interface{}{"chains": chains("openai-a"), "_role": "front"}, ".params"},
		"status 404":          {map[string]interface{}{"chains": chains("openai-a"), "failoverOn": map[string]interface{}{"statusCodes": []interface{}{float64(404)}}}, ".params"},
		"zero timeout":        {map[string]interface{}{"chains": chains("openai-a"), "perAttemptTimeout": "0s"}, ".params"},
		"threshold zero":      {map[string]interface{}{"chains": chains("openai-a"), "suspendAfterConsecutiveFailures": float64(0)}, ".params"},
		"no chains":           {map[string]interface{}{}, ".params"},
		"old targets":         {map[string]interface{}{"targets": []interface{}{map[string]interface{}{"provider": "openai-a", "model": "m"}}}, ".params"},
		"primary provider": {map[string]interface{}{"chains": []interface{}{map[string]interface{}{
			"primary":   map[string]interface{}{"provider": "openai-b", "model": "m"},
			"fallbacks": []interface{}{map[string]interface{}{"model": "n"}},
		}}}, ".params"},
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
		failoverOp(map[string]interface{}{"chains": chains("openai-a")}), router,
	}, nil))
	if assert.Len(t, errs, 1) {
		assert.Equal(t, "spec.operationPolicies[1].paths[0]", errs[0].Field)
	}
}

func TestValidateModelFailover_GlobalAtMostOnce(t *testing.T) {
	p := map[string]interface{}{"chains": chains("openai-a")}
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

func failoverProviderConfig(opParams map[string]interface{}, extra ...api.OperationPolicy) *api.LLMProviderConfiguration {
	ops := append([]api.OperationPolicy{failoverOp(opParams)}, extra...)
	return &api.LLMProviderConfiguration{
		ApiVersion: api.LLMProviderConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProviderConfigurationKindLlmProvider,
		Metadata:   api.Metadata{Name: "openai"},
		Spec: api.LLMProviderConfigData{
			DisplayName:       "openai",
			Version:           "v1.0",
			Template:          "openai",
			Upstream:          api.LLMProviderConfigData_Upstream{Url: strPtr("https://api.example.com")},
			AccessControl:     api.LLMAccessControl{Mode: api.AllowAll},
			OperationPolicies: &ops,
		},
	}
}

func providerFailoverErrors(t *testing.T, cfg *api.LLMProviderConfiguration) []ValidationError {
	t.Helper()
	var out []ValidationError
	for _, e := range NewLLMValidator().Validate(cfg) {
		if strings.Contains(e.Field, "olicies") {
			out = append(out, e)
		}
	}
	return out
}

// modelChain is one chain: primary, then fallback models in order.
func modelChain(primary string, fallbacks ...string) []interface{} {
	fl := make([]interface{}, len(fallbacks))
	for i, n := range fallbacks {
		fl[i] = map[string]interface{}{"model": n}
	}
	return []interface{}{map[string]interface{}{"primary": map[string]interface{}{"model": primary}, "fallbacks": fl}}
}

func TestValidateProviderModelFailover_Accepts(t *testing.T) {
	params := map[string]interface{}{"chains": []interface{}{map[string]interface{}{
		"primary":   map[string]interface{}{"model": "gpt-4o"},
		"fallbacks": []interface{}{map[string]interface{}{"provider": "openai", "model": "gpt-4o-mini"}},
	}}}
	assert.Empty(t, providerFailoverErrors(t, failoverProviderConfig(params)))
}

func TestValidateProviderModelFailover_Rejects(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"another provider": {"chains": []interface{}{map[string]interface{}{"primary": map[string]interface{}{"model": "m"}, "fallbacks": []interface{}{map[string]interface{}{"provider": "anthropic", "model": "c"}}}}},
		"duplicate model":  {"chains": modelChain("m", "m")},
		"bad timeout":      {"chains": modelChain("m", "n"), "perAttemptTimeout": "0s"},
		"internal key":     {"chains": modelChain("m", "n"), "_routeToTarget": false},
		"no chains":        {},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			errs := providerFailoverErrors(t, failoverProviderConfig(params))
			if assert.NotEmpty(t, errs) {
				assert.Equal(t, "spec.operationPolicies[0].paths[0].params", errs[0].Field)
			}
		})
	}
}

func roundRobinOp(path string, providers ...string) api.OperationPolicy {
	models := []interface{}{map[string]interface{}{"model": "m"}, map[string]interface{}{"model": "n"}}
	for _, p := range providers {
		models = append(models, map[string]interface{}{"model": "x", "provider": p})
	}
	return api.OperationPolicy{Name: "model-round-robin", Version: "v1", Paths: []api.OperationPolicyPath{{
		Path: path, Methods: []api.OperationPolicyPathMethods{"POST"}, Params: map[string]interface{}{"models": models},
	}}}
}

func TestValidateModelFailover_RoundRobinPlacement(t *testing.T) {
	fo := failoverOp(map[string]interface{}{"chains": chains("openai-b")})
	cases := map[string]struct {
		ops   []api.OperationPolicy
		field string // "" = accepted
	}{
		"model-only round-robin before failover": {[]api.OperationPolicy{roundRobinOp("/chat/completions"), fo}, ""},
		"round-robin after failover":             {[]api.OperationPolicy{fo, roundRobinOp("/chat/completions")}, "spec.operationPolicies[1].paths[0]"},
		"round-robin naming a provider":          {[]api.OperationPolicy{roundRobinOp("/chat/completions", "openai-b"), fo}, "spec.operationPolicies[0].paths[0]"},
		"round-robin on another operation":       {[]api.OperationPolicy{fo, roundRobinOp("/embeddings", "openai-b")}, ""},
		"round-robin on a covering wildcard":     {[]api.OperationPolicy{fo, roundRobinOp("/*")}, "spec.operationPolicies[1].paths[0]"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			errs := failoverErrors(t, failoverProxy(tc.ops, nil))
			if tc.field == "" {
				assert.Empty(t, errs)
				return
			}
			if assert.Len(t, errs, 1) {
				assert.Equal(t, tc.field, errs[0].Field)
			}
		})
	}
}

func TestValidateProviderModelFailover_RoundRobinMustComeFirst(t *testing.T) {
	errs := providerFailoverErrors(t, failoverProviderConfig(map[string]interface{}{"chains": modelChain("m", "n")}, roundRobinOp("/chat/completions")))
	if assert.Len(t, errs, 1) {
		assert.Equal(t, "spec.operationPolicies[1].paths[0]", errs[0].Field)
		assert.Contains(t, errs[0].Message, "must come before")
	}
}

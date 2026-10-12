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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

func modelFailoverDefs() map[string]models.PolicyDefinition {
	return map[string]models.PolicyDefinition{
		"model-failover|v0.1.0": {Name: "model-failover", Version: "v0.1.0"},
	}
}

func proxyWithFailoverParams(params map[string]interface{}) *api.LLMProxyConfiguration {
	return &api.LLMProxyConfiguration{
		Spec: api.LLMProxyConfigData{
			Provider: api.LLMProxyProvider{Id: "openai-primary"},
			OperationPolicies: &[]api.OperationPolicy{{
				Name:    "model-failover",
				Version: "v0",
				Paths: []api.OperationPolicyPath{{
					Path:    "/chat/completions",
					Methods: []api.OperationPolicyPathMethods{"POST"},
					Params:  params,
				}},
			}},
		},
	}
}

func validFailoverParams() map[string]interface{} {
	return map[string]interface{}{
		"targets": []interface{}{map[string]interface{}{
			"model":     "gpt-4o",
			"fallbacks": []interface{}{map[string]interface{}{"model": "claude", "provider": "anthropic"}},
		}},
		"statusCodes":           []interface{}{float64(429), float64(503)},
		"suspendDuration":       float64(30),
		"suspendAfterFailures":  float64(2),
		"maxSuspendMultiplier":  float64(8),
		"recoverAfterSuccesses": float64(3),
		"probeTimeout":          float64(60),
		"perTryTimeout":         float64(15),
	}
}

func TestValidateLLMProxyPolicies_ModelFailoverKnownParamsAccepted(t *testing.T) {
	validator := NewPolicyValidator(modelFailoverDefs())

	errors := validator.ValidateLLMProxyPolicies(proxyWithFailoverParams(validFailoverParams()))

	assert.Empty(t, errors)
}

func TestValidateLLMProxyPolicies_ModelFailoverUnknownParamsRejected(t *testing.T) {
	validator := NewPolicyValidator(modelFailoverDefs())
	params := validFailoverParams()
	params["perTryTimeoutMs"] = float64(15000)
	target := params["targets"].([]interface{})[0].(map[string]interface{})
	target["upstreamDefinition"] = "openai-eu"
	fallback := target["fallbacks"].([]interface{})[0].(map[string]interface{})
	fallback["region"] = "eu"

	errors := validator.ValidateLLMProxyPolicies(proxyWithFailoverParams(params))

	require.Len(t, errors, 3)
	fields := []string{errors[0].Field, errors[1].Field, errors[2].Field}
	assert.Contains(t, fields, "spec.operationPolicies[0].paths[0].params.perTryTimeoutMs")
	assert.Contains(t, fields, "spec.operationPolicies[0].paths[0].params.targets[0].upstreamDefinition")
	assert.Contains(t, fields, "spec.operationPolicies[0].paths[0].params.targets[0].fallbacks[0].region")
}

func TestValidateLLMProxyPolicies_ModelFailoverUnderProviderRejected(t *testing.T) {
	validator := NewPolicyValidator(modelFailoverDefs())
	cfg := proxyWithFailoverParams(validFailoverParams())
	cfg.Spec.Provider.Policies = &[]api.Policy{{Name: "model-failover", Version: "v0"}}
	cfg.Spec.AdditionalProviders = &[]api.LLMProxyAdditionalProvider{{
		Id:       "anthropic-provider",
		Policies: &[]api.Policy{{Name: "model-failover", Version: "v0"}},
	}}

	errors := validator.ValidateLLMProxyPolicies(cfg)

	require.Len(t, errors, 2)
	assert.Equal(t, "spec.provider.policies[0]", errors[0].Field)
	assert.Equal(t, "spec.additionalProviders[0].policies[0]", errors[1].Field)
	assert.Contains(t, errors[0].Message, "attach it in operationPolicies")
}

func TestValidateRestAPIPolicies_ModelFailoverUnderUpstreamRejected(t *testing.T) {
	validator := NewPolicyValidator(modelFailoverDefs())
	cfg := &api.RestAPI{}
	cfg.Spec.Upstream.Main.Policies = &[]api.Policy{{Name: "model-failover", Version: "v0"}}

	errors := validator.ValidateRestAPIPolicies(cfg)

	require.Len(t, errors, 1)
	assert.Equal(t, "spec.upstream.main.policies[0]", errors[0].Field)
}

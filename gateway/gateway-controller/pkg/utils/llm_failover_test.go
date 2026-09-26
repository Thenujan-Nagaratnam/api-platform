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

package utils

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/failover"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/storage"
)

// newFailoverTestTransformer saves three providers (two OpenAI "regions" and
// Anthropic) and returns a transformer over them.
func newFailoverTestTransformer(t *testing.T) *LLMProviderTransformer {
	t.Helper()
	store := storage.NewConfigStore()
	db := newTestSQLiteStorage(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, db.SaveLLMProviderTemplate(&models.StoredLLMProviderTemplate{
		UUID: "0000-db-template-id-0000-0000000000f0",
		Configuration: api.LLMProviderTemplate{
			ApiVersion: api.LLMProviderTemplateApiVersionGatewayApiPlatformWso2Comv1,
			Kind:       api.LLMProviderTemplateKindLlmProviderTemplate,
			Metadata:   api.Metadata{Name: "openai"},
			Spec:       api.LLMProviderTemplateData{DisplayName: "openai"},
		},
	}))
	for _, name := range []string{"openai-a", "openai-b", "anthropic"} {
		require.NoError(t, db.SaveConfig(&models.StoredConfig{
			UUID:        name + "-uuid",
			Kind:        string(api.LLMProviderConfigurationKindLlmProvider),
			Handle:      name,
			DisplayName: name,
			Version:     "v1.0",
			SourceConfiguration: api.LLMProviderConfiguration{
				ApiVersion: api.LLMProviderConfigurationApiVersionGatewayApiPlatformWso2Comv1,
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: name},
				Spec: api.LLMProviderConfigData{
					DisplayName:   name,
					Version:       "v1.0",
					Context:       stringPtr("/" + name),
					Template:      "openai",
					Upstream:      api.LLMProviderConfigData_Upstream{Url: stringPtr("https://example.com")},
					AccessControl: api.LLMAccessControl{Mode: api.AllowAll},
				},
			},
			DesiredState: models.StateDeployed,
		}))
	}
	return NewLLMProviderTransformer(store, db, &config.RouterConfig{ListenerPort: 8080}, newTestPolicyVersionResolver())
}

func failoverTestProxy(policyParams map[string]interface{}, global bool) *api.LLMProxyConfiguration {
	apiKey := func(header, value string) *api.LLMUpstreamAuth {
		return &api.LLMUpstreamAuth{Type: api.LLMUpstreamAuthTypeApiKey, Header: stringPtr(header), Value: stringPtr(value)}
	}
	spec := api.LLMProxyConfigData{
		DisplayName: "mf-proxy",
		Version:     "v1.0",
		Provider:    &api.LLMProxyProvider{Id: "openai-a", Auth: apiKey("Authorization", "Bearer key-a")},
		AdditionalProviders: &[]api.LLMProxyAdditionalProvider{
			{Id: "openai-b", Auth: apiKey("Authorization", "Bearer key-b")},
			{
				Id:   "anthropic",
				Auth: apiKey("x-api-key", "key-anthropic"),
				Transformer: &api.LLMProxyTransformer{
					Type:    "openai-to-anthropic-transformer",
					Version: "v0",
					Params:  &map[string]interface{}{"model": "authored-model"},
				},
			},
		},
	}
	pol := api.Policy{Name: failover.PolicyName, Version: "v0", Params: &policyParams}
	if global {
		spec.GlobalPolicies = &[]api.Policy{pol}
	} else {
		spec.Policies = &[]api.LLMPolicy{{
			Name:    failover.PolicyName,
			Version: "v0",
			Paths: []api.LLMPolicyPath{{
				Path:    "/chat/completions",
				Methods: []api.LLMPolicyPathMethods{"POST"},
				Params:  policyParams,
			}},
		}}
	}
	return &api.LLMProxyConfiguration{
		ApiVersion: api.LLMProxyConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProxyConfigurationKindLlmProxy,
		Metadata:   api.Metadata{Name: "mf-proxy"},
		Spec:       spec,
	}
}

func threeTargets() map[string]interface{} {
	return map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{"provider": "openai-a", "model": "gpt-4o"},
			map[string]interface{}{"provider": "openai-b", "model": "gpt-4o"},
			map[string]interface{}{"provider": "anthropic", "model": "claude-sonnet-4-5"},
		},
	}
}

// splitFailoverOps returns the POST /chat/completions front and dispatch operations.
func splitFailoverOps(t *testing.T, ops []api.Operation) (front, dispatch *api.Operation) {
	t.Helper()
	for i := range ops {
		op := &ops[i]
		if op.EffectiveMethod() != "POST" || op.EffectivePath() != "/chat/completions" {
			continue
		}
		if len(op.EffectiveHeaders()) > 0 {
			dispatch = op
		} else {
			front = op
		}
	}
	require.NotNil(t, front, "front operation")
	require.NotNil(t, dispatch, "dispatch operation")
	return front, dispatch
}

func policyNamed(policies *[]api.Policy, name string) *api.Policy {
	if policies == nil {
		return nil
	}
	for i := range *policies {
		if (*policies)[i].Name == name {
			return &(*policies)[i]
		}
	}
	return nil
}

func TestTransformProxy_FailoverSplitsFrontAndDispatch(t *testing.T) {
	result, err := newFailoverTestTransformer(t).Transform(failoverTestProxy(threeTargets(), false), &api.RestAPI{})
	require.NoError(t, err)
	front, dispatch := splitFailoverOps(t, result.Spec.Operations)

	// Front: the failover instance in the front role, and none of the
	// provider-scoped policies — they would convert or authenticate the
	// request before it reaches the dispatch hop.
	fp := policyNamed(front.Policies, failover.PolicyName)
	require.NotNil(t, fp)
	assert.Equal(t, string(failover.RoleFront), (*fp.Params)[failover.ParamRole])
	token, _ := (*fp.Params)[failover.ParamChainID].(string)
	require.NotEmpty(t, token)
	for _, p := range *front.Policies {
		assert.NotEqual(t, "openai-to-anthropic-transformer", p.Name, "no transformer on the front route")
		assert.Nil(t, p.ExecutionCondition, "no selected_provider-gated policy on the front route: %s", p.Name)
		assert.False(t, hasInternalLoopbackMarkerPolicy([]api.Policy{p}), "no loopback marker on the front route")
	}
	_, hasHop := (*fp.Params)[failover.ParamHopSecret]
	assert.False(t, hasHop, "the hop secret is only given to the dispatch role")

	// Dispatch: matched on the chain header, failover first, then the target
	// transformer, the per-target credentials, and the loopback marker.
	headers := dispatch.EffectiveHeaders()
	require.Len(t, headers, 1)
	assert.Equal(t, failover.HeaderChain, headers[0].Name)
	assert.Equal(t, token, headers[0].Value)
	pols := *dispatch.Policies
	assert.Equal(t, failover.PolicyName, pols[0].Name)
	assert.Equal(t, string(failover.RoleDispatch), (*pols[0].Params)[failover.ParamRole])
	assert.Equal(t, failover.HopSecret(), (*pols[0].Params)[failover.ParamHopSecret])
	assert.Equal(t, []interface{}{true, true, false}, (*pols[0].Params)[failover.ParamTargetNative])
	ids := (*pols[0].Params)[failover.ParamTargetIDs].([]interface{})
	require.Len(t, ids, 3)
	assert.True(t, hasInternalLoopbackMarkerPolicy([]api.Policy{pols[len(pols)-1]}), "loopback marker last")

	tr := policyNamed(dispatch.Policies, "openai-to-anthropic-transformer")
	require.NotNil(t, tr)
	upstreamT2 := failover.TargetUpstreamName(ids[2].(string))
	assert.Equal(t, upstreamT2, (*tr.Params)["providerId"])
	assert.Equal(t, "claude-sonnet-4-5", (*tr.Params)["model"], "the target's model overrides the attachment's")
	require.NotNil(t, tr.ExecutionCondition)
	assert.Equal(t, "'selected_provider' in request.Metadata && request.Metadata['selected_provider'] == '"+upstreamT2+"'", *tr.ExecutionCondition)
}

func TestTransformProxy_FailoverCredentialsAreIsolatedPerTarget(t *testing.T) {
	result, err := newFailoverTestTransformer(t).Transform(failoverTestProxy(threeTargets(), false), &api.RestAPI{})
	require.NoError(t, err)
	_, dispatch := splitFailoverOps(t, result.Spec.Operations)
	ids := (*policyNamed(dispatch.Policies, failover.PolicyName).Params)[failover.ParamTargetIDs].([]interface{})

	want := map[string]string{
		failover.TargetUpstreamName(ids[0].(string)): "Bearer key-a",
		failover.TargetUpstreamName(ids[1].(string)): "Bearer key-b",
		failover.TargetUpstreamName(ids[2].(string)): "key-anthropic",
	}
	got := map[string]string{}
	for _, p := range *dispatch.Policies {
		if p.ExecutionCondition == nil || p.Name == "openai-to-anthropic-transformer" {
			continue
		}
		// Every credential is gated on exactly one target and never fires
		// when no provider was selected.
		assert.True(t, strings.HasPrefix(*p.ExecutionCondition, "'selected_provider' in request.Metadata && "), *p.ExecutionCondition)
		for upstream := range want {
			if strings.HasSuffix(*p.ExecutionCondition, "== '"+upstream+"'") {
				got[upstream] = firstRequestHeaderValue(t, p.Params)
			}
		}
	}
	assert.Equal(t, want, got)
}

func TestTransformProxy_FailoverUpstreamsPointAtEachTargetsProviderRoute(t *testing.T) {
	result, err := newFailoverTestTransformer(t).Transform(failoverTestProxy(threeTargets(), false), &api.RestAPI{})
	require.NoError(t, err)
	_, dispatch := splitFailoverOps(t, result.Spec.Operations)
	ids := (*policyNamed(dispatch.Policies, failover.PolicyName).Params)[failover.ParamTargetIDs].([]interface{})

	defs := map[string]api.UpstreamDefinition{}
	for _, d := range *result.Spec.UpstreamDefinitions {
		defs[d.Name] = d
	}
	for k, provider := range []string{"openai-a", "openai-b", "anthropic"} {
		d, ok := defs[failover.TargetUpstreamName(ids[k].(string))]
		require.True(t, ok, "upstream for target %d", k)
		require.NotNil(t, d.BasePath)
		assert.Equal(t, "/"+provider, *d.BasePath)
		assert.Equal(t, "http://127.0.0.1:8080", d.Upstreams[0].Url)
	}
}

func TestTransformProxy_NonFailoverOperationsKeepProviderPolicies(t *testing.T) {
	result, err := newFailoverTestTransformer(t).Transform(failoverTestProxy(threeTargets(), false), &api.RestAPI{})
	require.NoError(t, err)
	for _, op := range result.Spec.Operations {
		if op.EffectivePath() == "/chat/completions" || op.Policies == nil {
			continue
		}
		assert.True(t, hasInternalLoopbackMarkerPolicy(*op.Policies), "%s %s keeps today's proxy behaviour", op.EffectiveMethod(), op.EffectivePath())
		assert.Nil(t, policyNamed(op.Policies, failover.PolicyName))
	}
}

func TestTransformProxy_GlobalFailoverAppliesToEveryOperation(t *testing.T) {
	result, err := newFailoverTestTransformer(t).Transform(failoverTestProxy(threeTargets(), true), &api.RestAPI{})
	require.NoError(t, err)
	if result.Spec.Policies != nil {
		assert.Nil(t, policyNamed(result.Spec.Policies, failover.PolicyName), "never left at API level, where it would also run on dispatch routes")
	}
	fronts, dispatches := 0, 0
	for _, op := range result.Spec.Operations {
		if len(op.EffectiveHeaders()) > 0 {
			dispatches++
			continue
		}
		p := policyNamed(op.Policies, failover.PolicyName)
		require.NotNil(t, p, "%s %s", op.EffectiveMethod(), op.EffectivePath())
		fronts++
	}
	assert.Equal(t, fronts, dispatches)
	assert.Greater(t, fronts, 0)
}

func TestTransformProxy_FailoverRejects(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"unknown provider": {"targets": []interface{}{map[string]interface{}{"provider": "nope", "model": "m"}}},
		"internal key":     {"targets": threeTargets()["targets"], failover.ParamRole: "dispatch"},
		"duplicate target": {"targets": []interface{}{
			map[string]interface{}{"provider": "openai-a", "model": "m"},
			map[string]interface{}{"provider": "openai-a", "model": "m"},
		}},
		"bad timeout": {"targets": threeTargets()["targets"], "perAttemptTimeout": "0s"},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newFailoverTestTransformer(t).Transform(failoverTestProxy(params, false), &api.RestAPI{})
			require.Error(t, err)
		})
	}
}

func TestTransformProxy_FailoverUsesEachAttachmentsOwnTransformer(t *testing.T) {
	cases := []struct {
		transformer string
		params      map[string]interface{}
	}{
		{"openai-to-anthropic-transformer", map[string]interface{}{"model": "authored"}},
		{"openai-to-azure-openai-transformer", map[string]interface{}{"apiVersion": "2024-02-15-preview"}},
		{"openai-to-bedrock-transformer", map[string]interface{}{"model": "authored"}},
		{"openai-to-gemini-transformer", map[string]interface{}{"model": "authored", "apiVersion": "v1beta"}},
		{"openai-to-mistral-transformer", map[string]interface{}{"model": "authored"}},
	}
	for _, tc := range cases {
		t.Run(tc.transformer, func(t *testing.T) {
			proxy := failoverTestProxy(threeTargets(), false)
			params := tc.params
			(*proxy.Spec.AdditionalProviders)[1].Transformer = &api.LLMProxyTransformer{
				Type: tc.transformer, Version: "v0", Params: &params,
			}
			result, err := newFailoverTestTransformer(t).Transform(proxy, &api.RestAPI{})
			require.NoError(t, err)
			_, dispatch := splitFailoverOps(t, result.Spec.Operations)
			ids := (*policyNamed(dispatch.Policies, failover.PolicyName).Params)[failover.ParamTargetIDs].([]interface{})

			tr := policyNamed(dispatch.Policies, tc.transformer)
			require.NotNil(t, tr, "the attachment's transformer runs on the dispatch route")
			assert.Equal(t, failover.TargetUpstreamName(ids[2].(string)), (*tr.Params)["providerId"])
			assert.Equal(t, "claude-sonnet-4-5", (*tr.Params)["model"], "the failover target's model is what the transformer requests")
			for k, v := range tc.params {
				if k != "model" {
					assert.Equal(t, v, (*tr.Params)[k], "other authored transformer params are kept")
				}
			}
			// The authored attachment params must not be mutated by the copy.
			if m, ok := params["model"]; ok {
				assert.Equal(t, "authored", m)
			}
		})
	}
}

func TestTransformProxy_FailoverTargetMayRepeatAProviderWithAnotherModel(t *testing.T) {
	params := map[string]interface{}{"targets": []interface{}{
		map[string]interface{}{"provider": "anthropic", "model": "claude-opus"},
		map[string]interface{}{"provider": "anthropic", "model": "claude-sonnet"},
	}}
	result, err := newFailoverTestTransformer(t).Transform(failoverTestProxy(params, false), &api.RestAPI{})
	require.NoError(t, err)
	_, dispatch := splitFailoverOps(t, result.Spec.Operations)

	models := map[string]string{}
	for _, p := range *dispatch.Policies {
		if p.Name == "openai-to-anthropic-transformer" {
			models[(*p.Params)["providerId"].(string)] = (*p.Params)["model"].(string)
		}
	}
	assert.Len(t, models, 2, "one transformer instance per target, each on its own upstream")
	got := make([]string, 0, len(models))
	for _, m := range models {
		got = append(got, m)
	}
	assert.ElementsMatch(t, []string{"claude-opus", "claude-sonnet"}, got)
}

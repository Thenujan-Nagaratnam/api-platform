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
	"encoding/json"
	"fmt"
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

// threeTargets is one chain: gpt-4o on the proxy's primary (openai-a), then
// gpt-4o on openai-b, then claude-sonnet-4-5 on anthropic. Flattened, those
// are targets 0-2; the pass-through target (openai-a) is 3.
func threeTargets() map[string]interface{} {
	return map[string]interface{}{"chains": []interface{}{chainOf("gpt-4o",
		map[string]interface{}{"provider": "openai-b", "model": "gpt-4o"},
		map[string]interface{}{"provider": "anthropic", "model": "claude-sonnet-4-5"},
	)}}
}

func chainOf(primary string, fallbacks ...map[string]interface{}) map[string]interface{} {
	fl := make([]interface{}, len(fallbacks))
	for i, f := range fallbacks {
		fl[i] = f
	}
	return map[string]interface{}{"primary": map[string]interface{}{"model": primary}, "fallbacks": fl}
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
	assert.Equal(t, []interface{}{true, true, false, true}, (*pols[0].Params)[failover.ParamTargetNative], "chain targets, then the pass-through target")
	ids := (*pols[0].Params)[failover.ParamTargetIDs].([]interface{})
	require.Len(t, ids, 4)
	resolved := (*pols[0].Params)["chains"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, "openai-a", resolved["primary"].(map[string]interface{})["provider"], "the primary runs on the proxy's primary provider")
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
		failover.TargetUpstreamName(ids[3].(string)): "Bearer key-a",
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
	for k, provider := range []string{"openai-a", "openai-b", "anthropic", "openai-a"} {
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
		"unknown provider": {"chains": []interface{}{chainOf("m", map[string]interface{}{"provider": "nope", "model": "m"})}},
		"internal key":     {"chains": threeTargets()["chains"], failover.ParamRole: "dispatch"},
		"duplicate target": {"chains": []interface{}{chainOf("m", map[string]interface{}{"provider": "openai-a", "model": "m"})}},
		"bad timeout":      {"chains": threeTargets()["chains"], "perAttemptTimeout": "0s"},
		"old targets":      {"targets": []interface{}{map[string]interface{}{"provider": "openai-a", "model": "m"}}},
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
	params := map[string]interface{}{"chains": []interface{}{chainOf("gpt-4o",
		map[string]interface{}{"provider": "anthropic", "model": "claude-opus"},
		map[string]interface{}{"provider": "anthropic", "model": "claude-sonnet"},
	)}}
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

// newProviderFailoverTransformer saves an openai-style template that reads the
// model from the body and a gemini-style one that reads it from the path.
func newProviderFailoverTransformer(t *testing.T) *LLMProviderTransformer {
	t.Helper()
	store := storage.NewConfigStore()
	db := newTestSQLiteStorage(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for name, id := range map[string]*api.ExtractionIdentifier{
		"openai": {Location: api.ExtractionIdentifierLocation("payload"), Identifier: "$.model"},
		"gemini": {Location: api.ExtractionIdentifierLocation("pathParam"), Identifier: "models/([a-zA-Z0-9.\\-]+)"},
	} {
		require.NoError(t, db.SaveLLMProviderTemplate(&models.StoredLLMProviderTemplate{
			UUID: "0000-db-template-id-0000-0000000000" + map[string]string{"openai": "f1", "gemini": "f2"}[name],
			Configuration: api.LLMProviderTemplate{
				ApiVersion: api.LLMProviderTemplateApiVersionGatewayApiPlatformWso2Comv1,
				Kind:       api.LLMProviderTemplateKindLlmProviderTemplate,
				Metadata:   api.Metadata{Name: name},
				Spec:       api.LLMProviderTemplateData{DisplayName: name, RequestModel: id},
			},
		}))
	}
	return NewLLMProviderTransformer(store, db, &config.RouterConfig{ListenerPort: 8080}, newTestPolicyVersionResolver())
}

func failoverProvider(template string, params map[string]interface{}, global bool) *api.LLMProviderConfiguration {
	return failoverProviderAt(template, "/chat/completions", params, global)
}

func failoverProviderAt(template, path string, params map[string]interface{}, global bool) *api.LLMProviderConfiguration {
	spec := api.LLMProviderConfigData{
		DisplayName:   "openai",
		Version:       "v1.0",
		Context:       stringPtr("/openai"),
		Template:      template,
		AccessControl: api.LLMAccessControl{Mode: api.AllowAll},
	}
	// The upstream's auth block is an anonymous struct; build it from JSON.
	if err := json.Unmarshal([]byte(`{"url":"https://api.example.com","auth":{"type":"api-key","header":"Authorization","value":"Bearer provider-key"}}`), &spec.Upstream); err != nil {
		panic(err)
	}
	pol := api.Policy{Name: failover.PolicyName, Version: "v0", Params: &params}
	if global {
		spec.GlobalPolicies = &[]api.Policy{pol}
	} else {
		spec.Policies = &[]api.LLMPolicy{{
			Name: failover.PolicyName, Version: "v0",
			Paths: []api.LLMPolicyPath{{Path: path, Methods: []api.LLMPolicyPathMethods{"POST"}, Params: params}},
		}}
	}
	return &api.LLMProviderConfiguration{
		ApiVersion: api.LLMProviderConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProviderConfigurationKindLlmProvider,
		Metadata:   api.Metadata{Name: "openai"},
		Spec:       spec,
	}
}

func twoModels() map[string]interface{} {
	return map[string]interface{}{"chains": []interface{}{chainOf("gpt-4o", map[string]interface{}{"provider": "openai", "model": "gpt-4o-mini"})}}
}

func isUpstreamAuth(p api.Policy) bool {
	return p.Name == "set-headers" && p.ExecutionCondition == nil && firstRequestHeaderValueOrEmpty(p.Params) == "Bearer provider-key"
}

func firstRequestHeaderValueOrEmpty(params *map[string]interface{}) string {
	defer func() { _ = recover() }()
	if params == nil {
		return ""
	}
	req, _ := (*params)["request"].(map[string]interface{})
	hs, _ := req["headers"].([]interface{})
	if len(hs) == 0 {
		return ""
	}
	h, _ := hs[0].(map[string]interface{})
	v, _ := h["value"].(string)
	return v
}

func TestTransformProvider_FailoverSplitsFrontAndDispatch(t *testing.T) {
	result, err := newProviderFailoverTransformer(t).Transform(failoverProvider("openai", twoModels(), false), &api.RestAPI{})
	require.NoError(t, err)
	front, dispatch := splitFailoverOps(t, result.Spec.Operations)

	fp := policyNamed(front.Policies, failover.PolicyName)
	require.NotNil(t, fp)
	assert.Equal(t, string(failover.RoleFront), (*fp.Params)[failover.ParamRole])
	assert.Equal(t, false, (*fp.Params)[failover.ParamRouteToTarget], "the front reads the model where the template says")
	for _, p := range *front.Policies {
		assert.False(t, isUpstreamAuth(p), "the upstream credential moves to the dispatch route")
	}

	pols := *dispatch.Policies
	require.Len(t, pols, 2, "dispatch = model-failover, then the provider's upstream credential")
	dp := pols[0]
	assert.Equal(t, string(failover.RoleDispatch), (*dp.Params)[failover.ParamRole])
	assert.Equal(t, false, (*dp.Params)[failover.ParamRouteToTarget], "every attempt goes to the provider's own upstream")
	assert.Equal(t, []interface{}{true, true, true}, (*dp.Params)[failover.ParamTargetNative], "two chain targets and the pass-through target")
	assert.Equal(t, failover.HopSecret(), (*dp.Params)[failover.ParamHopSecret])
	assert.Equal(t, []interface{}{map[string]interface{}{
		"primary":   map[string]interface{}{"provider": "openai", "model": "gpt-4o"},
		"fallbacks": []interface{}{map[string]interface{}{"provider": "openai", "model": "gpt-4o-mini"}},
	}}, (*dp.Params)["chains"], "the provider is filled in everywhere")
	assert.True(t, isUpstreamAuth(pols[1]), "the provider credential is applied on every attempt")
	assert.Nil(t, result.Spec.UpstreamDefinitions, "no per-target upstreams: the route keeps the provider's upstream")
}

func TestTransformProvider_FailoverLeavesOtherOperationsAlone(t *testing.T) {
	result, err := newProviderFailoverTransformer(t).Transform(failoverProvider("openai", twoModels(), false), &api.RestAPI{})
	require.NoError(t, err)
	for _, op := range result.Spec.Operations {
		if op.EffectivePath() == "/chat/completions" {
			continue
		}
		require.NotNil(t, op.Policies)
		hasAuth := false
		for _, p := range *op.Policies {
			hasAuth = hasAuth || isUpstreamAuth(p)
		}
		assert.True(t, hasAuth, "%s %s keeps the upstream credential", op.EffectiveMethod(), op.EffectivePath())
		assert.Nil(t, policyNamed(op.Policies, failover.PolicyName))
	}
}

func TestTransformProvider_GlobalFailoverGetsTemplateParams(t *testing.T) {
	result, err := newProviderFailoverTransformer(t).Transform(failoverProvider("openai", twoModels(), true), &api.RestAPI{})
	require.NoError(t, err, "a global attachment is checked against the template like an operation-level one")
	fronts, dispatches := 0, 0
	for _, op := range result.Spec.Operations {
		if len(op.EffectiveHeaders()) > 0 {
			dispatches++
		} else if policyNamed(op.Policies, failover.PolicyName) != nil {
			fronts++
		}
	}
	assert.Greater(t, fronts, 0)
	assert.Equal(t, fronts, dispatches)
}

func TestTransformProvider_FailoverRejects(t *testing.T) {
	cases := map[string]struct {
		template string
		params   map[string]interface{}
		msg      string
	}{
		"another provider": {"openai", map[string]interface{}{"chains": []interface{}{chainOf("m", map[string]interface{}{"provider": "anthropic", "model": "c"})}}, "attach \"anthropic\" to an LlmProxy"},
		"internal key":     {"openai", map[string]interface{}{"chains": twoModels()["chains"], "_role": "front"}, "reserved"},
		"duplicate model":  {"openai", map[string]interface{}{"chains": []interface{}{chainOf("m", map[string]interface{}{"model": "m"})}}, "more than once"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newProviderFailoverTransformer(t).Transform(failoverProvider(c.template, c.params, false), &api.RestAPI{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.msg)
		})
	}
}

func TestTransformProvider_PathModelTemplate(t *testing.T) {
	result, err := newProviderFailoverTransformer(t).Transform(failoverProviderAt("gemini", "/models/*", twoModels(), false), &api.RestAPI{})
	require.NoError(t, err, "a path-model template fails over by rewriting the path per attempt")
	var dp *api.Policy
	for _, op := range result.Spec.Operations {
		if op.EffectivePath() == "/models/*" && len(op.EffectiveHeaders()) > 0 {
			dp = policyNamed(op.Policies, failover.PolicyName)
		}
	}
	require.NotNil(t, dp)
	rm, ok := (*dp.Params)["requestModel"].(map[string]interface{})
	require.True(t, ok, "the dispatch role gets the template's requestModel")
	assert.Equal(t, "pathParam", fmt.Sprint(rm["location"]))

	_, err = newProviderFailoverTransformer(t).Transform(failoverProviderAt("gemini", "/models/gemini-2.5-pro:generateContent", twoModels(), false), &api.RestAPI{})
	require.Error(t, err, "a path that fixes the model would stop matching once the model is rewritten")
	assert.Contains(t, err.Error(), "use a wildcard path")
}

func TestChainTokenDiffersBetweenProxyAndProviderOfTheSameName(t *testing.T) {
	assert.NotEqual(t, chainToken("LlmProxy", "x", "POST", "/chat/completions"), chainToken("LlmProvider", "x", "POST", "/chat/completions"))
}

func TestTransformProxy_GlobalFailoverRunsAfterRoundRobin(t *testing.T) {
	proxy := failoverTestProxy(threeTargets(), true)
	proxy.Spec.Policies = &[]api.LLMPolicy{{
		Name: "model-round-robin", Version: "v1",
		Paths: []api.LLMPolicyPath{{Path: "/chat/completions", Methods: []api.LLMPolicyPathMethods{"POST"},
			Params: map[string]interface{}{"models": []interface{}{map[string]interface{}{"model": "gpt-4o"}, map[string]interface{}{"model": "gpt-4.1"}}}}},
	}}
	result, err := newFailoverTestTransformer(t).Transform(proxy, &api.RestAPI{})
	require.NoError(t, err)
	front, _ := splitFailoverOps(t, result.Spec.Operations)
	rr, fo := -1, -1
	for i, p := range *front.Policies {
		switch p.Name {
		case "model-round-robin":
			rr = i
		case failover.PolicyName:
			fo = i
		}
	}
	require.GreaterOrEqual(t, rr, 0)
	assert.Greater(t, fo, rr, "the front role must read the model round-robin picked")
}

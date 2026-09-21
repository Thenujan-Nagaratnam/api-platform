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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/storage"
)

const testTransformerPolicyName = "openai-to-anthropic-transformer"

// saveFailoverAuthTestProvider registers a minimal openai-templated LlmProvider
// so the LlmProxy transform below can resolve it during additionalProviders processing.
func saveFailoverAuthTestProvider(t *testing.T, db storage.Storage, name, context string) {
	t.Helper()
	providerSourceConfig := api.LLMProviderConfiguration{
		ApiVersion: api.LLMProviderConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProviderConfigurationKindLlmProvider,
		Metadata:   api.Metadata{Name: name},
		Spec: api.LLMProviderConfigData{
			DisplayName:   name,
			Version:       "v1.0",
			Context:       stringPtr(context),
			Template:      "openai",
			Upstream:      api.LLMProviderConfigData_Upstream{Url: stringPtr("https://example.com")},
			AccessControl: api.LLMAccessControl{Mode: api.AllowAll},
		},
	}
	require.NoError(t, db.SaveConfig(&models.StoredConfig{
		UUID:                name + "-uuid",
		Kind:                string(api.LLMProviderConfigurationKindLlmProvider),
		Handle:              name,
		DisplayName:         name,
		Version:             "v1.0",
		SourceConfiguration: providerSourceConfig,
		DesiredState:        models.StateDeployed,
	}))
}

func newFailoverAuthTestStore(t *testing.T) (storage.Storage, *LLMProviderTransformer) {
	t.Helper()
	store := storage.NewConfigStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := newTestSQLiteStorage(t, logger)

	template := &models.StoredLLMProviderTemplate{
		UUID: "failover-auth-test-template",
		Configuration: api.LLMProviderTemplate{
			ApiVersion: api.LLMProviderTemplateApiVersionGatewayApiPlatformWso2Comv1,
			Kind:       api.LLMProviderTemplateKindLlmProviderTemplate,
			Metadata:   api.Metadata{Name: "openai"},
			Spec:       api.LLMProviderTemplateData{DisplayName: "openai"},
		},
	}
	require.NoError(t, db.SaveLLMProviderTemplate(template))

	saveFailoverAuthTestProvider(t, db, "openai-provider", "/openai-provider")
	saveFailoverAuthTestProvider(t, db, "anthropic-provider", "/anthropic-provider")

	transformer := NewLLMProviderTransformer(store, db, &config.RouterConfig{ListenerPort: 8080}, newTestPolicyVersionResolver())
	return db, transformer
}

func findChatCompletionsOperation(t *testing.T, ops []api.Operation) *api.Operation {
	t.Helper()
	for i := range ops {
		if ops[i].Path != nil && *ops[i].Path == "/chat/completions" &&
			ops[i].Method != nil && *ops[i].Method == api.OperationMethod("POST") {
			return &ops[i]
		}
	}
	t.Fatal("expected a /chat/completions POST operation")
	return nil
}

// modelFailoverOperationPolicy is the author-facing attachment shape from the
// design doc's §3: an ordinary operationPolicies: entry whose params carry the
// policy's {targets, suspendDuration} shape.
func modelFailoverOperationPolicy() api.OperationPolicy {
	return api.OperationPolicy{
		Name:    modelFailoverPolicyName,
		Version: "v0",
		Paths: []api.OperationPolicyPath{{
			Path:    "/chat/completions",
			Methods: []api.OperationPolicyPathMethods{"POST"},
			Params: map[string]interface{}{
				"targets": []interface{}{
					map[string]interface{}{
						"target": map[string]interface{}{"model": "gpt-4o"},
						"fallbacks": []interface{}{
							map[string]interface{}{
								"model":    "claude-sonnet-4-5-20250929",
								"provider": "anthropic-upstream",
							},
						},
					},
				},
				"suspendDuration": 900,
			},
		}},
	}
}

// modelFailoverProxy builds an LlmProxy whose additional provider authenticates
// with oauth2 and carries an inline transformer — the two provider-scoped
// policy kinds the design's §8 says must gain a conditioned upstreamPolicies:
// instance per failover-referenced provider.
func modelFailoverProxy() *api.LLMProxyConfiguration {
	oauth2Params := map[string]interface{}{
		"grantType":     "client_credentials",
		"tokenEndpoint": "https://idp.example.com/oauth2/token",
		"clientId":      "gateway-client",
		"clientSecret":  "secret",
	}
	return &api.LLMProxyConfiguration{
		ApiVersion: api.LLMProxyConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProxyConfigurationKindLlmProxy,
		Metadata:   api.Metadata{Name: "model-failover-proxy"},
		Spec: api.LLMProxyConfigData{
			DisplayName: "model-failover-proxy",
			Version:     "v1.0",
			Provider: api.LLMProxyProvider{
				Id: "openai-provider",
				Auth: &api.LLMUpstreamAuth{
					Type:   api.LLMUpstreamAuthTypeApiKey,
					Header: stringPtr("Authorization"),
					Value:  stringPtr("Bearer primary"),
				},
			},
			AdditionalProviders: &[]api.LLMProxyAdditionalProvider{{
				Id: "anthropic-provider",
				As: stringPtr("anthropic-upstream"),
				Auth: &api.LLMUpstreamAuth{
					Type:         api.LLMUpstreamAuthTypeOauth2,
					PolicyParams: &oauth2Params,
				},
				Transformer: &api.LLMProxyTransformer{
					Type:    testTransformerPolicyName,
					Version: "v1",
				},
			}},
			OperationPolicies: &[]api.OperationPolicy{modelFailoverOperationPolicy()},
		},
	}
}

// upstreamAttachments returns the attachments of the named policy that run in
// the upstream-attempt phase (Upstream: true), in emitted order.
func upstreamAttachments(policies []api.Policy, name string) []api.Policy {
	var out []api.Policy
	for _, p := range policies {
		if p.Name == name && p.Upstream != nil && *p.Upstream {
			out = append(out, p)
		}
	}
	return out
}

// downstreamAttachments returns the attachments of the named policy that run in
// the ordinary downstream phase (Upstream unset/false), in emitted order.
func downstreamAttachments(policies []api.Policy, name string) []api.Policy {
	var out []api.Policy
	for _, p := range policies {
		if p.Name == name && (p.Upstream == nil || !*p.Upstream) {
			out = append(out, p)
		}
	}
	return out
}

func indexOfUpstreamAttachment(t *testing.T, policies []api.Policy, name string) int {
	t.Helper()
	for i, p := range policies {
		if p.Name == name && p.Upstream != nil && *p.Upstream {
			return i
		}
	}
	return -1
}

// TestTransform_ModelFailoverPolicy_AttachedDownstreamAndUpstreamWithProviderScopedInstances
// is the core Task 8 assertion: an operationPolicies: model-failover attachment
// produces a second, unconditioned upstreamPolicies: instance of itself carrying
// the same params, plus one conditioned upstreamPolicies: instance of every
// provider-scoped credential/transform policy for each provider the failover
// chain references — and the model-failover upstream instance is ordered before
// them, so the selected_provider metadata it seeds exists when their CEL
// conditions are evaluated (design §9's ordering requirement).
func TestTransform_ModelFailoverPolicy_AttachedDownstreamAndUpstreamWithProviderScopedInstances(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	result, err := transformer.Transform(modelFailoverProxy(), &api.RestAPI{})
	require.NoError(t, err)

	chatOp := findChatCompletionsOperation(t, result.Spec.Operations)
	require.NotNil(t, chatOp.Policies)
	policies := *chatOp.Policies

	downstreamFailover := downstreamAttachments(policies, modelFailoverPolicyName)
	require.Len(t, downstreamFailover, 1, "model-failover must stay attached downstream exactly once")
	assert.Nil(t, downstreamFailover[0].ExecutionCondition,
		"the downstream model-failover instance is unconditioned")

	upstreamFailover := upstreamAttachments(policies, modelFailoverPolicyName)
	require.Len(t, upstreamFailover, 1, "model-failover must also be attached via upstreamPolicies:")
	assert.Nil(t, upstreamFailover[0].ExecutionCondition,
		"the upstream model-failover instance is unconditioned — it is what seeds selected_provider")
	require.NotNil(t, upstreamFailover[0].Params)
	require.NotNil(t, downstreamFailover[0].Params)
	assert.Equal(t, (*downstreamFailover[0].Params)["targets"], (*upstreamFailover[0].Params)["targets"],
		"both attachments must carry the same params (same expanded map, per design §3)")
	assert.Equal(t, downstreamFailover[0].Version, upstreamFailover[0].Version)

	upstreamOauth2 := upstreamAttachments(policies, constants.UPSTREAM_AUTH_OAUTH2_POLICY_NAME)
	require.Len(t, upstreamOauth2, 1,
		"oauth2-generator must get one conditioned upstreamPolicies: attachment for the referenced provider")
	require.NotNil(t, upstreamOauth2[0].ExecutionCondition)
	assert.Contains(t, *upstreamOauth2[0].ExecutionCondition, "anthropic-upstream")
	require.NotNil(t, upstreamOauth2[0].Params)
	assert.Equal(t, "anthropic-upstream", (*upstreamOauth2[0].Params)["providerId"])

	upstreamTransformer := upstreamAttachments(policies, testTransformerPolicyName)
	require.Len(t, upstreamTransformer, 1,
		"an additionalProviders[].transformer must get the same conditioned upstreamPolicies: attachment")
	require.NotNil(t, upstreamTransformer[0].ExecutionCondition)
	assert.Contains(t, *upstreamTransformer[0].ExecutionCondition, "anthropic-upstream")

	// The primary provider is referenced implicitly by the target (no provider
	// field), so its api-key credential gets an upstream instance too.
	upstreamSetHeaders := upstreamAttachments(policies, constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME)
	require.Len(t, upstreamSetHeaders, 1,
		"the primary provider's own credential policy must get an upstream instance as well")
	require.NotNil(t, upstreamSetHeaders[0].ExecutionCondition)
	assert.Contains(t, *upstreamSetHeaders[0].ExecutionCondition, "openai-provider")

	// Ordering: model-failover's upstream instance must precede every
	// provider-scoped upstream instance that reads the metadata it seeds.
	failoverIdx := indexOfUpstreamAttachment(t, policies, modelFailoverPolicyName)
	oauth2Idx := indexOfUpstreamAttachment(t, policies, constants.UPSTREAM_AUTH_OAUTH2_POLICY_NAME)
	transformerIdx := indexOfUpstreamAttachment(t, policies, testTransformerPolicyName)
	setHeadersIdx := indexOfUpstreamAttachment(t, policies, constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME)
	require.NotEqual(t, -1, failoverIdx)
	require.NotEqual(t, -1, oauth2Idx)
	require.NotEqual(t, -1, transformerIdx)
	require.NotEqual(t, -1, setHeadersIdx)
	assert.Less(t, failoverIdx, oauth2Idx,
		"model-failover must be ordered before the policies that read its seeded metadata")
	assert.Less(t, failoverIdx, transformerIdx)
	assert.Less(t, failoverIdx, setHeadersIdx)

	// The pre-existing downstream mechanism must be untouched.
	downstreamOauth2 := downstreamAttachments(policies, constants.UPSTREAM_AUTH_OAUTH2_POLICY_NAME)
	require.Len(t, downstreamOauth2, 1)
	require.NotNil(t, downstreamOauth2[0].ExecutionCondition)
	require.NotNil(t, downstreamOauth2[0].Params)
	assert.Equal(t, "anthropic-upstream", (*downstreamOauth2[0].Params)["providerId"],
		"proxyUpstreamAuthPolicy must inject providerId into oauth2-generator's own params so it can self-gate a failover attempt")
	downstreamTransformer := downstreamAttachments(policies, testTransformerPolicyName)
	require.Len(t, downstreamTransformer, 1)
	require.NotNil(t, downstreamTransformer[0].ExecutionCondition)
}

// TestTransform_ModelFailoverPolicy_NoAttachment_NoUpstreamInstances is the
// zero-cost-when-unused guard: without a model-failover attachment nothing gains
// an upstreamPolicies: instance, so every existing LlmProxy is byte-identical.
func TestTransform_ModelFailoverPolicy_NoAttachment_NoUpstreamInstances(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	proxy := modelFailoverProxy()
	proxy.Spec.OperationPolicies = &[]api.OperationPolicy{{
		Name:    "llm-header-router",
		Version: "v1",
		Paths: []api.OperationPolicyPath{{
			Path:    "/chat/completions",
			Methods: []api.OperationPolicyPathMethods{"POST"},
			Params:  map[string]interface{}{"defaultProvider": "openai-provider"},
		}},
	}}

	result, err := transformer.Transform(proxy, &api.RestAPI{})
	require.NoError(t, err)

	chatOp := findChatCompletionsOperation(t, result.Spec.Operations)
	require.NotNil(t, chatOp.Policies)
	for _, p := range *chatOp.Policies {
		assert.True(t, p.Upstream == nil || !*p.Upstream,
			"policy %q must not gain an upstream-attempt instance without a model-failover attachment", p.Name)
	}
}

// TestTransform_ModelFailoverPolicy_ExistingUpstreamAttachmentNotDuplicated
// guards the idempotency of the synthesized attachment: an author who already
// spelled model-failover out under upstreamPolicies: gets exactly one upstream
// instance, not two.
func TestTransform_ModelFailoverPolicy_ExistingUpstreamAttachmentNotDuplicated(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	proxy := modelFailoverProxy()
	proxy.Spec.UpstreamPolicies = &[]api.OperationPolicy{modelFailoverOperationPolicy()}

	result, err := transformer.Transform(proxy, &api.RestAPI{})
	require.NoError(t, err)

	chatOp := findChatCompletionsOperation(t, result.Spec.Operations)
	require.NotNil(t, chatOp.Policies)
	assert.Len(t, upstreamAttachments(*chatOp.Policies, modelFailoverPolicyName), 1)
}

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

func policiesNamed(policies []api.Policy, name string) []api.Policy {
	var out []api.Policy
	for _, p := range policies {
		if p.Name == name {
			out = append(out, p)
		}
	}
	return out
}

func failoverProxyWithApiKeyProviders() *api.LLMProxyConfiguration {
	return &api.LLMProxyConfiguration{
		ApiVersion: api.LLMProxyConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProxyConfigurationKindLlmProxy,
		Metadata:   api.Metadata{Name: "failover-auth-proxy"},
		Spec: api.LLMProxyConfigData{
			DisplayName: "failover-auth-proxy",
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
					Type:   api.LLMUpstreamAuthTypeApiKey,
					Header: stringPtr("X-Provider-Key"),
					Value:  stringPtr("anthropic-loopback"),
				},
			}},
			Policies: &[]api.LLMPolicy{{
				Name:    "llm-header-router",
				Version: "v1",
				Paths: []api.LLMPolicyPath{{
					Path:    "/chat/completions",
					Methods: []api.LLMPolicyPathMethods{"POST"},
					Params: map[string]interface{}{
						"defaultProvider": "openai-provider",
					},
				}},
			}},
			Resilience: &api.LLMResilience{
				Failover: &api.LLMFailoverConfig{
					Targets: []api.LLMFailoverTargetEntry{{
						Target: api.LLMFailoverTarget{Model: "gpt-4o"},
						Fallbacks: []api.LLMFailoverTarget{{
							Model:    "claude-sonnet-4-5-20250929",
							Provider: stringPtr("anthropic-upstream"),
						}},
					}},
				},
			},
		},
	}
}

// TestLLMProviderTransformer_TransformProxy_FailoverAuthAttachedForApiKeyProviders
// proves the loopback auth gap fix: a resilience.failover block referencing
// both the primary (implicitly, via an omitted target provider) and an
// additionalProviders entry (explicitly, on the fallback) gets one
// llm-upstream-provider-auth attachment PER referenced provider, unconditional
// (no ExecutionCondition - gating is internal to the policy via
// UpstreamAttemptContext.ResolvedProvider), each carrying that provider's own
// header/value/providerId - additive to, not a replacement for, the existing
// set-headers+ExecutionCondition downstream attachment.
func TestLLMProviderTransformer_TransformProxy_FailoverAuthAttachedForApiKeyProviders(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	result, err := transformer.Transform(failoverProxyWithApiKeyProviders(), &api.RestAPI{})
	require.NoError(t, err)

	chatOp := findChatCompletionsOperation(t, result.Spec.Operations)
	require.NotNil(t, chatOp.Policies)

	failoverAuthPolicies := policiesNamed(*chatOp.Policies, constants.UPSTREAM_AUTH_FAILOVER_APIKEY_POLICY_NAME)
	require.Len(t, failoverAuthPolicies, 2, "expected one llm-upstream-provider-auth attachment per failover-referenced provider")

	byProvider := map[string]api.Policy{}
	for _, p := range failoverAuthPolicies {
		require.Nil(t, p.ExecutionCondition, "the failover-scoped auth policy must be unconditional - gating is internal via ResolvedProvider")
		require.NotNil(t, p.Params)
		providerID, _ := (*p.Params)["providerId"].(string)
		byProvider[providerID] = p
	}

	primary, ok := byProvider["openai-provider"]
	require.True(t, ok, "expected an attachment for the primary provider (the target's implicit default)")
	assert.Equal(t, "Authorization", (*primary.Params)["header"])
	assert.Equal(t, "Bearer primary", (*primary.Params)["value"])

	fallback, ok := byProvider["anthropic-upstream"]
	require.True(t, ok, "expected an attachment for the fallback's explicit provider")
	assert.Equal(t, "X-Provider-Key", (*fallback.Params)["header"])
	assert.Equal(t, "anthropic-loopback", (*fallback.Params)["value"])

	// The existing downstream mechanism must be untouched - both providers still
	// carry their conditional set-headers attachment for the ordinary,
	// non-failover single-provider path.
	setHeadersPolicies := policiesNamed(*chatOp.Policies, constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME)
	var conditionalSetHeaders int
	for _, p := range setHeadersPolicies {
		if p.ExecutionCondition != nil {
			conditionalSetHeaders++
		}
	}
	assert.Equal(t, 2, conditionalSetHeaders, "expected the pre-existing conditional set-headers attachment for both providers, unaffected by the new mechanism")
}

// TestLLMProviderTransformer_TransformProxy_NoFailoverBlock_NoFailoverAuthAttached
// is the zero-cost-when-unused regression guard: an otherwise-identical proxy
// with no resilience.failover block gets no llm-upstream-provider-auth
// attachment at all.
func TestLLMProviderTransformer_TransformProxy_NoFailoverBlock_NoFailoverAuthAttached(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	proxy := failoverProxyWithApiKeyProviders()
	proxy.Spec.Resilience = nil

	result, err := transformer.Transform(proxy, &api.RestAPI{})
	require.NoError(t, err)

	chatOp := findChatCompletionsOperation(t, result.Spec.Operations)
	require.NotNil(t, chatOp.Policies)

	assert.Empty(t, policiesNamed(*chatOp.Policies, constants.UPSTREAM_AUTH_FAILOVER_APIKEY_POLICY_NAME))
}

// TestLLMProviderTransformer_TransformProxy_FailoverOauth2InjectsProviderID
// proves the oauth2 side of the fix: proxyUpstreamAuthPolicy now injects
// providerId into oauth2-generator's own params unconditionally, so it can
// self-gate a failover attempt via UpstreamAttemptContext.ResolvedProvider -
// no separate llm-upstream-provider-auth attachment is needed or created for
// an oauth2-authenticated provider.
func TestLLMProviderTransformer_TransformProxy_FailoverOauth2InjectsProviderID(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	proxy := failoverProxyWithApiKeyProviders()
	oauth2Params := map[string]interface{}{
		"grantType":     "client_credentials",
		"tokenEndpoint": "https://idp.example.com/oauth2/token",
		"clientId":      "gateway-client",
		"clientSecret":  "secret",
	}
	proxy.Spec.Provider.Auth = &api.LLMUpstreamAuth{
		Type:         api.LLMUpstreamAuthTypeOauth2,
		PolicyParams: &oauth2Params,
	}

	result, err := transformer.Transform(proxy, &api.RestAPI{})
	require.NoError(t, err)

	chatOp := findChatCompletionsOperation(t, result.Spec.Operations)
	require.NotNil(t, chatOp.Policies)

	oauth2Policies := policiesNamed(*chatOp.Policies, constants.UPSTREAM_AUTH_OAUTH2_POLICY_NAME)
	require.Len(t, oauth2Policies, 1)
	require.NotNil(t, oauth2Policies[0].Params)
	assert.Equal(t, "openai-provider", (*oauth2Policies[0].Params)["providerId"])

	// oauth2 self-gates internally - no separate failover-auth attachment for it.
	failoverAuthPolicies := policiesNamed(*chatOp.Policies, constants.UPSTREAM_AUTH_FAILOVER_APIKEY_POLICY_NAME)
	for _, p := range failoverAuthPolicies {
		assert.NotEqual(t, "openai-provider", (*p.Params)["providerId"],
			"oauth2 provider must not also get a separate llm-upstream-provider-auth attachment")
	}
}

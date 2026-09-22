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

	// A provider a model-failover chain references must NOT ALSO get a
	// downstream attachment: the upstream-attempt instance above already
	// covers every attempt, including the first (Envoy's upstream ext_proc
	// phase runs on attempt 1 too, not just retries) — a downstream copy
	// would fire unconditionally before model-failover's own routing
	// decision even runs, and nothing removes it if a later attempt
	// resolves to a different provider whose credential uses a different
	// header name, leaking this one alongside the correct one.
	assert.Empty(t, downstreamAttachments(policies, constants.UPSTREAM_AUTH_OAUTH2_POLICY_NAME),
		"a failover-referenced additional provider's credential must not also be attached downstream")
	assert.Empty(t, downstreamAttachments(policies, testTransformerPolicyName),
		"a failover-referenced additional provider's transformer must not also be attached downstream")
	// set-headers also implements the unrelated, always-present internal
	// loopback marker (x-wso2-internal-loopback) — exclude it, the same way
	// llm_transformer_multiprovider_test.go already does, to isolate the
	// primary's actual credential attachment.
	assert.Empty(t, credentialAttachments(downstreamAttachments(policies, constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME)),
		"the primary provider is failover-referenced too (implicit target), so its credential must not be attached downstream either")
}

// credentialAttachments filters out the internal loopback marker instance
// (also named "set-headers") from a list of downstream/upstream attachments,
// isolating the actual provider-credential instance(s).
func credentialAttachments(policies []api.Policy) []api.Policy {
	var out []api.Policy
	for _, p := range policies {
		if !hasInternalLoopbackMarkerPolicy([]api.Policy{p}) {
			out = append(out, p)
		}
	}
	return out
}

// TestTransform_ModelFailoverPolicy_NonReferencedProviderKeepsDownstreamAttachment
// guards the fix's precise scope: only providers a model-failover chain
// actually references lose their downstream attachment. A third provider used
// solely via some other downstream selection mechanism (here simulated by just
// not naming it in any target/fallback) keeps its ordinary downstream
// credential/transformer instance untouched.
func TestTransform_ModelFailoverPolicy_NonReferencedProviderKeepsDownstreamAttachment(t *testing.T) {
	db, transformer := newFailoverAuthTestStore(t)
	saveFailoverAuthTestProvider(t, db, "mistral-provider", "/mistral-provider")

	proxy := modelFailoverProxy()
	additional := append(*proxy.Spec.AdditionalProviders, api.LLMProxyAdditionalProvider{
		Id: "mistral-provider",
		As: stringPtr("mistral-upstream"),
		Auth: &api.LLMUpstreamAuth{
			Type:   api.LLMUpstreamAuthTypeApiKey,
			Header: stringPtr("Authorization"),
			Value:  stringPtr("Bearer mistral"),
		},
		Transformer: &api.LLMProxyTransformer{
			Type:    "openai-to-mistral-transformer",
			Version: "v0",
		},
	})
	proxy.Spec.AdditionalProviders = &additional

	result, err := transformer.Transform(proxy, &api.RestAPI{})
	require.NoError(t, err)

	chatOp := findChatCompletionsOperation(t, result.Spec.Operations)
	require.NotNil(t, chatOp.Policies)
	policies := *chatOp.Policies

	// mistral-upstream is not named in the failover chain's target or
	// fallbacks at all, so it must keep its ordinary downstream attachment.
	// The primary also uses "set-headers" (api-key-auth) downstream, but the
	// primary IS failover-referenced (the implicit target) so its downstream
	// copy is skipped — leaving only mistral's own instance under this name
	// (credentialAttachments also excludes the always-present, unrelated
	// internal loopback marker, which shares this same policy name).
	downstreamMistralAuth := credentialAttachments(downstreamAttachments(policies, constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME))
	require.Len(t, downstreamMistralAuth, 1, "only mistral-upstream's own downstream credential should remain")
	require.NotNil(t, downstreamMistralAuth[0].ExecutionCondition)
	assert.Contains(t, *downstreamMistralAuth[0].ExecutionCondition, "mistral-upstream")

	downstreamMistralTransformer := downstreamAttachments(policies, "openai-to-mistral-transformer")
	require.Len(t, downstreamMistralTransformer, 1,
		"mistral-upstream's transformer is untouched since it is not referenced by any model-failover chain")

	// It must NOT gain an upstream-attempt instance either — only chain-
	// referenced providers do.
	assert.Empty(t, upstreamAttachments(policies, "openai-to-mistral-transformer"),
		"a non-referenced provider gains no upstream-attempt instance")
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

// TestTransform_ModelFailoverPolicy_SynthesizedUpstreamInstanceIsExactlyOne
// pins that the synthesized upstream-attempt instance never duplicates itself
// across repeated Transform calls or otherwise. upstreamPolicies: has no
// author-facing schema field any more — there is no way for an author to have
// pre-declared one for the controller to detect and skip — so this is the
// only source of that instance, always exactly one.
func TestTransform_ModelFailoverPolicy_SynthesizedUpstreamInstanceIsExactlyOne(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	proxy := modelFailoverProxy()

	result, err := transformer.Transform(proxy, &api.RestAPI{})
	require.NoError(t, err)

	chatOp := findChatCompletionsOperation(t, result.Spec.Operations)
	require.NotNil(t, chatOp.Policies)
	assert.Len(t, upstreamAttachments(*chatOp.Policies, modelFailoverPolicyName), 1)
}

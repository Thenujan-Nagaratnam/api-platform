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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
)

// TestTransform_ProviderUpstreamPolicies_GatedAndOrdered: provider.policies and
// additionalProviders[].policies become upstream-attempt instances gated to
// their own provider, ordered after model-failover's upstream instance and
// before the provider's transformer and auth.
func TestTransform_ProviderUpstreamPolicies_GatedAndOrdered(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	proxy := modelFailoverProxy()
	proxy.Spec.Provider.Policies = &[]api.Policy{
		{Name: "pii-masking", Version: "v1"},
	}
	authorCondition := "request.Headers['x-mask'] == 'on'"
	(*proxy.Spec.AdditionalProviders)[0].Policies = &[]api.Policy{
		{Name: "pii-masking", Version: "v1"},
		{Name: "policy-x", Version: "v1", ExecutionCondition: &authorCondition},
	}

	result, err := transformer.Transform(proxy, &api.RestAPI{})
	require.NoError(t, err)

	chatOp := findChatCompletionsOperation(t, result.Spec.Operations)
	require.NotNil(t, chatOp.Policies)
	policies := *chatOp.Policies

	masking := upstreamAttachments(policies, "pii-masking")
	require.Len(t, masking, 2, "one upstream pii-masking instance per provider that lists it")
	require.NotNil(t, masking[0].ExecutionCondition)
	assert.Equal(t, selectedProviderExecutionCondition("openai-provider", true), *masking[0].ExecutionCondition,
		"the primary's policies also run when no provider was selected")
	require.NotNil(t, masking[1].ExecutionCondition)
	assert.Equal(t, selectedProviderExecutionCondition("anthropic-upstream", false), *masking[1].ExecutionCondition)

	policyX := upstreamAttachments(policies, "policy-x")
	require.Len(t, policyX, 1)
	require.NotNil(t, policyX[0].ExecutionCondition)
	assert.Equal(t,
		"("+selectedProviderExecutionCondition("anthropic-upstream", false)+") && ("+authorCondition+")",
		*policyX[0].ExecutionCondition, "an author condition is ANDed with the provider gate")

	assert.Empty(t, downstreamAttachments(policies, "pii-masking"), "provider policies never run downstream")
	assert.Empty(t, downstreamAttachments(policies, "policy-x"))

	failoverIdx := indexOfUpstreamAttachment(t, policies, modelFailoverPolicyName)
	maskingIdx := indexOfUpstreamAttachment(t, policies, "pii-masking")
	transformerIdx := indexOfUpstreamAttachment(t, policies, testTransformerPolicyName)
	require.NotEqual(t, -1, failoverIdx)
	require.NotEqual(t, -1, maskingIdx)
	require.NotEqual(t, -1, transformerIdx)
	assert.Less(t, failoverIdx, maskingIdx, "model-failover first: it decides the attempt's provider")
	assert.Less(t, maskingIdx, transformerIdx, "provider policies see the client's shape, before the transformer")
}

// TestTransform_ProviderUpstreamPolicies_WithoutFailover: with no model-failover
// on the proxy, the primary's policies still attach (gated with the
// no-selection default) and run on the primary upstream.
func TestTransform_ProviderUpstreamPolicies_WithoutFailover(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	proxy := modelFailoverProxy()
	proxy.Spec.OperationPolicies = nil
	proxy.Spec.Provider.Policies = &[]api.Policy{{Name: "pii-masking", Version: "v1"}}

	result, err := transformer.Transform(proxy, &api.RestAPI{})
	require.NoError(t, err)

	for _, op := range result.Spec.Operations {
		require.NotNil(t, op.Policies)
		masking := upstreamAttachments(*op.Policies, "pii-masking")
		require.Len(t, masking, 1, "every proxy operation carries the primary's upstream policy")
		assert.Equal(t, selectedProviderExecutionCondition("openai-provider", true), *masking[0].ExecutionCondition)
	}
}

// TestTransform_LlmProviderUpstreamPolicies_CarriedToMainUpstream:
// LlmProvider spec.upstream.policies land on the derived RestApi's main
// upstream, where the RestApi transform attaches them upstream-phase.
func TestTransform_LlmProviderUpstreamPolicies_CarriedToMainUpstream(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	policies := []api.Policy{{Name: "pii-masking", Version: "v1"}}
	provider := &api.LLMProviderConfiguration{
		ApiVersion: api.LLMProviderConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProviderConfigurationKindLlmProvider,
		Metadata:   api.Metadata{Name: "upstream-policies-provider"},
		Spec: api.LLMProviderConfigData{
			DisplayName: "upstream-policies-provider",
			Version:     "v1.0",
			Template:    "openai",
			Upstream: api.LLMProviderConfigData_Upstream{
				Url:      stringPtr("https://example.com"),
				Policies: &policies,
			},
			AccessControl: api.LLMAccessControl{Mode: api.AllowAll},
		},
	}

	result, err := transformer.Transform(provider, &api.RestAPI{})
	require.NoError(t, err)
	require.NotNil(t, result.Spec.Upstream.Main.Policies)
	assert.Equal(t, policies, *result.Spec.Upstream.Main.Policies)
}

// TestTransform_LlmProviderUpstreamAuth_LastInMainUpstreamPolicies: the
// provider's upstream auth is compiled into the main upstream's policies,
// after the author's entries, and is no longer attached to the operations.
func TestTransform_LlmProviderUpstreamAuth_LastInMainUpstreamPolicies(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	policies := []api.Policy{{Name: "pii-masking", Version: "v1"}}
	provider := &api.LLMProviderConfiguration{
		ApiVersion: api.LLMProviderConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProviderConfigurationKindLlmProvider,
		Metadata:   api.Metadata{Name: "upstream-auth-provider"},
		Spec: api.LLMProviderConfigData{
			DisplayName: "upstream-auth-provider",
			Version:     "v1.0",
			Template:    "openai",
			Upstream: api.LLMProviderConfigData_Upstream{
				Url:      stringPtr("https://example.com"),
				Auth:     apiKeyUpstreamAuth("Authorization", "Bearer secret"),
				Policies: &policies,
			},
			AccessControl: api.LLMAccessControl{Mode: api.AllowAll},
		},
	}

	result, err := transformer.Transform(provider, &api.RestAPI{})
	require.NoError(t, err)
	require.NotNil(t, result.Spec.Upstream.Main.Policies)
	main := *result.Spec.Upstream.Main.Policies
	require.Len(t, main, 2)
	assert.Equal(t, "pii-masking", main[0].Name)
	assert.Equal(t, constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME, main[1].Name)
	assert.Len(t, policies, 1, "the author's list is not mutated")

	for _, op := range result.Spec.Operations {
		if op.Policies == nil {
			continue
		}
		for _, p := range *op.Policies {
			assert.NotEqual(t, constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME, p.Name,
				"upstream auth must not be attached to operation %s %s", op.EffectiveMethod(), op.EffectivePath())
		}
	}
}

// TestTransform_SingleProviderProxy_AuthUpstreamWithoutFailover: a proxy with
// one provider needs no model-failover to identify the attempt's provider, so
// its auth runs upstream after its policies, gated with the no-selection
// default, and nothing provider-scoped runs downstream.
func TestTransform_SingleProviderProxy_AuthUpstreamWithoutFailover(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	proxy := modelFailoverProxy()
	proxy.Spec.OperationPolicies = nil
	proxy.Spec.AdditionalProviders = nil
	proxy.Spec.Provider.Policies = &[]api.Policy{{Name: "pii-masking", Version: "v1"}}

	result, err := transformer.Transform(proxy, &api.RestAPI{})
	require.NoError(t, err)

	require.NotEmpty(t, result.Spec.Operations)
	op := result.Spec.Operations[0]
	require.NotNil(t, op.Policies)
	policies := *op.Policies

	assert.Empty(t, credentialAttachments(downstreamAttachments(policies, constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME)))
	auth := credentialAttachments(upstreamAttachments(policies, constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME))
	require.Len(t, auth, 1)
	require.NotNil(t, auth[0].ExecutionCondition)
	assert.Equal(t, selectedProviderExecutionCondition("openai-provider", true), *auth[0].ExecutionCondition)

	maskingIdx := indexOfUpstreamAttachment(t, policies, "pii-masking")
	authIdx := -1
	for i, p := range policies {
		if p.Name == constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME && p.Upstream != nil && *p.Upstream && len(credentialAttachments([]api.Policy{p})) == 1 {
			authIdx = i
		}
	}
	require.NotEqual(t, -1, maskingIdx)
	assert.Less(t, maskingIdx, authIdx, "auth runs after the provider's policies")
}

// TestTransform_MultiProviderProxyWithoutFailover_AuthStaysDownstream: with
// several providers and no model-failover, the upstream phase cannot tell
// which provider an attempt is for, so transformer and auth keep their
// downstream, selection-gated attachment.
func TestTransform_MultiProviderProxyWithoutFailover_AuthStaysDownstream(t *testing.T) {
	_, transformer := newFailoverAuthTestStore(t)

	proxy := modelFailoverProxy()
	proxy.Spec.OperationPolicies = nil

	result, err := transformer.Transform(proxy, &api.RestAPI{})
	require.NoError(t, err)

	require.NotEmpty(t, result.Spec.Operations)
	op := result.Spec.Operations[0]
	require.NotNil(t, op.Policies)
	policies := *op.Policies

	assert.Empty(t, credentialAttachments(upstreamAttachments(policies, constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME)))
	assert.Empty(t, upstreamAttachments(policies, testTransformerPolicyName))
	assert.NotEmpty(t, credentialAttachments(downstreamAttachments(policies, constants.UPSTREAM_AUTH_APIKEY_POLICY_NAME)))
	assert.Len(t, downstreamAttachments(policies, testTransformerPolicyName), 1)
}

// effectiveOperations returns spec's operations with the main upstream's
// policies appended to each, mirroring how the RestApi transform builds every
// main-vhost route chain: the operation's policies, then the upstream's list.
func effectiveOperations(spec api.APIConfigData) []api.Operation {
	out := make([]api.Operation, len(spec.Operations))
	copy(out, spec.Operations)
	if spec.Upstream.Main.Policies == nil || len(*spec.Upstream.Main.Policies) == 0 {
		return out
	}
	for i := range out {
		chain := []api.Policy{}
		if out[i].Policies != nil {
			chain = append(chain, *out[i].Policies...)
		}
		chain = append(chain, *spec.Upstream.Main.Policies...)
		out[i].Policies = &chain
	}
	return out
}

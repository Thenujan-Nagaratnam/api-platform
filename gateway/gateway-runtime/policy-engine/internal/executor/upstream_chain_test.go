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

package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/testutils"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestExecuteUpstreamRequestPolicies_EmptyPolicyList(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	executor := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	upCtx := testutils.NewTestUpstreamAttemptContext([]byte(`{"model":"gpt-4o"}`))

	result, err := executor.ExecuteUpstreamRequestPolicies(ctx, []policy.Policy{}, upCtx, []policy.PolicySpec{}, "api", "route")

	require.NoError(t, err)
	assert.NotNil(t, result)
	assert.Empty(t, result.Results)
	assert.False(t, result.ShortCircuited)
}

func TestExecuteUpstreamRequestPolicies_SkipsPolicyNotImplementingInterface(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	executor := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	upCtx := testutils.NewTestUpstreamAttemptContext([]byte(`{}`))
	// NoopPolicy implements RequestPolicy, not UpstreamRequestPolicy — must be
	// skipped silently, exactly like non-implementing policies are skipped in
	// the existing downstream phases.
	policies := []policy.Policy{&testutils.NoopPolicy{}}
	specs := []policy.PolicySpec{newPolicySpec("noop", "v1.0.0", true, nil)}

	result, err := executor.ExecuteUpstreamRequestPolicies(ctx, policies, upCtx, specs, "api", "route")

	require.NoError(t, err)
	assert.Empty(t, result.Results)
}

func TestExecuteUpstreamRequestPolicies_ExecutesForBackend(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	executor := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	original := []byte(`{"model":"gpt-4o"}`)
	upCtx := testutils.NewTestUpstreamAttemptContext(original)

	var sawBackend string
	var sawBytes []byte
	pol := &testutils.ConfigurableUpstreamMockPolicy{
		Name: "api-key-auth",
		MockMode: policy.ProcessingMode{
			UpstreamRequestMode: policy.BodyModeBuffer,
		},
		OnReqFn: func(c *policy.UpstreamAttemptContext, _ map[string]interface{}) policy.RequestAction {
			sawBackend = c.Name
			sawBytes = c.OriginalRequestRaw
			return policy.UpstreamRequestModifications{
				HeadersToSet: map[string]string{"authorization": "Bearer backend-specific-key"},
			}
		},
	}
	specs := []policy.PolicySpec{newPolicySpec("api-key-auth", "v1.0.0", true, nil)}

	result, err := executor.ExecuteUpstreamRequestPolicies(ctx, []policy.Policy{pol}, upCtx, specs, "api", "route")

	require.NoError(t, err)
	require.Len(t, result.Results, 1)
	assert.Equal(t, "test-backend", sawBackend)
	assert.Equal(t, original, sawBytes)
	assert.False(t, result.ShortCircuited)
}

func TestExecuteUpstreamRequestPolicies_DisabledPolicySkipped(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	executor := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	upCtx := testutils.NewTestUpstreamAttemptContext([]byte(`{}`))
	called := false
	pol := &testutils.ConfigurableUpstreamMockPolicy{
		MockMode: policy.ProcessingMode{UpstreamRequestMode: policy.BodyModeBuffer},
		OnReqFn: func(_ *policy.UpstreamAttemptContext, _ map[string]interface{}) policy.RequestAction {
			called = true
			return nil
		},
	}
	specs := []policy.PolicySpec{newPolicySpec("disabled-auth", "v1.0.0", false, nil)}

	result, err := executor.ExecuteUpstreamRequestPolicies(ctx, []policy.Policy{pol}, upCtx, specs, "api", "route")

	require.NoError(t, err)
	require.Len(t, result.Results, 1)
	assert.True(t, result.Results[0].Skipped)
	assert.False(t, called)
}

func TestExecuteUpstreamRequestPolicies_ShortCircuit(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	executor := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	upCtx := testutils.NewTestUpstreamAttemptContext([]byte(`{}`))
	pol := &testutils.ConfigurableUpstreamMockPolicy{
		MockMode: policy.ProcessingMode{UpstreamRequestMode: policy.BodyModeBuffer},
		OnReqFn: func(_ *policy.UpstreamAttemptContext, _ map[string]interface{}) policy.RequestAction {
			return policy.ImmediateResponse{StatusCode: 401}
		},
	}
	specs := []policy.PolicySpec{newPolicySpec("auth-fail", "v1.0.0", true, nil)}

	result, err := executor.ExecuteUpstreamRequestPolicies(ctx, []policy.Policy{pol}, upCtx, specs, "api", "route")

	require.NoError(t, err)
	assert.True(t, result.ShortCircuited)
	ir, ok := result.FinalAction.(policy.ImmediateResponse)
	require.True(t, ok)
	assert.Equal(t, 401, ir.StatusCode)
}

func TestExecuteUpstreamRequestPolicies_TransformedBodyReachesNextPolicy(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	executor := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	original := []byte(`{"model":"gpt-4o"}`)
	upCtx := testutils.NewTestUpstreamAttemptContext(original)

	translated := []byte(`{"anthropic_version":"bedrock-2023-05-31"}`)
	transformer := &testutils.ConfigurableUpstreamMockPolicy{
		MockMode: policy.ProcessingMode{UpstreamRequestMode: policy.BodyModeBuffer},
		OnReqFn: func(_ *policy.UpstreamAttemptContext, _ map[string]interface{}) policy.RequestAction {
			return policy.UpstreamRequestModifications{Body: translated}
		},
	}
	// A later policy (e.g. an auth policy signing the request) must see the
	// transformer's output in Body — but OriginalRequestRaw must stay the
	// client's untouched bytes, since a future retry attempt is built fresh
	// from OriginalRequestRaw, never from a previous attempt's Body mutation.
	var sawBody, sawOriginal []byte
	authPolicy := &testutils.ConfigurableUpstreamMockPolicy{
		MockMode: policy.ProcessingMode{UpstreamRequestMode: policy.BodyModeBuffer},
		OnReqFn: func(c *policy.UpstreamAttemptContext, _ map[string]interface{}) policy.RequestAction {
			sawBody = c.Body.Content
			sawOriginal = c.OriginalRequestRaw
			return nil
		},
	}
	specs := []policy.PolicySpec{
		newPolicySpec("transformer", "v1.0.0", true, nil),
		newPolicySpec("auth", "v1.0.0", true, nil),
	}

	_, err := executor.ExecuteUpstreamRequestPolicies(ctx, []policy.Policy{transformer, authPolicy}, upCtx, specs, "api", "route")

	require.NoError(t, err)
	assert.Equal(t, translated, sawBody, "the auth policy must see the transformer's output")
	assert.Equal(t, original, sawOriginal, "OriginalRequestRaw must never be mutated by a policy in the chain")
}

func TestExecuteUpstreamResponsePolicies_ExecutesForWinningBackend(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	executor := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	upCtx := testutils.NewTestUpstreamAttemptContext([]byte(`{}`))
	var sawBackend string
	pol := &testutils.ConfigurableUpstreamMockPolicy{
		MockMode: policy.ProcessingMode{UpstreamResponseMode: policy.BodyModeBuffer},
		OnRespFn: func(c *policy.UpstreamAttemptContext, _ map[string]interface{}) policy.ResponseAction {
			sawBackend = c.Name
			return policy.DownstreamResponseModifications{}
		},
	}
	specs := []policy.PolicySpec{newPolicySpec("bedrock-transformer", "v1.0.0", true, nil)}

	result, err := executor.ExecuteUpstreamResponsePolicies(ctx, []policy.Policy{pol}, upCtx, specs, "api", "route")

	require.NoError(t, err)
	require.Len(t, result.Results, 1)
	assert.Equal(t, "test-backend", sawBackend)
	assert.False(t, result.ShortCircuited)
}

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

// upstreamHeaderMockPolicy is a header-only policy (like set-headers) — no
// body-phase interface at all — exercising the upstream-attempt phase's
// header dispatch, which reuses RequestHeaderPolicy/ResponseHeaderPolicy
// unmodified.
type upstreamHeaderMockPolicy struct {
	mode    policy.ProcessingMode
	onReq   func(*policy.RequestHeaderContext) policy.RequestHeaderAction
	onResp  func(*policy.ResponseHeaderContext) policy.ResponseHeaderAction
	sawPath string
}

func (p *upstreamHeaderMockPolicy) Mode() policy.ProcessingMode { return p.mode }

func (p *upstreamHeaderMockPolicy) OnRequestHeaders(_ context.Context, ctx *policy.RequestHeaderContext, _ map[string]interface{}) policy.RequestHeaderAction {
	p.sawPath = ctx.Path
	if p.onReq != nil {
		return p.onReq(ctx)
	}
	return nil
}

func (p *upstreamHeaderMockPolicy) OnResponseHeaders(_ context.Context, ctx *policy.ResponseHeaderContext, _ map[string]interface{}) policy.ResponseHeaderAction {
	if p.onResp != nil {
		return p.onResp(ctx)
	}
	return nil
}

func TestExecuteUpstreamAttemptRequestPolicies_EmptyPolicyList(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	exec := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	reqCtx := testutils.NewTestUpstreamAttemptRequestContext([]byte(`{"model":"gpt-4o"}`))

	action, err := exec.ExecuteUpstreamAttemptRequestPolicies(ctx, []policy.Policy{}, reqCtx, []policy.PolicySpec{}, "api", "route")

	require.NoError(t, err)
	assert.Nil(t, action)
}

func TestExecuteUpstreamAttemptRequestPolicies_SkipsPolicyNotImplementingInterface(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	exec := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	reqCtx := testutils.NewTestUpstreamAttemptRequestContext([]byte(`{}`))
	// A header-only policy has no RequestPolicy implementation — must be
	// skipped silently by the request-body dispatch, the same as any
	// non-implementing policy is skipped in the downstream phases.
	policies := []policy.Policy{&upstreamHeaderMockPolicy{mode: policy.ProcessingMode{RequestHeaderMode: policy.HeaderModeProcess}}}
	specs := []policy.PolicySpec{newPolicySpec("hdr-only", "v1.0.0", true, nil)}

	action, err := exec.ExecuteUpstreamAttemptRequestPolicies(ctx, policies, reqCtx, specs, "api", "route")

	require.NoError(t, err)
	assert.Nil(t, action)
}

func TestExecuteUpstreamAttemptRequestPolicies_ExecutesForBackend(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	exec := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	original := []byte(`{"model":"gpt-4o"}`)
	reqCtx := testutils.NewTestUpstreamAttemptRequestContext(original)

	var sawBackend string
	var sawBody []byte
	var sawDownstreamNil bool
	pol := &testutils.ConfigurableUpstreamMockPolicy{
		Name: "api-key-auth",
		OnReqFn: func(c *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
			sawBackend = c.Upstream.Name
			sawBody = c.Body.Content
			sawDownstreamNil = c.Downstream == nil
			return policy.UpstreamRequestModifications{
				HeadersToSet: map[string]string{"authorization": "Bearer backend-specific-key"},
			}
		},
	}
	specs := []policy.PolicySpec{newPolicySpec("api-key-auth", "v1.0.0", true, nil)}

	action, err := exec.ExecuteUpstreamAttemptRequestPolicies(ctx, []policy.Policy{pol}, reqCtx, specs, "api", "route")

	require.NoError(t, err)
	require.NotNil(t, action)
	assert.Equal(t, "test-backend", sawBackend)
	assert.Equal(t, original, sawBody)
	assert.True(t, sawDownstreamNil, "Downstream must be nil for an upstream-attempt invocation")
	assert.Equal(t, []string{"Bearer backend-specific-key"}, reqCtx.Headers.UnsafeInternalValues()["authorization"])
}

func TestExecuteUpstreamAttemptRequestPolicies_DisabledPolicySkipped(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	exec := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	reqCtx := testutils.NewTestUpstreamAttemptRequestContext([]byte(`{}`))
	called := false
	pol := &testutils.ConfigurableUpstreamMockPolicy{
		OnReqFn: func(_ *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
			called = true
			return nil
		},
	}
	specs := []policy.PolicySpec{newPolicySpec("disabled-auth", "v1.0.0", false, nil)}

	_, err := exec.ExecuteUpstreamAttemptRequestPolicies(ctx, []policy.Policy{pol}, reqCtx, specs, "api", "route")

	require.NoError(t, err)
	assert.False(t, called)
}

func TestExecuteUpstreamAttemptRequestPolicies_ShortCircuit(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	exec := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	reqCtx := testutils.NewTestUpstreamAttemptRequestContext([]byte(`{}`))
	pol := &testutils.ConfigurableUpstreamMockPolicy{
		OnReqFn: func(_ *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
			return policy.ImmediateResponse{StatusCode: 401}
		},
	}
	specs := []policy.PolicySpec{newPolicySpec("auth-fail", "v1.0.0", true, nil)}

	action, err := exec.ExecuteUpstreamAttemptRequestPolicies(ctx, []policy.Policy{pol}, reqCtx, specs, "api", "route")

	require.NoError(t, err)
	ir, ok := action.(policy.ImmediateResponse)
	require.True(t, ok)
	assert.Equal(t, 401, ir.StatusCode)
}

func TestExecuteUpstreamAttemptRequestPolicies_TransformedBodyReachesNextPolicy(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	exec := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	original := []byte(`{"model":"gpt-4o"}`)
	reqCtx := testutils.NewTestUpstreamAttemptRequestContext(original)

	translated := []byte(`{"anthropic_version":"bedrock-2023-05-31"}`)
	transformer := &testutils.ConfigurableUpstreamMockPolicy{
		OnReqFn: func(_ *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
			return policy.UpstreamRequestModifications{Body: translated}
		},
	}
	// A later policy (e.g. an auth policy signing the request) must see the
	// transformer's output threaded into Body — the same accumulation the
	// downstream request phase already relies on (executor.applyRequestModifications).
	var sawBody []byte
	authPolicy := &testutils.ConfigurableUpstreamMockPolicy{
		OnReqFn: func(c *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
			sawBody = c.Body.Content
			return nil
		},
	}
	specs := []policy.PolicySpec{
		newPolicySpec("transformer", "v1.0.0", true, nil),
		newPolicySpec("auth", "v1.0.0", true, nil),
	}

	_, err := exec.ExecuteUpstreamAttemptRequestPolicies(ctx, []policy.Policy{transformer, authPolicy}, reqCtx, specs, "api", "route")

	require.NoError(t, err)
	assert.Equal(t, translated, sawBody, "the auth policy must see the transformer's output")
}

func TestExecuteUpstreamAttemptResponsePolicies_ExecutesForWinningBackend(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	exec := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	respCtx := testutils.NewTestUpstreamAttemptResponseContext([]byte(`{}`), []byte(`{}`))
	var sawBackend string
	pol := &testutils.ConfigurableUpstreamMockPolicy{
		OnRespFn: func(c *policy.ResponseContext, _ map[string]interface{}) policy.ResponseAction {
			sawBackend = c.Upstream.Name
			return policy.DownstreamResponseModifications{}
		},
	}
	specs := []policy.PolicySpec{newPolicySpec("bedrock-transformer", "v1.0.0", true, nil)}

	action, err := exec.ExecuteUpstreamAttemptResponsePolicies(ctx, []policy.Policy{pol}, respCtx, specs, "api", "route")

	require.NoError(t, err)
	require.NotNil(t, action)
	assert.Equal(t, "test-backend", sawBackend)
}

// TestExecuteUpstreamAttemptRequestHeaderPolicies_HeaderOnlyPolicyRunsUnmodified
// covers the target scenario: a policy like set-headers, implementing only
// RequestHeaderPolicy, runs on the upstream-attempt request-header phase via
// the exact same interface it already implements downstream — no wrapper, no
// separate upstream interface.
func TestExecuteUpstreamAttemptRequestHeaderPolicies_HeaderOnlyPolicyRunsUnmodified(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	exec := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	reqCtx := &policy.RequestHeaderContext{
		SharedContext: &policy.SharedContext{Metadata: map[string]interface{}{}},
		Headers:       policy.NewHeaders(nil),
		Path:          "/v1/test",
		Method:        "POST",
		Upstream:      &policy.UpstreamRequestContext{Name: "test-backend", URL: "https://backend.example.com", BasePath: "/v1"},
	}
	pol := &upstreamHeaderMockPolicy{
		mode: policy.ProcessingMode{RequestHeaderMode: policy.HeaderModeProcess},
		onReq: func(_ *policy.RequestHeaderContext) policy.RequestHeaderAction {
			return policy.UpstreamRequestHeaderModifications{HeadersToSet: map[string]string{"x-set-headers": "1"}}
		},
	}
	specs := []policy.PolicySpec{newPolicySpec("set-headers", "v1.0.0", true, nil)}

	action, err := exec.ExecuteUpstreamAttemptRequestHeaderPolicies(ctx, []policy.Policy{pol}, reqCtx, specs, "api", "route")

	require.NoError(t, err)
	require.NotNil(t, action)
	assert.Equal(t, reqCtx.Path, pol.sawPath)
	assert.Equal(t, []string{"1"}, reqCtx.Headers.UnsafeInternalValues()["x-set-headers"])
}

func TestExecuteUpstreamAttemptResponseHeaderPolicies_HeaderOnlyPolicyRunsUnmodified(t *testing.T) {
	tracer := noop.NewTracerProvider().Tracer("test")
	exec := NewChainExecutor(nil, nil, tracer)

	ctx := context.Background()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:   &policy.SharedContext{Metadata: map[string]interface{}{}},
		ResponseHeaders: policy.NewHeaders(nil),
		Upstream:        &policy.UpstreamResponseContext{Name: "test-backend", URL: "https://backend.example.com", BasePath: "/v1"},
	}
	pol := &upstreamHeaderMockPolicy{
		mode: policy.ProcessingMode{ResponseHeaderMode: policy.HeaderModeProcess},
		onResp: func(_ *policy.ResponseHeaderContext) policy.ResponseHeaderAction {
			return policy.DownstreamResponseHeaderModifications{HeadersToSet: map[string]string{"x-set-headers-resp": "1"}}
		},
	}
	specs := []policy.PolicySpec{newPolicySpec("set-headers", "v1.0.0", true, nil)}

	action, err := exec.ExecuteUpstreamAttemptResponseHeaderPolicies(ctx, []policy.Policy{pol}, respCtx, specs, "api", "route")

	require.NoError(t, err)
	require.NotNil(t, action)
	assert.Equal(t, []string{"1"}, respCtx.ResponseHeaders.UnsafeInternalValues()["x-set-headers-resp"])
}

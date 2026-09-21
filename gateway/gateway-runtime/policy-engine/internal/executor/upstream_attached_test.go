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
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/registry"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/testutils"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
	"go.opentelemetry.io/otel/trace/noop"
)

// plainBodyPolicy is an ordinary policy: one implementation, only the plain
// RequestPolicy/ResponsePolicy interface, no upstream-phase awareness at all.
type plainBodyPolicy struct {
	sawPath string
	sawBody string
}

func (p *plainBodyPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{RequestBodyMode: policy.BodyModeBuffer, ResponseBodyMode: policy.BodyModeBuffer}
}

func (p *plainBodyPolicy) OnRequestBody(_ context.Context, rc *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
	p.sawPath = rc.Path
	p.sawBody = string(rc.Body.Content)
	return policy.UpstreamRequestModifications{Body: []byte("rewritten"), HeadersToSet: map[string]string{"x-plain": "1"}}
}

func (p *plainBodyPolicy) OnResponseBody(_ context.Context, rc *policy.ResponseContext, _ map[string]interface{}) policy.ResponseAction {
	return policy.DownstreamResponseModifications{Body: []byte("resp:" + string(rc.ResponseBody.Content))}
}

// headerOnlyPolicy has no body-phase interface (like set-headers/oauth2-generator) —
// only RequestHeaderPolicy/ResponseHeaderPolicy, declared via the same four
// pre-existing ProcessingMode fields every downstream header policy uses.
type headerOnlyPolicy struct {
	sawPath string
}

func (headerOnlyPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{RequestHeaderMode: policy.HeaderModeProcess, ResponseHeaderMode: policy.HeaderModeProcess}
}

func (p *headerOnlyPolicy) OnRequestHeaders(_ context.Context, rc *policy.RequestHeaderContext, _ map[string]interface{}) policy.RequestHeaderAction {
	p.sawPath = rc.Path
	return policy.UpstreamRequestHeaderModifications{HeadersToSet: map[string]string{"x-hdr-only": "1"}}
}

func (p *headerOnlyPolicy) OnResponseHeaders(_ context.Context, _ *policy.ResponseHeaderContext, _ map[string]interface{}) policy.ResponseHeaderAction {
	return policy.DownstreamResponseHeaderModifications{HeadersToSet: map[string]string{"x-hdr-only-resp": "1"}}
}

// noHeaderOrBodyPolicy implements neither phase (Mode() reports nothing).
type noHeaderOrBodyPolicy struct{}

func (noHeaderOrBodyPolicy) Mode() policy.ProcessingMode { return policy.ProcessingMode{} }

func TestWrapUpstreamAttached_RunsPlainPolicyOnlyUpstream(t *testing.T) {
	inner := &plainBodyPolicy{}
	wrapped := registry.WrapUpstreamAttached(inner, "plain", "route")

	// The downstream chain must never see it.
	mode := wrapped.Mode()
	assert.Contains(t, []policy.BodyProcessingMode{"", policy.BodyModeSkip}, mode.RequestBodyMode)
	assert.Contains(t, []policy.BodyProcessingMode{"", policy.BodyModeSkip}, mode.ResponseBodyMode)
	_, isReq := wrapped.(policy.RequestPolicy)
	_, isResp := wrapped.(policy.ResponsePolicy)
	assert.False(t, isReq)
	assert.False(t, isResp)
	assert.Equal(t, policy.BodyModeBuffer, mode.UpstreamRequestMode)
	assert.Equal(t, policy.BodyModeBuffer, mode.UpstreamResponseMode)

	exec := NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test"))
	upCtx := testutils.NewTestUpstreamAttemptContext([]byte("orig"))
	specs := []policy.PolicySpec{newPolicySpec("plain", "v1", true, nil)}

	_, err := exec.ExecuteUpstreamRequestPolicies(context.Background(), []policy.Policy{wrapped}, upCtx, specs, "api", "route")
	require.NoError(t, err)
	assert.Equal(t, "orig", inner.sawBody)
	assert.Equal(t, upCtx.Path, inner.sawPath)
	assert.Equal(t, "rewritten", string(upCtx.Body.Content))
	assert.Equal(t, []string{"1"}, upCtx.Headers.UnsafeInternalValues()["x-plain"])

	upCtx.Body = &policy.Body{Content: []byte("upstream-reply"), Present: true, EndOfStream: true}
	_, err = exec.ExecuteUpstreamResponsePolicies(context.Background(), []policy.Policy{wrapped}, upCtx, specs, "api", "route")
	require.NoError(t, err)
	assert.Equal(t, "resp:upstream-reply", string(upCtx.Body.Content))
}

// TestWrapUpstreamAttached_HeaderOnlyPolicyRunsViaBodyPhaseDelegation covers
// the mechanism added for "header phase in upstream too": a policy with no
// body-phase interface (set-headers, oauth2-generator) still runs on every
// upstream attempt, through the same pre-existing UpstreamRequestPolicy/
// UpstreamResponsePolicy interfaces and UpstreamRequestMode/UpstreamResponseMode
// fields as a body policy — no new SDK interface, no new ProcessingMode field.
// The wrapper delegates to OnRequestHeaders/OnResponseHeaders and converts the
// returned header action into the equivalent body-phase action, which the
// existing upstream executor and header-mutation delivery path already handle.
func TestWrapUpstreamAttached_HeaderOnlyPolicyRunsViaBodyPhaseDelegation(t *testing.T) {
	inner := &headerOnlyPolicy{}
	wrapped := registry.WrapUpstreamAttached(inner, "hdr", "route")

	mode := wrapped.Mode()
	assert.Equal(t, policy.BodyModeBuffer, mode.UpstreamRequestMode)
	assert.Equal(t, policy.BodyModeBuffer, mode.UpstreamResponseMode)
	_, isReq := wrapped.(policy.RequestPolicy)
	_, isReqHdr := wrapped.(policy.RequestHeaderPolicy)
	assert.False(t, isReq)
	assert.False(t, isReqHdr, "the wrapper itself must not implement RequestHeaderPolicy or the downstream chain would run it too")

	exec := NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test"))
	upCtx := testutils.NewTestUpstreamAttemptContext([]byte("orig"))
	specs := []policy.PolicySpec{newPolicySpec("hdr", "v1", true, nil)}

	_, err := exec.ExecuteUpstreamRequestPolicies(context.Background(), []policy.Policy{wrapped}, upCtx, specs, "api", "route")
	require.NoError(t, err)
	assert.Equal(t, upCtx.Path, inner.sawPath)
	assert.Equal(t, []string{"1"}, upCtx.Headers.UnsafeInternalValues()["x-hdr-only"])

	_, err = exec.ExecuteUpstreamResponsePolicies(context.Background(), []policy.Policy{wrapped}, upCtx, specs, "api", "route")
	require.NoError(t, err)
	assert.Equal(t, []string{"1"}, upCtx.Headers.UnsafeInternalValues()["x-hdr-only-resp"])
}

func TestWrapUpstreamAttached_NoPhaseGetsNoUpstreamPhase(t *testing.T) {
	mode := registry.WrapUpstreamAttached(noHeaderOrBodyPolicy{}, "none", "route").Mode()
	assert.Contains(t, []policy.BodyProcessingMode{"", policy.BodyModeSkip}, mode.UpstreamRequestMode)
	assert.Contains(t, []policy.BodyProcessingMode{"", policy.BodyModeSkip}, mode.UpstreamResponseMode)
}

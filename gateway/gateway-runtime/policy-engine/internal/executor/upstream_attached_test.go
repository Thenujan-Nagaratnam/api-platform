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

// headerOnlyPolicy has no body-phase interface (like set-headers).
type headerOnlyPolicy struct{}

func (headerOnlyPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{RequestHeaderMode: policy.HeaderModeProcess}
}

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

func TestWrapUpstreamAttached_HeaderOnlyPolicyGetsNoUpstreamPhase(t *testing.T) {
	mode := registry.WrapUpstreamAttached(headerOnlyPolicy{}, "hdr", "route").Mode()
	assert.Contains(t, []policy.BodyProcessingMode{"", policy.BodyModeSkip}, mode.UpstreamRequestMode)
	assert.Contains(t, []policy.BodyProcessingMode{"", policy.BodyModeSkip}, mode.UpstreamResponseMode)
}

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

package kernel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// The kernel seeds nothing into an attempt's SharedContext: attempt identity
// (selected_provider/selected_model) is written by whichever upstream-attempt
// policy owns the chain — today model-failover's own OnRequestHeaders.
func TestNewUpstreamAttemptSharedContext_StartsEmptyAndWritable(t *testing.T) {
	shared := NewUpstreamAttemptSharedContext()

	require.NotNil(t, shared)
	require.NotNil(t, shared.Metadata)
	assert.Empty(t, shared.Metadata)

	shared.Metadata["selected_provider"] = "anthropic-upstream"
	assert.Equal(t, "anthropic-upstream", shared.Metadata["selected_provider"])
}

func TestBuildUpstreamAttemptRequestContext_SeedsBodyFromOriginal(t *testing.T) {
	original := []byte(`{"model":"gpt-4o"}`)
	shared := NewUpstreamAttemptSharedContext()
	headers := policy.NewHeaders(map[string][]string{"content-type": {"application/json"}})

	reqCtx := BuildUpstreamAttemptRequestContext(shared, "", headers, original, "openai-primary", "https://api.openai.com", "/v1", "POST", "/v1/chat/completions")

	require.NotNil(t, reqCtx)
	require.NotNil(t, reqCtx.Upstream)
	assert.Equal(t, "openai-primary", reqCtx.Upstream.Name)
	assert.Equal(t, "https://api.openai.com", reqCtx.Upstream.URL)
	assert.Equal(t, "/v1", reqCtx.Upstream.BasePath)
	require.NotNil(t, reqCtx.Body)
	assert.Equal(t, original, reqCtx.Body.Content)
	assert.Nil(t, reqCtx.Downstream, "Downstream must be nil — the signal that this is an upstream-attempt invocation")
}

func TestBuildUpstreamAttemptRequestContext_RetryUsesOriginalNotPreviousOutput(t *testing.T) {
	original := []byte(`{"model":"gpt-4o"}`)

	// Attempt 1 goes to the primary and its transformer mutates the working
	// body (simulated here — the point under test is that this mutation
	// never leaks into how a later attempt is built).
	attempt1 := BuildUpstreamAttemptRequestContext(NewUpstreamAttemptSharedContext(), "", policy.NewHeaders(nil), original,
		"openai-primary", "https://api.openai.com", "/v1", "POST", "/v1/chat/completions")
	attempt1.Body.Content = []byte(`{"totally":"different, mutated by attempt 1's transformer"}`)

	// Attempt 2 (the fallback, a different provider) must be built fresh from
	// the SAME cached original bytes — never from attempt1's mutated Body.
	attempt2 := BuildUpstreamAttemptRequestContext(NewUpstreamAttemptSharedContext(), "", policy.NewHeaders(nil), original,
		"anthropic-fallback", "https://api.anthropic.com", "/v1", "POST", "/v1/messages")

	assert.Equal(t, "anthropic-fallback", attempt2.Upstream.Name)
	assert.Equal(t, original, attempt2.Body.Content, "attempt 2 must start from the original bytes, not attempt 1's mutated output")
}

func TestBuildUpstreamAttemptRequestContext_SharedContextIsNeverNil(t *testing.T) {
	// A policy's OnRequestBody routinely writes through reqCtx.SharedContext
	// (e.g. aws-authentication's authSuccess/authFailure record an
	// AuthContext there) — a nil SharedContext panics the whole
	// policy-engine connection on the very first real upstream-phase signing
	// attempt, so it must always be a real, usable pointer, mirroring every
	// other per-request context the kernel builds.
	reqCtx := BuildUpstreamAttemptRequestContext(NewUpstreamAttemptSharedContext(), "", policy.NewHeaders(nil), []byte(`{}`),
		"backend", "https://example.com", "/v1", "POST", "/v1/chat/completions")

	require.NotNil(t, reqCtx.SharedContext)
	assert.NotPanics(t, func() {
		reqCtx.SharedContext.AuthContext = &policy.AuthContext{Authenticated: true}
	})
	require.NotNil(t, reqCtx.SharedContext.Metadata)
	assert.NotPanics(t, func() {
		reqCtx.SharedContext.Metadata["k"] = "v"
	})
}

func TestBuildUpstreamAttemptRequestContext_OriginalSliceNotAliasedByBody(t *testing.T) {
	original := []byte(`{"model":"gpt-4o"}`)

	reqCtx := BuildUpstreamAttemptRequestContext(NewUpstreamAttemptSharedContext(), "", policy.NewHeaders(nil), original,
		"backend", "https://example.com", "/v1", "POST", "/v1/chat/completions")

	// Mutating Body.Content (as a transformer policy would, via
	// UpstreamRequestModifications.Body) must never mutate the byte slice a
	// LATER attempt's original bytes are built from — otherwise attempt 2
	// would silently observe attempt 1's transformation even though it's
	// re-reading "the original".
	reqCtx.Body.Content[0] = 'X'

	assert.Equal(t, byte('{'), original[0], "the caller's original slice must not be corrupted by mutating the attempt's Body")
}

func TestBuildUpstreamAttemptResponseContext_SetsResponseStatusAndBackend(t *testing.T) {
	shared := NewUpstreamAttemptSharedContext()
	headers := policy.NewHeaders(map[string][]string{"content-type": {"application/json"}})

	respCtx := BuildUpstreamAttemptResponseContext(shared, "", headers, []byte(`{"model":"gpt-4o"}`), []byte(`{"result":"ok"}`),
		"anthropic-upstream-cluster", "https://api.anthropic.com", "/v1", "POST", "/v1/messages", 500)

	require.NotNil(t, respCtx)
	assert.Equal(t, 500, respCtx.ResponseStatus)
	require.NotNil(t, respCtx.Upstream)
	assert.Equal(t, "anthropic-upstream-cluster", respCtx.Upstream.Name)
	assert.Equal(t, []byte(`{"model":"gpt-4o"}`), respCtx.RequestBody.Content)
	assert.Equal(t, []byte(`{"result":"ok"}`), respCtx.ResponseBody.Content)
	assert.Nil(t, respCtx.Downstream, "Downstream must be nil — the signal that this is an upstream-attempt invocation")
}

func TestBuildUpstreamAttemptRequestHeaderContext_SharesHeadersWithBodyPhase(t *testing.T) {
	shared := NewUpstreamAttemptSharedContext()
	headers := policy.NewHeaders(map[string][]string{"content-type": {"application/json"}})

	hdrCtx := BuildUpstreamAttemptRequestHeaderContext(shared, "", headers, "backend", "https://example.com", "/v1", "POST", "/v1/chat/completions")
	// A header-phase policy mutates via UnsafeInternalValues() — the same
	// object must be visible from the later body-phase context.
	hdrCtx.Headers.UnsafeInternalValues()["x-set-headers"] = []string{"1"}

	reqCtx := BuildUpstreamAttemptRequestContext(shared, "", headers, []byte(`{}`), "backend", "https://example.com", "/v1", "POST", "/v1/chat/completions")

	assert.Equal(t, []string{"1"}, reqCtx.Headers.UnsafeInternalValues()["x-set-headers"])
}

func TestBuildUpstreamAttemptRequestHeaderContext_SetsRouteCluster(t *testing.T) {
	shared := NewUpstreamAttemptSharedContext()
	headers := policy.NewHeaders(nil)

	hdrCtx := BuildUpstreamAttemptRequestHeaderContext(shared, "failover_agg_chat_0", headers,
		"openai-provider-cluster", "https://api.openai.com", "/v1", "POST", "/chat/completions")

	require.NotNil(t, hdrCtx.Upstream)
	assert.Equal(t, "failover_agg_chat_0", hdrCtx.Upstream.RouteCluster)
	assert.Equal(t, "openai-provider-cluster", hdrCtx.Upstream.Name, "RouteCluster must not overwrite the resolved Name")
}

func TestBuildUpstreamAttemptResponseContext_SetsRouteCluster(t *testing.T) {
	shared := NewUpstreamAttemptSharedContext()
	headers := policy.NewHeaders(nil)

	respCtx := BuildUpstreamAttemptResponseContext(shared, "failover_agg_chat_0", headers,
		[]byte(`{}`), []byte(`{}`), "openai-provider-cluster", "https://api.openai.com", "/v1", "POST", "/chat/completions", 200)

	require.NotNil(t, respCtx.Upstream)
	assert.Equal(t, "failover_agg_chat_0", respCtx.Upstream.RouteCluster)
}

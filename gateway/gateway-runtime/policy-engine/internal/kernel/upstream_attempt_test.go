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

func TestBuildUpstreamAttemptContext_SeedsBodyFromOriginal(t *testing.T) {
	original := []byte(`{"model":"gpt-4o"}`)

	attemptCtx := BuildUpstreamAttemptContext(original, map[string][]string{"content-type": {"application/json"}}, "openai-primary", "https://api.openai.com", "/v1", "POST", "/v1/chat/completions", false, "", "", 0)

	require.NotNil(t, attemptCtx)
	assert.Equal(t, "openai-primary", attemptCtx.Name)
	assert.Equal(t, "https://api.openai.com", attemptCtx.URL)
	assert.Equal(t, "/v1", attemptCtx.BasePath)
	assert.Equal(t, original, attemptCtx.OriginalRequestRaw)
	require.NotNil(t, attemptCtx.Body)
	assert.Equal(t, original, attemptCtx.Body.Content)
	assert.False(t, attemptCtx.IsRetry)
	assert.Empty(t, attemptCtx.ResolvedModel)
	assert.Empty(t, attemptCtx.ResolvedProvider)
}

func TestBuildUpstreamAttemptContext_SetsResolvedModelAndProviderForFailoverAttempt(t *testing.T) {
	original := []byte(`{"model":"gpt-4o"}`)

	attemptCtx := BuildUpstreamAttemptContext(original, nil, "anthropic-upstream-cluster", "https://api.anthropic.com", "/v1", "POST", "/v1/messages", true, "claude-3-5-sonnet-20241022", "anthropic-upstream", 0)

	require.NotNil(t, attemptCtx)
	assert.Equal(t, "claude-3-5-sonnet-20241022", attemptCtx.ResolvedModel)
	assert.Equal(t, "anthropic-upstream", attemptCtx.ResolvedProvider)
}

func TestBuildUpstreamAttemptContext_RetryUsesOriginalNotPreviousOutput(t *testing.T) {
	original := []byte(`{"model":"gpt-4o"}`)

	// Attempt 1 goes to the primary and its transformer mutates the working
	// body (simulated here — the point under test is that this mutation
	// never leaks into how a later attempt is built).
	attempt1 := BuildUpstreamAttemptContext(original, nil, "openai-primary", "https://api.openai.com", "/v1", "POST", "/v1/chat/completions", false, "", "", 0)
	attempt1.Body.Content = []byte(`{"totally":"different, mutated by attempt 1's transformer"}`)

	// Attempt 2 (the fallback, a different provider) must be built fresh from
	// the SAME cached original bytes — never from attempt1's mutated Body.
	attempt2 := BuildUpstreamAttemptContext(original, nil, "anthropic-fallback", "https://api.anthropic.com", "/v1", "POST", "/v1/messages", true, "", "", 0)

	assert.Equal(t, "anthropic-fallback", attempt2.Name)
	assert.True(t, attempt2.IsRetry)
	assert.Equal(t, original, attempt2.OriginalRequestRaw)
	assert.Equal(t, original, attempt2.Body.Content, "attempt 2 must start from the original bytes, not attempt 1's mutated output")
}

func TestBuildUpstreamAttemptContext_SharedContextIsNeverNil(t *testing.T) {
	// A policy's OnUpstreamRequestBody routinely writes through
	// upCtx.SharedContext (e.g. aws-authentication's authSuccess/authFailure
	// record an AuthContext there) — confirmed live: a nil SharedContext
	// panics the whole policy-engine connection on the very first real
	// upstream-phase signing attempt. UpstreamAttemptContext embeds
	// *SharedContext, so it must always be a real, usable pointer, mirroring
	// every other per-request context the kernel builds.
	attemptCtx := BuildUpstreamAttemptContext([]byte(`{}`), nil, "backend", "https://example.com", "/v1", "POST", "/v1/chat/completions", false, "", "", 0)

	require.NotNil(t, attemptCtx.SharedContext)
	assert.NotPanics(t, func() {
		attemptCtx.SharedContext.AuthContext = &policy.AuthContext{Authenticated: true}
	})
	require.NotNil(t, attemptCtx.SharedContext.Metadata)
	assert.NotPanics(t, func() {
		attemptCtx.SharedContext.Metadata["k"] = "v"
	})
}

func TestBuildUpstreamAttemptContext_SetsResponseStatusCode(t *testing.T) {
	attemptCtx := BuildUpstreamAttemptContext([]byte(`{}`), nil, "anthropic-upstream-cluster", "https://api.anthropic.com", "/v1", "POST", "/v1/messages", true, "claude-3-5-sonnet-20241022", "anthropic-upstream", 500)

	assert.Equal(t, 500, attemptCtx.ResponseStatusCode)
}

func TestBuildUpstreamAttemptContext_OriginalSliceNotAliasedByBody(t *testing.T) {
	original := []byte(`{"model":"gpt-4o"}`)

	attemptCtx := BuildUpstreamAttemptContext(original, nil, "backend", "https://example.com", "/v1", "POST", "/v1/chat/completions", false, "", "", 0)

	// Mutating Body.Content (as a transformer policy would, via
	// UpstreamRequestModifications.Body) must never mutate the byte slice a
	// LATER attempt's OriginalRequestRaw reads from — otherwise attempt 2
	// would silently observe attempt 1's transformation even though it's
	// re-reading "the original".
	attemptCtx.Body.Content[0] = 'X'

	assert.Equal(t, byte('{'), original[0], "the caller's original slice must not be corrupted by mutating the attempt's Body")
}

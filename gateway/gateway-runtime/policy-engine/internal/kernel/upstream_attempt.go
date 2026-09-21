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
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// selectedProviderMetadataKey/selectedModelMetadataKey are the
// SharedContext.Metadata keys llm-header-router writes downstream and
// provider-scoped policies read to self-gate (e.g. the OpenAI->Anthropic
// transformer's shouldRunForSelected). Seeding them here from the attempt's
// resolved model/provider lets a policy using that same convention run
// unmodified per attempt: it no-ops for a backend it doesn't own and runs for
// the one it does.
const (
	selectedProviderMetadataKey = "selected_provider"
	selectedModelMetadataKey    = "selected_model"
)

// NewUpstreamAttemptSharedContext builds the SharedContext for one upstream
// attempt, seeded with the attempt's resolved provider/model (both empty for
// an attempt resolved outside a declared resilience.failover chain).
func NewUpstreamAttemptSharedContext(model, provider string) *policy.SharedContext {
	shared := &policy.SharedContext{Metadata: make(map[string]interface{})}
	if provider != "" {
		shared.Metadata[selectedProviderMetadataKey] = provider
	}
	if model != "" {
		shared.Metadata[selectedModelMetadataKey] = model
	}
	return shared
}

// BuildUpstreamAttemptRequestHeaderContext constructs a *policy.RequestHeaderContext
// for one upstream attempt's request-header phase — the same context type and
// RequestHeaderPolicy.OnRequestHeaders interface a policy already implements
// for the downstream phase. Downstream is left nil: the signal a policy uses
// to tell this invocation apart from a genuine downstream one (see
// RequestHeaderContext's own doc comment in the SDK). headers is reused
// (same *policy.Headers) for the later request-body-phase context so header
// mutations from this phase are visible there.
func BuildUpstreamAttemptRequestHeaderContext(
	shared *policy.SharedContext,
	headers *policy.Headers,
	backendName, backendURL, basePath, method, outboundPath string,
) *policy.RequestHeaderContext {
	return &policy.RequestHeaderContext{
		SharedContext: shared,
		Headers:       headers,
		Path:          outboundPath,
		Method:        method,
		Upstream:      &policy.UpstreamRequestContext{Name: backendName, URL: backendURL, BasePath: basePath},
	}
}

// BuildUpstreamAttemptRequestContext is
// BuildUpstreamAttemptRequestHeaderContext's body-phase counterpart. original
// is the client's original request body, captured once downstream and
// replayed unchanged into every attempt — the caller must always pass that
// same cached slice, never a previous attempt's already-mutated output. It is
// defensively copied into Body so a policy's in-place mutation can never
// corrupt the caller's cached slice for a later attempt.
func BuildUpstreamAttemptRequestContext(
	shared *policy.SharedContext,
	headers *policy.Headers,
	original []byte,
	backendName, backendURL, basePath, method, outboundPath string,
) *policy.RequestContext {
	bodyCopy := make([]byte, len(original))
	copy(bodyCopy, original)

	return &policy.RequestContext{
		SharedContext: shared,
		Headers:       headers,
		Body: &policy.Body{
			Content:     bodyCopy,
			EndOfStream: true,
			Present:     len(original) > 0,
		},
		Path:     outboundPath,
		Method:   method,
		Upstream: &policy.UpstreamRequestContext{Name: backendName, URL: backendURL, BasePath: basePath},
	}
}

// BuildUpstreamAttemptResponseHeaderContext constructs a
// *policy.ResponseHeaderContext for one upstream attempt's response-header
// phase. responseHeaders is reused (same *policy.Headers) for the later
// response-body-phase context so header mutations from this phase are
// visible there.
func BuildUpstreamAttemptResponseHeaderContext(
	shared *policy.SharedContext,
	responseHeaders *policy.Headers,
	backendName, backendURL, basePath, requestMethod, requestPath string,
	statusCode int,
) *policy.ResponseHeaderContext {
	return &policy.ResponseHeaderContext{
		SharedContext:   shared,
		RequestPath:     requestPath,
		RequestMethod:   requestMethod,
		ResponseHeaders: responseHeaders,
		ResponseStatus:  statusCode,
		Upstream: &policy.UpstreamResponseContext{
			Name: backendName, URL: backendURL, BasePath: basePath,
			Response: &policy.UpstreamResponse{Headers: responseHeaders, StatusCode: statusCode},
		},
	}
}

// BuildUpstreamAttemptResponseContext is
// BuildUpstreamAttemptResponseHeaderContext's body-phase counterpart.
// originalRequestRaw is the client's original request body (see
// BuildUpstreamAttemptRequestContext's own doc); body is this attempt's
// upstream response body.
func BuildUpstreamAttemptResponseContext(
	shared *policy.SharedContext,
	responseHeaders *policy.Headers,
	originalRequestRaw, body []byte,
	backendName, backendURL, basePath, requestMethod, requestPath string,
	statusCode int,
) *policy.ResponseContext {
	return &policy.ResponseContext{
		SharedContext: shared,
		RequestBody: &policy.Body{
			Content:     originalRequestRaw,
			Present:     len(originalRequestRaw) > 0,
			EndOfStream: true,
		},
		RequestPath:     requestPath,
		RequestMethod:   requestMethod,
		ResponseHeaders: responseHeaders,
		ResponseBody: &policy.Body{
			Content:     body,
			EndOfStream: true,
			Present:     len(body) > 0,
		},
		ResponseStatus: statusCode,
		Upstream: &policy.UpstreamResponseContext{
			Name: backendName, URL: backendURL, BasePath: basePath,
			Response: &policy.UpstreamResponse{Headers: responseHeaders, StatusCode: statusCode},
		},
	}
}

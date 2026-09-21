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

// BuildUpstreamAttemptContext constructs a fresh *policy.UpstreamAttemptContext
// for a single upstream attempt, seeded from the client's original request
// bytes — captured once, downstream, before any policy mutated them.
//
// This is the contract that makes cross-backend failover correct: the caller
// (the upstream ext_proc server, invoked fresh by Envoy on every attempt
// including retries to a different backend) must always pass the SAME cached
// original bytes here, never a previous attempt's already-translated output.
// This function enforces its half of that contract by defensively copying
// original into Body, so a policy's in-place mutation of this attempt's
// working body (or of the returned context generally) can never corrupt the
// caller's cached original slice for a subsequent attempt.
// model and provider identify which model/provider this attempt represents
// for a route resolved from a declared resilience.failover chain — both
// empty for every other resolution path, which leaves
// UpstreamAttemptContext.ResolvedModel/ResolvedProvider empty exactly as
// before this parameter pair existed.
//
// statusCode is meaningful only for a response-phase build (the caller
// passes 0 at the request phase, before any response exists) — it becomes
// UpstreamAttemptContext.ResponseStatusCode.
func BuildUpstreamAttemptContext(
	original []byte,
	headers map[string][]string,
	backendName, backendURL, basePath, method, outboundPath string,
	isRetry bool,
	model, provider string,
	statusCode int,
) *policy.UpstreamAttemptContext {
	bodyCopy := make([]byte, len(original))
	copy(bodyCopy, original)

	return &policy.UpstreamAttemptContext{
		SharedContext: &policy.SharedContext{
			Metadata: make(map[string]interface{}),
		},
		UpstreamRequestContext: &policy.UpstreamRequestContext{
			Name:     backendName,
			URL:      backendURL,
			BasePath: basePath,
		},
		ResolvedModel:      model,
		ResolvedProvider:   provider,
		Method:             method,
		Path:               outboundPath,
		Headers:            policy.NewHeaders(headers),
		ResponseStatusCode: statusCode,
		Body: &policy.Body{
			Content:     bodyCopy,
			EndOfStream: true,
			Present:     len(original) > 0,
		},
		OriginalRequestRaw: original,
		IsRetry:            isRetry,
	}
}

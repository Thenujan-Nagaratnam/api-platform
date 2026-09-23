/*
 * Copyright (c) 2025, WSO2 LLC. (https://www.wso2.com).
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

package registry

import (
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// PolicyChain is a container for a complete policy processing pipeline for a route
type PolicyChain struct {
	// Ordered list of policies to execute (all implement Policy interface)
	Policies []policy.Policy

	// Policy specifications (aligned with Policies)
	PolicySpecs []policy.PolicySpec

	// Computed flag: true if any policy requires request body access.
	// Determines whether ext_proc uses SKIP or BUFFERED mode for request body.
	RequiresRequestBody bool

	// Computed flag: true if any policy requires response body access.
	// Determines whether ext_proc uses SKIP or BUFFERED mode for response body.
	RequiresResponseBody bool

	// Computed flag: true when every request-body policy also implements
	// StreamingRequestPolicy. When false, the kernel forces BUFFERED mode
	// for request body even if some policies support streaming.
	SupportsRequestStreaming bool

	// Computed flag: true when every response-body policy also implements
	// StreamingResponsePolicy. When true and the upstream response signals
	// streaming (Transfer-Encoding: chunked or Content-Type: text/event-stream),
	// the kernel upgrades Envoy to FULL_DUPLEX_STREAMED mode for the response body.
	// Any buffered-only policy in the chain forces this to false.
	SupportsResponseStreaming bool

	// Computed flag: true if any policy has a CEL execution condition.
	// When false, CEL evaluation is skipped entirely during execution.
	HasExecutionConditions bool

	// Computed flag: true if any policy declares RequestHeaderMode=PROCESS in Mode()
	// AND implements the RequestHeaderPolicy interface. Note: this flag does NOT
	// control Envoy header transport (headers always flow for lifecycle reasons).
	// It reflects callback participation intent.
	RequiresRequestHeader bool

	// Computed flag: true if any policy declares ResponseHeaderMode=PROCESS in Mode()
	// AND implements the ResponseHeaderPolicy interface. Note: this flag does NOT
	// control Envoy header transport (headers always flow for lifecycle reasons).
	// It reflects callback participation intent.
	RequiresResponseHeader bool

	// Computed flag: true if UpstreamPolicies contains at least one policy
	// implementing RequestPolicy or RequestHeaderPolicy. Drives whether the
	// control plane attaches the per-backend upstream ext_proc filter for this
	// route at all — routes with no such policy pay zero cost.
	RequiresUpstreamRequest bool

	// Computed flag: response-phase analog of RequiresUpstreamRequest — true
	// if UpstreamPolicies contains at least one policy implementing
	// ResponsePolicy or ResponseHeaderPolicy.
	RequiresUpstreamResponse bool

	// UpstreamPolicies holds every policy attached via upstreamPolicies: (see
	// the LlmProvider/LlmProxy schema) — never also in Policies, since the
	// attachment point alone decides the phase: a policy attached under
	// operationPolicies: runs downstream only, one attached under
	// upstreamPolicies: runs per upstream attempt only (once per Envoy
	// attempt, including retries to a different backend). Attaching the same
	// policy under both runs it in both phases, as two separate instances.
	// There is no separate upstream interface: the kernel invokes the same
	// RequestPolicy/ResponsePolicy/RequestHeaderPolicy/ResponseHeaderPolicy
	// methods a policy already implements for the downstream phase, against a
	// context built for that attempt (see those context types' own doc
	// comments in the SDK).
	UpstreamPolicies []policy.Policy

	// UpstreamPolicySpecs is aligned with UpstreamPolicies, mirroring PolicySpecs.
	UpstreamPolicySpecs []policy.PolicySpec
}

// ComputeUpstreamRequirements inspects a chain's UpstreamPolicies list and
// reports whether it needs the request/response upstream-attempt phase at
// all, by checking which interfaces are actually implemented.
func ComputeUpstreamRequirements(policies []policy.Policy) (requiresRequest, requiresResponse bool) {
	for _, p := range policies {
		if _, ok := p.(policy.RequestPolicy); ok {
			requiresRequest = true
		}
		if _, ok := p.(policy.RequestHeaderPolicy); ok {
			requiresRequest = true
		}
		if _, ok := p.(policy.ResponsePolicy); ok {
			requiresResponse = true
		}
		if _, ok := p.(policy.ResponseHeaderPolicy); ok {
			requiresResponse = true
		}
	}
	return
}

/*
 *  Copyright (c) 2026, WSO2 LLC. (http://www.wso2.com) All Rights Reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 */

package policyv1alpha2

import policyenginev1 "github.com/wso2/api-platform/sdk/core/policyengine"

// Body represents HTTP request or response body data
type Body struct {
	// Content is the body payload (may be nil)
	Content []byte

	// EndOfStream indicates if this is the final chunk of body data
	// true = complete body received, false = more chunks may follow
	EndOfStream bool

	// Present indicates if body data is available
	// false = body was not sent or not buffered (e.g., streaming)
	// true = body is available (Content may still be nil for empty bodies)
	Present bool
}

// DownstreamContext identifies the downstream client and carries a snapshot of
// the client request.
type DownstreamContext struct {
	Request *DownstreamRequest
}

// DownstreamRequest holds a snapshot of the request as received from the
// downstream client, captured before any policy mutation is applied.
type DownstreamRequest struct {
	Headers   *Headers
	Path      string
	Method    string
	Authority string
	Scheme    string
}

// UpstreamRequestContext identifies the route's resolved upstream target during
// the request phase.
type UpstreamRequestContext struct {
	Name     string
	URL      string
	BasePath string
}

// UpstreamAttemptContext is passed to UpstreamRequestPolicy.OnUpstreamRequestBody
// and UpstreamResponsePolicy.OnUpstreamResponseBody. It carries the resolved
// backend for this specific attempt plus the original client request, captured
// once downstream and replayed unchanged to every attempt — so a retry to a
// different backend always re-translates/re-authenticates from the client's
// actual bytes, never from a previous attempt's already-mutated output.
type UpstreamAttemptContext struct {
	*SharedContext
	*UpstreamRequestContext

	// ResolvedModel and ResolvedProvider identify which model/provider this
	// specific attempt represents, for a route whose upstream was resolved
	// from a declared resilience.failover chain (see gateway-runtime's
	// UpstreamExternalProcessorServer.resolveBackend). Both are empty for an
	// attempt resolved any other way (a route's plain DefaultUpstream, the
	// global cluster index, or the :authority fallback) — a policy checking
	// ResolvedProvider before branching its transform direction naturally
	// no-ops on every non-failover route without an extra feature flag.
	ResolvedModel    string
	ResolvedProvider string

	// Method and Path are this attempt's resolved outbound request line — the
	// method/path that will actually be dialed against this backend (already
	// combined with the backend's BasePath and any earlier routing mutation),
	// not the client-facing request line. A policy that needs to build a
	// synthetic *http.Request for signing (e.g. AWS SigV4) uses these plus URL
	// directly; unlike RequestContext, there is no separate APIContext to
	// strip — Path is already the correct outbound path for this backend.
	Method string
	Path   string

	// Headers are this attempt's request headers (read-only for policies via
	// Get()/Has()/Iterate(); mutated by the kernel via UnsafeInternalValues()),
	// seeded fresh from the client's original headers on every attempt.
	Headers *Headers

	// Body is this attempt's working request body — seeded from
	// OriginalRequestRaw at the start of every attempt, then threaded through
	// each policy in the chain as it mutates it (e.g. a transformer runs
	// before an auth policy that must sign the transformed bytes). Never
	// carried over from a previous attempt.
	Body *Body

	// OriginalRequestRaw is the client's original request body, captured once
	// downstream before any policy (upstream or downstream) mutated it, and
	// replayed unchanged into Body at the start of every attempt — a retry to
	// a different backend always starts from these bytes, never from a
	// previous attempt's already-translated output.
	OriginalRequestRaw []byte

	// IsRetry is true when this is not the first attempt for this client
	// request — i.e. a prior attempt against a different (or the same)
	// backend already failed. Set from genuine per-invocation state, not
	// inferred from an Envoy attempt-count header.
	IsRetry bool

	// ResponseStatusCode is this attempt's upstream HTTP response status,
	// valid only from UpstreamResponsePolicy.OnUpstreamResponseBody — always
	// 0 during the request phase (OnUpstreamRequestBody), since no response
	// exists yet. A response-shape-translating policy (e.g. an OpenAI ->
	// Anthropic transformer choosing between its success and error response
	// shape) reads this rather than inspecting Body for a heuristic
	// error/success marker.
	ResponseStatusCode int

	// ResponseStatusOverride is the OUTPUT counterpart to ResponseStatusCode:
	// nil leaves the real upstream status code unchanged; the kernel sets it
	// from the accumulated DownstreamResponseModifications.StatusCode of
	// every response-phase policy that ran in this attempt's chain (last
	// non-nil write wins, mirroring HeadersToSet semantics), then applies it
	// to the response actually sent downstream. Policies never set this
	// directly — it exists on this struct only so the kernel can accumulate
	// it across the whole chain the same way it does Headers/Body, rather
	// than reading only the last-executed policy's own returned action.
	ResponseStatusOverride *int
}

// UpstreamResponseContext identifies the route's resolved upstream target during
// the response phase and carries a snapshot of the upstream response.
type UpstreamResponseContext struct {
	Name     string
	URL      string
	BasePath string
	Response *UpstreamResponse
}

// UpstreamResponse holds a snapshot of the response as received from the
// upstream backend, captured before any policy mutation is applied.
type UpstreamResponse struct {
	Headers    *Headers
	StatusCode int
}

// SharedContext contains data shared across request and response phases
type SharedContext struct {
	// ProjectID is the project ID which the API is associated with
	ProjectID string

	// Unique request identifier for correlation
	// Generated by Kernel, immutable
	RequestID string

	// Shared metadata for inter-policy communication
	// Persists from request phase through response phase
	// Policies read/write this map to coordinate behavior
	Metadata map[string]interface{}

	// API metadata fields (populated by policy engine at request time)
	// These provide context about which API and operation is being processed

	// APIId is the unique identifier of the API (e.g., UUID)
	APIId string

	// APIName is the name of the API (e.g., "PetStore")
	APIName string

	// APIVersion is the version of the API (e.g., "v1.0.0")
	APIVersion string

	// APIKind is the type of the API
	APIKind APIKind

	// APIContext is the base context path of the API (e.g., "/petstore")
	// This is the base path without the version
	APIContext string

	// OperationPath is the operation path pattern from the API definition
	// (e.g., "/pets/{id}" for a parameterized path)
	// This differs from RequestHeaderContext.Path which contains the actual request path
	// with resolved parameters (e.g., "/petstore/v1.0.0/pets/123")
	OperationPath string

	// ResolvedOperation is the canonical protocol operation this request resolved
	// to, on an API kind whose operation cannot be read off the route.
	//
	// OperationPath is not a substitute. A multiplexed transport puts every
	// operation on one path — an A2A JSON-RPC endpoint serves all eleven
	// operations at the same URL — so OperationPath is identical for all of them
	// and the operation is only knowable after the request has been inspected.
	//
	// Written by the policy engine once the request's policy chain has been
	// bound, before any policy in that chain runs, and derived from the same
	// chain key that selected the chain. It therefore cannot name one operation
	// while another operation's policies (its authentication, its rate limits)
	// actually ran.
	//
	// Empty for a route whose chain is fixed by the route itself, which is every
	// API kind that shipped before Agent — so a policy must treat "" as "not
	// applicable", never as a failure.
	ResolvedOperation string

	// ResolutionAttributes are the protocol-derived facts about this request that
	// the route's resolver captured in the same pass that identified the
	// operation — an A2A message's contextId and taskId, for instance.
	//
	// They exist so the request payload is parsed once, and they are the only way
	// a body-sourced value can reach a *request-header-phase* policy, since
	// RequestHeaderContext carries no body of its own. Read via Get, Lookup, Len
	// and Iterate; see ResolutionAttributes for what may be in here, how far it
	// can be trusted, and why it is not a plain map.
	//
	// The zero value is the empty set, which is what a route resolved by the route
	// itself carries — so, as with ResolvedOperation above, a policy reads "no
	// attributes" as "not applicable" rather than as a failure to resolve.
	ResolutionAttributes ResolutionAttributes

	// AuthContext stores structured authentication information populated by auth policies.
	// Nil until an auth policy runs. Use Previous for multi-layer auth chains.
	AuthContext *AuthContext
}

// ─── Request-phase contexts ──────────────────────────────────────────────────

// RequestHeaderContext is passed to RequestHeaderPolicy.OnRequestHeaders.
// The request body is not yet available at this phase.
type RequestHeaderContext struct {
	*SharedContext

	// Current request headers (read-only for policies)
	Headers   *Headers
	Path      string
	Method    string
	Authority string
	Scheme    string
	Vhost     string

	// Downstream holds the snapshot of the client request headers, captured
	// before any policy mutation.
	Downstream *DownstreamContext

	// Upstream identifies the route's resolved upstream target for this
	// request.
	Upstream *UpstreamRequestContext
}

// RequestContext is passed to RequestPolicy.OnRequestBody.
// The complete buffered request body is available.
type RequestContext struct {
	*SharedContext

	// Current request headers (read-only for policies)
	// Policies use Get(), Has(), Iterate() methods for read-only access
	// Kernel updates via UnsafeInternalValues()
	Headers   *Headers
	Body      *Body
	Path      string
	Method    string
	Authority string
	Scheme    string
	Vhost     string

	// Deprecated: UpstreamInfo exposes the internal Envoy cluster name and its
	// resolved-upstream shape was incorrect. Use Upstream (*UpstreamRequestContext)
	// instead, which exposes Name rather than the internal cluster name.
	// Retained for backward compatibility; will be removed in a future release.
	UpstreamInfo *policyenginev1.UpstreamInfo

	// Downstream holds the snapshot of the client request headers, captured
	// before any policy mutation.
	Downstream *DownstreamContext

	// Upstream identifies the route's resolved upstream target for this
	// request.
	Upstream *UpstreamRequestContext
}

// ─── Response-phase contexts ─────────────────────────────────────────────────

// ResponseHeaderContext is passed to ResponseHeaderPolicy.OnResponseHeaders.
// The response body is not yet available at this phase.
type ResponseHeaderContext struct {
	*SharedContext

	// Original request data (read-only, from request phase)
	RequestHeaders *Headers
	RequestBody    *Body
	RequestPath    string
	RequestMethod  string

	// Current response headers (read-only for policies)
	ResponseHeaders *Headers

	// Current response status code
	ResponseStatus int

	// Downstream holds the snapshot of the client request headers, captured
	// before any policy mutation.
	Downstream *DownstreamContext

	// Upstream identifies the route's resolved upstream target and carries the
	// snapshot of the upstream response headers, captured before any policy
	// mutation.
	Upstream *UpstreamResponseContext
}

// ResponseContext is passed to ResponsePolicy.OnResponseBody.
// The complete buffered response body is available.
type ResponseContext struct {
	*SharedContext

	// Original request data (read-only, from request phase)
	RequestHeaders *Headers
	RequestBody    *Body
	RequestPath    string
	RequestMethod  string

	// Current response headers (read-only for policies)
	// Policies use Get(), Has(), Iterate() methods for read-only access
	// Kernel updates via UnsafeInternalValues()
	ResponseHeaders *Headers

	// Current response body (mutable)
	// nil if no body or body not required
	ResponseBody *Body

	// Current response status code
	ResponseStatus int

	// Downstream holds the snapshot of the client request headers, captured
	// before any policy mutation.
	Downstream *DownstreamContext

	// Upstream identifies the route's resolved upstream target and carries the
	// snapshot of the upstream response headers, captured before any policy
	// mutation.
	Upstream *UpstreamResponseContext
}

// ─── Streaming contexts ──────────────────────────────────────────────────────

// StreamBody holds a single chunk of body data delivered during streaming processing.
// Unlike Body, it does not carry the "Present" flag — a chunk is always present by definition.
type StreamBody struct {
	// Chunk is the raw bytes for this piece of body data.
	// May be empty when EndOfStream is true (a terminal signal with no payload).
	Chunk []byte

	// EndOfStream signals that no more chunks will follow for this message.
	// Policies should finalize any accumulated state when true.
	EndOfStream bool

	// Index is the zero-based sequential position of this chunk within the stream.
	// The kernel increments this for every chunk delivered to the policy chain.
	// Useful for accumulation logic (e.g. "have I seen enough chunks yet?"),
	// first-chunk initialization, and debug logging.
	Index uint64
}

// RequestStreamContext is the per-chunk context passed to StreamingRequestBodyPolicy.
// It is structurally identical to RequestHeaderContext today, but kept as a distinct
// type so that streaming-specific fields (e.g. accumulated byte count, chunk index)
// can be added in the future without changing the header-phase contract.
// Headers are read-only; body data arrives via the StreamBody argument, not this struct.
type RequestStreamContext struct {
	*SharedContext

	// Current request headers (read-only)
	Headers   *Headers
	Path      string
	Method    string
	Authority string
	Scheme    string
	Vhost     string

	// Downstream holds the snapshot of the client request headers, captured
	// before any policy mutation.
	Downstream *DownstreamContext

	// Upstream identifies the route's resolved upstream target for this
	// request.
	Upstream *UpstreamRequestContext
}

// ResponseStreamContext is the per-chunk context passed to StreamingResponseBodyPolicy.
// It mirrors ResponseHeaderContext — response body is delivered chunk-by-chunk
// via the StreamBody argument.
type ResponseStreamContext struct {
	*SharedContext

	// Original request data (read-only, from request phase)
	RequestHeaders *Headers
	RequestBody    *Body
	RequestPath    string
	RequestMethod  string

	// Current response headers (read-only)
	ResponseHeaders *Headers

	// Current response status code
	ResponseStatus int

	// Downstream holds the snapshot of the client request headers, captured
	// before any policy mutation.
	Downstream *DownstreamContext

	// Upstream identifies the route's resolved upstream target and carries the
	// snapshot of the upstream response headers, captured before any policy
	// mutation.
	Upstream *UpstreamResponseContext
}

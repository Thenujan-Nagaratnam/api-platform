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
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/constants"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/executor"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/registry"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// ResolvedFailoverProviderHeader carries the winning attempt's resolved
// provider identity (e.g. "anthropic-upstream") from an upstream-attempt
// policy's response phase to the downstream ext_proc's own response
// processing (execution_context.go's buildResponseContexts), which reads it
// to correct the analytics event's provider attribution — the route-level
// template_handle/provider_name the downstream phase otherwise uses is fixed
// at request time and reflects only the PRIMARY provider, wrong for a request
// a fallback actually served. The kernel only consumes and strips this header;
// the model-failover policy (gateway/dev-policies/model-failover) is what sets
// it, from its own upstream-attempt OnResponseHeaders. Purely internal: the
// downstream phase strips it before the response reaches the client, the
// same way an ordinary set-headers policy's internal markers are.
const ResolvedFailoverProviderHeader = "x-wso2-resolved-failover-provider"

// UpstreamExternalProcessorServer is the per-cluster (upstream) ext_proc
// service — the counterpart to ExternalProcessorServer, which is attached
// once at the listener (downstream) level and runs the general policy chain
// exactly once per client request.
//
// Envoy invokes THIS service fresh for every upstream attempt, including
// retries to a different backend cluster (see go-network-service-hardening's
// upstream_codec requirement and gateway-controller's attachUpstreamPolicyFilter),
// scoped to whichever cluster a given attempt targets. Because a single
// upstream-policy-engine cluster can be shared by many backend clusters'
// filter attachments (see gateway-controller's clusterNeedsUpstreamPolicyFilter),
// this server does not know its backend at construction time — it learns the
// route (to look up the applicable policy chain — see extractRouteKey) and
// the specific backend cluster (see extractClusterName) from Envoy-supplied
// request attributes on every invocation.
//
// URL/BasePath are resolved from the route's compiled-in RouteConfig.Metadata.DefaultUpstream
// (synced over xDS alongside the policy chain — see xdsclient/handler.go) when
// this attempt's cluster matches that route's single default upstream cluster.
// A route with more than one real backend cluster (a future multi-cluster
// redirect/failover target) has no per-cluster resolution yet: a mismatched
// cluster name deliberately leaves URL empty rather than guessing, so a
// policy needing it (e.g. AWS SigV4 signing, which must sign against the real
// backend host) fails cleanly instead of silently signing the wrong host.
type UpstreamExternalProcessorServer struct {
	kernel        *Kernel
	chainExecutor *executor.ChainExecutor
}

// NewUpstreamExternalProcessorServer constructs the upstream ext_proc service.
func NewUpstreamExternalProcessorServer(k *Kernel, chainExecutor *executor.ChainExecutor) *UpstreamExternalProcessorServer {
	return &UpstreamExternalProcessorServer{kernel: k, chainExecutor: chainExecutor}
}

// upstreamAttemptState is the per-stream state carried from the
// RequestHeaders phase (where routing/backend identity is known) to the
// RequestBody/ResponseBody phases (where the actual bytes arrive).
type upstreamAttemptState struct {
	routeKey    string
	clusterName string
	// rawClusterName is the xds.cluster_name attribute exactly as Envoy
	// reported it for this attempt. For an aggregate-routed attempt this is
	// the aggregate cluster's own name — exposed to policies via RouteCluster
	// (kernel.BuildUpstreamAttempt*Context), which is how a chain-aware policy
	// such as model-failover identifies which chain this attempt belongs to.
	rawClusterName string
	chain          *registry.PolicyChain
	method         string
	path           string
	backendURL     string
	basePath       string

	// statusCode is this attempt's upstream response status, captured at the
	// ResponseHeaders phase and surfaced to response-phase policies.
	statusCode int

	// sharedContext is built once at the RequestHeaders phase (see
	// kernel.NewUpstreamAttemptSharedContext) and reused for every
	// upstream-attempt policy phase in this attempt, so Metadata a
	// header-phase policy writes (e.g. an AuthContext, or model-failover's
	// selected_provider/selected_model) is visible to the body-phase policies
	// that run after it — mirroring how the downstream executor threads one
	// SharedContext across its own phases.
	sharedContext *policy.SharedContext

	// requestHeaders/responseHeaders are built once (RequestHeaders/ResponseHeaders
	// phase respectively) and reused for the later body-phase context, so
	// mutations an upstream-attempt header-phase policy makes are visible to
	// the body-phase policies that run after it in the same attempt.
	requestHeaders  *policy.Headers
	responseHeaders *policy.Headers

	// originalRequestBody is the client's request body exactly as Envoy
	// delivered it at this attempt's RequestBody phase, captured unconditionally
	// (independent of RequiresUpstreamRequest) so the later response phase can
	// populate ResponseContext.RequestBody correctly even when no request-phase
	// upstream policy ran.
	originalRequestBody []byte
}

// Process implements extprocv3.ExternalProcessorServer for the upstream
// (per-cluster) attachment. Deliberately narrower than the downstream
// server's Process: no CEL conditions, no streaming, no route/operation
// resolution — routing is already decided by the time Envoy reaches this
// filter, so this only ever dispatches to the upstream-phase policies in an
// already-resolved chain.
func (s *UpstreamExternalProcessorServer) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	var state upstreamAttemptState

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}

		var resp *extprocv3.ProcessingResponse
		switch r := req.Request.(type) {
		case *extprocv3.ProcessingRequest_RequestHeaders:
			state.routeKey = extractAttribute(req, "xds.route_name")
			state.clusterName = extractAttribute(req, "xds.cluster_name")
			state.rawClusterName = state.clusterName
			state.chain = s.kernel.GetPolicyChainForKey(state.routeKey)
			state.method, state.path = extractMethodAndPath(r.RequestHeaders)
			resolved := s.resolveBackend(state.routeKey, state.clusterName)
			state.backendURL = resolved.URL
			state.basePath = resolved.BasePath
			var pathMutation *extprocv3.HeaderMutation
			if state.backendURL == "" {
				if authority := extractAuthority(r.RequestHeaders); authority != "" {
					// Cluster-name-keyed resolution can't identify the real
					// backend for an envoy.clusters.aggregate attempt — Envoy
					// reports the aggregate's own name via xds.cluster_name on
					// every attempt, never the real member it actually dialed
					// (confirmed live). :authority is the one signal that's
					// always attempt-accurate: it's the host Envoy is actually
					// about to connect to, regardless of which cluster owns
					// the filter chain. Scheme is not derivable from any
					// ext_proc attribute, so this assumes plain HTTP — a real
					// TLS backend reached only through an aggregate cluster
					// needs a scheme-aware follow-up, not silently guessed here.
					state.backendURL = "http://" + authority
				}
			}
			slog.DebugContext(ctx, "[upstream-extproc] request headers received",
				"route", state.routeKey, "cluster", state.clusterName,
				"chain_found", state.chain != nil,
				"requires_upstream_request", state.chain != nil && state.chain.RequiresUpstreamRequest,
				"resolved_url", state.backendURL, "method", state.method, "path", state.path)

			state.sharedContext = NewUpstreamAttemptSharedContext()
			originalRequestHeaders := extractHeaderMap(r.RequestHeaders)
			state.requestHeaders = policy.NewHeaders(originalRequestHeaders)

			var immediate *extprocv3.ImmediateResponse
			if state.chain != nil && state.chain.RequiresUpstreamRequest {
				reqHdrCtx := BuildUpstreamAttemptRequestHeaderContext(state.sharedContext, state.rawClusterName, state.requestHeaders,
					state.clusterName, state.backendURL, state.basePath, state.method, state.path)
				action, err := s.chainExecutor.ExecuteUpstreamAttemptRequestHeaderPolicies(ctx, state.chain.UpstreamPolicies, reqHdrCtx, state.chain.UpstreamPolicySpecs, "", state.routeKey)
				if err != nil {
					slog.ErrorContext(ctx, "[upstream-extproc] upstream request header policy execution failed", "error", err, "route", state.routeKey, "cluster", state.clusterName)
				} else if imm, ok := action.(policy.ImmediateResponse); ok {
					immediate = buildImmediateResponse(imm)
				} else if reqHdrCtx.Path != state.path {
					state.path = reqHdrCtx.Path
					pathMutation = buildHeaderValueOptions(map[string]string{":path": reqHdrCtx.Path})
				}
			}

			if immediate != nil {
				resp = &extprocv3.ProcessingResponse{
					Response: &extprocv3.ProcessingResponse_ImmediateResponse{ImmediateResponse: immediate},
				}
			} else {
				headersResp := &extprocv3.HeadersResponse{}
				headerMutation := mergeAttemptHeaderMutations(pathMutation, diffHeaderMutation(originalRequestHeaders, state.requestHeaders))
				if headerMutation != nil {
					headersResp.Response = &extprocv3.CommonResponse{HeaderMutation: headerMutation}
				}
				resp = &extprocv3.ProcessingResponse{
					Response: &extprocv3.ProcessingResponse_RequestHeaders{
						RequestHeaders: headersResp,
					},
				}
			}

		case *extprocv3.ProcessingRequest_RequestBody:
			resp = s.processRequestBody(ctx, r.RequestBody, &state)

		case *extprocv3.ProcessingRequest_ResponseHeaders:
			state.statusCode = extractStatusCode(r.ResponseHeaders)

			originalResponseHeaders := extractHeaderMap(r.ResponseHeaders)
			state.responseHeaders = policy.NewHeaders(originalResponseHeaders)

			var immediate *extprocv3.ImmediateResponse
			if state.chain != nil && state.chain.RequiresUpstreamResponse {
				respHdrCtx := BuildUpstreamAttemptResponseHeaderContext(state.sharedContext, state.rawClusterName, state.responseHeaders,
					state.clusterName, state.backendURL, state.basePath, state.method, state.path, state.statusCode)
				action, err := s.chainExecutor.ExecuteUpstreamAttemptResponseHeaderPolicies(ctx, state.chain.UpstreamPolicies, respHdrCtx, state.chain.UpstreamPolicySpecs, "", state.routeKey)
				if err != nil {
					slog.ErrorContext(ctx, "[upstream-extproc] upstream response header policy execution failed", "error", err, "route", state.routeKey, "cluster", state.clusterName)
				} else if imm, ok := action.(policy.ImmediateResponse); ok {
					immediate = buildImmediateResponse(imm)
				}
			}

			if immediate != nil {
				resp = &extprocv3.ProcessingResponse{
					Response: &extprocv3.ProcessingResponse_ImmediateResponse{ImmediateResponse: immediate},
				}
			} else {
				headersResp := &extprocv3.HeadersResponse{}
				if headerMutation := diffHeaderMutation(originalResponseHeaders, state.responseHeaders); headerMutation != nil {
					headersResp.Response = &extprocv3.CommonResponse{HeaderMutation: headerMutation}
				}
				resp = &extprocv3.ProcessingResponse{
					Response: &extprocv3.ProcessingResponse_ResponseHeaders{
						ResponseHeaders: headersResp,
					},
				}
			}

		case *extprocv3.ProcessingRequest_ResponseBody:
			resp = s.processResponseBody(ctx, r.ResponseBody, &state)

		default:
			slog.WarnContext(ctx, "[upstream-extproc] unhandled processing request type", "type", req.Request)
			resp = &extprocv3.ProcessingResponse{}
		}

		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (s *UpstreamExternalProcessorServer) processRequestBody(ctx context.Context, body *extprocv3.HttpBody, state *upstreamAttemptState) *extprocv3.ProcessingResponse {
	state.originalRequestBody = body.Body

	if state.chain == nil || !state.chain.RequiresUpstreamRequest {
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestBody{
				RequestBody: &extprocv3.BodyResponse{Response: &extprocv3.CommonResponse{}},
			},
		}
	}

	if state.sharedContext == nil {
		state.sharedContext = NewUpstreamAttemptSharedContext()
	}
	if state.requestHeaders == nil {
		state.requestHeaders = policy.NewHeaders(nil)
	}

	originalRequestHeadersForBody := cloneHeaderMap(state.requestHeaders.UnsafeInternalValues())
	reqCtx := BuildUpstreamAttemptRequestContext(state.sharedContext, state.rawClusterName, state.requestHeaders, body.Body,
		state.clusterName, state.backendURL, state.basePath, state.method, state.path)
	action, err := s.chainExecutor.ExecuteUpstreamAttemptRequestPolicies(ctx, state.chain.UpstreamPolicies, reqCtx, state.chain.UpstreamPolicySpecs, "", state.routeKey)
	if err != nil {
		slog.ErrorContext(ctx, "[upstream-extproc] upstream request policy execution failed", "error", err, "route", state.routeKey, "cluster", state.clusterName)
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_RequestBody{
				RequestBody: &extprocv3.BodyResponse{Response: &extprocv3.CommonResponse{}},
			},
		}
	}
	if imm, ok := action.(policy.ImmediateResponse); ok {
		return &extprocv3.ProcessingResponse{
			Response: &extprocv3.ProcessingResponse_ImmediateResponse{ImmediateResponse: buildImmediateResponse(imm)},
		}
	}

	commonResp := &extprocv3.CommonResponse{}

	// Built from the chain's ACCUMULATED reqCtx state, not from the returned
	// action alone — action is whichever policy happened to run last (e.g. an
	// auth policy setting only an API-key header), which silently discards an
	// earlier policy's own returned mutations (e.g. a transformer's
	// Path/Body translation) once a later policy's action replaces it.
	// reqCtx.Headers/Body/Path are threaded through and updated by every
	// policy in turn (executor.applyRequestModifications), so they reflect
	// the full chain's combined effect regardless of execution order. Diffed
	// against the snapshot taken before the chain ran (rather than dumping
	// every current header) since, unlike the pre-header-phase-dispatch
	// design, reqCtx.Headers now starts seeded from Envoy's real request
	// headers, not empty.
	var pathMutation *extprocv3.HeaderMutation

	// Path rewrite (e.g. a transformer changing "/chat/completions" to
	// the target provider's own "/v1/messages") must reach Envoy as a
	// ":path" header mutation the same way any other header does —
	// there is no separate BodyResponse.Path field, and without this
	// the policy's returned Path is silently dropped, so the mutated
	// Body is sent to the WRONG upstream path.
	//
	// For a failover/additionalProviders attempt this hop is itself a
	// loopback into gateway-runtime's own listener: state.basePath is
	// the resolved provider's own route context (e.g.
	// "/anthropic-provider"), which that provider's downstream route
	// strips via its own RegexRewrite before prepending ITS OWN
	// registered upstream base path (gateway-controller/pkg/xds/
	// translator.go's context-strip + upstream-prepend rewrite — the
	// same combination gateway-controller/lua/request_transformation.lua's
	// compute_upstream_path already performs for the non-retry case).
	// Sending the policy's path alone, with no context prefix,
	// would fail that route match entirely.
	if reqCtx.Path != state.path {
		pathMutation = buildHeaderValueOptions(map[string]string{":path": joinBasePathAndOperation(state.basePath, reqCtx.Path)})
		state.path = reqCtx.Path
	}

	commonResp.HeaderMutation = mergeAttemptHeaderMutations(pathMutation, diffHeaderMutation(originalRequestHeadersForBody, reqCtx.Headers))
	if !bytes.Equal(reqCtx.Body.Content, body.Body) {
		commonResp.BodyMutation = &extprocv3.BodyMutation{
			Mutation: &extprocv3.BodyMutation_Body{Body: reqCtx.Body.Content},
		}
		// A mutated body whose length no longer matches an
		// already-set Content-Length is a hard Envoy error
		// ("mismatch_between_content_length_and_the_length_of_the_mutated_body"),
		// not a silent pass-through — confirmed live for a
		// transformer's translated (differently-sized) body.
		if commonResp.HeaderMutation == nil {
			commonResp.HeaderMutation = &extprocv3.HeaderMutation{}
		}
		setContentLengthHeader(commonResp.HeaderMutation, len(reqCtx.Body.Content))
	}

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{Response: commonResp},
		},
	}
}

func (s *UpstreamExternalProcessorServer) processResponseBody(ctx context.Context, body *extprocv3.HttpBody, state *upstreamAttemptState) *extprocv3.ProcessingResponse {
	commonResp := &extprocv3.CommonResponse{}

	if state.chain != nil && state.chain.RequiresUpstreamResponse {
		if state.sharedContext == nil {
			state.sharedContext = NewUpstreamAttemptSharedContext()
		}
		if state.responseHeaders == nil {
			state.responseHeaders = policy.NewHeaders(nil)
		}

		originalResponseHeadersForBody := cloneHeaderMap(state.responseHeaders.UnsafeInternalValues())
		respCtx := BuildUpstreamAttemptResponseContext(state.sharedContext, state.rawClusterName, state.responseHeaders,
			state.originalRequestBody, body.Body,
			state.clusterName, state.backendURL, state.basePath, state.method, state.path, state.statusCode)
		action, err := s.chainExecutor.ExecuteUpstreamAttemptResponsePolicies(ctx, state.chain.UpstreamPolicies, respCtx, state.chain.UpstreamPolicySpecs, "", state.routeKey)
		if err != nil {
			slog.ErrorContext(ctx, "[upstream-extproc] upstream response policy execution failed", "error", err, "route", state.routeKey, "cluster", state.clusterName)
		} else if imm, ok := action.(policy.ImmediateResponse); ok {
			return &extprocv3.ProcessingResponse{
				Response: &extprocv3.ProcessingResponse_ImmediateResponse{ImmediateResponse: buildImmediateResponse(imm)},
			}
		} else {
			// Built from the chain's ACCUMULATED respCtx state, not from the
			// returned action alone — the same fix applied to processRequestBody,
			// and needed for the same reason: the returned action is whichever
			// policy ran last, which silently discards an earlier policy's own
			// returned mutations once a later policy's action replaces it. Diffed
			// against the pre-chain snapshot for the same reason processRequestBody
			// diffs — respCtx.ResponseHeaders now starts seeded from Envoy's real
			// response headers, not empty.
			var statusMutation *extprocv3.HeaderMutation
			if respCtx.ResponseStatus != 0 && respCtx.ResponseStatus != state.statusCode {
				statusMutation = buildHeaderValueOptions(map[string]string{":status": strconv.Itoa(respCtx.ResponseStatus)})
			}

			commonResp.HeaderMutation = mergeAttemptHeaderMutations(statusMutation, diffHeaderMutation(originalResponseHeadersForBody, respCtx.ResponseHeaders))
			if !bytes.Equal(respCtx.ResponseBody.Content, body.Body) {
				commonResp.BodyMutation = &extprocv3.BodyMutation{
					Mutation: &extprocv3.BodyMutation_Body{Body: respCtx.ResponseBody.Content},
				}
				if commonResp.HeaderMutation == nil {
					commonResp.HeaderMutation = &extprocv3.HeaderMutation{}
				}
				setContentLengthHeader(commonResp.HeaderMutation, len(respCtx.ResponseBody.Content))
			}
		}
	}

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ResponseBody{
			ResponseBody: &extprocv3.BodyResponse{Response: commonResp},
		},
	}
}

// extractMethodAndPath reads the :method/:path pseudo-headers Envoy delivers
// in the upstream RequestHeaders message. Because this filter runs on the
// upstream (per-cluster) connection, these are already the final, resolved
// values for this specific attempt — any route-level rewrite has already
// been applied by Envoy before dispatch — so no further path assembly
// (combining a base path with the client-facing path) is needed here.
func extractMethodAndPath(headers *extprocv3.HttpHeaders) (method, path string) {
	for _, h := range headers.GetHeaders().GetHeaders() {
		switch h.GetKey() {
		case ":method":
			method = headerValue(h)
		case ":path":
			path = headerValue(h)
		}
	}
	return method, path
}

// extractHeaderMap reads every header from an upstream RequestHeaders/ResponseHeaders
// message into a lowercased map, pseudo-headers included — the raw material for
// building a *policy.Headers for the upstream-attempt header phase (see
// kernel.BuildUpstreamAttemptRequestHeaderContext/BuildUpstreamAttemptResponseHeaderContext).
func extractHeaderMap(headers *extprocv3.HttpHeaders) map[string][]string {
	result := make(map[string][]string)
	for _, h := range headers.GetHeaders().GetHeaders() {
		key := strings.ToLower(h.GetKey())
		result[key] = append(result[key], headerValue(h))
	}
	return result
}

// cloneHeaderMap returns a shallow copy of a header map's keys (values are
// small string slices reused as-is), so a snapshot taken before a policy
// chain runs isn't affected by the chain mutating the live map in place —
// UnsafeInternalValues() returns that live map directly, not a copy.
func cloneHeaderMap(values map[string][]string) map[string][]string {
	clone := make(map[string][]string, len(values))
	for k, v := range values {
		clone[k] = v
	}
	return clone
}

// diffHeaderMutation computes the Envoy HeaderMutation representing what an
// upstream-attempt header-phase policy chain changed, diffing original (as
// received from Envoy, before any policy ran) against final (the chain's
// accumulated state after running). Pseudo-headers (":path", ":method", ...)
// are excluded — :path/:method changes are surfaced separately by the caller,
// since they need their own dedicated handling (a rewritten :path also needs
// state.path updated for the later body phase).
func diffHeaderMutation(original map[string][]string, final *policy.Headers) *extprocv3.HeaderMutation {
	if final == nil {
		return nil
	}
	finalValues := final.UnsafeInternalValues()
	mutation := &extprocv3.HeaderMutation{}
	for k, v := range finalValues {
		if strings.HasPrefix(k, ":") || len(v) == 0 {
			continue
		}
		if orig, ok := original[k]; !ok || !headerValuesEqual(orig, v) {
			mutation.SetHeaders = append(mutation.SetHeaders, &corev3.HeaderValueOption{
				Header:       &corev3.HeaderValue{Key: k, RawValue: []byte(v[0])},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			})
		}
	}
	for k := range original {
		if strings.HasPrefix(k, ":") {
			continue
		}
		if v, ok := finalValues[k]; !ok || len(v) == 0 {
			mutation.RemoveHeaders = append(mutation.RemoveHeaders, k)
		}
	}
	if len(mutation.SetHeaders) == 0 && len(mutation.RemoveHeaders) == 0 {
		return nil
	}
	return mutation
}

func headerValuesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mergeAttemptHeaderMutations combines two possibly-nil HeaderMutations (e.g. a
// :path rewrite computed separately from a policy-driven header diff) into
// one, since Envoy's CommonResponse carries only a single HeaderMutation.
func mergeAttemptHeaderMutations(muts ...*extprocv3.HeaderMutation) *extprocv3.HeaderMutation {
	merged := &extprocv3.HeaderMutation{}
	any := false
	for _, m := range muts {
		if m == nil {
			continue
		}
		any = true
		merged.SetHeaders = append(merged.SetHeaders, m.SetHeaders...)
		merged.RemoveHeaders = append(merged.RemoveHeaders, m.RemoveHeaders...)
	}
	if !any {
		return nil
	}
	return merged
}

// extractAuthority reads the :authority pseudo-header — the host Envoy is
// actually about to dial for this attempt — from the upstream RequestHeaders
// message. See its call site's doc comment for why this is needed as a
// fallback alongside cluster-name-keyed resolution, not a replacement for it
// (it has no scheme, so a plain hostname-only cluster-index match is always
// preferred when available).
func extractAuthority(headers *extprocv3.HttpHeaders) string {
	for _, h := range headers.GetHeaders().GetHeaders() {
		if h.GetKey() == ":authority" {
			return headerValue(h)
		}
	}
	return ""
}

// headerValue reads an Envoy HeaderValue's content, preferring RawValue (set
// when the value may not be valid UTF-8) and falling back to Value.
func headerValue(h *corev3.HeaderValue) string {
	if len(h.GetRawValue()) > 0 {
		return string(h.GetRawValue())
	}
	return h.GetValue()
}

// backendResolution is resolveBackend's result for a single upstream attempt.
type backendResolution struct {
	URL      string
	BasePath string
}

// resolveBackend looks up this attempt's backend: the route's own compiled-in
// default upstream (the common case: one real backend cluster per route),
// then the deployment-wide cluster index (see Kernel.clusterUpstreams) for a
// retry that landed on some other route's real cluster. An empty result,
// never a guess, means the cluster is unknown to this deployment entirely —
// including an envoy.clusters.aggregate attempt, where Envoy reports the
// aggregate's own name via xds.cluster_name rather than the real member it
// dialed; the caller's :authority fallback covers that case.
func (s *UpstreamExternalProcessorServer) resolveBackend(routeKey, clusterName string) backendResolution {
	if rc := s.kernel.GetRouteConfig(routeKey); rc != nil {
		if def := rc.Metadata.DefaultUpstream; def != nil && def.ClusterName == clusterName {
			return backendResolution{URL: def.URL, BasePath: def.BasePath}
		}
	}
	if info, ok := s.kernel.GetUpstreamByCluster(clusterName); ok {
		return backendResolution{URL: info.URL, BasePath: info.BasePath}
	}
	return backendResolution{}
}

// joinBasePathAndOperation combines a resolved backend's base path with a
// route's operation-relative path (e.g. "/anthropic-provider" + "/chat/completions"
// -> "/anthropic-provider/chat/completions"), normalizing the separator so
// neither a missing nor a doubled slash can occur regardless of how either
// piece was stored (a root base path of "/" or "" must not produce
// "//chat/completions"). Returns "" when operationPath is empty — nothing to
// rewrite to, so the caller leaves :path untouched rather than clobbering it.
func joinBasePathAndOperation(basePath, operationPath string) string {
	if operationPath == "" {
		return ""
	}
	base := strings.TrimSuffix(basePath, "/")
	op := operationPath
	if !strings.HasPrefix(op, "/") {
		op = "/" + op
	}
	return base + op
}

// extractStatusCode reads the upstream response's ":status" pseudo-header
// from the upstream ResponseHeaders message. Returns 0 if absent or
// unparseable, which a response-phase policy's own status check (e.g.
// model-failover's ">= 500" suspension trigger) correctly treats as "not a
// failure" rather than crashing on a missing value.
func extractStatusCode(headers *extprocv3.HttpHeaders) int {
	for _, h := range headers.GetHeaders().GetHeaders() {
		if h.GetKey() != ":status" {
			continue
		}
		if code, err := strconv.Atoi(headerValue(h)); err == nil {
			return code
		}
		break
	}
	return 0
}

// extractAttribute reads a single string CEL attribute Envoy attached under
// the well-known ext_proc filter namespace — the same mechanism and the same
// namespace key the downstream server's extractRouteKey uses, just
// generalized to any requested attribute name.
func extractAttribute(req *extprocv3.ProcessingRequest, name string) string {
	if req.Attributes == nil {
		return ""
	}
	extProcAttrs, ok := req.Attributes[constants.ExtProcFilter]
	if !ok || extProcAttrs.Fields == nil {
		return ""
	}
	if v, ok := extProcAttrs.Fields[name]; ok {
		return v.GetStringValue()
	}
	return ""
}

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
	"context"
	"errors"
	"io"
	"log/slog"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"

	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/constants"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/executor"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/registry"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

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
	chain       *registry.PolicyChain
	method      string
	path        string
	backendURL  string
	basePath    string
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
			state.chain = s.kernel.GetPolicyChainForKey(state.routeKey)
			state.method, state.path = extractMethodAndPath(r.RequestHeaders)
			state.backendURL, state.basePath = s.resolveBackend(state.routeKey, state.clusterName)
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
			resp = &extprocv3.ProcessingResponse{
				Response: &extprocv3.ProcessingResponse_RequestHeaders{
					RequestHeaders: &extprocv3.HeadersResponse{},
				},
			}

		case *extprocv3.ProcessingRequest_RequestBody:
			resp = s.processRequestBody(ctx, r.RequestBody, &state)

		case *extprocv3.ProcessingRequest_ResponseHeaders:
			resp = &extprocv3.ProcessingResponse{
				Response: &extprocv3.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extprocv3.HeadersResponse{},
				},
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
	commonResp := &extprocv3.CommonResponse{}

	if state.chain != nil && state.chain.RequiresUpstreamRequest {
		upCtx := BuildUpstreamAttemptContext(body.Body, nil, state.clusterName, state.backendURL, state.basePath, state.method, state.path, false)
		result, err := s.chainExecutor.ExecuteUpstreamRequestPolicies(ctx, state.chain.Policies, upCtx, state.chain.PolicySpecs, "", state.routeKey)
		if err != nil {
			slog.ErrorContext(ctx, "[upstream-extproc] upstream request policy execution failed", "error", err, "route", state.routeKey, "cluster", state.clusterName)
		} else if result.FinalAction != nil {
			if mods, ok := result.FinalAction.(policy.UpstreamRequestModifications); ok {
				commonResp.HeaderMutation = buildHeaderValueOptions(mods.HeadersToSet)
				if mods.Body != nil {
					commonResp.BodyMutation = &extprocv3.BodyMutation{
						Mutation: &extprocv3.BodyMutation_Body{Body: mods.Body},
					}
				}
			}
		}
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
		upCtx := BuildUpstreamAttemptContext(body.Body, nil, state.clusterName, state.backendURL, state.basePath, state.method, state.path, false)
		result, err := s.chainExecutor.ExecuteUpstreamResponsePolicies(ctx, state.chain.Policies, upCtx, state.chain.PolicySpecs, "", state.routeKey)
		if err != nil {
			slog.ErrorContext(ctx, "[upstream-extproc] upstream response policy execution failed", "error", err, "route", state.routeKey, "cluster", state.clusterName)
		} else if result.FinalAction != nil {
			if mods, ok := result.FinalAction.(policy.DownstreamResponseModifications); ok {
				commonResp.HeaderMutation = buildHeaderValueOptions(mods.HeadersToSet)
				if mods.Body != nil {
					commonResp.BodyMutation = &extprocv3.BodyMutation{
						Mutation: &extprocv3.BodyMutation_Body{Body: mods.Body},
					}
				}
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

// resolveBackend looks up this attempt's backend URL/base path. It first
// tries the route's own compiled-in default upstream (the common case: one
// real backend cluster per route). If this attempt's cluster isn't that
// route's default — e.g. a retry that landed on an aggregate cluster's other
// priority member, a real cluster this deployment created for some other
// route — it falls back to the deployment-wide cluster index (see
// Kernel.clusterUpstreams). Returns empty strings, never a guess, only when
// the cluster is unknown to this deployment entirely.
func (s *UpstreamExternalProcessorServer) resolveBackend(routeKey, clusterName string) (url, basePath string) {
	if rc := s.kernel.GetRouteConfig(routeKey); rc != nil && rc.Metadata.DefaultUpstream != nil {
		if def := rc.Metadata.DefaultUpstream; def.ClusterName == clusterName {
			return def.URL, def.BasePath
		}
	}
	if info, ok := s.kernel.GetUpstreamByCluster(clusterName); ok {
		return info.URL, info.BasePath
	}
	return "", ""
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

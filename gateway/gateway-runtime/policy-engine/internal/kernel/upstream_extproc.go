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
	"io"
	"log/slog"
	"strconv"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// UpstreamExternalProcessorServer is the second, minimal ext_proc gRPC server
// hosted in this same policy-engine process — wired into Envoy's per-cluster
// UPSTREAM HTTP filter chain (see gateway-controller's Task 8), not the
// per-listener downstream chain ExternalProcessorServer (extproc.go) serves.
// It handles the request-headers phase (every attempt) and, additively, the
// request-body phase for clusters whose ext_proc filter enables
// RequestBodyMode: BUFFERED (model-failover body rewriting, Task 3/6) — this
// filter attachment point has no sensible response phase for this feature
// (see the design doc). It resolves route -> policy chain via the exact same
// in-memory registry the downstream server uses (s.kernel.GetPolicyChain),
// so a policy's cached state (e.g. oauth2-generator's token cache) is
// naturally shared between both entry points with zero duplication.
type UpstreamExternalProcessorServer struct {
	extprocv3.UnimplementedExternalProcessorServer
	kernel *Kernel
}

// NewUpstreamExternalProcessorServer constructs the server. k must be the
// same *Kernel instance the downstream ExternalProcessorServer uses (see
// cmd/policy-engine/main.go, Task 4) — this is what makes chain/state sharing
// automatic rather than something this type has to arrange itself.
func NewUpstreamExternalProcessorServer(k *Kernel) *UpstreamExternalProcessorServer {
	return &UpstreamExternalProcessorServer{kernel: k}
}

// Process implements extprocv3.ExternalProcessorServer. Two message types are
// possible now: RequestHeaders (every attempt) and RequestBody (only for
// clusters whose ext_proc filter has RequestBodyMode: BUFFERED — see Task 6;
// a headers-only cluster, e.g. plain retry-refresh, never sends this second
// message at all). attemptCount is parsed once from the headers message and
// reused for the body message on the same stream — both arrive for the same
// individual dial attempt.
func (s *UpstreamExternalProcessorServer) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	var routeKey string
	attemptCount := 1

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		var resp *extprocv3.ProcessingResponse
		switch v := req.Request.(type) {
		case *extprocv3.ProcessingRequest_RequestHeaders:
			routeKey = extractRouteKeyFromAttributes(req)
			resp, attemptCount, err = s.processRequestHeaders(ctx, req, routeKey)
			if err != nil {
				slog.ErrorContext(ctx, "upstream ext_proc: failed to process request headers, failing open", "error", err)
				resp = emptyContinueRequestHeadersResponse()
			}
		case *extprocv3.ProcessingRequest_RequestBody:
			resp, err = s.processRequestBody(ctx, v.RequestBody, routeKey, attemptCount)
			if err != nil {
				slog.ErrorContext(ctx, "upstream ext_proc: failed to process request body, failing open", "error", err)
				resp = emptyContinueRequestBodyResponse()
			}
		default:
			resp = emptyContinueRequestHeadersResponse()
		}

		if err := stream.Send(resp); err != nil {
			return status.Errorf(codes.Internal, "upstream ext_proc: failed to send response: %v", err)
		}
	}
}

// emptyContinueRequestHeadersResponse is the fail-open / no-op response: no
// header mutation, request proceeds unchanged.
func emptyContinueRequestHeadersResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{},
			},
		},
	}
}

// emptyContinueRequestBodyResponse is the fail-open / no-op response for the
// body phase: no body mutation, the original buffered bytes proceed as-is.
func emptyContinueRequestBodyResponse() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{Response: &extprocv3.CommonResponse{}},
		},
	}
}

// processRequestHeaders resolves the route's policy chain and dispatches to
// every policy implementing UpstreamAttemptPolicy, in chain order. A policy
// that doesn't implement it (the common case — rate limiting, analytics,
// transforms) is silently skipped via the type assertion; this is what makes
// the mechanism generic with zero per-policy wiring in this server. routeKey
// is resolved once by Process and passed in so it can be reused for the body
// message on the same stream. It also returns the parsed attemptCount so
// Process can carry it forward to processRequestBody.
func (s *UpstreamExternalProcessorServer) processRequestHeaders(ctx context.Context, req *extprocv3.ProcessingRequest, routeKey string) (*extprocv3.ProcessingResponse, int, error) {
	chain := s.kernel.GetPolicyChain(routeKey)
	if chain == nil {
		return emptyContinueRequestHeadersResponse(), 1, nil
	}

	headers := req.GetRequestHeaders()
	attemptCount := 1
	headersMap := make(map[string][]string)
	if headers.GetHeaders() != nil {
		for _, h := range headers.GetHeaders().GetHeaders() {
			key := h.Key
			value := string(h.RawValue)
			headersMap[key] = append(headersMap[key], value)
			if key == "x-envoy-attempt-count" {
				if n, err := strconv.Atoi(value); err == nil && n > 0 {
					attemptCount = n
				}
			}
		}
	}

	actx := &policy.UpstreamAttemptContext{
		AttemptCount: attemptCount,
		Headers:      policy.NewHeaders(headersMap),
	}

	headersToSet := make(map[string]string)
	for _, p := range chain.Policies {
		attemptPolicy, ok := p.(policy.UpstreamAttemptPolicy)
		if !ok {
			continue
		}
		action := attemptPolicy.OnUpstreamAttemptRequestHeaders(ctx, actx)
		mods, ok := action.(policy.UpstreamAttemptHeaderModifications)
		if !ok {
			continue
		}
		for k, v := range mods.HeadersToSet {
			headersToSet[k] = v
		}
	}

	if len(headersToSet) == 0 {
		return emptyContinueRequestHeadersResponse(), attemptCount, nil
	}

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extprocv3.HeadersResponse{
				Response: &extprocv3.CommonResponse{
					HeaderMutation: buildHeaderValueOptions(headersToSet),
				},
			},
		},
	}, attemptCount, nil
}

// processRequestBody dispatches the SAME OnUpstreamAttemptRequestHeaders hook
// again (not a new interface method — see the SDK design note in Task 2),
// this time with Body populated, and applies any returned Body mutation with
// a matching Content-Length. Policies that never set actx.Body-derived logic
// (headers-only consumers like oauth2-generator) simply won't have this
// method invoked at all for their clusters, since their clusters never
// enable RequestBodyMode — this handler only ever runs for clusters that do.
func (s *UpstreamExternalProcessorServer) processRequestBody(ctx context.Context, body *extprocv3.HttpBody, routeKey string, attemptCount int) (*extprocv3.ProcessingResponse, error) {
	chain := s.kernel.GetPolicyChain(routeKey)
	if chain == nil {
		return emptyContinueRequestBodyResponse(), nil
	}

	actx := &policy.UpstreamAttemptContext{
		AttemptCount: attemptCount,
		Body:         &policy.Body{Content: body.GetBody(), Present: true, EndOfStream: true},
	}

	var mutatedBody []byte
	for _, p := range chain.Policies {
		attemptPolicy, ok := p.(policy.UpstreamAttemptPolicy)
		if !ok {
			continue
		}
		action := attemptPolicy.OnUpstreamAttemptRequestHeaders(ctx, actx)
		mods, ok := action.(policy.UpstreamAttemptHeaderModifications)
		if !ok || mods.Body == nil {
			continue
		}
		mutatedBody = mods.Body // last write wins, matching HeadersToSet's own convention
	}

	if mutatedBody == nil {
		return emptyContinueRequestBodyResponse(), nil
	}

	// buildHeaderValueOptions(map[string]string{}) would return nil here (its
	// own empty-map guard), and setContentLengthHeader dereferences its
	// mutation argument — so the mutation must be allocated directly rather
	// than routed through buildHeaderValueOptions for this empty-starting-map
	// case.
	headerMutation := &extprocv3.HeaderMutation{}
	setContentLengthHeader(headerMutation, len(mutatedBody))

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_RequestBody{
			RequestBody: &extprocv3.BodyResponse{
				Response: &extprocv3.CommonResponse{
					BodyMutation:   &extprocv3.BodyMutation{Mutation: &extprocv3.BodyMutation_Body{Body: mutatedBody}},
					HeaderMutation: headerMutation,
				},
			},
		},
	}, nil
}

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
	"strconv"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/registry"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
	"google.golang.org/grpc/metadata"
	structpb "google.golang.org/protobuf/types/known/structpb"
)

// fakeUpstreamAttemptPolicy proves the dispatch loop invokes exactly the
// policies implementing UpstreamAttemptPolicy, via type assertion, ignoring
// every other policy in the chain (rate-limit/analytics-shaped policies that
// don't implement it).
type fakeUpstreamAttemptPolicy struct{ lastAttempt int }

func (p *fakeUpstreamAttemptPolicy) Mode() policy.ProcessingMode { return policy.ProcessingMode{} }
func (p *fakeUpstreamAttemptPolicy) OnUpstreamAttemptRequestHeaders(_ context.Context, actx *policy.UpstreamAttemptContext) policy.UpstreamAttemptAction {
	p.lastAttempt = actx.AttemptCount
	if actx.AttemptCount <= 1 {
		return policy.UpstreamAttemptHeaderModifications{}
	}
	return policy.UpstreamAttemptHeaderModifications{HeadersToSet: map[string]string{"Authorization": "Bearer refreshed"}}
}

// nonParticipatingPolicy implements only the base Policy interface — proves
// the dispatch loop skips it via type assertion, not a hardcoded name check.
type nonParticipatingPolicy struct{}

func (nonParticipatingPolicy) Mode() policy.ProcessingMode { return policy.ProcessingMode{} }

// newTestRouteConfigAndChain builds a Kernel with a policy chain registered
// under routeKey, using the package's real chain-registration entry point
// (RegisterRoute — see mapper.go and its use throughout kernel_test.go /
// body_mode_test.go / extproc_test.go) rather than a new test-only setter:
// processRequestHeaders only ever reads the chain via Kernel.GetPolicyChain,
// which RegisterRoute already populates, so no additional mechanism is
// needed.
func newTestRouteConfigAndChain(t *testing.T, routeKey string, chain *registry.PolicyChain) *Kernel {
	t.Helper()
	k := NewKernel()
	k.RegisterRoute(routeKey, chain)
	return k
}

func attrsFor(routeKey string) map[string]*structpb.Struct {
	return map[string]*structpb.Struct{
		"envoy.filters.http.ext_proc": {
			Fields: map[string]*structpb.Value{
				"xds.route_name": structpb.NewStringValue(routeKey),
			},
		},
	}
}

func TestUpstreamExtProc_DispatchesOnlyToImplementingPolicies(t *testing.T) {
	fp := &fakeUpstreamAttemptPolicy{}
	chain := &registry.PolicyChain{Policies: []policy.Policy{nonParticipatingPolicy{}, fp}}
	k := newTestRouteConfigAndChain(t, "test-route", chain)
	s := NewUpstreamExternalProcessorServer(k)

	req := &extprocv3.ProcessingRequest{
		Attributes: attrsFor("test-route"),
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
					{Key: "x-envoy-attempt-count", RawValue: []byte("2")},
				}},
			},
		},
	}

	resp, _, err := s.processRequestHeaders(context.Background(), req, "test-route")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fp.lastAttempt != 2 {
		t.Fatalf("expected the implementing policy to observe AttemptCount=2, got %d", fp.lastAttempt)
	}
	rh, ok := resp.Response.(*extprocv3.ProcessingResponse_RequestHeaders)
	if !ok {
		t.Fatalf("expected a RequestHeaders response, got %T", resp.Response)
	}
	mutation := rh.RequestHeaders.GetResponse().GetHeaderMutation()
	if mutation == nil || len(mutation.SetHeaders) != 1 || string(mutation.SetHeaders[0].Header.RawValue) != "Bearer refreshed" {
		t.Fatalf("expected the refreshed Authorization header to be set, got %#v", mutation)
	}
}

func TestUpstreamExtProc_MissingAttemptCountHeaderTreatedAsOne(t *testing.T) {
	fp := &fakeUpstreamAttemptPolicy{}
	chain := &registry.PolicyChain{Policies: []policy.Policy{fp}}
	k := newTestRouteConfigAndChain(t, "test-route", chain)
	s := NewUpstreamExternalProcessorServer(k)

	req := &extprocv3.ProcessingRequest{
		Attributes: attrsFor("test-route"),
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{Headers: &corev3.HeaderMap{}},
		},
	}
	if _, _, err := s.processRequestHeaders(context.Background(), req, "test-route"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fp.lastAttempt != 1 {
		t.Fatalf("expected a missing attempt-count header to be treated as attempt 1, got %d", fp.lastAttempt)
	}
}

func TestUpstreamExtProc_UnknownRouteReturnsEmptyContinue(t *testing.T) {
	k := NewKernel()
	s := NewUpstreamExternalProcessorServer(k)
	req := &extprocv3.ProcessingRequest{
		Attributes: attrsFor("no-such-route"),
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{Headers: &corev3.HeaderMap{}},
		},
	}
	resp, _, err := s.processRequestHeaders(context.Background(), req, "no-such-route")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rh, ok := resp.Response.(*extprocv3.ProcessingResponse_RequestHeaders)
	if !ok || rh.RequestHeaders.GetResponse().GetHeaderMutation() != nil {
		t.Fatalf("expected an empty continue response for an unknown route, got %#v", resp.Response)
	}
}

// fakeBodyMutatingPolicy always returns a Body mutation regardless of
// attempt count, proving Process's RequestBody dispatch (and the
// Content-Length it computes) independently of the headers-phase logic
// already covered by fakeUpstreamAttemptPolicy above.
type fakeBodyMutatingPolicy struct {
	wantBody []byte
}

func (p *fakeBodyMutatingPolicy) Mode() policy.ProcessingMode { return policy.ProcessingMode{} }
func (p *fakeBodyMutatingPolicy) OnUpstreamAttemptRequestHeaders(_ context.Context, _ *policy.UpstreamAttemptContext) policy.UpstreamAttemptAction {
	return policy.UpstreamAttemptHeaderModifications{Body: p.wantBody}
}

// newTestKernelWithPolicyChain builds a Kernel with a policy chain registered
// under routeKey directly from the given policies, via the same
// Kernel.RegisterRoute entry point newTestRouteConfigAndChain above uses.
func newTestKernelWithPolicyChain(t *testing.T, routeKey string, policies ...policy.Policy) *Kernel {
	t.Helper()
	k := NewKernel()
	k.RegisterRoute(routeKey, &registry.PolicyChain{Policies: policies})
	return k
}

// fakeProcessServer is a minimal in-memory stand-in for
// extprocv3.ExternalProcessor_ProcessServer: it replays a fixed sequence of
// ProcessingRequest messages via Recv (returning io.EOF once exhausted, like
// a real closed stream) and records every ProcessingResponse handed to Send,
// so a test can drive Process end-to-end across multiple messages on the
// same "stream".
type fakeProcessServer struct {
	t             *testing.T
	requests      []*extprocv3.ProcessingRequest
	idx           int
	sentResponses []*extprocv3.ProcessingResponse
}

func newFakeProcessServer(t *testing.T, requests []*extprocv3.ProcessingRequest) *fakeProcessServer {
	t.Helper()
	return &fakeProcessServer{t: t, requests: requests}
}

func (f *fakeProcessServer) Send(resp *extprocv3.ProcessingResponse) error {
	f.sentResponses = append(f.sentResponses, resp)
	return nil
}

func (f *fakeProcessServer) Recv() (*extprocv3.ProcessingRequest, error) {
	if f.idx >= len(f.requests) {
		return nil, io.EOF
	}
	req := f.requests[f.idx]
	f.idx++
	return req, nil
}

func (f *fakeProcessServer) Context() context.Context     { return context.Background() }
func (f *fakeProcessServer) SetHeader(metadata.MD) error  { return nil }
func (f *fakeProcessServer) SendHeader(metadata.MD) error { return nil }
func (f *fakeProcessServer) SetTrailer(metadata.MD)       {}
func (f *fakeProcessServer) SendMsg(m any) error          { return nil }
func (f *fakeProcessServer) RecvMsg(m any) error          { return nil }

// requestHeadersMsg builds a RequestHeaders-case ProcessingRequest, mirroring
// attrsFor's route-attribute shape and RawValue-based header fixtures already
// used by the header-only tests above.
func requestHeadersMsg(t *testing.T, routeKey string, headers map[string]string) *extprocv3.ProcessingRequest {
	t.Helper()
	headerValues := make([]*corev3.HeaderValue, 0, len(headers))
	for k, v := range headers {
		headerValues = append(headerValues, &corev3.HeaderValue{Key: k, RawValue: []byte(v)})
	}
	return &extprocv3.ProcessingRequest{
		Attributes: attrsFor(routeKey),
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: &corev3.HeaderMap{Headers: headerValues},
			},
		},
	}
}

// requestBodyMsg builds a RequestBody-case ProcessingRequest carrying the
// given buffered body bytes.
func requestBodyMsg(t *testing.T, body []byte) *extprocv3.ProcessingRequest {
	t.Helper()
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestBody{
			RequestBody: &extprocv3.HttpBody{Body: body, EndOfStream: true},
		},
	}
}

// headerValue finds a header by key (case-insensitively) among a
// HeaderMutation's SetHeaders, or "" if absent/nil.
func headerValue(mutation *extprocv3.HeaderMutation, key string) string {
	if mutation == nil {
		return ""
	}
	for _, opt := range mutation.GetSetHeaders() {
		if strings.EqualFold(opt.GetHeader().GetKey(), key) {
			return string(opt.GetHeader().GetRawValue())
		}
	}
	return ""
}

func TestProcess_RequestBodyAppliesBodyMutationWithContentLength(t *testing.T) {
	k := newTestKernelWithPolicyChain(t, "test-route", &fakeBodyMutatingPolicy{
		wantBody: []byte(`{"model":"gpt-4o-mini","marker":"attempt-2"}`),
	})
	s := NewUpstreamExternalProcessorServer(k)

	stream := newFakeProcessServer(t, []*extprocv3.ProcessingRequest{
		requestHeadersMsg(t, "test-route", map[string]string{"x-envoy-attempt-count": "2"}),
		requestBodyMsg(t, []byte(`{"model":"gpt-4o","original":true}`)),
	})

	if err := s.Process(stream); err != nil {
		t.Fatalf("Process returned error: %v", err)
	}

	bodyResp := stream.sentResponses[1].GetRequestBody()
	if bodyResp == nil {
		t.Fatalf("expected a RequestBody response for the second message, got %#v", stream.sentResponses[1])
	}
	mutation := bodyResp.GetResponse().GetBodyMutation().GetBody()
	if string(mutation) != `{"model":"gpt-4o-mini","marker":"attempt-2"}` {
		t.Errorf("expected mutated body, got %q", mutation)
	}
	cl := headerValue(bodyResp.GetResponse().GetHeaderMutation(), "content-length")
	if cl != strconv.Itoa(len(mutation)) {
		t.Errorf("expected content-length %d, got %q", len(mutation), cl)
	}
}

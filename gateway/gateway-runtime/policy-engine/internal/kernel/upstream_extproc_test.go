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
func (p *fakeUpstreamAttemptPolicy) OnUpstreamAttemptRequest(_ context.Context, actx *policy.UpstreamAttemptContext) policy.UpstreamAttemptAction {
	p.lastAttempt = actx.AttemptCount
	if actx.AttemptCount <= 1 {
		return policy.UpstreamAttemptRequestModifications{}
	}
	return policy.UpstreamAttemptRequestModifications{HeadersToSet: map[string]string{"Authorization": "Bearer refreshed"}}
}

// authorityCapturingPolicy proves whether :authority (an HTTP/2 pseudo-header) survives from
// Envoy's ext_proc RequestHeaders message into UpstreamAttemptContext.Headers — this is the
// prerequisite fact for host/authority-based per-attempt policy scoping (an alternative to
// AttemptCount-based scoping, which breaks for model-failover's own skip-ahead-after-suspend
// redirect; see gateway/spec/prds/llm-cross-provider-failover.md's Open Questions).
type authorityCapturingPolicy struct{ observedAuthority []string }

func (p *authorityCapturingPolicy) Mode() policy.ProcessingMode { return policy.ProcessingMode{} }
func (p *authorityCapturingPolicy) OnUpstreamAttemptRequest(_ context.Context, actx *policy.UpstreamAttemptContext) policy.UpstreamAttemptAction {
	p.observedAuthority = actx.Headers.Get(":authority")
	return policy.UpstreamAttemptRequestModifications{}
}

// nonParticipatingPolicy implements only the base Policy interface — proves
// the dispatch loop skips it via type assertion, not a hardcoded name check.
type nonParticipatingPolicy struct{}

func (nonParticipatingPolicy) Mode() policy.ProcessingMode { return policy.ProcessingMode{} }

// newTestRouteConfigAndChain builds a Kernel with a policy chain registered
// under routeKey, using the package's real chain-registration entry point
// (RegisterRoute — see mapper.go and its use throughout kernel_test.go /
// body_mode_test.go / extproc_test.go) rather than a new test-only setter:
// processUpstreamAttemptRequestHeaders only ever reads the chain via Kernel.GetPolicyChain,
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

	resp, _, err := s.processUpstreamAttemptRequestHeaders(context.Background(), req, "test-route")
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

// TestUpstreamExtProc_AuthorityPseudoHeaderReachesPolicy is a fact-finding test, not a
// regression guard for any shipped behavior: processUpstreamAttemptRequestHeaders (see its
// own source) copies every header Envoy sends into headersMap with no filtering at all -
// this proves that includes HTTP/2 pseudo-headers like :authority, which Envoy's ext_proc
// protocol sends alongside ordinary headers by default. Confirms a per-attempt policy CAN
// read the actual resolved destination for this specific dial, not just AttemptCount.
func TestUpstreamExtProc_AuthorityPseudoHeaderReachesPolicy(t *testing.T) {
	fp := &authorityCapturingPolicy{}
	chain := &registry.PolicyChain{Policies: []policy.Policy{fp}}
	k := newTestRouteConfigAndChain(t, "test-route", chain)
	s := NewUpstreamExternalProcessorServer(k)

	req := &extprocv3.ProcessingRequest{
		Attributes: attrsFor("test-route"),
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
					{Key: "x-envoy-attempt-count", RawValue: []byte("2")},
					{Key: ":authority", RawValue: []byte("anthropic-backup.internal")},
				}},
			},
		},
	}

	if _, _, err := s.processUpstreamAttemptRequestHeaders(context.Background(), req, "test-route"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fp.observedAuthority) != 1 || fp.observedAuthority[0] != "anthropic-backup.internal" {
		t.Fatalf("expected :authority to reach the policy via actx.Headers.Get(\":authority\"), got %#v", fp.observedAuthority)
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
	if _, _, err := s.processUpstreamAttemptRequestHeaders(context.Background(), req, "test-route"); err != nil {
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
	resp, _, err := s.processUpstreamAttemptRequestHeaders(context.Background(), req, "no-such-route")
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
func (p *fakeBodyMutatingPolicy) OnUpstreamAttemptRequest(_ context.Context, _ *policy.UpstreamAttemptContext) policy.UpstreamAttemptAction {
	return policy.UpstreamAttemptRequestModifications{Body: p.wantBody}
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

// recordingResponseObserverPolicy implements policy.UpstreamAttemptResponseObserver
// (plus the base policy.Policy interface via Mode, matching every other fake
// policy in this file) and records the last UpstreamAttemptResponseContext it
// was invoked with.
type recordingResponseObserverPolicy struct {
	lastAttemptCount   int
	lastResponseStatus int
	lastRequestID      string
}

func (r *recordingResponseObserverPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{}
}
func (r *recordingResponseObserverPolicy) OnUpstreamAttemptResponse(_ context.Context, actx *policy.UpstreamAttemptResponseContext) {
	r.lastAttemptCount = actx.AttemptCount
	r.lastResponseStatus = actx.ResponseStatus
	r.lastRequestID = actx.RequestID
}

// responseHeaders builds an *extprocv3.HttpHeaders carrying the given
// headers — the real payload type for the ProcessingRequest_ResponseHeaders
// oneof variant (confirmed against the vendored external_processor.pb.go;
// it is the same HttpHeaders type requestHeadersMsg above uses for the
// request direction, not a separate HttpResponse type).
func responseHeaders(headers map[string]string) *extprocv3.HttpHeaders {
	headerValues := make([]*corev3.HeaderValue, 0, len(headers))
	for k, v := range headers {
		headerValues = append(headerValues, &corev3.HeaderValue{Key: k, RawValue: []byte(v)})
	}
	return &extprocv3.HttpHeaders{Headers: &corev3.HeaderMap{Headers: headerValues}}
}

func TestProcessUpstreamAttemptResponse_DispatchesToObserverPolicy(t *testing.T) {
	observer := &recordingResponseObserverPolicy{}
	k := newTestKernelWithPolicyChain(t, "test-route", observer)
	s := NewUpstreamExternalProcessorServer(k)

	respHeaders := responseHeaders(map[string]string{
		":status":      "401",
		"x-request-id": "req-123",
	})

	resp, err := s.processUpstreamAttemptResponse(context.Background(), respHeaders, "test-route", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if observer.lastAttemptCount != 1 {
		t.Errorf("lastAttemptCount = %d, want 1", observer.lastAttemptCount)
	}
	if observer.lastResponseStatus != 401 {
		t.Errorf("lastResponseStatus = %d, want 401", observer.lastResponseStatus)
	}
	if observer.lastRequestID != "req-123" {
		t.Errorf("lastRequestID = %q, want %q", observer.lastRequestID, "req-123")
	}
	rh, ok := resp.Response.(*extprocv3.ProcessingResponse_ResponseHeaders)
	if !ok {
		t.Fatalf("expected a ResponseHeaders response, got %T", resp.Response)
	}
	if rh.ResponseHeaders.GetResponse().GetHeaderMutation() != nil {
		t.Errorf("expected no header mutation from a read-only observer phase, got %#v", rh.ResponseHeaders.GetResponse().GetHeaderMutation())
	}
}

func TestProcessUpstreamAttemptResponse_UnknownRouteReturnsEmptyContinue(t *testing.T) {
	k := NewKernel()
	s := NewUpstreamExternalProcessorServer(k)

	resp, err := s.processUpstreamAttemptResponse(context.Background(), responseHeaders(map[string]string{":status": "200"}), "no-such-route", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rh, ok := resp.Response.(*extprocv3.ProcessingResponse_ResponseHeaders)
	if !ok {
		t.Fatalf("expected a ResponseHeaders response, got %T", resp.Response)
	}
	if rh.ResponseHeaders.GetResponse().GetHeaderMutation() != nil {
		t.Errorf("expected an empty continue response for an unknown route, got %#v", resp.Response)
	}
}

// responseHeadersMsg builds a ResponseHeaders-case ProcessingRequest,
// mirroring requestHeadersMsg's shape for the response direction.
func responseHeadersMsg(t *testing.T, routeKey string, headers map[string]string) *extprocv3.ProcessingRequest {
	t.Helper()
	return &extprocv3.ProcessingRequest{
		Attributes: attrsFor(routeKey),
		Request: &extprocv3.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: responseHeaders(headers),
		},
	}
}

func TestProcess_DispatchesResponseHeadersMessageToObserverPolicy(t *testing.T) {
	observer := &recordingResponseObserverPolicy{}
	k := newTestKernelWithPolicyChain(t, "test-route", observer)
	s := NewUpstreamExternalProcessorServer(k)

	stream := newFakeProcessServer(t, []*extprocv3.ProcessingRequest{
		requestHeadersMsg(t, "test-route", map[string]string{"x-envoy-attempt-count": "2"}),
		responseHeadersMsg(t, "test-route", map[string]string{":status": "503", "x-request-id": "req-abc"}),
	})

	if err := s.Process(stream); err != nil {
		t.Fatalf("Process returned error: %v", err)
	}

	if observer.lastAttemptCount != 2 {
		t.Errorf("lastAttemptCount = %d, want 2 (carried forward from the request-headers message)", observer.lastAttemptCount)
	}
	if observer.lastResponseStatus != 503 {
		t.Errorf("lastResponseStatus = %d, want 503", observer.lastResponseStatus)
	}
	if observer.lastRequestID != "req-abc" {
		t.Errorf("lastRequestID = %q, want %q", observer.lastRequestID, "req-abc")
	}

	if len(stream.sentResponses) != 2 {
		t.Fatalf("expected 2 responses sent, got %d", len(stream.sentResponses))
	}
	if _, ok := stream.sentResponses[1].Response.(*extprocv3.ProcessingResponse_ResponseHeaders); !ok {
		t.Fatalf("expected the second response to be a ResponseHeaders response, got %T", stream.sentResponses[1].Response)
	}
}

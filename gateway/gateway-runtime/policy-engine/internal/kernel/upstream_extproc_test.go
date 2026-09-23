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
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/executor"
	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/registry"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
	policyenginev1 "github.com/wso2/api-platform/sdk/core/policyengine"
)

// upstreamAuthStub is a minimal RequestPolicy for exercising the upstream
// ext_proc server end-to-end without a real backend policy — attached via
// upstreamPolicies: (chain.UpstreamPolicies), it runs via the same
// OnRequestBody a downstream-attached instance would use.
type upstreamAuthStub struct{}

func (upstreamAuthStub) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{RequestBodyMode: policy.BodyModeBuffer}
}

func (upstreamAuthStub) OnRequestBody(_ context.Context, reqCtx *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
	return policy.UpstreamRequestModifications{
		HeadersToSet: map[string]string{"authorization": "Bearer signed-for-" + reqCtx.Upstream.Name},
	}
}

func requestHeadersReqWithRouteAndCluster(routeKey, clusterName string) *extprocv3.ProcessingRequest {
	attrs, _ := structpb.NewStruct(map[string]interface{}{
		"xds.route_name":   routeKey,
		"xds.cluster_name": clusterName,
	})
	return &extprocv3.ProcessingRequest{
		Attributes: map[string]*structpb.Struct{"envoy.filters.http.ext_proc": attrs},
		Request: &extprocv3.ProcessingRequest_RequestHeaders{
			RequestHeaders: &extprocv3.HttpHeaders{
				Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
					{Key: ":path", RawValue: []byte("/v1/chat/completions")},
					{Key: ":method", RawValue: []byte("POST")},
				}},
			},
		},
	}
}

func requestHeadersReqWithAuthority(routeKey, clusterName, authority string) *extprocv3.ProcessingRequest {
	req := requestHeadersReqWithRouteAndCluster(routeKey, clusterName)
	req.GetRequestHeaders().Headers.Headers = append(req.GetRequestHeaders().Headers.Headers,
		&corev3.HeaderValue{Key: ":authority", RawValue: []byte(authority)})
	return req
}

func responseHeadersReqWithStatus(status string) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_ResponseHeaders{
			ResponseHeaders: &extprocv3.HttpHeaders{
				Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
					{Key: ":status", RawValue: []byte(status)},
				}},
			},
		},
	}
}

func requestBodyReq(body []byte) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestBody{
			RequestBody: &extprocv3.HttpBody{Body: body, EndOfStream: true},
		},
	}
}

// upstreamEchoStub reports back whatever URL/Method/Path the kernel resolved
// for this attempt, so tests can assert on them without a real backend
// policy — attached via upstreamPolicies: (chain.UpstreamPolicies).
type upstreamEchoStub struct{}

func (upstreamEchoStub) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{RequestBodyMode: policy.BodyModeBuffer}
}

func (upstreamEchoStub) OnRequestBody(_ context.Context, reqCtx *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
	return policy.UpstreamRequestModifications{
		HeadersToSet: map[string]string{
			"x-resolved-url":     reqCtx.Upstream.URL,
			"x-resolved-method":  reqCtx.Method,
			"x-resolved-path":    reqCtx.Path,
			"x-resolved-cluster": reqCtx.Upstream.Name,
		},
	}
}

func headerMutationMap(t *testing.T, resp *extprocv3.ProcessingResponse) map[string]string {
	t.Helper()
	mutation := resp.GetRequestBody().GetResponse().GetHeaderMutation()
	require.NotNil(t, mutation)
	headers := make(map[string]string, len(mutation.SetHeaders))
	for _, h := range mutation.SetHeaders {
		headers[h.Header.Key] = string(h.Header.RawValue)
	}
	return headers
}

func TestUpstreamProcess_ResolvesBackendURLAndPathFromRouteConfig(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		UpstreamPolicies:        []policy.Policy{upstreamEchoStub{}},
		UpstreamPolicySpecs:     []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{
		"chat-route": {
			Metadata: RouteMetadata{
				DefaultUpstream: &policyenginev1.UpstreamInfo{
					ClusterName: "aws-bedrock-fallback",
					URL:         "https://bedrock-runtime.us-east-1.amazonaws.com",
					BasePath:    "/v1",
				},
			},
		},
	})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithRouteAndCluster("chat-route", "aws-bedrock-fallback"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	headers := headerMutationMap(t, stream.responses[1])
	assert.Equal(t, "https://bedrock-runtime.us-east-1.amazonaws.com", headers["x-resolved-url"])
	assert.Equal(t, "POST", headers["x-resolved-method"])
	assert.Equal(t, "/v1/chat/completions", headers["x-resolved-path"])
}

func TestUpstreamProcess_ClusterMismatch_LeavesURLEmpty(t *testing.T) {
	// A retry attempt targeting a cluster this deployment has never seen at
	// all (neither this route's own default nor any other route's) must not
	// silently sign against the wrong host — URL stays empty so the
	// downstream policy fails closed instead of guessing.
	k := NewKernel()
	chain := &registry.PolicyChain{
		UpstreamPolicies:        []policy.Policy{upstreamEchoStub{}},
		UpstreamPolicySpecs:     []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{
		"chat-route": {
			Metadata: RouteMetadata{
				DefaultUpstream: &policyenginev1.UpstreamInfo{
					ClusterName: "aws-bedrock-fallback",
					URL:         "https://bedrock-runtime.us-east-1.amazonaws.com",
				},
			},
		},
	})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithRouteAndCluster("chat-route", "some-other-cluster"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	headers := headerMutationMap(t, stream.responses[1])
	assert.Empty(t, headers["x-resolved-url"])
}

func TestUpstreamProcess_RetryLandsOnAnotherRoutesCluster_ResolvesViaGlobalIndex(t *testing.T) {
	// Models a single-request failover retry: Envoy's route still reports
	// this route's own name (xds.route_name — so the SAME policy chain
	// applies to every attempt), but a retry can land on a real cluster this
	// deployment created for a DIFFERENT route entirely (e.g. an
	// aggregate-cluster's second priority member). That cluster's URL is
	// known — just not as *this* route's DefaultUpstream — so resolution
	// must fall back to a global cluster-name index instead of failing
	// closed the way an unknown cluster correctly does above.
	k := NewKernel()
	chain := &registry.PolicyChain{
		UpstreamPolicies:        []policy.Policy{upstreamEchoStub{}},
		UpstreamPolicySpecs:     []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("provider-a-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{
		"provider-a-route": {
			Metadata: RouteMetadata{
				DefaultUpstream: &policyenginev1.UpstreamInfo{
					ClusterName: "cluster-a",
					URL:         "http://backend-a.internal:18091",
					BasePath:    "/v1",
				},
			},
		},
		"provider-b-route": {
			Metadata: RouteMetadata{
				DefaultUpstream: &policyenginev1.UpstreamInfo{
					ClusterName: "cluster-b",
					URL:         "http://backend-b.internal:18092",
					BasePath:    "/v2",
				},
			},
		},
	})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithRouteAndCluster("provider-a-route", "cluster-b"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	headers := headerMutationMap(t, stream.responses[1])
	assert.Equal(t, "http://backend-b.internal:18092", headers["x-resolved-url"])
}

func TestUpstreamProcess_AggregateClusterAttempt_ResolvesFromAuthorityHeader(t *testing.T) {
	// Confirmed live: when a route dispatches through an envoy.clusters.aggregate
	// pseudo-cluster (the real single-request-failover shape — priority 0/1
	// member clusters, retry_priority escalating between them), the upstream
	// ext_proc filter only fires when attached to the AGGREGATE cluster itself,
	// not its real members — and when it does, Envoy's xds.cluster_name
	// attribute reports the AGGREGATE's own name on every attempt, never the
	// real member actually dialed. Cluster-name-keyed resolution (route-scoped
	// or the global index) can therefore never identify the real backend for
	// this case — the one this whole mechanism exists for. The only
	// attempt-accurate signal left is the actual :authority header Envoy is
	// about to dial, which this test proves resolveBackend falls back to.
	k := NewKernel()
	chain := &registry.PolicyChain{
		UpstreamPolicies:        []policy.Policy{upstreamEchoStub{}},
		UpstreamPolicySpecs:     []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("provider-a-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{
		"provider-a-route": {
			Metadata: RouteMetadata{
				DefaultUpstream: &policyenginev1.UpstreamInfo{
					ClusterName: "cluster-a",
					URL:         "http://backend-a.internal:18091",
				},
			},
		},
	})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithAuthority("provider-a-route", "failover-aggregate", "backend-b.internal:18092"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	headers := headerMutationMap(t, stream.responses[1])
	assert.Equal(t, "http://backend-b.internal:18092", headers["x-resolved-url"])
}

// upstreamHeaderResolvingStub stands in for a chain-aware upstream-attempt
// policy (model-failover) that knows this attempt's real backend when
// cluster-name-keyed resolution could not: it writes the base path back onto
// the context and corrects :path. Its body phase echoes what the kernel
// carried forward, which is what the test below asserts on.
type upstreamHeaderResolvingStub struct{}

func (upstreamHeaderResolvingStub) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{RequestHeaderMode: policy.HeaderModeProcess, RequestBodyMode: policy.BodyModeBuffer}
}

func (upstreamHeaderResolvingStub) OnRequestHeaders(_ context.Context, reqCtx *policy.RequestHeaderContext, _ map[string]interface{}) policy.RequestHeaderAction {
	reqCtx.Upstream.BasePath = "/anthropic-provider"
	corrected := "/anthropic-provider/chat/completions"
	return policy.UpstreamRequestHeaderModifications{Path: &corrected}
}

func (upstreamHeaderResolvingStub) OnRequestBody(_ context.Context, reqCtx *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
	return policy.UpstreamRequestModifications{
		HeadersToSet: map[string]string{
			"x-resolved-base-path": reqCtx.Upstream.BasePath,
			"x-resolved-path":      reqCtx.Path,
		},
	}
}

// TestUpstreamProcess_AdoptsBasePathAHeaderPhasePolicyResolved pins the generic
// hand-back: resolveBackend keys off xds.cluster_name, which for an
// envoy.clusters.aggregate attempt is the aggregate's own name and resolves
// nothing, leaving state.basePath empty. A header-phase policy that does know
// the member writes it onto the context, and the kernel carries it into the
// rest of the attempt — with no knowledge of what kind of chain it is.
func TestUpstreamProcess_AdoptsBasePathAHeaderPhasePolicyResolved(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		UpstreamPolicies:        []policy.Policy{upstreamHeaderResolvingStub{}},
		UpstreamPolicySpecs:     []policy.PolicySpec{{Name: "upstream-header-resolving-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": {Metadata: RouteMetadata{}}})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithRouteAndCluster("chat-route", "failover_agg_chat_0"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	// The policy's corrected :path reaches Envoy as a header mutation.
	pathMutation := stream.responses[0].GetRequestHeaders().GetResponse().GetHeaderMutation()
	require.NotNil(t, pathMutation)
	var correctedPath string
	for _, h := range pathMutation.SetHeaders {
		if h.Header.Key == ":path" {
			correctedPath = string(h.Header.RawValue)
		}
	}
	assert.Equal(t, "/anthropic-provider/chat/completions", correctedPath)

	// ...and the base path it resolved is what the body phase sees.
	headers := headerMutationMap(t, stream.responses[1])
	assert.Equal(t, "/anthropic-provider", headers["x-resolved-base-path"])
	assert.Equal(t, "/anthropic-provider/chat/completions", headers["x-resolved-path"])
}

// upstreamResponseTranslatorStub mimics a shape-translating response policy
// (e.g. openai-to-anthropic-transformer's OnResponseBody): it rewrites the
// body and sets its own header, and runs FIRST in the chain. Attached via
// upstreamPolicies: (chain.UpstreamPolicies), it runs via the same
// OnResponseBody a downstream-attached instance would use.
type upstreamResponseTranslatorStub struct{}

func (upstreamResponseTranslatorStub) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{ResponseBodyMode: policy.BodyModeBuffer}
}

func (upstreamResponseTranslatorStub) OnResponseBody(_ context.Context, _ *policy.ResponseContext, _ map[string]interface{}) policy.ResponseAction {
	return policy.DownstreamResponseModifications{
		Body:         []byte(`{"translated":true}`),
		HeadersToSet: map[string]string{"content-type": "application/json"},
	}
}

// upstreamResponseHeaderOnlyStub mimics a second, unrelated response-phase
// policy (e.g. an analytics/status-mapping policy) that runs AFTER the
// translator and touches only its own header and the status code — never the
// body. Before the accumulation fix, this policy's action became FinalAction
// and silently replaced the translator's own Body/HeadersToSet.
type upstreamResponseHeaderOnlyStub struct{}

func (upstreamResponseHeaderOnlyStub) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{ResponseBodyMode: policy.BodyModeBuffer}
}

func (upstreamResponseHeaderOnlyStub) OnResponseBody(_ context.Context, _ *policy.ResponseContext, _ map[string]interface{}) policy.ResponseAction {
	status := 201
	return policy.DownstreamResponseModifications{
		HeadersToSet: map[string]string{"x-second-policy": "ran"},
		StatusCode:   &status,
	}
}

func responseHeaderMutationMap(t *testing.T, resp *extprocv3.ProcessingResponse) map[string]string {
	t.Helper()
	mutation := resp.GetResponseBody().GetResponse().GetHeaderMutation()
	require.NotNil(t, mutation)
	headers := make(map[string]string, len(mutation.SetHeaders))
	for _, h := range mutation.SetHeaders {
		headers[h.Header.Key] = string(h.Header.RawValue)
	}
	return headers
}

// TestUpstreamProcess_ResponseChain_AccumulatesAcrossPolicies is the
// regression test for the response-phase counterpart of the request-body
// chain-accumulation bug: ExecuteUpstreamResponsePolicies' result.FinalAction
// is whichever policy ran last, which silently drops an earlier policy's own
// Body/HeadersToSet once processResponseBody reads only FinalAction instead
// of the chain's accumulated upCtx state. With two response-phase policies
// attached, the translator's Body and content-type header, the second
// policy's own header, AND its status-code override must all survive into
// the single ext_proc response actually sent to Envoy.
func TestUpstreamProcess_ResponseChain_AccumulatesAcrossPolicies(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		UpstreamPolicies: []policy.Policy{upstreamResponseTranslatorStub{}, upstreamResponseHeaderOnlyStub{}},
		UpstreamPolicySpecs: []policy.PolicySpec{
			{Name: "response-translator-stub", Version: "v1", Enabled: true},
			{Name: "response-header-only-stub", Version: "v1", Enabled: true},
		},
		RequiresUpstreamResponse: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{
		"chat-route": {
			Metadata: RouteMetadata{
				DefaultUpstream: &policyenginev1.UpstreamInfo{
					ClusterName: "plain-cluster",
					URL:         "https://plain.example.com",
				},
			},
		},
	})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithRouteAndCluster("chat-route", "plain-cluster"),
		requestBodyReq([]byte(`{}`)),
		responseHeadersReqWithStatus("200"),
		responseBodyReq(`{"original":true}`),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 4)

	headers := responseHeaderMutationMap(t, stream.responses[3])
	assert.Equal(t, "application/json", headers["content-type"], "the FIRST policy's header must survive a LATER policy running after it")
	assert.Equal(t, "ran", headers["x-second-policy"], "the SECOND policy's own header must also be present")
	assert.Equal(t, "201", headers[":status"], "the second policy's StatusCode override must reach Envoy as a :status mutation")

	bodyMutation := stream.responses[3].GetResponseBody().GetResponse().GetBodyMutation()
	require.NotNil(t, bodyMutation, "the FIRST policy's body translation must not be dropped by the second policy running after it")
	assert.Equal(t, `{"translated":true}`, string(bodyMutation.GetBody()))
}

func TestUpstreamProcess_NoPolicyChain_PassesThroughUnmodified(t *testing.T) {
	k := NewKernel()
	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithRouteAndCluster("no-such-route", "unresolved-backend"),
		requestBodyReq([]byte(`{}`)),
	})

	err := server.Process(stream)

	require.NoError(t, err)
	require.Len(t, stream.responses, 2)
	bodyResp := stream.responses[1].GetRequestBody()
	require.NotNil(t, bodyResp)
	assert.Nil(t, bodyResp.GetResponse().GetHeaderMutation())
}

func TestUpstreamProcess_RunsUpstreamPhasePolicyForThisBackend(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		UpstreamPolicies:        []policy.Policy{upstreamAuthStub{}},
		UpstreamPolicySpecs:     []policy.PolicySpec{{Name: "upstream-auth-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithRouteAndCluster("chat-route", "aws-bedrock-fallback"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)

	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	bodyResp := stream.responses[1].GetRequestBody()
	require.NotNil(t, bodyResp)
	mutation := bodyResp.GetResponse().GetHeaderMutation()
	require.NotNil(t, mutation)
	require.Len(t, mutation.SetHeaders, 1)
	assert.Equal(t, "authorization", mutation.SetHeaders[0].Header.Key)
	assert.Equal(t, "Bearer signed-for-aws-bedrock-fallback", string(mutation.SetHeaders[0].Header.RawValue))
}

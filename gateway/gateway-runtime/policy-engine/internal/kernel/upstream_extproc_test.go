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

// upstreamAuthStub is a minimal UpstreamRequestPolicy for exercising the
// upstream ext_proc server end-to-end without a real backend policy.
type upstreamAuthStub struct{}

func (upstreamAuthStub) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{UpstreamRequestMode: policy.BodyModeBuffer}
}

func (upstreamAuthStub) OnUpstreamRequestBody(_ context.Context, upCtx *policy.UpstreamAttemptContext, _ map[string]interface{}) policy.RequestAction {
	return policy.UpstreamRequestModifications{
		HeadersToSet: map[string]string{"authorization": "Bearer signed-for-" + upCtx.Name},
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

func requestBodyReq(body []byte) *extprocv3.ProcessingRequest {
	return &extprocv3.ProcessingRequest{
		Request: &extprocv3.ProcessingRequest_RequestBody{
			RequestBody: &extprocv3.HttpBody{Body: body, EndOfStream: true},
		},
	}
}

// upstreamEchoStub reports back whatever URL/Method/Path the kernel resolved
// for this attempt, so tests can assert on them without a real backend policy.
type upstreamEchoStub struct{}

func (upstreamEchoStub) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{UpstreamRequestMode: policy.BodyModeBuffer}
}

func (upstreamEchoStub) OnUpstreamRequestBody(_ context.Context, upCtx *policy.UpstreamAttemptContext, _ map[string]interface{}) policy.RequestAction {
	return policy.UpstreamRequestModifications{
		HeadersToSet: map[string]string{
			"x-resolved-url":    upCtx.URL,
			"x-resolved-method": upCtx.Method,
			"x-resolved-path":   upCtx.Path,
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
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
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
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
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
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
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
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
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
		Policies:                []policy.Policy{upstreamAuthStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-auth-stub", Version: "v1", Enabled: true}},
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

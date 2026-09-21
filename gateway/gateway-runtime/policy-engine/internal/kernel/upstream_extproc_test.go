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

func requestHeadersReqWithAttemptCount(routeKey, clusterName, attemptCount string) *extprocv3.ProcessingRequest {
	req := requestHeadersReqWithRouteAndCluster(routeKey, clusterName)
	req.GetRequestHeaders().Headers.Headers = append(req.GetRequestHeaders().Headers.Headers,
		&corev3.HeaderValue{Key: "x-envoy-attempt-count", RawValue: []byte(attemptCount)})
	return req
}

// requestHeadersReqWithAttemptCountAndPath is like requestHeadersReqWithAttemptCount
// but overrides the fixed ":path" requestHeadersReqWithRouteAndCluster sets, so a
// test can simulate the STALE path a downstream-phase rewrite baked in before an
// earlier attempt (never recomputed by Envoy across retries).
func requestHeadersReqWithAttemptCountAndPath(routeKey, clusterName, attemptCount, path string) *extprocv3.ProcessingRequest {
	req := requestHeadersReqWithAttemptCount(routeKey, clusterName, attemptCount)
	for _, h := range req.GetRequestHeaders().Headers.Headers {
		if h.Key == ":path" {
			h.RawValue = []byte(path)
		}
	}
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
			"x-resolved-url":      upCtx.URL,
			"x-resolved-method":   upCtx.Method,
			"x-resolved-path":     upCtx.Path,
			"x-resolved-cluster":  upCtx.Name,
			"x-resolved-model":    upCtx.ResolvedModel,
			"x-resolved-provider": upCtx.ResolvedProvider,
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

// failoverRouteConfig builds a RouteConfig carrying one declared failover
// target — gpt-4o on the primary (openai), falling back to Claude on
// Anthropic — matching the wire shape gateway-controller's
// policyxds/snapshot.go produces (aggregate_cluster + chain[]).
func failoverRouteConfig() *RouteConfig {
	return &RouteConfig{
		Metadata: RouteMetadata{
			// Deliberately distinct per member (matching a real
			// additionalProviders topology, where each provider is its own
			// loopback-routed LlmProvider with its own context) - two
			// members sharing one base path would silently mask the
			// stale-:path bug resolveBackend's caller must correct for.
			OperationPath:                  "/chat/completions",
			FailoverSuspendDurationSeconds: 900,
			FailoverTargets: []FailoverTarget{
				{
					AggregateCluster: "failover_agg_chat_0",
					Model:            "gpt-4o",
					Chain: []FailoverChainEntry{
						{
							Model:    "gpt-4o",
							Provider: "openai-provider",
							Upstream: policyenginev1.UpstreamInfo{
								ClusterName: "openai-provider-cluster",
								URL:         "https://api.openai.com/v1",
								BasePath:    "/openai-provider",
							},
						},
						{
							Model:    "claude-3-5-sonnet-20241022",
							Provider: "anthropic-upstream",
							Upstream: policyenginev1.UpstreamInfo{
								ClusterName: "anthropic-upstream-cluster",
								URL:         "https://api.anthropic.com/v1",
								BasePath:    "/anthropic-provider",
							},
						},
					},
				},
			},
		},
	}
}

func TestUpstreamProcess_FailoverAttempt1_ResolvesTargetFromChain(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": failoverRouteConfig()})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithAttemptCount("chat-route", "failover_agg_chat_0", "1"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	headers := headerMutationMap(t, stream.responses[1])
	assert.Equal(t, "https://api.openai.com/v1", headers["x-resolved-url"])
	assert.Equal(t, "openai-provider-cluster", headers["x-resolved-cluster"])
	assert.Equal(t, "gpt-4o", headers["x-resolved-model"])
	assert.Equal(t, "openai-provider", headers["x-resolved-provider"])
}

func TestUpstreamProcess_FailoverAttempt2_ResolvesFirstFallbackFromChain(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": failoverRouteConfig()})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithAttemptCount("chat-route", "failover_agg_chat_0", "2"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	headers := headerMutationMap(t, stream.responses[1])
	assert.Equal(t, "https://api.anthropic.com/v1", headers["x-resolved-url"])
	assert.Equal(t, "anthropic-upstream-cluster", headers["x-resolved-cluster"])
	assert.Equal(t, "claude-3-5-sonnet-20241022", headers["x-resolved-model"])
	assert.Equal(t, "anthropic-upstream", headers["x-resolved-provider"])
}

// TestUpstreamProcess_FailoverAttempt2_RewritesStalePathToResolvedProvidersBasePath
// is the regression test for a real bug caught by live e2e verification: the
// downstream phase rewrites :path exactly once, before Envoy's first dispatch,
// using whichever provider that attempt resolved to. Envoy's own retry
// reuses that same :path verbatim across every escalation — it does not get
// recomputed the way Host does via AutoHostRewrite. Without resolveBackend's
// caller correcting it, a retry that escalates to a DIFFERENT provider's own
// loopback route (a different base path) gets silently routed back to the
// ORIGINAL failed provider by the loopback listener's own path-prefix
// matching, regardless of which real cluster Envoy actually dialed.
func TestUpstreamProcess_FailoverAttempt2_RewritesStalePathToResolvedProvidersBasePath(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": failoverRouteConfig()})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	// Simulates the stale :path a downstream rewrite baked in for the FIRST
	// (primary/openai) attempt, now reused verbatim on attempt 2's retry to
	// the anthropic-upstream fallback.
	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithAttemptCountAndPath("chat-route", "failover_agg_chat_0", "2", "/openai-provider/chat/completions"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	headerMutation := stream.responses[0].GetRequestHeaders().GetResponse().GetHeaderMutation()
	require.NotNil(t, headerMutation, "expected a :path correction for an attempt landing on a different provider's base path")
	var correctedPath string
	for _, h := range headerMutation.SetHeaders {
		if h.Header.Key == ":path" {
			correctedPath = string(h.Header.RawValue)
		}
	}
	assert.Equal(t, "/anthropic-provider/chat/completions", correctedPath)

	// The corrected path must also be what policies see via upCtx.Path (e.g.
	// a signing policy building a synthetic request for the real backend).
	headers := headerMutationMap(t, stream.responses[1])
	assert.Equal(t, "/anthropic-provider/chat/completions", headers["x-resolved-path"])
}

// TestUpstreamProcess_FailoverAttempt2_EmitsResolvedProviderHeaderForAnalytics
// is the regression test for the analytics-attribution fix: a request that
// actually escalated to a fallback must carry ResolvedFailoverProviderHeader
// on its response, so the downstream ext_proc can correct ai:providername
// away from the route's PRIMARY-provider-derived template label. This is
// deliberately unconditional on any response-phase policy being attached —
// the chain here only declares RequiresUpstreamRequest, matching a plain
// api-key-only failover setup with no transformer.
func TestUpstreamProcess_FailoverAttempt2_EmitsResolvedProviderHeaderForAnalytics(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": failoverRouteConfig()})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithAttemptCount("chat-route", "failover_agg_chat_0", "2"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
		responseHeadersReqWithStatus("200"),
		responseBodyReq(`{"ok":true}`),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 4)

	mutation := stream.responses[3].GetResponseBody().GetResponse().GetHeaderMutation()
	require.NotNil(t, mutation, "attempt 2 resolved to the fallback — expected the resolved-provider header")
	var got string
	for _, h := range mutation.SetHeaders {
		if h.Header.Key == ResolvedFailoverProviderHeader {
			got = string(h.Header.RawValue)
		}
	}
	assert.Equal(t, "anthropic-upstream", got)
}

// TestUpstreamProcess_FailoverAttempt1_NoResolvedProviderHeaderWhenPrimaryServes
// proves the fix doesn't relabel ordinary, primary-served traffic just
// because its route happens to declare a resilience.failover block — only an
// attempt that actually escalated past the primary carries the header.
func TestUpstreamProcess_FailoverAttempt1_NoResolvedProviderHeaderWhenPrimaryServes(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": failoverRouteConfig()})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithAttemptCount("chat-route", "failover_agg_chat_0", "1"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
		responseHeadersReqWithStatus("200"),
		responseBodyReq(`{"ok":true}`),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 4)

	mutation := stream.responses[3].GetResponseBody().GetResponse().GetHeaderMutation()
	if mutation != nil {
		for _, h := range mutation.SetHeaders {
			assert.NotEqual(t, ResolvedFailoverProviderHeader, h.Header.Key,
				"the primary succeeding on attempt 1 must never carry the resolved-provider override")
		}
	}
}

// TestUpstreamProcess_FailoverAttempt1_NoPathMutationWhenAlreadyCorrect proves
// the fix doesn't set a redundant header mutation when the client-observed
// :path already matches attempt 1's resolved base path (the common case: no
// retry has happened yet).
func TestUpstreamProcess_FailoverAttempt1_NoPathMutationWhenAlreadyCorrect(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": failoverRouteConfig()})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithAttemptCountAndPath("chat-route", "failover_agg_chat_0", "1", "/openai-provider/chat/completions"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	assert.Nil(t, stream.responses[0].GetRequestHeaders().GetResponse(), "no :path correction needed when the resolved base path already matches")
}

func TestUpstreamProcess_FailoverMissingAttemptCountHeader_DefaultsToChainIndexZero(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": failoverRouteConfig()})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	// No x-envoy-attempt-count header at all — must resolve exactly like attempt 1.
	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithRouteAndCluster("chat-route", "failover_agg_chat_0"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	headers := headerMutationMap(t, stream.responses[1])
	assert.Equal(t, "https://api.openai.com/v1", headers["x-resolved-url"])
	assert.Equal(t, "gpt-4o", headers["x-resolved-model"])
}

func TestUpstreamProcess_FailoverAttemptPastChainEnd_LeavesURLEmpty(t *testing.T) {
	// More attempts than configured chain members shouldn't happen — the
	// route's RetryPolicy.NumRetries never allows it — but resolution must
	// fail closed rather than read past the slice or silently wrap around.
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": failoverRouteConfig()})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithAttemptCount("chat-route", "failover_agg_chat_0", "3"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	headers := headerMutationMap(t, stream.responses[1])
	assert.Empty(t, headers["x-resolved-url"])
	assert.Empty(t, headers["x-resolved-model"])
}

func TestUpstreamProcess_FailoverRoute_UnrelatedClusterFallsThroughToDefaultUpstream(t *testing.T) {
	// A route that HAS a failover block can still receive a plain (non-retry)
	// attempt against its own default upstream cluster (e.g. the very first
	// dispatch, before Envoy ever enters the aggregate) — the failover-target
	// scan must not shadow that existing resolution path.
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	rc := failoverRouteConfig()
	rc.Metadata.DefaultUpstream = &policyenginev1.UpstreamInfo{
		ClusterName: "plain-default-cluster",
		URL:         "https://plain-default.example.com",
		BasePath:    "/v1",
	}
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": rc})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithRouteAndCluster("chat-route", "plain-default-cluster"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
	})

	err := server.Process(stream)
	require.NoError(t, err)
	require.Len(t, stream.responses, 2)

	headers := headerMutationMap(t, stream.responses[1])
	assert.Equal(t, "https://plain-default.example.com", headers["x-resolved-url"])
	assert.Empty(t, headers["x-resolved-model"], "a non-aggregate attempt must not pick up failover identity")
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

func TestUpstreamProcess_FailoverAttemptFails5xx_SuspendsTarget(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": failoverRouteConfig()})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithAttemptCount("chat-route", "failover_agg_chat_0", "1"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
		responseHeadersReqWithStatus("500"),
	})

	err := server.Process(stream)
	require.NoError(t, err)

	assert.True(t, k.suspension.IsSuspended(suspensionKey("chat-route", "gpt-4o", "openai-provider")))
}

func TestUpstreamProcess_FailoverAttemptSucceeds_DoesNotSuspend(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": failoverRouteConfig()})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithAttemptCount("chat-route", "failover_agg_chat_0", "1"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
		responseHeadersReqWithStatus("200"),
	})

	err := server.Process(stream)
	require.NoError(t, err)

	assert.False(t, k.suspension.IsSuspended(suspensionKey("chat-route", "gpt-4o", "openai-provider")))
}

func TestUpstreamProcess_FailoverAttempt4xx_DoesNotSuspend(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": failoverRouteConfig()})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithAttemptCount("chat-route", "failover_agg_chat_0", "1"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
		responseHeadersReqWithStatus("429"),
	})

	err := server.Process(stream)
	require.NoError(t, err)

	assert.False(t, k.suspension.IsSuspended(suspensionKey("chat-route", "gpt-4o", "openai-provider")), "a client error must not suspend — only 5xx matches RetryPolicy.RetryOn")
}

func TestUpstreamProcess_FailoverSuspendDurationZero_DoesNotSuspend(t *testing.T) {
	k := NewKernel()
	chain := &registry.PolicyChain{
		Policies:                []policy.Policy{upstreamEchoStub{}},
		PolicySpecs:             []policy.PolicySpec{{Name: "upstream-echo-stub", Version: "v1", Enabled: true}},
		RequiresUpstreamRequest: true,
	}
	k.RegisterRoute("chat-route", chain)
	rc := failoverRouteConfig()
	rc.Metadata.FailoverSuspendDurationSeconds = 0
	k.ApplyWholeRouteConfigs(map[string]*RouteConfig{"chat-route": rc})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithAttemptCount("chat-route", "failover_agg_chat_0", "1"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
		responseHeadersReqWithStatus("500"),
	})

	err := server.Process(stream)
	require.NoError(t, err)

	assert.False(t, k.suspension.IsSuspended(suspensionKey("chat-route", "gpt-4o", "openai-provider")), "suspendDuration: 0 must disable suspension tracking entirely")
}

func TestUpstreamProcess_NonFailoverAttempt5xx_DoesNotSuspend(t *testing.T) {
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
					ClusterName: "plain-cluster",
					URL:         "https://plain.example.com",
				},
			},
		},
	})

	server := NewUpstreamExternalProcessorServer(k, executor.NewChainExecutor(nil, nil, noop.NewTracerProvider().Tracer("test")))

	stream := newMockStream([]*extprocv3.ProcessingRequest{
		requestHeadersReqWithRouteAndCluster("chat-route", "plain-cluster"),
		requestBodyReq([]byte(`{"model":"gpt-4o"}`)),
		responseHeadersReqWithStatus("500"),
	})

	err := server.Process(stream)
	require.NoError(t, err)

	k.suspension.mu.Lock()
	entries := len(k.suspension.until)
	k.suspension.mu.Unlock()
	assert.Zero(t, entries, "a plain non-failover attempt has no model/provider identity, so nothing should be suspended")
}

// upstreamResponseTranslatorStub mimics a shape-translating response policy
// (e.g. openai-to-anthropic-transformer's OnUpstreamResponseBody): it rewrites
// the body and sets its own header, and runs FIRST in the chain.
type upstreamResponseTranslatorStub struct{}

func (upstreamResponseTranslatorStub) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{UpstreamResponseMode: policy.BodyModeBuffer}
}

func (upstreamResponseTranslatorStub) OnUpstreamResponseBody(_ context.Context, _ *policy.UpstreamAttemptContext, _ map[string]interface{}) policy.ResponseAction {
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
	return policy.ProcessingMode{UpstreamResponseMode: policy.BodyModeBuffer}
}

func (upstreamResponseHeaderOnlyStub) OnUpstreamResponseBody(_ context.Context, _ *policy.UpstreamAttemptContext, _ map[string]interface{}) policy.ResponseAction {
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
		Policies: []policy.Policy{upstreamResponseTranslatorStub{}, upstreamResponseHeaderOnlyStub{}},
		PolicySpecs: []policy.PolicySpec{
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

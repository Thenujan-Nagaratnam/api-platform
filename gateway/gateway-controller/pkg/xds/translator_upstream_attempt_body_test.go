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

package xds

import (
	"testing"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	resource "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// attemptBodyTestDefinitions builds a policy-definition registry in the real "name|fullVersion"
// key format pkg/config uses, declaring one body-opt-in policy, one policy that explicitly
// opts out, and one inert policy declaring no x-wso2-upstream-attempt block at all.
func attemptBodyTestDefinitions() map[string]models.PolicyDefinition {
	return map[string]models.PolicyDefinition{
		"body-opt-in-policy|v1.0.0": {
			Name:            "body-opt-in-policy",
			Version:         "v1.0.0",
			UpstreamAttempt: &models.UpstreamAttemptMetadata{Body: true},
		},
		"inert-policy|v1.0.0": {
			Name:    "inert-policy",
			Version: "v1.0.0",
		},
	}
}

// TestCollectClustersNeedingUpstreamAttemptBodyFilter_DiscoversByMetadataAndRetryGate proves
// discovery requires BOTH the explicit x-wso2-upstream-attempt: {body: true} declaration AND
// route-level retry configuration (via the retryConfiguredClusters gate) — neither alone is
// enough — and that a dynamic cluster_header route is skipped for the same reason
// collectClustersNeedingUpstreamResponseObserver skips one.
func TestCollectClustersNeedingUpstreamAttemptBodyFilter_DiscoversByMetadataAndRetryGate(t *testing.T) {
	defs := attemptBodyTestDefinitions()
	latest := config.BuildLatestVersionIndex(defs)

	rdc := &models.RuntimeDeployConfig{
		Routes: map[string]*models.Route{
			"GET|/retrying-with-body|localhost":    {Upstream: models.RouteUpstream{ClusterKey: "retrying-with-body-cluster"}},
			"GET|/retrying-without-body|localhost": {Upstream: models.RouteUpstream{ClusterKey: "retrying-without-body-cluster"}},
			"GET|/not-retrying|localhost":          {Upstream: models.RouteUpstream{ClusterKey: "not-retrying-cluster"}},
			"GET|/dynamic|localhost":               {Upstream: models.RouteUpstream{UseClusterHeader: true, DefaultCluster: "dynamic-cluster"}},
		},
		PolicyChains: map[string]*models.PolicyChain{
			"GET|/retrying-with-body|localhost":    {Policies: []models.Policy{{Name: "body-opt-in-policy", Version: "v1"}}},
			"GET|/retrying-without-body|localhost": {Policies: []models.Policy{{Name: "inert-policy", Version: "v1"}}},
			"GET|/not-retrying|localhost":          {Policies: []models.Policy{{Name: "body-opt-in-policy", Version: "v1"}}},
			"GET|/dynamic|localhost":               {Policies: []models.Policy{{Name: "body-opt-in-policy", Version: "v1"}}},
		},
	}

	// Mirrors what collectClustersNeedingUpstreamFilter would have accumulated for these
	// routes: only the two clusters actually backing a retry-configured route are "retrying".
	// "not-retrying-cluster" is deliberately absent, even though its policy declares body:
	// true — the opt-in must never apply to a non-retrying route.
	retryConfiguredClusters := map[string]bool{
		"retrying-with-body-cluster":    true,
		"retrying-without-body-cluster": true,
	}

	dest := make(map[string]bool)
	collectClustersNeedingUpstreamAttemptBodyFilter(rdc, defs, latest, retryConfiguredClusters, dest)

	assert.True(t, dest["retrying-with-body-cluster"], "retry-configured cluster with a body-declaring policy must be marked")
	assert.False(t, dest["retrying-without-body-cluster"], "retry-configured cluster with no body-declaring policy must not be marked")
	assert.False(t, dest["not-retrying-cluster"], "a body-declaring policy on a non-retrying route must not mark its cluster")
	assert.False(t, dest["dynamic-cluster"], "a cluster_header route must not be marked — its cluster resolves only per-request")
	assert.Len(t, dest, 1)
}

// TestTranslateConfigs_RDCPath_SameEndpointRetryClusterGetsBodyFilterWhenPolicyDeclaresIt is an
// end-to-end proof through the real TranslateConfigs path: a PLAIN same-endpoint
// resilience.retry route (no retry-source, no aggregate cluster) whose chain includes a policy
// declaring x-wso2-upstream-attempt: {body: true} gets RequestBodyMode: BUFFERED on its
// cluster — the gap this mechanism closes.
func TestTranslateConfigs_RDCPath_SameEndpointRetryClusterGetsBodyFilterWhenPolicyDeclaresIt(t *testing.T) {
	logger := createTestLogger()
	translator := NewTranslator(logger, testRouterConfig(), nil, testConfig())
	translator.SetPolicyDefinitions(attemptBodyTestDefinitions())

	backend := &models.UpstreamCluster{Endpoints: []models.Endpoint{{Host: "same-endpoint-backend", Port: 9999}}}
	rdc := &models.RuntimeDeployConfig{
		Metadata:         models.Metadata{Kind: "RestApi"},
		UpstreamClusters: map[string]*models.UpstreamCluster{"same-endpoint": backend},
		Routes: map[string]*models.Route{
			"GET|/api/v1.0/items|localhost": {
				Method: "GET", Path: "/api/v1.0/items", OperationPath: "/items",
				Timeout:  &models.RouteTimeout{Retry: &api.Retry{StatusCodes: []int{503}}},
				Upstream: models.RouteUpstream{ClusterKey: "same-endpoint"},
			},
		},
		PolicyChains: map[string]*models.PolicyChain{
			"GET|/api/v1.0/items|localhost": {Policies: []models.Policy{{Name: "body-opt-in-policy", Version: "v1"}}},
		},
	}

	translator.SetTransformers(map[string]models.ConfigTransformer{
		"RestApi": fakeRDCTransformer{"uuid-body-opt-in-1": rdc},
	})

	configs := []*models.StoredConfig{
		{UUID: "uuid-body-opt-in-1", Kind: "RestApi", DesiredState: models.StateDeployed},
	}

	resources, err := translator.TranslateConfigs(configs, "test-correlation-id")
	require.NoError(t, err)

	var sameEndpointCluster, internalCluster *cluster.Cluster
	for _, res := range resources[resource.ClusterType] {
		c, ok := res.(*cluster.Cluster)
		require.True(t, ok)
		switch c.Name {
		case "same-endpoint":
			sameEndpointCluster = c
		case constants.UpstreamRefreshPolicyEngineClusterName:
			internalCluster = c
		}
	}
	require.NotNil(t, sameEndpointCluster, "expected the RDC-path cluster 'same-endpoint' to be present")
	require.NotNil(t, internalCluster, "expected the internal ext_proc cluster to be registered")

	protoAny, ok := sameEndpointCluster.TypedExtensionProtocolOptions[upstreamHTTPProtocolOptionsKey]
	require.True(t, ok, "cluster backing a same-endpoint retry route with a body-declaring policy must get the upstream filter attached")

	var protocolOptions httpv3.HttpProtocolOptions
	require.NoError(t, protoAny.UnmarshalTo(&protocolOptions))
	require.Len(t, protocolOptions.HttpFilters, 2, "exactly one upstream ext_proc filter plus the mandatory terminal upstream_codec filter")

	var extProcConfig extproc.ExternalProcessor
	require.NoError(t, protocolOptions.HttpFilters[0].GetTypedConfig().UnmarshalTo(&extProcConfig))
	assert.Equal(t, extproc.ProcessingMode_SEND, extProcConfig.ProcessingMode.RequestHeaderMode)
	assert.Equal(t, extproc.ProcessingMode_BUFFERED, extProcConfig.ProcessingMode.RequestBodyMode,
		"a same-endpoint retry route's cluster must get body buffering once a policy on it explicitly opts in")
}

// TestTranslateConfigs_RDCPath_SameEndpointRetryClusterStaysHeadersOnlyWithoutBodyDeclaration
// proves the opt-in is not a silent default: the identical route/retry shape as the test
// above, but with a policy chain that never declares x-wso2-upstream-attempt: {body: true},
// must keep getting only the existing headers-only refresh filter — no buffering cost paid.
func TestTranslateConfigs_RDCPath_SameEndpointRetryClusterStaysHeadersOnlyWithoutBodyDeclaration(t *testing.T) {
	logger := createTestLogger()
	translator := NewTranslator(logger, testRouterConfig(), nil, testConfig())
	translator.SetPolicyDefinitions(attemptBodyTestDefinitions())

	backend := &models.UpstreamCluster{Endpoints: []models.Endpoint{{Host: "headers-only-backend", Port: 9999}}}
	rdc := &models.RuntimeDeployConfig{
		Metadata:         models.Metadata{Kind: "RestApi"},
		UpstreamClusters: map[string]*models.UpstreamCluster{"headers-only": backend},
		Routes: map[string]*models.Route{
			"GET|/api/v1.0/items|localhost": {
				Method: "GET", Path: "/api/v1.0/items", OperationPath: "/items",
				Timeout:  &models.RouteTimeout{Retry: &api.Retry{StatusCodes: []int{503}}},
				Upstream: models.RouteUpstream{ClusterKey: "headers-only"},
			},
		},
		PolicyChains: map[string]*models.PolicyChain{
			"GET|/api/v1.0/items|localhost": {Policies: []models.Policy{{Name: "inert-policy", Version: "v1"}}},
		},
	}

	translator.SetTransformers(map[string]models.ConfigTransformer{
		"RestApi": fakeRDCTransformer{"uuid-headers-only-1": rdc},
	})

	configs := []*models.StoredConfig{
		{UUID: "uuid-headers-only-1", Kind: "RestApi", DesiredState: models.StateDeployed},
	}

	resources, err := translator.TranslateConfigs(configs, "test-correlation-id")
	require.NoError(t, err)

	var headersOnlyCluster *cluster.Cluster
	for _, res := range resources[resource.ClusterType] {
		c, ok := res.(*cluster.Cluster)
		require.True(t, ok)
		if c.Name == "headers-only" {
			headersOnlyCluster = c
		}
	}
	require.NotNil(t, headersOnlyCluster)

	protoAny, ok := headersOnlyCluster.TypedExtensionProtocolOptions[upstreamHTTPProtocolOptionsKey]
	require.True(t, ok, "cluster must still get the headers-only upstream filter from its native retry policy")

	var protocolOptions httpv3.HttpProtocolOptions
	require.NoError(t, protoAny.UnmarshalTo(&protocolOptions))

	var extProcConfig extproc.ExternalProcessor
	require.NoError(t, protocolOptions.HttpFilters[0].GetTypedConfig().UnmarshalTo(&extProcConfig))
	assert.Equal(t, extproc.ProcessingMode_SEND, extProcConfig.ProcessingMode.RequestHeaderMode)
	assert.Equal(t, extproc.ProcessingMode_NONE, extProcConfig.ProcessingMode.RequestBodyMode,
		"must stay the headers-only refresh filter (RequestBodyMode: NONE, the zero value) — no policy on this route opted into body buffering")
}

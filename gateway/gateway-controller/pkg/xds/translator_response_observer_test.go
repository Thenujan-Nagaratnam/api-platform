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

// TestCreateUpstreamResponseObserverExtProcFilter_SetsResponseHeaderModeSend proves the
// response-observer filter requests both the request-headers phase (so attempt-request
// dispatch, the mechanism createUpstreamRefreshExtProcFilter provides, keeps working
// alongside it) and the response-headers phase (so a policy implementing
// UpstreamAttemptResponseObserver can see why a specific attempt failed).
func TestCreateUpstreamResponseObserverExtProcFilter_SetsResponseHeaderModeSend(t *testing.T) {
	logger := createTestLogger()
	routerCfg := testRouterConfig()
	routerCfg.PolicyEngine = config.PolicyEngineConfig{
		Host:             "localhost",
		Port:             50051,
		TimeoutMs:        1000,
		MessageTimeoutMs: 500,
	}
	cfg := testConfig()
	cfg.Router = *routerCfg
	translator := NewTranslator(logger, routerCfg, nil, cfg)

	filter, err := translator.createUpstreamResponseObserverExtProcFilter()
	require.NoError(t, err)
	require.NotNil(t, filter)
	assert.Equal(t, constants.ExtProcFilterName+"_upstream_response_observer", filter.Name)
	assert.NotEqual(t, constants.ExtProcFilterName, filter.Name, "must not collide with the main downstream filter's name")
	assert.NotEqual(t, constants.ExtProcFilterName+"_upstream_refresh", filter.Name, "must not collide with the headers-only upstream filter's name")
	assert.NotEqual(t, constants.ExtProcFilterName+"_upstream_body", filter.Name, "must not collide with the body-buffering upstream filter's name")

	var extProcConfig extproc.ExternalProcessor
	require.NoError(t, filter.GetTypedConfig().UnmarshalTo(&extProcConfig))
	assert.True(t, extProcConfig.FailureModeAllow, "response-observer filter must fail open, unlike the downstream filter")
	assert.Equal(t, extproc.ProcessingMode_SEND, extProcConfig.ProcessingMode.RequestHeaderMode,
		"attempt-request dispatch must keep working alongside the new response phase")
	assert.Equal(t, extproc.ProcessingMode_SEND, extProcConfig.ProcessingMode.ResponseHeaderMode)
	assert.Equal(t, constants.UpstreamRefreshPolicyEngineClusterName,
		extProcConfig.GrpcService.GetEnvoyGrpc().ClusterName, "targets the same internal ext_proc cluster as the other two upstream filters")
}

// observerTestDefinitions builds a policy-definition registry in the real "name|fullVersion"
// key format pkg/config uses, declaring one response-observer policy and one inert policy.
func observerTestDefinitions() map[string]models.PolicyDefinition {
	return map[string]models.PolicyDefinition{
		"observer-policy|v1.0.0": {
			Name:                     "observer-policy",
			Version:                  "v1.0.0",
			UpstreamResponseObserver: true,
		},
		"inert-policy|v1.0.0": {
			Name:    "inert-policy",
			Version: "v1.0.0",
		},
	}
}

// TestCollectClustersNeedingUpstreamResponseObserver_DiscoversByMetadata proves discovery is
// driven purely by the declared x-wso2-upstream-response-observer metadata, never by policy
// name, and that a dynamic cluster_header route is skipped (its cluster identity resolves
// only per-request, unavailable at xDS-build time — the same exclusion
// collectClustersNeedingUpstreamFilter already applies).
func TestCollectClustersNeedingUpstreamResponseObserver_DiscoversByMetadata(t *testing.T) {
	defs := observerTestDefinitions()
	latest := config.BuildLatestVersionIndex(defs)

	rdc := &models.RuntimeDeployConfig{
		Routes: map[string]*models.Route{
			"GET|/observed|localhost": {Upstream: models.RouteUpstream{ClusterKey: "observed-cluster"}},
			"GET|/inert|localhost":    {Upstream: models.RouteUpstream{ClusterKey: "inert-cluster"}},
			"GET|/dynamic|localhost":  {Upstream: models.RouteUpstream{UseClusterHeader: true, DefaultCluster: "dynamic-cluster"}},
		},
		PolicyChains: map[string]*models.PolicyChain{
			"GET|/observed|localhost": {Policies: []models.Policy{{Name: "observer-policy", Version: "v1"}}},
			"GET|/inert|localhost":    {Policies: []models.Policy{{Name: "inert-policy", Version: "v1"}}},
			"GET|/dynamic|localhost":  {Policies: []models.Policy{{Name: "observer-policy", Version: "v1"}}},
		},
	}

	dest := make(map[string]bool)
	collectClustersNeedingUpstreamResponseObserver(rdc, defs, latest, dest)

	assert.True(t, dest["observed-cluster"], "expected the cluster behind the observer-declaring policy to be marked")
	assert.False(t, dest["inert-cluster"], "a policy not declaring the flag must not mark its cluster")
	assert.False(t, dest["dynamic-cluster"], "a cluster_header route must not be marked — its cluster resolves only per-request")
	assert.Len(t, dest, 1)
}

// TestTranslateConfigs_RDCPath_ClusterGetsResponseObserverFilterWhenPolicyDeclaresIt is an
// end-to-end proof through the real TranslateConfigs path: a route whose chain includes a
// policy declaring x-wso2-upstream-response-observer: true gets its cluster's
// TypedExtensionProtocolOptions populated, and the internal ext_proc cluster this filter
// targets is registered. No policy in this plan sets the flag yet, so this exercises the
// wiring in isolation via a synthetic policy-definition registry.
func TestTranslateConfigs_RDCPath_ClusterGetsResponseObserverFilterWhenPolicyDeclaresIt(t *testing.T) {
	logger := createTestLogger()
	translator := NewTranslator(logger, testRouterConfig(), nil, testConfig())
	translator.SetPolicyDefinitions(observerTestDefinitions())

	backend := &models.UpstreamCluster{Endpoints: []models.Endpoint{{Host: "observed-backend", Port: 9999}}}
	rdc := &models.RuntimeDeployConfig{
		Metadata:         models.Metadata{Kind: "RestApi"},
		UpstreamClusters: map[string]*models.UpstreamCluster{"observed": backend},
		Routes: map[string]*models.Route{
			"GET|/api/v1.0/items|localhost": {
				Method: "GET", Path: "/api/v1.0/items", OperationPath: "/items",
				Upstream: models.RouteUpstream{ClusterKey: "observed"},
			},
		},
		PolicyChains: map[string]*models.PolicyChain{
			"GET|/api/v1.0/items|localhost": {Policies: []models.Policy{{Name: "observer-policy", Version: "v1"}}},
		},
	}

	translator.SetTransformers(map[string]models.ConfigTransformer{
		"RestApi": fakeRDCTransformer{"uuid-observer-1": rdc},
	})

	configs := []*models.StoredConfig{
		{UUID: "uuid-observer-1", Kind: "RestApi", DesiredState: models.StateDeployed},
	}

	resources, err := translator.TranslateConfigs(configs, "test-correlation-id")
	require.NoError(t, err)

	var observedCluster, internalCluster *cluster.Cluster
	for _, res := range resources[resource.ClusterType] {
		c, ok := res.(*cluster.Cluster)
		require.True(t, ok)
		switch c.Name {
		case "observed":
			observedCluster = c
		case constants.UpstreamRefreshPolicyEngineClusterName:
			internalCluster = c
		}
	}
	require.NotNil(t, observedCluster, "expected the RDC-path cluster 'observed' to be present")
	require.NotNil(t, internalCluster, "expected the internal ext_proc cluster to be registered")

	_, ok := observedCluster.TypedExtensionProtocolOptions[upstreamHTTPProtocolOptionsKey]
	assert.True(t, ok, "cluster backing a route with a response-observer-declaring policy must get the upstream filter attached")
}

// TestTranslateConfigs_RDCPath_ClusterInBothRetryAndResponseObserverSetsKeepsRetryFilterOnly
// proves the exclusivity guard in TranslateConfigs: a cluster whose route both carries a
// native resilience.retry RetryPolicy (landing it in clustersNeedingUpstreamFilter, the
// headers-only set) AND has a policy declaring x-wso2-upstream-response-observer on its chain
// (which would otherwise land it in clustersNeedingUpstreamResponseObserver too) must end up
// with exactly the pre-existing headers-only filter attached — never a second filter, and
// never silently overwritten by the response-observer filter's ProcessingMode.
func TestTranslateConfigs_RDCPath_ClusterInBothRetryAndResponseObserverSetsKeepsRetryFilterOnly(t *testing.T) {
	logger := createTestLogger()
	translator := NewTranslator(logger, testRouterConfig(), nil, testConfig())
	translator.SetPolicyDefinitions(observerTestDefinitions())

	backend := &models.UpstreamCluster{Endpoints: []models.Endpoint{{Host: "shared-backend", Port: 9999}}}
	rdc := &models.RuntimeDeployConfig{
		Metadata:         models.Metadata{Kind: "RestApi"},
		UpstreamClusters: map[string]*models.UpstreamCluster{"shared": backend},
		Routes: map[string]*models.Route{
			"GET|/api/v1.0/items|localhost": {
				Method: "GET", Path: "/api/v1.0/items", OperationPath: "/items",
				Timeout:  &models.RouteTimeout{Retry: &api.Retry{StatusCodes: []int{503}}},
				Upstream: models.RouteUpstream{ClusterKey: "shared"},
			},
		},
		PolicyChains: map[string]*models.PolicyChain{
			// This chain has nothing to do with retry — it's the response-observer
			// policy alone, attached to a route that ALSO happens to carry a plain
			// resilience.retry (set via Timeout.Retry above). The two mechanisms are
			// independent; this is exactly the "same cluster, both sets" collision
			// the exclusivity guard exists to handle.
			"GET|/api/v1.0/items|localhost": {Policies: []models.Policy{{Name: "observer-policy", Version: "v1"}}},
		},
	}

	translator.SetTransformers(map[string]models.ConfigTransformer{
		"RestApi": fakeRDCTransformer{"uuid-exclusivity-1": rdc},
	})

	configs := []*models.StoredConfig{
		{UUID: "uuid-exclusivity-1", Kind: "RestApi", DesiredState: models.StateDeployed},
	}

	resources, err := translator.TranslateConfigs(configs, "test-correlation-id")
	require.NoError(t, err)

	var sharedCluster *cluster.Cluster
	for _, res := range resources[resource.ClusterType] {
		c, ok := res.(*cluster.Cluster)
		require.True(t, ok)
		if c.Name == "shared" {
			sharedCluster = c
		}
	}
	require.NotNil(t, sharedCluster)

	protoAny, ok := sharedCluster.TypedExtensionProtocolOptions[upstreamHTTPProtocolOptionsKey]
	require.True(t, ok, "cluster must still get the headers-only upstream filter from its native retry policy")

	var protocolOptions httpv3.HttpProtocolOptions
	require.NoError(t, protoAny.UnmarshalTo(&protocolOptions))
	require.Len(t, protocolOptions.HttpFilters, 2, "exactly one upstream ext_proc filter plus the mandatory terminal upstream_codec filter — never two ext_proc filters")

	upstreamFilter := protocolOptions.HttpFilters[0]
	assert.Equal(t, constants.ExtProcFilterName+"_upstream_refresh", upstreamFilter.Name,
		"a cluster already needing the headers-only retry filter must keep it, not get silently overwritten by the response-observer filter")

	var extProcConfig extproc.ExternalProcessor
	require.NoError(t, upstreamFilter.GetTypedConfig().UnmarshalTo(&extProcConfig))
	assert.Equal(t, extproc.ProcessingMode_SEND, extProcConfig.ProcessingMode.RequestHeaderMode)
	assert.Equal(t, extproc.ProcessingMode_DEFAULT, extProcConfig.ProcessingMode.ResponseHeaderMode,
		"must be the headers-only refresh filter's ProcessingMode (ResponseHeaderMode: DEFAULT) — the response-observer filter (ResponseHeaderMode: SEND) must not have been attached")
}

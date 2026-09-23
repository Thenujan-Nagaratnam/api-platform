/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 */

package xds

import (
	"testing"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	compositev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/composite/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

func TestBuildFailoverCompositeClusters_IsolatesEveryMember(t *testing.T) {
	rf := &models.RouteFailover{
		SuspendDurationSeconds: 5,
		RetriableStatusCodes:   []int{429, 503},
		Targets: []models.RouteFailoverTarget{{
			Model:  "gpt-4o",
			Target: models.RouteFailoverEntry{ClusterKey: "provider-a"},
			Fallbacks: []models.RouteFailoverEntry{
				{ClusterKey: "provider-a"}, // same physical provider, distinct model/health unit
				{ClusterKey: "provider-b"},
			},
		}},
	}
	sources := map[string]*cluster.Cluster{
		"provider-a": testFailoverSourceCluster(t, "provider-a", "a.example", 443),
		"provider-b": testFailoverSourceCluster(t, "provider-b", "b.example", 443),
	}

	clusters, err := buildFailoverCompositeClusters(rf, "POST|/chat/completions|main", sources)
	require.NoError(t, err)
	require.Len(t, clusters, 6, "three isolated leaves, one full-chain composite, and two suffix composites (one per fallback position)")

	for i := 0; i < 3; i++ {
		leaf := clusters[i]
		assert.Equal(t, FailoverLeafClusterName("POST|/chat/completions|main", 0, i), leaf.Name)
		// Leaves carry no outlier_detection/CommonLbConfig panic override: live
		// verification against a real Envoy showed ejecting a leaf breaks
		// failover instead of helping it — see
		// configureFailoverLeafCircuitBreaker's doc comment. Cross-request
		// suspension is policy-side again (model-failover's isSuspended).
		assert.Nil(t, leaf.OutlierDetection)
		assert.Nil(t, leaf.CommonLbConfig)
		if protocolAny, ok := leaf.TypedExtensionProtocolOptions[constants.HttpProtocolOptionsTypedConfigKey]; ok {
			var opts httpv3.HttpProtocolOptions
			require.NoError(t, protocolAny.UnmarshalTo(&opts))
			assert.Nil(t, opts.GetOutlierDetection(), "RetriableStatusCodes no longer drives a leaf-level outlier error matcher")
		}
	}
	assert.NotEqual(t, clusters[0].Name, clusters[1].Name, "same physical provider members still need isolated health")

	composite := clusters[3]
	assert.Equal(t, AggregateClusterName("POST|/chat/completions|main", 0), composite.Name)
	var cfg compositev3.ClusterConfig
	require.NoError(t, composite.GetClusterType().GetTypedConfig().UnmarshalTo(&cfg))
	require.Len(t, cfg.Clusters, 3)
	for i, entry := range cfg.Clusters {
		assert.Equal(t, clusters[i].Name, entry.Name)
	}

	// Suffix composites: one per fallback position, each covering that
	// position through the end of the chain — the downstream policy's
	// suspended-primary bypass dispatches at these instead of a bare leaf so
	// a failure of the bypassed-to member can still retry within the request
	// (see SuffixCompositeClusterName's doc comment).
	suffixFrom1 := clusters[4]
	assert.Equal(t, SuffixCompositeClusterName("POST|/chat/completions|main", 0, 1), suffixFrom1.Name)
	var suffixFrom1Cfg compositev3.ClusterConfig
	require.NoError(t, suffixFrom1.GetClusterType().GetTypedConfig().UnmarshalTo(&suffixFrom1Cfg))
	require.Len(t, suffixFrom1Cfg.Clusters, 2, "covers positions 1 and 2")
	assert.Equal(t, clusters[1].Name, suffixFrom1Cfg.Clusters[0].Name)
	assert.Equal(t, clusters[2].Name, suffixFrom1Cfg.Clusters[1].Name)

	suffixFrom2 := clusters[5]
	assert.Equal(t, SuffixCompositeClusterName("POST|/chat/completions|main", 0, 2), suffixFrom2.Name)
	var suffixFrom2Cfg compositev3.ClusterConfig
	require.NoError(t, suffixFrom2.GetClusterType().GetTypedConfig().UnmarshalTo(&suffixFrom2Cfg))
	require.Len(t, suffixFrom2Cfg.Clusters, 1, "covers only position 2")
	assert.Equal(t, clusters[2].Name, suffixFrom2Cfg.Clusters[0].Name)
}

// TestBuildFailoverCompositeClusters_LeavesRaiseEnvoysRetryCeiling pins
// design §9.3: every leaf lifts Envoy's default max_retries (3) to
// failoverLeafMaxRetries, so an outage fails over every in-flight request
// rather than just the first three.
func TestBuildFailoverCompositeClusters_LeavesRaiseEnvoysRetryCeiling(t *testing.T) {
	rf := &models.RouteFailover{
		Targets: []models.RouteFailoverTarget{{
			Model:     "gpt-4o",
			Target:    models.RouteFailoverEntry{ClusterKey: "provider-a"},
			Fallbacks: []models.RouteFailoverEntry{{ClusterKey: "provider-b"}},
		}},
	}
	sources := map[string]*cluster.Cluster{
		"provider-a": testFailoverSourceCluster(t, "provider-a", "a.example", 443),
		"provider-b": testFailoverSourceCluster(t, "provider-b", "b.example", 443),
	}

	clusters, err := buildFailoverCompositeClusters(rf, "POST|/chat/completions|main", sources)
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		leaf := clusters[i]
		require.NotNil(t, leaf.CircuitBreakers, "leaf %q must set its own retry ceiling", leaf.Name)
		require.Len(t, leaf.CircuitBreakers.Thresholds, 1)
		assert.Equal(t, uint32(failoverLeafMaxRetries), leaf.CircuitBreakers.Thresholds[0].GetMaxRetries().GetValue())
	}
}

func TestBuildFailoverCompositeClusters_MissingSourceFailsClosed(t *testing.T) {
	rf := &models.RouteFailover{Targets: []models.RouteFailoverTarget{{
		Target: models.RouteFailoverEntry{ClusterKey: "missing"},
	}}}
	_, err := buildFailoverCompositeClusters(rf, "route", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source cluster")
}

func TestFailoverClusterNamesAreStableAndPositionSpecific(t *testing.T) {
	routeKey := "POST|/chat/completions|main"
	assert.Equal(t, AggregateClusterName(routeKey, 0), AggregateClusterName(routeKey, 0))
	assert.NotEqual(t, AggregateClusterName(routeKey, 0), AggregateClusterName(routeKey, 1))
	assert.NotEqual(t, FailoverLeafClusterName(routeKey, 0, 0), FailoverLeafClusterName(routeKey, 0, 1))
}

func testFailoverSourceCluster(t *testing.T, name, host string, port uint32) *cluster.Cluster {
	t.Helper()
	c := &cluster.Cluster{
		Name: name,
		LoadAssignment: &endpoint.ClusterLoadAssignment{
			ClusterName: name,
			Endpoints: []*endpoint.LocalityLbEndpoints{{LbEndpoints: []*endpoint.LbEndpoint{{
				HostIdentifier: &endpoint.LbEndpoint_Endpoint{Endpoint: &endpoint.Endpoint{Address: &core.Address{
					Address: &core.Address_SocketAddress{SocketAddress: &core.SocketAddress{
						Address:       host,
						PortSpecifier: &core.SocketAddress_PortValue{PortValue: port},
					}},
				}}},
			}}}},
		},
	}
	require.NoError(t, attachUpstreamPolicyFilter(c, constants.UpstreamPolicyEngineClusterName))
	return c
}

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
	"time"

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
		SuspendDurationSeconds:    5,
		MaxSuspendDurationSeconds: 40,
		RetriableStatusCodes:      []int{429, 503},
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
	require.Len(t, clusters, 4, "three isolated leaves plus one composite")

	for i := 0; i < 3; i++ {
		leaf := clusters[i]
		assert.Equal(t, FailoverLeafClusterName("POST|/chat/completions|main", 0, i), leaf.Name)
		require.NotNil(t, leaf.OutlierDetection)
		assert.Equal(t, uint32(1), leaf.OutlierDetection.GetConsecutive_5Xx().GetValue())
		assert.Equal(t, uint32(100), leaf.OutlierDetection.GetMaxEjectionPercent().GetValue())
		assert.True(t, leaf.OutlierDetection.GetAlwaysEjectOneHost().GetValue())
		assert.Equal(t, 5*time.Second, leaf.OutlierDetection.GetBaseEjectionTime().AsDuration())
		assert.Equal(t, 40*time.Second, leaf.OutlierDetection.GetMaxEjectionTime().AsDuration())
		assert.Equal(t, float64(0), leaf.GetCommonLbConfig().GetHealthyPanicThreshold().GetValue())

		var opts httpv3.HttpProtocolOptions
		require.NoError(t, leaf.TypedExtensionProtocolOptions[constants.HttpProtocolOptionsTypedConfigKey].UnmarshalTo(&opts))
		require.NotNil(t, opts.GetOutlierDetection().GetErrorMatcher(), "configured statuses must drive passive health")
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
}

// TestBuildFailoverCompositeClusters_MaxConcurrentRetriesSetsLeafCircuitBreaker
// pins design §9.3's retry resource protection: each leaf cluster's own
// concurrent-retry ceiling, not a route-level mechanism, since retries fan
// out across whichever leaf the composite cluster selects next.
func TestBuildFailoverCompositeClusters_MaxConcurrentRetriesSetsLeafCircuitBreaker(t *testing.T) {
	rf := &models.RouteFailover{
		MaxConcurrentRetries: 10,
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
		require.NotNil(t, leaf.CircuitBreakers, "leaf %q must bound concurrent retries", leaf.Name)
		require.Len(t, leaf.CircuitBreakers.Thresholds, 1)
		require.NotNil(t, leaf.CircuitBreakers.Thresholds[0].MaxRetries)
		assert.Equal(t, uint32(10), leaf.CircuitBreakers.Thresholds[0].MaxRetries.GetValue())
	}
}

// Unconfigured (0) must leave CircuitBreakers nil so Envoy's own default
// (max_retries: 3) applies exactly as it would with no failover involved —
// never an explicit 0, which would mean "no retries allowed at all".
func TestBuildFailoverCompositeClusters_MaxConcurrentRetriesUnsetLeavesCircuitBreakersNil(t *testing.T) {
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
		assert.Nil(t, clusters[i].CircuitBreakers)
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

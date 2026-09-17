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
	aggregatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/aggregate/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	policyenginev1 "github.com/wso2/api-platform/sdk/core/policyengine"
)

func TestBuildFailoverAggregateClusters_OneAggregatePerTarget(t *testing.T) {
	rf := &models.RouteFailover{
		Targets: []models.RouteFailoverTarget{
			{
				Model:  "gpt-4o",
				Target: models.RouteFailoverEntry{ClusterKey: "upstream_main_openai_443", Upstream: policyenginev1.UpstreamInfo{ClusterName: "upstream_main_openai_443"}},
				Fallbacks: []models.RouteFailoverEntry{
					{ClusterKey: "upstream_anthropic_443", Upstream: policyenginev1.UpstreamInfo{ClusterName: "upstream_anthropic_443"}},
				},
			},
			{
				Model:  "gpt-4o-mini",
				Target: models.RouteFailoverEntry{ClusterKey: "upstream_main_openai_443", Upstream: policyenginev1.UpstreamInfo{ClusterName: "upstream_main_openai_443"}},
				Fallbacks: []models.RouteFailoverEntry{
					{ClusterKey: "upstream_anthropic_haiku_443", Upstream: policyenginev1.UpstreamInfo{ClusterName: "upstream_anthropic_haiku_443"}},
				},
			},
		},
	}

	clusters, err := buildFailoverAggregateClusters(rf, "POST|/chat/completions|main")
	require.NoError(t, err)
	require.Len(t, clusters, 2, "one aggregate cluster per targets[] entry")

	names := map[string]bool{}
	for i, c := range clusters {
		names[c.Name] = true
		assert.Equal(t, AggregateClusterName("POST|/chat/completions|main", i), c.Name)

		anyOpts, ok := c.TypedExtensionProtocolOptions[constants.HttpProtocolOptionsTypedConfigKey]
		require.True(t, ok, "upstream ext_proc filter must be attached to the aggregate cluster itself")
		var opts httpv3.HttpProtocolOptions
		require.NoError(t, anyOpts.UnmarshalTo(&opts))
		assert.NoError(t, opts.ValidateAll(), "must pass Envoy's own proto validation, same class of bug this session already found once")
		require.Len(t, opts.HttpFilters, 2)
		assert.Equal(t, constants.UpstreamExtProcFilterName, opts.HttpFilters[0].Name)
		assert.Equal(t, constants.UpstreamCodecFilterName, opts.HttpFilters[1].Name)
	}
	assert.Len(t, names, 2, "aggregate cluster names must be unique per target entry")
}

func TestBuildFailoverAggregateClusters_MemberOrderMatchesPriority(t *testing.T) {
	rf := &models.RouteFailover{
		Targets: []models.RouteFailoverTarget{{
			Model:  "gpt-4o",
			Target: models.RouteFailoverEntry{ClusterKey: "primary-cluster"},
			Fallbacks: []models.RouteFailoverEntry{
				{ClusterKey: "fallback-1"},
				{ClusterKey: "fallback-2"},
			},
		}},
	}

	clusters, err := buildFailoverAggregateClusters(rf, "route-key")
	require.NoError(t, err)
	require.Len(t, clusters, 1)

	var aggCfg aggregateConfigForTest
	require.NoError(t, unmarshalAggregateConfig(t, clusters[0], &aggCfg))
	assert.Equal(t, []string{"primary-cluster", "fallback-1", "fallback-2"}, aggCfg.Clusters)
}

type aggregateConfigForTest struct {
	Clusters []string
}

func unmarshalAggregateConfig(t *testing.T, c *cluster.Cluster, out *aggregateConfigForTest) error {
	t.Helper()
	ct, ok := c.ClusterDiscoveryType.(*cluster.Cluster_ClusterType)
	require.True(t, ok, "expected a custom cluster type (aggregate)")
	var agg aggregatev3.ClusterConfig
	if err := ct.ClusterType.TypedConfig.UnmarshalTo(&agg); err != nil {
		return err
	}
	out.Clusters = agg.Clusters
	return nil
}

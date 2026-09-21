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
	"fmt"
	"strings"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	aggregatev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/aggregate/v3"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// AggregateClusterName deterministically names the aggregate cluster for the
// targetIndex'th entry of routeKey's failover block. Stable across redeploys
// (routeKey + index never change for the same declared target unless the
// operator reorders the model-failover policy's targets, which is an intentional
// config change, not a spurious xDS re-version). Exported: pkg/policyxds
// needs this same name to know what to put on the xDS-metadata wire.
func AggregateClusterName(routeKey string, targetIndex int) string {
	return fmt.Sprintf("failover_agg_%s_%d", sanitizeClusterNameComponent(routeKey), targetIndex)
}

// sanitizeClusterNameComponent strips characters Envoy cluster names can't
// safely contain (a route key looks like "POST|/chat/completions|main").
func sanitizeClusterNameComponent(s string) string {
	replacer := strings.NewReplacer("|", "_", "/", "_", "*", "_", " ", "_")
	return replacer.Replace(s)
}

// buildFailoverAggregateClusters builds one envoy.clusters.aggregate cluster
// per rf.Targets entry, members in priority order (target, then fallbacks in
// order — aggregate cluster priority is assigned by list position). The
// upstream ext_proc filter is attached to the AGGREGATE cluster itself, not
// its members — confirmed live this session that attaching to the real
// members never fires when reached through an aggregate (see the corrected
// doc comment on attachUpstreamPolicyFilter in upstream_policy_filter.go).
func buildFailoverAggregateClusters(rf *models.RouteFailover, routeKey string) ([]*cluster.Cluster, error) {
	clusters := make([]*cluster.Cluster, 0, len(rf.Targets))
	for i, target := range rf.Targets {
		memberNames := make([]string, 0, len(target.Fallbacks)+1)
		memberNames = append(memberNames, target.Target.ClusterKey)
		for _, fb := range target.Fallbacks {
			memberNames = append(memberNames, fb.ClusterKey)
		}

		aggConfigAny, err := anypb.New(&aggregatev3.ClusterConfig{Clusters: memberNames})
		if err != nil {
			return nil, fmt.Errorf("failed to marshal aggregate cluster config for %q target %d: %w", routeKey, i, err)
		}

		aggCluster := &cluster.Cluster{
			Name:     AggregateClusterName(routeKey, i),
			LbPolicy: cluster.Cluster_CLUSTER_PROVIDED,
			ClusterDiscoveryType: &cluster.Cluster_ClusterType{
				ClusterType: &cluster.Cluster_CustomClusterType{
					Name:        "envoy.clusters.aggregate",
					TypedConfig: aggConfigAny,
				},
			},
		}
		if err := attachUpstreamPolicyFilter(aggCluster, constants.UpstreamPolicyEngineClusterName); err != nil {
			return nil, fmt.Errorf("failed to attach upstream policy filter to aggregate cluster %q: %w", aggCluster.Name, err)
		}
		clusters = append(clusters, aggCluster)
	}
	return clusters, nil
}

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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	compositev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/composite/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// AggregateClusterName is retained as the wire-contract helper used by the
// policy params, but now names an envoy.clusters.composite cluster.
func AggregateClusterName(routeKey string, targetIndex int) string {
	return fmt.Sprintf("failover_composite_%s_%d", shortStableID(routeKey), targetIndex)
}

// FailoverLeafClusterName names the independently healthy/ejectable cluster
// for one member of a failover chain. Position 0 is the primary.
func FailoverLeafClusterName(routeKey string, targetIndex, memberIndex int) string {
	identity := fmt.Sprintf("%s|%d|%d", routeKey, targetIndex, memberIndex)
	return "failover_leaf_" + shortStableID(identity)
}

// SuffixCompositeClusterName names a composite cluster covering members
// [fromMemberIndex, end) of a failover chain — a "chain starting partway
// through" cluster the downstream policy dispatches to when it has already
// determined every member before fromMemberIndex is currently suspended
// (see model-failover's OnRequestBody). fromMemberIndex must be >= 1: the
// fromMemberIndex==0 suffix is just the chain's own AggregateClusterName,
// which already exists and needs no separate cluster.
func SuffixCompositeClusterName(routeKey string, targetIndex, fromMemberIndex int) string {
	identity := fmt.Sprintf("%s|%d|suffix|%d", routeKey, targetIndex, fromMemberIndex)
	return "failover_suffix_" + shortStableID(identity)
}

func shortStableID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

// buildFailoverCompositeClusters creates one retry-aware composite cluster
// per targets[] entry and a dedicated leaf cluster per member.
func buildFailoverCompositeClusters(rf *models.RouteFailover, routeKey string, sourceClusters map[string]*cluster.Cluster) ([]*cluster.Cluster, error) {
	var out []*cluster.Cluster
	for targetIndex, target := range rf.Targets {
		members := make([]models.RouteFailoverEntry, 0, len(target.Fallbacks)+1)
		members = append(members, target.Target)
		members = append(members, target.Fallbacks...)

		compositeEntries := make([]*compositev3.ClusterConfig_ClusterEntry, 0, len(members))
		for memberIndex, member := range members {
			source, ok := sourceClusters[member.ClusterKey]
			if !ok {
				return nil, fmt.Errorf("failover member source cluster %q not found", member.ClusterKey)
			}
			leaf, ok := proto.Clone(source).(*cluster.Cluster)
			if !ok {
				return nil, fmt.Errorf("failed to clone failover member source cluster %q", member.ClusterKey)
			}
			leafName := FailoverLeafClusterName(routeKey, targetIndex, memberIndex)
			leaf.Name = leafName
			if leaf.LoadAssignment != nil {
				leaf.LoadAssignment.ClusterName = leafName
				for _, locality := range leaf.LoadAssignment.Endpoints {
					for _, lbEp := range locality.LbEndpoints {
						lbEp.Metadata = applyMemberClusterIdentityMetadata(lbEp.Metadata, leafName)
					}
				}
			}
			configureFailoverLeafCircuitBreaker(leaf)
			out = append(out, leaf)
			compositeEntries = append(compositeEntries, &compositev3.ClusterConfig_ClusterEntry{Name: leafName})
		}

		composite, err := buildCompositeCluster(AggregateClusterName(routeKey, targetIndex), compositeEntries)
		if err != nil {
			return nil, fmt.Errorf("build composite cluster for %q target %d: %w", routeKey, targetIndex, err)
		}
		out = append(out, composite)

		// One suffix composite per non-primary position, each covering
		// [memberIndex, end) — see SuffixCompositeClusterName's doc comment.
		// Cheap: every entry is just a reference to an already-built leaf
		// cluster name, not a duplicated cluster definition, and the count is
		// linear in chain length, not combinatorial in suspension state.
		for memberIndex := 1; memberIndex < len(members); memberIndex++ {
			suffix, err := buildCompositeCluster(SuffixCompositeClusterName(routeKey, targetIndex, memberIndex), compositeEntries[memberIndex:])
			if err != nil {
				return nil, fmt.Errorf("build suffix composite cluster for %q target %d from %d: %w", routeKey, targetIndex, memberIndex, err)
			}
			out = append(out, suffix)
		}
	}
	return out, nil
}

// buildCompositeCluster wraps clusterEntries (leaf cluster name references,
// in order) into one envoy.clusters.composite cluster, with the upstream
// policy filter attached exactly as every failover composite needs (per
// composite entry point, not per leaf — see attachUpstreamPolicyFilter's own
// call sites for why this must be on every composite, full-chain or suffix).
func buildCompositeCluster(name string, clusterEntries []*compositev3.ClusterConfig_ClusterEntry) (*cluster.Cluster, error) {
	compositeAny, err := anypb.New(&compositev3.ClusterConfig{Clusters: clusterEntries})
	if err != nil {
		return nil, fmt.Errorf("marshal composite cluster config: %w", err)
	}
	composite := &cluster.Cluster{
		Name:     name,
		LbPolicy: cluster.Cluster_CLUSTER_PROVIDED,
		ClusterDiscoveryType: &cluster.Cluster_ClusterType{ClusterType: &cluster.Cluster_CustomClusterType{
			Name:        "envoy.clusters.composite",
			TypedConfig: compositeAny,
		}},
	}
	if err := attachUpstreamPolicyFilter(composite, constants.UpstreamPolicyEngineClusterName); err != nil {
		return nil, fmt.Errorf("attach upstream policy filter to composite cluster %q: %w", name, err)
	}
	return composite, nil
}

// configureFailoverLeafCircuitBreaker bounds concurrent retry traffic PER
// LEAF (design §9.3), since a retry storm fans out across whichever leaf the
// composite cluster selects next, not the composite cluster itself
// (envoy.clusters.aggregate/composite cluster types have no circuit breaker
// config of their own). Envoy's own default (max_retries: 3) would let only
// three requests fail over at once — during a real outage every in-flight
// request retries, so the rest would surface the primary's error. The ceiling
// is failoverLeafMaxRetries instead: the same 1024 Envoy already uses as its
// default max_requests, since each retry is itself a request to that leaf.
//
// Leaves deliberately carry no outlier_detection. An earlier version of this
// design ejected a leaf's single host after enough consecutive failures, but
// live verification (gateway/dev-policies/run-outlier-threshold-proof.sh)
// against a real Envoy showed this is actively harmful: envoy.clusters.
// composite selects purely by retry-attempt count regardless of host health
// (confirmed via Envoy's own doc comment on ClusterConfig — "unlike the
// standard aggregate cluster which uses health-based selection, the
// composite cluster uses the retry attempt count to deterministically select
// which sub-cluster to route to"), so ejecting a leaf never helps a retry
// skip it. Worse, when attempt 1 (always cluster[0]) lands on an
// already-ejected leaf, cluster/host selection fails before any upstream
// request is dispatched ("no healthy upstream", response flag UH) — and no
// retry_on value can retry that: reset-before-request only covers a host
// that was picked and then reset before its request was sent, and Envoy has
// no supported way to retry when zero hosts were available to pick from at
// all (see https://github.com/envoyproxy/envoy/issues/11307, closed
// unresolved — a maintainer explains the load balancer's host set is fixed
// for the life of the request's routing context). So an ejected primary
// doesn't get skipped; it takes down every request for the whole ejection
// window instead. Cross-request suspension of a known-bad primary is
// policy-side again instead (model-failover's isSuspended/recordOutcome,
// see OnRequestBody), which enters the chain at a healthy fallback's suffix
// composite rather than relying on Envoy to skip an ejected member.
func configureFailoverLeafCircuitBreaker(c *cluster.Cluster) {
	c.CircuitBreakers = &cluster.CircuitBreakers{
		Thresholds: []*cluster.CircuitBreakers_Thresholds{{
			MaxRetries: wrapperspb.UInt32(failoverLeafMaxRetries),
		}},
	}
}

const failoverLeafMaxRetries = 1024

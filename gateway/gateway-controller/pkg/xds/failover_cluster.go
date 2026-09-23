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
	"strconv"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	matcher "github.com/envoyproxy/go-control-plane/envoy/config/common/matcher/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	compositev3 "github.com/envoyproxy/go-control-plane/envoy/extensions/clusters/composite/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
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
			if err := configureFailoverOutlierDetection(leaf, rf); err != nil {
				return nil, fmt.Errorf("configure failover leaf %q: %w", leafName, err)
			}
			out = append(out, leaf)
			compositeEntries = append(compositeEntries, &compositev3.ClusterConfig_ClusterEntry{Name: leafName})
		}

		compositeAny, err := anypb.New(&compositev3.ClusterConfig{Clusters: compositeEntries})
		if err != nil {
			return nil, fmt.Errorf("marshal composite cluster config for %q target %d: %w", routeKey, targetIndex, err)
		}
		composite := &cluster.Cluster{
			Name:     AggregateClusterName(routeKey, targetIndex),
			LbPolicy: cluster.Cluster_CLUSTER_PROVIDED,
			ClusterDiscoveryType: &cluster.Cluster_ClusterType{ClusterType: &cluster.Cluster_CustomClusterType{
				Name:        "envoy.clusters.composite",
				TypedConfig: compositeAny,
			}},
		}
		if err := attachUpstreamPolicyFilter(composite, constants.UpstreamPolicyEngineClusterName); err != nil {
			return nil, fmt.Errorf("attach upstream policy filter to composite cluster %q: %w", composite.Name, err)
		}
		out = append(out, composite)
	}
	return out, nil
}

func configureFailoverOutlierDetection(c *cluster.Cluster, rf *models.RouteFailover) error {
	base := time.Duration(rf.SuspendDurationSeconds) * time.Second
	if base <= 0 {
		base = 5 * time.Second
	}
	max := time.Duration(rf.MaxSuspendDurationSeconds) * time.Second
	if max <= 0 {
		max = base * 8
	}
	c.OutlierDetection = &cluster.OutlierDetection{
		Consecutive_5Xx:                        wrapperspb.UInt32(1),
		EnforcingConsecutive_5Xx:               wrapperspb.UInt32(100),
		SplitExternalLocalOriginErrors:         true,
		ConsecutiveLocalOriginFailure:          wrapperspb.UInt32(1),
		EnforcingConsecutiveLocalOriginFailure: wrapperspb.UInt32(100),
		BaseEjectionTime:                       durationpb.New(base),
		MaxEjectionTime:                        durationpb.New(max),
		MaxEjectionPercent:                     wrapperspb.UInt32(100),
		AlwaysEjectOneHost:                     wrapperspb.Bool(true),
	}
	c.CommonLbConfig = &cluster.Cluster_CommonLbConfig{
		HealthyPanicThreshold: &typev3.Percent{Value: 0},
	}

	// design §9.3 retry resource protection: bound concurrent retry traffic
	// PER LEAF, since a retry storm fans out across whichever leaf the
	// composite cluster selects next, not the composite cluster itself
	// (envoy.clusters.aggregate/composite cluster types have no circuit
	// breaker config of their own). 0/unset leaves CircuitBreakers nil so
	// Envoy's own default (max_retries: 3) applies — an explicit 0 would mean
	// "no retries allowed", which is not what an omitted config means.
	if rf.MaxConcurrentRetries > 0 {
		c.CircuitBreakers = &cluster.CircuitBreakers{
			Thresholds: []*cluster.CircuitBreakers_Thresholds{{
				MaxRetries: wrapperspb.UInt32(uint32(rf.MaxConcurrentRetries)),
			}},
		}
	}

	if len(rf.RetriableStatusCodes) == 0 {
		return nil
	}
	protocolAny, ok := c.TypedExtensionProtocolOptions[constants.HttpProtocolOptionsTypedConfigKey]
	var opts httpv3.HttpProtocolOptions
	if ok {
		if err := protocolAny.UnmarshalTo(&opts); err != nil {
			return fmt.Errorf("unmarshal HTTP protocol options: %w", err)
		}
	} else {
		opts.UpstreamProtocolOptions = &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{
			ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
				ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_HttpProtocolOptions{},
			},
		}
	}
	opts.OutlierDetection = &httpv3.HttpProtocolOptions_OutlierDetection{ErrorMatcher: statusCodeErrorMatcher(rf.RetriableStatusCodes)}
	updated, err := anypb.New(&opts)
	if err != nil {
		return fmt.Errorf("marshal HTTP protocol options: %w", err)
	}
	if c.TypedExtensionProtocolOptions == nil {
		c.TypedExtensionProtocolOptions = map[string]*anypb.Any{}
	}
	c.TypedExtensionProtocolOptions[constants.HttpProtocolOptionsTypedConfigKey] = updated
	return nil
}

func statusCodeErrorMatcher(codes []int) *matcher.MatchPredicate {
	rules := make([]*matcher.MatchPredicate, 0, len(codes))
	for _, code := range codes {
		rules = append(rules, &matcher.MatchPredicate{Rule: &matcher.MatchPredicate_HttpResponseHeadersMatch{
			HttpResponseHeadersMatch: &matcher.HttpHeadersMatch{Headers: []*route.HeaderMatcher{{
				Name:                 ":status",
				HeaderMatchSpecifier: &route.HeaderMatcher_ExactMatch{ExactMatch: strconv.Itoa(code)},
			}}},
		}})
	}
	if len(rules) == 1 {
		return rules[0]
	}
	return &matcher.MatchPredicate{Rule: &matcher.MatchPredicate_OrMatch{
		OrMatch: &matcher.MatchPredicate_MatchSet{Rules: rules},
	}}
}

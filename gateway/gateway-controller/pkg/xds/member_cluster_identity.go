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
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// collectMemberClusterKeys returns every real Envoy cluster name referenced
// by ANY model-failover chain on ANY route — the primary AND every fallback,
// regardless of whether suspension (SuspendDurationSeconds) is enabled for
// that chain. This isn't about suspension config at all: it identifies which
// real clusters need their own-identity metadata stamped, purely so an
// upstream-attempt policy can correctly resolve which chain member it's
// dialing (see applyMemberClusterIdentityMetadata's doc comment) — needed
// for every model-failover chain, suspended or not.
func collectMemberClusterKeys(rdc *models.RuntimeDeployConfig) map[string]bool {
	keys := map[string]bool{}
	for _, rdcRoute := range rdc.Routes {
		failover := rdcRoute.Upstream.Failover
		if failover == nil {
			continue
		}
		for _, target := range failover.Targets {
			if target.Target.ClusterKey != "" {
				keys[target.Target.ClusterKey] = true
			}
			for _, fb := range target.Fallbacks {
				if fb.ClusterKey != "" {
					keys[fb.ClusterKey] = true
				}
			}
		}
	}
	return keys
}

// applyMemberClusterIdentityMetadata stamps every LbEndpoint of c with c's
// own name under a well-known filter_metadata namespace/key
// (constants.MemberClusterIdentityMetadataNamespace/Key) — merging into any
// existing per-endpoint Metadata (e.g. peer.service tracing tags set
// elsewhere) rather than replacing it.
//
// Why: for an attempt routed through a model-failover chain's
// envoy.clusters.aggregate cluster, Envoy reports only the aggregate's own
// name via xds.cluster_name on every attempt against it — never the real
// member it actually dialed (confirmed live). x-envoy-attempt-count can't
// safely stand in for "which member is this" either: it counts dial
// attempts, not priorities, so it diverges from declared chain position
// whenever Envoy's load balancer skips a priority due to host health (e.g.
// outlier_detection ejecting the primary) without that being a retry. This
// metadata is the one signal that's actually dial-accurate: the upstream
// policy reads it back via the xds.upstream_host_metadata ext_proc
// attribute (confirmed live — see gateway-runtime's extractMemberClusterName)
// and matches it directly against its own declared FailoverTarget.ClusterName
// entries, with no attempt-count inference involved at all.
//
// This cluster can be shared by multiple independent model-failover chains
// (clusters are named purely by hostname+scheme, no route/proxy scoping —
// see sanitizeClusterName), but that's never a conflict here: every chain
// that references this cluster agrees on its own name by construction, so
// there's nothing to reconcile.
func applyMemberClusterIdentityMetadata(c *core.Metadata, name string) *core.Metadata {
	if c == nil {
		c = &core.Metadata{}
	}
	if c.FilterMetadata == nil {
		c.FilterMetadata = map[string]*structpb.Struct{}
	}
	c.FilterMetadata[constants.MemberClusterIdentityMetadataNamespace] = &structpb.Struct{
		Fields: map[string]*structpb.Value{
			constants.MemberClusterIdentityMetadataKey: structpb.NewStringValue(name),
		},
	}
	return c
}

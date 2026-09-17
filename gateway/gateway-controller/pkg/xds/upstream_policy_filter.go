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
	"fmt"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	upstreamcodecv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/upstream_codec/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	httpv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
)

// createUpstreamPolicyEngineCluster creates the Envoy cluster for the
// upstream (per-cluster) ext_proc service — a distinct socket from
// createPolicyEngineCluster's downstream one (see UpstreamPolicyEngineClusterName's
// doc comment for why they must be separate connections).
//
// Deliberately UDS-only for this first pass, unlike createPolicyEngineCluster
// (which also supports TCP + mTLS): the upstream service is expected to run
// co-located with gateway-runtime on the same host/pod, the same trust
// boundary UDS already assumes for the downstream cluster. Extend this the
// same way createPolicyEngineCluster was extended if a TCP/TLS deployment
// mode is ever needed here too — not by guessing at unused config surface now.
func createUpstreamPolicyEngineCluster() *cluster.Cluster {
	lbEndpoint := &endpoint.LbEndpoint{
		HostIdentifier: &endpoint.LbEndpoint_Endpoint{
			Endpoint: &endpoint.Endpoint{
				Address: &core.Address{
					Address: &core.Address_Pipe{
						Pipe: &core.Pipe{Path: constants.DefaultUpstreamPolicyEngineSocketPath},
					},
				},
			},
		},
	}

	return &cluster.Cluster{
		Name:                 constants.UpstreamPolicyEngineClusterName,
		ConnectTimeout:       durationpb.New(5 * time.Second),
		ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_STATIC},
		LbPolicy:             cluster.Cluster_ROUND_ROBIN,
		LoadAssignment: &endpoint.ClusterLoadAssignment{
			ClusterName: constants.UpstreamPolicyEngineClusterName,
			Endpoints: []*endpoint.LocalityLbEndpoints{
				{LbEndpoints: []*endpoint.LbEndpoint{lbEndpoint}},
			},
		},
		Http2ProtocolOptions: &core.Http2ProtocolOptions{},
	}
}

// shouldAttachUpstreamPolicyFilter decides whether a route needs the
// per-cluster upstream ext_proc filter at all. Routes with no upstream-phase
// policy (the overwhelming majority) pay zero cost — no filter is attached.
func shouldAttachUpstreamPolicyFilter(requiresUpstreamRequest, requiresUpstreamResponse bool) bool {
	return requiresUpstreamRequest || requiresUpstreamResponse
}

// attachUpstreamPolicyFilter attaches the per-cluster (upstream) ext_proc
// filter to c's TypedExtensionProtocolOptions, terminated by the mandatory
// upstream_codec filter.
//
// This must be called once per REAL member cluster a route can dispatch to —
// never on an aggregate-cluster pseudo-object (envoy.clusters.aggregate has no
// hosts of its own; chooseHost() always delegates to a member cluster's
// ClusterInfo, so a filter attached to the aggregate cluster's own name never
// fires). Callers building an aggregate-cluster construct for model failover
// must call this on each of the aggregate's real member clusters individually.
//
// Idempotent: calling this more than once on the same cluster leaves exactly
// one filter chain in place rather than accumulating duplicates.
func attachUpstreamPolicyFilter(c *cluster.Cluster, upstreamPolicyEngineClusterName string) error {
	extProcConfig := &extprocv3.ExternalProcessor{
		GrpcService: &core.GrpcService{
			TargetSpecifier: &core.GrpcService_EnvoyGrpc_{
				EnvoyGrpc: &core.GrpcService_EnvoyGrpc{
					ClusterName: upstreamPolicyEngineClusterName,
				},
			},
		},
		// Fail closed: if the upstream policy engine is unreachable, the
		// request must not reach the backend unauthenticated/untranslated —
		// mirrors the downstream ext_proc filter's FailureModeAllow: false.
		FailureModeAllow: false,
		RequestAttributes: []string{
			constants.ExtProcRequestAttributeRouteName,
			constants.ExtProcRequestAttributeClusterName,
		},
		ProcessingMode: &extprocv3.ProcessingMode{
			RequestHeaderMode: extprocv3.ProcessingMode_SEND,
			RequestBodyMode:   extprocv3.ProcessingMode_BUFFERED,
		},
		MetadataOptions: &extprocv3.MetadataOptions{
			ReceivingNamespaces: &extprocv3.MetadataOptions_MetadataNamespaces{
				Untyped: []string{constants.ExtProcMetadataNamespace},
			},
			ForwardingNamespaces: &extprocv3.MetadataOptions_MetadataNamespaces{
				Untyped: []string{constants.ExtProcMetadataNamespace},
			},
		},
	}
	extProcAny, err := anypb.New(extProcConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal upstream ext_proc config: %w", err)
	}

	codecAny, err := anypb.New(&upstreamcodecv3.UpstreamCodec{})
	if err != nil {
		return fmt.Errorf("failed to marshal upstream_codec config: %w", err)
	}

	opts := &httpv3.HttpProtocolOptions{
		// Required oneof — Envoy rejects the whole cluster at CDS warming
		// time with "upstream_protocol_options: is required" if unset. The
		// member cluster's own top-level config carries no explicit HTTP
		// version here (see createCluster/createWeightedCluster), so this
		// mirrors that implicit HTTP/1.1 default explicitly rather than
		// guessing at HTTP/2.
		UpstreamProtocolOptions: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_{
			ExplicitHttpConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig{
				ProtocolConfig: &httpv3.HttpProtocolOptions_ExplicitHttpConfig_HttpProtocolOptions{
					HttpProtocolOptions: &core.Http1ProtocolOptions{},
				},
			},
		},
		HttpFilters: []*hcm.HttpFilter{
			{
				Name:       constants.UpstreamExtProcFilterName,
				ConfigType: &hcm.HttpFilter_TypedConfig{TypedConfig: extProcAny},
			},
			{
				// Terminal filter — must be last. See the doc comment above.
				Name:       constants.UpstreamCodecFilterName,
				ConfigType: &hcm.HttpFilter_TypedConfig{TypedConfig: codecAny},
			},
		},
	}
	optsAny, err := anypb.New(opts)
	if err != nil {
		return fmt.Errorf("failed to marshal upstream HttpProtocolOptions: %w", err)
	}

	if c.TypedExtensionProtocolOptions == nil {
		c.TypedExtensionProtocolOptions = make(map[string]*anypb.Any)
	}
	c.TypedExtensionProtocolOptions[constants.HttpProtocolOptionsTypedConfigKey] = optsAny
	return nil
}

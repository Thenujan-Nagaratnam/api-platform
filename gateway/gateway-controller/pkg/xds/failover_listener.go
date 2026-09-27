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
	"strings"
	"time"

	accesslog "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	luav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/lua/v3"
	router "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/router/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	internalupstream "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/internal_upstream/v3"
	rawbuffer "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/raw_buffer/v3"
	matcher "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/failover"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// applyRouteFailover turns a route the RDC marked for model-failover into its
// front or dispatch form. See package failover for the two-route model.
func (t *Translator) applyRouteFailover(r *route.Route, rdcRoute *models.Route) {
	fo := rdcRoute.Failover
	if fo == nil {
		return
	}
	setRouteFailoverRole(r, fo.Role)
	action := r.GetRoute()
	if action == nil {
		return
	}

	switch failover.Role(fo.Role) {
	case failover.RoleFront:
		// Every attempt goes to the dispatch hop with the client's path
		// unchanged; the dispatch route does the per-target rewrite.
		action.ClusterSpecifier = &route.RouteAction_Cluster{Cluster: failover.DispatchClusterName}
		action.RegexRewrite = nil
		action.HostRewriteSpecifier = nil
		// The overall deadline is derived from the per-attempt one rather than
		// the gateway's generic route default. A timeout the publisher set
		// explicitly only raises it, and an explicit zero (disabled) stays off.
		switch configured := configuredRouteTimeout(rdcRoute); {
		case configured == nil:
			action.Timeout = durationpb.New(fo.RouteTimeout)
		case *configured != 0 && *configured < fo.RouteTimeout:
			action.Timeout = durationpb.New(fo.RouteTimeout)
		}
		action.RetryPolicy = &route.RetryPolicy{
			RetryOn:       fo.RetryOn,
			NumRetries:    wrapperspb.UInt32(uint32(fo.NumRetries)),
			PerTryTimeout: durationpb.New(fo.PerTryTimeout),
			RetriableHeaders: []*route.HeaderMatcher{{
				Name:                 failover.HeaderRetry,
				HeaderMatchSpecifier: &route.HeaderMatcher_PresentMatch{PresentMatch: true},
			}},
			RetryBackOff: &route.RetryPolicy_RetryBackOff{
				BaseInterval: durationpb.New(failover.RetryBackOffBase),
				MaxInterval:  durationpb.New(failover.RetryBackOffMax),
			},
		}
		r.RequestBodyBufferLimit = wrapperspb.UInt64(t.routerConfig.Failover.MaxRequestBodyBytes)
		// The router adds this after ext_proc, overwriting any client value.
		r.RequestHeadersToAdd = append(r.RequestHeadersToAdd, &core.HeaderValueOption{
			Header:       &core.HeaderValue{Key: failover.HeaderChain, Value: fo.ChainID},
			AppendAction: core.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
		})
		r.ResponseHeadersToRemove = append(r.ResponseHeadersToRemove,
			failover.HeaderRetry, failover.HeaderExhausted, failover.HeaderUpstreamFailure)

	case failover.RoleDispatch:
		// Match any path: a transformer's path rewrite clears the route cache,
		// and the re-lookup must land on this same route. The chain header
		// (already among the match headers) keeps it unique.
		r.Match.PathSpecifier = &route.RouteMatch_Prefix{Prefix: "/"}
		// Attempt timing belongs to the front route's per_try_timeout; a
		// route timeout here would also cut long streaming responses.
		action.Timeout = durationpb.New(0)
		r.RequestHeadersToRemove = append(r.RequestHeadersToRemove, failover.HeaderChain)
	}
}

func configuredRouteTimeout(r *models.Route) *time.Duration {
	if r.Timeout == nil {
		return nil
	}
	return r.Timeout.Timeout
}

func setRouteFailoverRole(r *route.Route, role string) {
	filterMetadata := ensureRouteFilterMetadata(r)
	routeMeta := filterMetadata[envoyRouteMetadataNamespace]
	if routeMeta == nil {
		routeMeta = &structpb.Struct{Fields: map[string]*structpb.Value{}}
		filterMetadata[envoyRouteMetadataNamespace] = routeMeta
	} else if routeMeta.Fields == nil {
		routeMeta.Fields = map[string]*structpb.Value{}
	}
	routeMeta.Fields[failover.RouteMetadataKey] = structpb.NewStringValue(role)
}

// isFailoverDispatchRoute reports whether a route belongs on the internal
// dispatch listener rather than a client-facing virtual host.
func isFailoverDispatchRoute(r *route.Route) bool {
	meta := r.GetMetadata().GetFilterMetadata()[envoyRouteMetadataNamespace]
	if meta == nil {
		return false
	}
	return meta.GetFields()[failover.RouteMetadataKey].GetStringValue() == string(failover.RoleDispatch)
}

// failoverLocalReplyConfig annotates Envoy's own replies to transport failures
// with their response flags, but only on requests carrying the hop secret —
// i.e. the dispatch hop's loopback request to a provider route. The dispatch
// policy reads the flags to tell a connection failure, reset or timeout from
// a 503/504 the provider itself sent.
//
// On the client-facing listener the hop filter has already moved the secret
// from the request header into dynamic metadata (so it is never forwarded to
// a provider), and the mapper matches the metadata. On the internal dispatch
// listener nothing strips the header, so the mapper matches it directly.
func failoverLocalReplyConfig(matchMetadata bool) *hcm.LocalReplyConfig {
	flags := strings.Split(failover.UpstreamFailureFlags, ",")
	secret := &matcher.StringMatcher{MatchPattern: &matcher.StringMatcher_Exact{Exact: failover.HopSecret()}}
	var hopFilter *accesslog.AccessLogFilter
	if matchMetadata {
		hopFilter = &accesslog.AccessLogFilter{FilterSpecifier: &accesslog.AccessLogFilter_MetadataFilter{
			MetadataFilter: &accesslog.MetadataFilter{
				Matcher: &matcher.MetadataMatcher{
					Filter: failover.HopMetadataNamespace,
					Path:   []*matcher.MetadataMatcher_PathSegment{{Segment: &matcher.MetadataMatcher_PathSegment_Key{Key: failover.HopMetadataKey}}},
					Value:  &matcher.ValueMatcher{MatchPattern: &matcher.ValueMatcher_StringMatch{StringMatch: secret}},
				},
				// Without this a request that never carried the secret would match.
				MatchIfKeyNotFound: wrapperspb.Bool(false),
			},
		}}
	} else {
		hopFilter = &accesslog.AccessLogFilter{FilterSpecifier: &accesslog.AccessLogFilter_HeaderFilter{
			HeaderFilter: &accesslog.HeaderFilter{Header: &route.HeaderMatcher{
				Name:                 failover.HeaderHop,
				HeaderMatchSpecifier: &route.HeaderMatcher_StringMatch{StringMatch: secret},
			}},
		}}
	}
	return &hcm.LocalReplyConfig{
		Mappers: []*hcm.ResponseMapper{{
			Filter: &accesslog.AccessLogFilter{
				FilterSpecifier: &accesslog.AccessLogFilter_AndFilter{
					AndFilter: &accesslog.AndFilter{
						Filters: []*accesslog.AccessLogFilter{
							{FilterSpecifier: &accesslog.AccessLogFilter_ResponseFlagFilter{
								ResponseFlagFilter: &accesslog.ResponseFlagFilter{Flags: flags},
							}},
							hopFilter,
						},
					},
				},
			},
			HeadersToAdd: []*core.HeaderValueOption{{
				Header:       &core.HeaderValue{Key: failover.HeaderUpstreamFailure, Value: "%RESPONSE_FLAGS%"},
				AppendAction: core.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			}},
		}},
	}
}

// failoverHopLuaScript takes the hop secret off an incoming request and keeps
// it only as dynamic metadata, so it is never forwarded to a provider but the
// local-reply mapper can still see it.
const failoverHopLuaScript = `function envoy_on_request(handle)
  local hop = handle:headers():get("` + failover.HeaderHop + `")
  if hop ~= nil then
    handle:streamInfo():dynamicMetadata():set("` + failover.HopMetadataNamespace + `", "` + failover.HopMetadataKey + `", hop)
    handle:headers():remove("` + failover.HeaderHop + `")
  end
end
`

// createFailoverHopFilter builds the client-facing listener's hop filter. It
// must run after ext_proc (the provider hop's policies run first) and before
// the router forwards the request.
func createFailoverHopFilter() (*hcm.HttpFilter, error) {
	luaAny, err := anypb.New(&luav3.Lua{
		DefaultSourceCode: &core.DataSource{Specifier: &core.DataSource_InlineString{InlineString: failoverHopLuaScript}},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal failover hop filter: %w", err)
	}
	return &hcm.HttpFilter{Name: failover.HopFilterName, ConfigType: &hcm.HttpFilter_TypedConfig{TypedConfig: luaAny}}, nil
}

// createFailoverDispatchResources builds the internal listener every
// model-failover attempt passes through, its route configuration, and the
// internal cluster front routes retry against.
func (t *Translator) createFailoverDispatchResources(dispatchRoutes []*route.Route) (*listener.Listener, *route.RouteConfiguration, *cluster.Cluster, error) {
	routes := SortRoutesByPriority(dispatchRoutes)
	routeConfig := &route.RouteConfiguration{
		Name: failover.DispatchRouteConfigName,
		VirtualHosts: []*route.VirtualHost{{
			Name:    failover.InternalListenerName,
			Domains: []string{"*"},
			Routes:  routes,
		}},
	}

	extProcFilter, err := t.createExtProcFilter()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create ext_proc filter for failover dispatch: %w", err)
	}
	luaFilter, err := t.createLuaFilter()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create lua filter for failover dispatch: %w", err)
	}
	routerAny, err := anypb.New(&router.Router{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create router config: %w", err)
	}

	// No access log: the front hop already logs the client request, and the
	// provider hop logs each attempt; a third entry per attempt would double
	// count traffic.
	manager := &hcm.HttpConnectionManager{
		CodecType:  hcm.HttpConnectionManager_AUTO,
		StatPrefix: failover.InternalListenerName,
		RouteSpecifier: &hcm.HttpConnectionManager_Rds{
			Rds: &hcm.Rds{
				ConfigSource: &core.ConfigSource{
					ResourceApiVersion:    core.ApiVersion_V3,
					ConfigSourceSpecifier: &core.ConfigSource_Ads{Ads: &core.AggregatedConfigSource{}},
					InitialFetchTimeout:   durationpb.New(0),
				},
				RouteConfigName: failover.DispatchRouteConfigName,
			},
		},
		HttpFilters: []*hcm.HttpFilter{
			extProcFilter,
			luaFilter,
			{Name: wellknown.Router, ConfigType: &hcm.HttpFilter_TypedConfig{TypedConfig: routerAny}},
		},
		StreamIdleTimeout: durationpb.New(t.routerConfig.HTTPListener.Timeouts.StreamIdleTimeout),
		// Same path canonicalization as the client-facing listener, so route,
		// policy and authz matching on this hop sees the same path.
		NormalizePath:                wrapperspb.Bool(!t.routerConfig.HTTPListener.DisablePathNormalization),
		MergeSlashes:                 !t.routerConfig.HTTPListener.DisablePathNormalization,
		PathWithEscapedSlashesAction: convertPathWithEscapedSlashesAction(t.routerConfig.HTTPListener.PathWithEscapedSlashesAction),
		LocalReplyConfig:             failoverLocalReplyConfig(false),
	}
	managerAny, err := anypb.New(manager)
	if err != nil {
		return nil, nil, nil, err
	}

	lis := &listener.Listener{
		Name: failover.InternalListenerName,
		ListenerSpecifier: &listener.Listener_InternalListener{
			InternalListener: &listener.Listener_InternalListenerConfig{},
		},
		FilterChains: []*listener.FilterChain{{
			Filters: []*listener.Filter{{
				Name:       wellknown.HTTPConnectionManager,
				ConfigType: &listener.Filter_TypedConfig{TypedConfig: managerAny},
			}},
		}},
		PerConnectionBufferLimitBytes: wrapperspb.UInt32(t.routerConfig.HTTPListener.PerConnectionBufferLimitBytes),
	}

	c, err := createFailoverDispatchCluster()
	if err != nil {
		return nil, nil, nil, err
	}
	return lis, routeConfig, c, nil
}

func createFailoverDispatchCluster() (*cluster.Cluster, error) {
	rawAny, err := anypb.New(&rawbuffer.RawBuffer{})
	if err != nil {
		return nil, err
	}
	internalAny, err := anypb.New(&internalupstream.InternalUpstreamTransport{
		TransportSocket: &core.TransportSocket{
			Name:       "envoy.transport_sockets.raw_buffer",
			ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: rawAny},
		},
	})
	if err != nil {
		return nil, err
	}
	return &cluster.Cluster{
		Name:                 failover.DispatchClusterName,
		ConnectTimeout:       durationpb.New(time.Second),
		ClusterDiscoveryType: &cluster.Cluster_Type{Type: cluster.Cluster_STATIC},
		// Retries in flight are bounded per chain by num_retries; the
		// cluster-wide default of 3 would reject retries under load.
		CircuitBreakers: &cluster.CircuitBreakers{
			Thresholds: []*cluster.CircuitBreakers_Thresholds{{
				MaxRetries: wrapperspb.UInt32(failover.DispatchMaxRetries),
			}},
		},
		LoadAssignment: &endpoint.ClusterLoadAssignment{
			ClusterName: failover.DispatchClusterName,
			Endpoints: []*endpoint.LocalityLbEndpoints{{
				LbEndpoints: []*endpoint.LbEndpoint{{
					HostIdentifier: &endpoint.LbEndpoint_Endpoint{
						Endpoint: &endpoint.Endpoint{
							Address: &core.Address{
								Address: &core.Address_EnvoyInternalAddress{
									EnvoyInternalAddress: &core.EnvoyInternalAddress{
										AddressNameSpecifier: &core.EnvoyInternalAddress_ServerListenerName{
											ServerListenerName: failover.InternalListenerName,
										},
									},
								},
							},
						},
					},
				}},
			}},
		},
		TransportSocket: &core.TransportSocket{
			Name:       "envoy.transport_sockets.internal_upstream",
			ConfigType: &core.TransportSocket_TypedConfig{TypedConfig: internalAny},
		},
	}, nil
}

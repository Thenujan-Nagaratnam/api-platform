/*
 *  Copyright (c) 2026, WSO2 LLC. (http://www.wso2.com) All Rights Reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 */

package xds

import (
	"fmt"
	"sort"

	accesslog "github.com/envoyproxy/go-control-plane/envoy/config/accesslog/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	celfilter "github.com/envoyproxy/go-control-plane/envoy/extensions/access_loggers/filters/cel/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// Errors the AI gateway returns on LLM routes (LlmProvider / LlmProxy) use the
// conventional OpenAI-compatible non-streaming HTTP envelope, since clients of
// those APIs are OpenAI SDKs and compatible tooling:
// {"error": {"message", "type", "param", "code"}}. Policies and the
// policy engine render their own errors that way; this file covers the two error
// sources that never reach them — Envoy's own local replies for routing/upstream failures,
// and requests under an LLM context that match no route.

// envoyRouteAPIKindKey records a route's API kind under the wso2.route namespace,
// so HCM-level config (the local reply mapper) can tell LLM routes apart.
const envoyRouteAPIKindKey = "api_kind"

// llmNotFoundBody is the 404 for a request under an LLM API's context that matches
// none of its routes.
const llmNotFoundBody = `{"error":{"message":"The requested resource was not found.","type":"not_found_error","param":null,"code":null}}`

// llmRouteCELExpression matches requests routed to an LLM API route.
var llmRouteCELExpression = fmt.Sprintf(
	`xds.route_metadata.filter_metadata[%q][%q] in [%q, %q]`,
	envoyRouteMetadataNamespace, envoyRouteAPIKindKey,
	string(models.KindLlmProvider), string(models.KindLlmProxy),
)

// upstreamFailureResponseFlags are the Envoy response flags of a local reply Envoy
// itself generates because the upstream could not produce a response at request
// time (missing cluster, no healthy host, connect failure, timeout, reset, overflow, ...). Scoping the
// mapper to them matters: a policy's ext_proc ImmediateResponse is also delivered as
// a local reply, but carries none of these flags, so an error body a policy already
// rendered is never re-wrapped. Configuration faults are included because they are
// still errors returned to the caller during an LLM API invocation.
var upstreamFailureResponseFlags = []string{
	"NC",    // route references a missing cluster
	"UH",    // no healthy upstream
	"UF",    // upstream connection failure
	"UT",    // upstream request timeout
	"UC",    // upstream connection termination
	"UR",    // upstream remote reset
	"URX",   // upstream retry limit exceeded
	"UO",    // upstream overflow (circuit breaking)
	"LR",    // connection local reset
	"UMSDR", // upstream max stream duration reached
	"UPE",   // upstream protocol error
}

func isLLMKind(kind string) bool {
	return kind == string(models.KindLlmProvider) || kind == string(models.KindLlmProxy)
}

// setRouteAPIKind records the route's API kind under wso2.route/api_kind, merging
// into any existing wso2.route struct. An empty kind is a no-op.
func setRouteAPIKind(r *route.Route, kind string) {
	if r == nil || kind == "" {
		return
	}
	filterMetadata := ensureRouteFilterMetadata(r)
	routeMeta := filterMetadata[envoyRouteMetadataNamespace]
	if routeMeta == nil {
		routeMeta = &structpb.Struct{Fields: map[string]*structpb.Value{}}
		filterMetadata[envoyRouteMetadataNamespace] = routeMeta
	} else if routeMeta.Fields == nil {
		routeMeta.Fields = map[string]*structpb.Value{}
	}
	routeMeta.Fields[envoyRouteAPIKindKey] = structpb.NewStringValue(kind)
}

// createLLMLocalReplyConfig rewrites Envoy's upstream-failure local replies on LLM
// routes (plain text such as "no healthy upstream") into the conventional
// OpenAI-compatible non-streaming HTTP JSON envelope. The original HTTP status is
// preserved so clients retain the correct classification and retry semantics.
func createLLMLocalReplyConfig() (*hcm.LocalReplyConfig, error) {
	celAny, err := anypb.New(&celfilter.ExpressionFilter{Expression: llmRouteCELExpression})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CEL filter for LLM local replies: %w", err)
	}
	body, err := structpb.NewStruct(map[string]interface{}{
		"error": map[string]interface{}{
			"message": "%LOCAL_REPLY_BODY%",
			"type":    "server_error",
			"param":   nil,
			"code":    nil,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to build LLM local reply body format: %w", err)
	}
	return &hcm.LocalReplyConfig{
		Mappers: []*hcm.ResponseMapper{{
			Filter: &accesslog.AccessLogFilter{
				FilterSpecifier: &accesslog.AccessLogFilter_AndFilter{
					AndFilter: &accesslog.AndFilter{
						Filters: []*accesslog.AccessLogFilter{
							{FilterSpecifier: &accesslog.AccessLogFilter_ResponseFlagFilter{
								ResponseFlagFilter: &accesslog.ResponseFlagFilter{Flags: upstreamFailureResponseFlags},
							}},
							{FilterSpecifier: &accesslog.AccessLogFilter_ExtensionFilter{
								ExtensionFilter: &accesslog.ExtensionFilter{
									Name:       "envoy.access_loggers.extension_filters.cel",
									ConfigType: &accesslog.ExtensionFilter_TypedConfig{TypedConfig: celAny},
								},
							}},
						},
					},
				},
			},
			BodyFormatOverride: &core.SubstitutionFormatString{
				Format:      &core.SubstitutionFormatString_JsonFormat{JsonFormat: body},
				ContentType: "application/json",
			},
		}},
	}, nil
}

// llmNotFoundRoutes builds one catch-all 404 route per LLM API context, so a
// request under an LLM context that matches none of its routes gets an
// OpenAI-format 404 instead of the vhost-wide no-api-found response. Longer
// contexts come first so a nested context wins over its parent. These routes must
// be placed after every API route and before the vhost's no-api-found route.
func llmNotFoundRoutes(contexts map[string]struct{}) ([]*route.Route, error) {
	if len(contexts) == 0 {
		return nil, nil
	}
	ordered := make([]string, 0, len(contexts))
	for c := range contexts {
		ordered = append(ordered, c)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) != len(ordered[j]) {
			return len(ordered[i]) > len(ordered[j])
		}
		return ordered[i] < ordered[j]
	})

	extProcDisabledAny, err := anypb.New(&extproc.ExtProcPerRoute{
		Override: &extproc.ExtProcPerRoute_Disabled{Disabled: true},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal ExtProcPerRoute for LLM not-found route: %w", err)
	}

	routes := make([]*route.Route, 0, len(ordered))
	for _, apiContext := range ordered {
		r := &route.Route{
			Name: "llm-no-route-found:" + apiContext,
			Match: &route.RouteMatch{
				// Matches the context itself and anything below it at a segment
				// boundary — "/openai" and "/openai/x", never "/openaix".
				PathSpecifier: &route.RouteMatch_PathSeparatedPrefix{PathSeparatedPrefix: apiContext},
			},
			Action: &route.Route_DirectResponse{
				DirectResponse: &route.DirectResponseAction{
					Status: 404,
					Body: &core.DataSource{
						Specifier: &core.DataSource_InlineString{InlineString: llmNotFoundBody},
					},
				},
			},
			ResponseHeadersToAdd: []*core.HeaderValueOption{{
				Header: &core.HeaderValue{Key: "content-type", Value: "application/json"},
			}},
			TypedPerFilterConfig: map[string]*anypb.Any{
				constants.ExtProcFilterName: extProcDisabledAny,
			},
		}
		routes = append(routes, r)
	}
	return routes, nil
}

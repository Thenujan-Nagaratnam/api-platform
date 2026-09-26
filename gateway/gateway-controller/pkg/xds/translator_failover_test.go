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
	"testing"
	"time"

	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/failover"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

func failoverTestTranslator() *Translator {
	routerCfg := testRouterConfig()
	routerCfg.Failover = config.RouterFailoverConfig{MaxRequestBodyBytes: 4 << 20}
	cfg := testConfig()
	cfg.Router = *routerCfg
	return NewTranslator(createTestLogger(), routerCfg, nil, cfg)
}

func failoverTestRDC(role string) (*models.Route, *models.RuntimeDeployConfig) {
	r := &models.Route{
		Method:        "POST",
		Path:          "/mf-proxy/chat/completions",
		OperationPath: "/chat/completions",
		Vhost:         "main.local",
		Upstream: models.RouteUpstream{
			ClusterKey:       "upstream_main",
			UseClusterHeader: true,
			DefaultCluster:   "upstream_main",
		},
	}
	if role == string(failover.RoleDispatch) {
		r.MatchHeaders = []models.RouteHeaderMatch{{Name: failover.HeaderChain, Value: "tok", Type: "Exact"}}
		r.Failover = &models.RouteFailover{Role: role, ChainID: "tok"}
	} else {
		r.Failover = &models.RouteFailover{
			Role: role, ChainID: "tok", NumRetries: 2,
			PerTryTimeout: 5 * time.Second, RouteTimeout: 17 * time.Second,
			RetryOn: "retriable-headers,connect-failure,reset",
		}
	}
	rdc := &models.RuntimeDeployConfig{
		Metadata: models.Metadata{Kind: "LlmProxy"},
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"upstream_main": {BasePath: "/openai-a"},
		},
	}
	return r, rdc
}

func TestFailover_FrontRoute(t *testing.T) {
	tr := failoverTestTranslator()
	rdcRoute, rdc := failoverTestRDC(string(failover.RoleFront))
	r := tr.createRouteFromRDC("POST|/mf-proxy/chat/completions|main.local", rdcRoute, rdc)
	require.NoError(t, r.Validate())

	action := r.GetRoute()
	assert.Equal(t, failover.DispatchClusterName, action.GetCluster(), "every attempt goes to the dispatch hop")
	assert.Nil(t, action.GetRegexRewrite(), "the client path reaches the dispatch hop unchanged")
	assert.Nil(t, action.GetHostRewriteSpecifier())
	assert.Equal(t, 17*time.Second, action.GetTimeout().AsDuration())

	rp := action.GetRetryPolicy()
	require.NotNil(t, rp)
	assert.Equal(t, "retriable-headers,connect-failure,reset", rp.GetRetryOn())
	assert.EqualValues(t, 2, rp.GetNumRetries().GetValue())
	assert.Equal(t, 5*time.Second, rp.GetPerTryTimeout().AsDuration())
	require.Len(t, rp.GetRetriableHeaders(), 1)
	assert.Equal(t, failover.HeaderRetry, rp.GetRetriableHeaders()[0].GetName())
	assert.True(t, rp.GetRetriableHeaders()[0].GetPresentMatch())
	assert.Equal(t, time.Millisecond, rp.GetRetryBackOff().GetBaseInterval().AsDuration())
	assert.EqualValues(t, 4<<20, r.GetRequestBodyBufferLimit().GetValue())

	require.Len(t, r.GetRequestHeadersToAdd(), 1)
	assert.Equal(t, failover.HeaderChain, r.GetRequestHeadersToAdd()[0].GetHeader().GetKey())
	assert.Equal(t, "tok", r.GetRequestHeadersToAdd()[0].GetHeader().GetValue())
	assert.Subset(t, r.GetResponseHeadersToRemove(), []string{failover.HeaderRetry, failover.HeaderExhausted, failover.HeaderUpstreamFailure})
	assert.Contains(t, r.GetRequestHeadersToRemove(), constants.TargetUpstreamHeader)
	assert.False(t, isFailoverDispatchRoute(r))
}

func TestFailover_FrontRouteKeepsLargerConfiguredTimeoutAndDisabledTimeout(t *testing.T) {
	tr := failoverTestTranslator()
	for name, tc := range map[string]struct {
		configured time.Duration
		want       time.Duration
	}{
		"larger configured wins": {120 * time.Second, 120 * time.Second},
		"smaller is raised":      {3 * time.Second, 17 * time.Second},
		"disabled stays off":     {0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			rdcRoute, rdc := failoverTestRDC(string(failover.RoleFront))
			d := tc.configured
			rdcRoute.Timeout = &models.RouteTimeout{Timeout: &d}
			r := tr.createRouteFromRDC("k", rdcRoute, rdc)
			assert.Equal(t, tc.want, r.GetRoute().GetTimeout().AsDuration())
		})
	}
}

func TestFailover_DispatchRoute(t *testing.T) {
	tr := failoverTestTranslator()
	rdcRoute, rdc := failoverTestRDC(string(failover.RoleDispatch))
	r := tr.createRouteFromRDC("POST|/mf-proxy/chat/completions|main.local|hdr", rdcRoute, rdc)
	require.NoError(t, r.Validate())

	assert.True(t, isFailoverDispatchRoute(r))
	assert.Equal(t, "/", r.GetMatch().GetPrefix(), "must still match after a transformer rewrites the path")
	var sawChain bool
	for _, h := range r.GetMatch().GetHeaders() {
		if h.GetName() == failover.HeaderChain {
			sawChain = true
			assert.Equal(t, "tok", h.GetStringMatch().GetExact())
		}
	}
	assert.True(t, sawChain, "matched on the chain header")
	assert.Equal(t, constants.TargetUpstreamHeader, r.GetRoute().GetClusterHeader(), "per-target cluster selection as on a proxy route")
	assert.NotNil(t, r.GetRoute().GetRegexRewrite(), "same rewrite semantics as a proxy route")
	assert.Equal(t, time.Duration(0), r.GetRoute().GetTimeout().AsDuration(), "attempt timing belongs to the front route")
	assert.Contains(t, r.GetRequestHeadersToRemove(), failover.HeaderChain)
	assert.Nil(t, r.GetRoute().GetRetryPolicy())
}

func TestFailover_NonFailoverRouteUnchanged(t *testing.T) {
	tr := failoverTestTranslator()
	rdcRoute, rdc := failoverTestRDC(string(failover.RoleFront))
	rdcRoute.Failover = nil
	r := tr.createRouteFromRDC("k", rdcRoute, rdc)
	assert.Nil(t, r.GetRoute().GetRetryPolicy())
	assert.Equal(t, constants.TargetUpstreamHeader, r.GetRoute().GetClusterHeader())
	assert.False(t, isFailoverDispatchRoute(r))
}

func TestFailover_DispatchResources(t *testing.T) {
	tr := failoverTestTranslator()
	rdcRoute, rdc := failoverTestRDC(string(failover.RoleDispatch))
	dr := tr.createRouteFromRDC("k", rdcRoute, rdc)

	lis, rc, c, err := tr.createFailoverDispatchResources([]*route.Route{dr})
	require.NoError(t, err)
	require.NoError(t, lis.Validate())
	require.NoError(t, rc.Validate())
	require.NoError(t, c.Validate())

	assert.Equal(t, failover.InternalListenerName, lis.GetName())
	assert.NotNil(t, lis.GetInternalListener(), "never bound to a socket")
	assert.Nil(t, lis.GetAddress())

	var manager hcm.HttpConnectionManager
	require.NoError(t, lis.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(&manager))
	assert.Equal(t, failover.DispatchRouteConfigName, manager.GetRds().GetRouteConfigName())
	assert.True(t, manager.GetNormalizePath().GetValue(), "path canonicalization as on the main listener")
	assert.True(t, manager.GetMergeSlashes())
	assert.Empty(t, manager.GetAccessLog(), "no per-attempt access log on the internal hop")
	require.Len(t, manager.GetHttpFilters(), 3)
	assert.Equal(t, constants.ExtProcFilterName, manager.GetHttpFilters()[0].GetName())
	assert.NotNil(t, manager.GetLocalReplyConfig())

	assert.Equal(t, failover.DispatchRouteConfigName, rc.GetName())
	require.Len(t, rc.GetVirtualHosts(), 1)
	assert.Equal(t, []string{"*"}, rc.GetVirtualHosts()[0].GetDomains())

	assert.Equal(t, failover.DispatchClusterName, c.GetName())
	assert.Equal(t, failover.InternalListenerName,
		c.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetEnvoyInternalAddress().GetServerListenerName())
	assert.Equal(t, "envoy.transport_sockets.internal_upstream", c.GetTransportSocket().GetName())
	assert.EqualValues(t, failover.DispatchMaxRetries, c.GetCircuitBreakers().GetThresholds()[0].GetMaxRetries().GetValue())
}

func TestFailover_MainListenerAnnotatesTransportFailuresOnlyForHopSecret(t *testing.T) {
	tr := failoverTestTranslator()
	lis, _, err := tr.createListener(nil, false)
	require.NoError(t, err)
	var manager hcm.HttpConnectionManager
	require.NoError(t, lis.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(&manager))

	// The hop filter runs after ext_proc and just before the router, so the
	// secret is off the request before it is forwarded to a provider.
	filters := manager.GetHttpFilters()
	require.GreaterOrEqual(t, len(filters), 3)
	assert.Equal(t, failover.HopFilterName, filters[len(filters)-2].GetName())
	assert.Equal(t, constants.ExtProcFilterName, filters[0].GetName())

	mappers := manager.GetLocalReplyConfig().GetMappers()
	require.Len(t, mappers, 1)
	and := mappers[0].GetFilter().GetAndFilter().GetFilters()
	require.Len(t, and, 2)
	assert.ElementsMatch(t, []string{"UF", "URX", "UH", "UO", "UT", "UC", "DC", "LR"}, and[0].GetResponseFlagFilter().GetFlags())
	md := and[1].GetMetadataFilter()
	require.NotNil(t, md, "matches the hop secret in dynamic metadata, not the (stripped) header")
	assert.Equal(t, failover.HopMetadataNamespace, md.GetMatcher().GetFilter())
	assert.Equal(t, failover.HopMetadataKey, md.GetMatcher().GetPath()[0].GetKey())
	assert.Equal(t, failover.HopSecret(), md.GetMatcher().GetValue().GetStringMatch().GetExact())
	assert.False(t, md.GetMatchIfKeyNotFound().GetValue(), "a request without the secret must not match")
	require.Len(t, mappers[0].GetHeadersToAdd(), 1)
	assert.Equal(t, failover.HeaderUpstreamFailure, mappers[0].GetHeadersToAdd()[0].GetHeader().GetKey())
	assert.Equal(t, "%RESPONSE_FLAGS%", mappers[0].GetHeadersToAdd()[0].GetHeader().GetValue())
	assert.Nil(t, mappers[0].GetStatusCode(), "the status and body of the local reply are left alone")
	assert.Nil(t, mappers[0].GetBody())
}

func TestFailover_DispatchListenerMatchesHopHeader(t *testing.T) {
	cfg := failoverLocalReplyConfig(false)
	hf := cfg.GetMappers()[0].GetFilter().GetAndFilter().GetFilters()[1].GetHeaderFilter()
	require.NotNil(t, hf)
	assert.Equal(t, failover.HeaderHop, hf.GetHeader().GetName())
	assert.Equal(t, failover.HopSecret(), hf.GetHeader().GetStringMatch().GetExact())
}

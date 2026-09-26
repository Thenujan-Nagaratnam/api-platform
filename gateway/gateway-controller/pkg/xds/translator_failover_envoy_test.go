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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	bootstrap "github.com/envoyproxy/go-control-plane/envoy/config/bootstrap/v3"
	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	internallistener "github.com/envoyproxy/go-control-plane/envoy/extensions/bootstrap/internal_listener/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/failover"
)

// TestFailover_GeneratedConfigPassesEnvoyValidation feeds the generated front
// listener, dispatch listener, routes and clusters to a real Envoy binary in
// --mode validate. It is skipped unless ENVOY_BIN names one, e.g.
//
//	ENVOY_BIN=$(which envoy) go test ./pkg/xds -run GeneratedConfigPassesEnvoyValidation
func TestFailover_GeneratedConfigPassesEnvoyValidation(t *testing.T) {
	envoyBin := os.Getenv("ENVOY_BIN")
	if envoyBin == "" {
		t.Skip("set ENVOY_BIN to run Envoy config validation")
	}
	tr := failoverTestTranslator()

	frontRDC, rdc := failoverTestRDC(string(failover.RoleFront))
	front := tr.createRouteFromRDC("front", frontRDC, rdc)
	dispatchRDC, _ := failoverTestRDC(string(failover.RoleDispatch))
	dispatch := tr.createRouteFromRDC("dispatch", dispatchRDC, rdc)

	mainListener, _, err := tr.createListener([]*route.VirtualHost{{Name: "main", Domains: []string{"*"}, Routes: []*route.Route{front}}}, false)
	require.NoError(t, err)
	dispatchListener, dispatchRC, dispatchCluster, err := tr.createFailoverDispatchResources([]*route.Route{dispatch})
	require.NoError(t, err)

	// Validation mode has no ADS server, so inline each listener's routes.
	inlineRoutes(t, mainListener, &route.RouteConfiguration{
		Name:         SharedRouteConfigName,
		VirtualHosts: []*route.VirtualHost{{Name: "main", Domains: []string{"*"}, Routes: []*route.Route{front}}},
	})
	inlineRoutes(t, dispatchListener, dispatchRC)

	ilAny, err := anypb.New(&internallistener.InternalListener{})
	require.NoError(t, err)
	upstream := tr.createCluster("upstream_main", mustParseURL(t, "http://127.0.0.1:9"), nil, nil)
	bs := &bootstrap.Bootstrap{
		Node:                &core.Node{Id: "validate", Cluster: "validate"},
		BootstrapExtensions: []*core.TypedExtensionConfig{{Name: "envoy.bootstrap.internal_listener", TypedConfig: ilAny}},
		StaticResources: &bootstrap.Bootstrap_StaticResources{
			Listeners: []*listener.Listener{mainListener, dispatchListener},
			Clusters:  []*cluster.Cluster{dispatchCluster, tr.createPolicyEngineCluster(), upstream},
		},
	}
	out, err := protojson.Marshal(bs)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "bootstrap.json")
	require.NoError(t, os.WriteFile(path, out, 0o600))

	cmd := exec.Command(envoyBin, "--mode", "validate", "-c", path, "-l", "error")
	combined, err := cmd.CombinedOutput()
	require.NoError(t, err, "envoy rejected the generated config:\n%s", combined)
}

func inlineRoutes(t *testing.T, l *listener.Listener, rc *route.RouteConfiguration) {
	t.Helper()
	f := l.GetFilterChains()[0].GetFilters()[0]
	var manager hcm.HttpConnectionManager
	require.NoError(t, f.GetTypedConfig().UnmarshalTo(&manager))
	manager.RouteSpecifier = &hcm.HttpConnectionManager_RouteConfig{RouteConfig: rc}
	manager.Tracing = nil
	manager.AccessLog = nil
	a, err := anypb.New(&manager)
	require.NoError(t, err)
	f.ConfigType = &listener.Filter_TypedConfig{TypedConfig: a}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

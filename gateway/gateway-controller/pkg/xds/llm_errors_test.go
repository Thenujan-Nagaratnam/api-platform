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
	"encoding/json"
	"testing"

	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

func TestSetRouteAPIKind_MergesIntoRouteMetadata(t *testing.T) {
	r := &route.Route{}
	setRouteHTTPRoute(r, "/openai/chat/completions")
	setRouteAPIKind(r, string(models.KindLlmProvider))

	fields := r.GetMetadata().GetFilterMetadata()[envoyRouteMetadataNamespace].GetFields()
	assert.Equal(t, "/openai/chat/completions", fields[envoyRouteHTTPRouteKey].GetStringValue())
	assert.Equal(t, "LlmProvider", fields[envoyRouteAPIKindKey].GetStringValue())

	empty := &route.Route{}
	setRouteAPIKind(empty, "")
	assert.Nil(t, empty.Metadata, "an empty kind must not create a metadata shell")
}

func TestCreateLLMLocalReplyConfig(t *testing.T) {
	cfg, err := createLLMLocalReplyConfig()
	require.NoError(t, err)
	require.NoError(t, cfg.Validate())
	require.Len(t, cfg.Mappers, 1)

	mapper := cfg.Mappers[0]
	filters := mapper.GetFilter().GetAndFilter().GetFilters()
	require.Len(t, filters, 2, "mapper must require both an upstream-failure flag and an LLM route")
	assert.Contains(t, filters[0].GetResponseFlagFilter().GetFlags(), "UH")
	assert.Contains(t, filters[0].GetResponseFlagFilter().GetFlags(), "UT")
	assert.Contains(t, filters[0].GetResponseFlagFilter().GetFlags(), "NC",
		"configuration faults still surface during an invocation")
	assert.Equal(t, "envoy.access_loggers.extension_filters.cel", filters[1].GetExtensionFilter().GetName())
	assert.Equal(t,
		`xds.route_metadata.filter_metadata["wso2.route"]["api_kind"] in ["LlmProvider", "LlmProxy"]`,
		llmRouteCELExpression)

	assert.Nil(t, mapper.StatusCode, "the mapper must preserve Envoy's original status")

	format := mapper.GetBodyFormatOverride()
	assert.Equal(t, "application/json", format.GetContentType())
	errObj := format.GetJsonFormat().GetFields()["error"].GetStructValue().GetFields()
	assert.Equal(t, "%LOCAL_REPLY_BODY%", errObj["message"].GetStringValue())
	assert.Equal(t, "server_error", errObj["type"].GetStringValue())
	for _, key := range []string{"param", "code"} {
		require.Contains(t, errObj, key)
		_, isNull := errObj[key].GetKind().(*structpb.Value_NullValue)
		assert.True(t, isNull, "%s must be JSON null", key)
	}
}

func TestLLMNotFoundRoutes(t *testing.T) {
	routes, err := llmNotFoundRoutes(nil)
	require.NoError(t, err)
	assert.Empty(t, routes)

	routes, err = llmNotFoundRoutes(map[string]struct{}{"/openai": {}, "/openai/v2": {}, "/anthropic": {}})
	require.NoError(t, err)
	require.Len(t, routes, 3)

	// Nested contexts first, so /openai/v2 is not shadowed by /openai.
	idx := map[string]int{}
	for i, r := range routes {
		idx[r.GetMatch().GetPathSeparatedPrefix()] = i
	}
	assert.Less(t, idx["/openai/v2"], idx["/openai"])
	for _, r := range routes {
		require.NoError(t, r.Validate())
		dr := r.GetDirectResponse()
		assert.Equal(t, uint32(404), dr.GetStatus())
		var body struct {
			Error struct {
				Message string  `json:"message"`
				Type    string  `json:"type"`
				Param   *string `json:"param"`
				Code    *string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(dr.GetBody().GetInlineString()), &body))
		assert.Equal(t, "not_found_error", body.Error.Type)
		assert.NotEmpty(t, body.Error.Message)
	}
}

type staticRDCTransformer struct{ rdc *models.RuntimeDeployConfig }

func (s staticRDCTransformer) Transform(*models.StoredConfig) (*models.RuntimeDeployConfig, error) {
	return s.rdc, nil
}

func TestTranslateConfigs_LLMErrorsAreOpenAIFormat(t *testing.T) {
	translator := NewTranslator(createTestLogger(), testRouterConfig(), nil, testConfig())
	routeKey := "POST|/openai/chat/completions|*"
	translator.SetTransformers(map[string]models.ConfigTransformer{
		string(models.KindLlmProvider): staticRDCTransformer{rdc: &models.RuntimeDeployConfig{
			Metadata: models.Metadata{Kind: string(models.KindLlmProvider), Handle: "openai", Version: "v1"},
			Context:  "/openai",
			Routes: map[string]*models.Route{
				routeKey: {
					Method:        "POST",
					Path:          "/openai/chat/completions",
					OperationPath: "/chat/completions",
					Vhost:         "*",
					Upstream:      models.RouteUpstream{ClusterKey: "upstream_openai"},
				},
			},
		}},
	})

	resources, err := translator.TranslateConfigs([]*models.StoredConfig{{
		UUID:         "llm-1",
		Kind:         string(models.KindLlmProvider),
		DesiredState: models.StateDeployed,
	}}, "test-correlation-id")
	require.NoError(t, err)

	// The LLM route carries its kind, and an OpenAI 404 catch-all for its context
	// sits after it and before the vhost-wide no-api-found route.
	var checkedVHost bool
	for _, res := range resources[resource.RouteType] {
		for _, vh := range res.(*route.RouteConfiguration).GetVirtualHosts() {
			apiIdx, llmIdx, catchAllIdx := -1, -1, -1
			for i, r := range vh.Routes {
				switch r.Name {
				case routeKey:
					apiIdx = i
					kind := r.GetMetadata().GetFilterMetadata()[envoyRouteMetadataNamespace].GetFields()[envoyRouteAPIKindKey]
					assert.Equal(t, "LlmProvider", kind.GetStringValue())
				case "llm-no-route-found:/openai":
					llmIdx = i
				case "no-api-found":
					catchAllIdx = i
				}
			}
			if apiIdx == -1 {
				continue
			}
			checkedVHost = true
			require.NotEqual(t, -1, llmIdx, "vhost %q missing the LLM not-found route", vh.Name)
			assert.Less(t, apiIdx, llmIdx)
			assert.Less(t, llmIdx, catchAllIdx)
		}
	}
	assert.True(t, checkedVHost, "LLM route not found in any virtual host")

	// Every HTTP connection manager maps Envoy's upstream-failure replies.
	var checkedHCM bool
	for _, res := range resources[resource.ListenerType] {
		for _, fc := range res.(*listener.Listener).GetFilterChains() {
			for _, f := range fc.GetFilters() {
				if f.GetName() != wellknown.HTTPConnectionManager {
					continue
				}
				manager := &hcm.HttpConnectionManager{}
				require.NoError(t, f.GetTypedConfig().UnmarshalTo(manager))
				require.NotNil(t, manager.GetLocalReplyConfig())
				require.Len(t, manager.GetLocalReplyConfig().GetMappers(), 1)
				checkedHCM = true
			}
		}
	}
	assert.True(t, checkedHCM, "no HTTP connection manager found")
}

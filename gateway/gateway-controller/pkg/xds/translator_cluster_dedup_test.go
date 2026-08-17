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

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/require"
	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

func TestResolveOrCreateUpstreamDefinitionCluster_SameNameReturnsSameCluster(t *testing.T) {
	tr := NewTranslator(createTestLogger(), testRouterConfig(), nil, testConfig())
	seen := map[string]*cluster.Cluster{}
	def := api.UpstreamDefinition{
		Name: "azure-eastus",
		Upstreams: []struct {
			Url    string `json:"url" yaml:"url"`
			Weight *int   `json:"weight,omitempty" yaml:"weight,omitempty"`
		}{{Url: "http://sample-backend:5000"}},
	}
	name1, err := tr.resolveOrCreateUpstreamDefinitionCluster("azure-eastus", def, "LlmProvider", "abc-123", seen)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	name2, err := tr.resolveOrCreateUpstreamDefinitionCluster("azure-eastus", def, "LlmProvider", "abc-123", seen)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if name1 != name2 {
		t.Errorf("expected the same cluster name on repeat resolution, got %q then %q", name1, name2)
	}
	if len(seen) != 1 {
		t.Errorf("expected exactly one cluster registered in seen, got %d", len(seen))
	}
}

// TestTranslateConfigs_MainRefAndNamedDefinitionShareOneCluster proves the fix at the
// resource level: an API whose main upstream references a named upstreamDefinition by
// `ref`, where that same definition also appears in upstreamDefinitions (the live scenario
// this task fixes — previously two independent code paths each built their own cluster for
// the identical backend), must produce exactly ONE Envoy cluster for that target.
func TestTranslateConfigs_MainRefAndNamedDefinitionShareOneCluster(t *testing.T) {
	logger := createTestLogger()
	translator := NewTranslator(logger, testRouterConfig(), nil, testConfig())

	ref := "azure-eastus"
	cfg := makeRestAPIWithUpstreamRefAndDefinitions("uuid-dedup-1", "api-dedup", "/api-dedup", ref,
		[]api.UpstreamDefinition{
			{
				Name: ref,
				Upstreams: []struct {
					Url    string `json:"url" yaml:"url"`
					Weight *int   `json:"weight,omitempty" yaml:"weight,omitempty"`
				}{{Url: "http://sample-backend:5000"}},
			},
		})

	resources, err := translator.TranslateConfigs([]*models.StoredConfig{cfg}, "test-correlation-id")
	require.NoError(t, err)

	matching := 0
	for _, res := range resources[resource.ClusterType] {
		c, ok := res.(*cluster.Cluster)
		require.True(t, ok)
		if c.Name == "upstream_RestApi_uuid-dedup-1_"+ref {
			matching++
		}
	}
	require.Equal(t, 1, matching, "expected exactly one cluster for the shared upstream definition, got %d", matching)
}

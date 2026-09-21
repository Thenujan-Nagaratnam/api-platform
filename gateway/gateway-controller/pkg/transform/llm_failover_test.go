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

package transform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	policyenginev1 "github.com/wso2/api-platform/sdk/core/policyengine"
)

// TestResolveFailoverEntry_AllThreeWaysOfNamingThePrimaryProvider covers the
// three ways a validated failover chain member can refer to the proxy's own
// primary provider — omitting `provider`, and explicitly naming it either as the
// primary's own id or (bundled here per the fix report) as an additionalProvider's
// bare .Id with no .As set — and confirms all resolve to a correct entry rather
// than a translate-time error. Regression test for the fix that made an explicit
// `provider: <primary's own id>` (accepted by parseModelFailoverParams, which
// seeds the primary's own id into its allowed set) fall through to the
// named-cluster scan instead of the primary path, where it could never be found
// (the primary's own cluster is stored with an empty Name).
func TestResolveFailoverEntry_AllThreeWaysOfNamingThePrimaryProvider(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		UpstreamClusters: map[string]*models.UpstreamCluster{
			"upstream_main_openai_com_443": {
				Name:      "", // primary/main slot cluster — empty Name, not a usable lookup key
				BasePath:  "/",
				Endpoints: []models.Endpoint{{Host: "openai.com", Port: 443}},
				TLS:       &models.UpstreamTLS{Enabled: true},
			},
			"upstream_cohere-provider_cohere_com_443": {
				Name:      "cohere-provider", // additionalProvider with no .As — bare .Id is the lookup key
				BasePath:  "/",
				Endpoints: []models.Endpoint{{Host: "cohere.com", Port: 443}},
				TLS:       &models.UpstreamTLS{Enabled: true},
			},
		},
	}
	r := &models.Route{
		Upstream: models.RouteUpstream{
			ClusterKey: "upstream_main_openai_com_443",
			Default: &policyenginev1.UpstreamInfo{
				ClusterName: "upstream_main_openai_com_443",
				URL:         "https://openai.com",
				BasePath:    "/",
			},
		},
	}
	const primaryProviderID = "openai-provider"

	t.Run("provider omitted", func(t *testing.T) {
		entry, err := resolveFailoverEntry(rdc, r, modelFailoverTarget{Model: "gpt-4o"}, primaryProviderID)
		require.NoError(t, err)
		assert.Equal(t, "upstream_main_openai_com_443", entry.ClusterKey)
		assert.Equal(t, primaryProviderID, entry.Provider)
	})

	t.Run("provider explicitly names the primary's own id", func(t *testing.T) {
		entry, err := resolveFailoverEntry(rdc, r, modelFailoverTarget{Model: "gpt-4o", Provider: primaryProviderID}, primaryProviderID)
		require.NoError(t, err)
		assert.Equal(t, "upstream_main_openai_com_443", entry.ClusterKey)
		assert.Equal(t, primaryProviderID, entry.Provider)
	})

	t.Run("provider names an additionalProvider's bare id with no as set", func(t *testing.T) {
		entry, err := resolveFailoverEntry(rdc, r, modelFailoverTarget{Model: "command-r", Provider: "cohere-provider"}, primaryProviderID)
		require.NoError(t, err)
		assert.Equal(t, "upstream_cohere-provider_cohere_com_443", entry.ClusterKey)
		assert.Equal(t, "cohere-provider", entry.Provider)
	})
}

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

package xdsclient

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFailoverTargets_AbsentKey_ReturnsNil(t *testing.T) {
	assert.Nil(t, parseFailoverTargets(map[string]interface{}{}))
}

func TestParseFailoverTargets_DecodesChainInPriorityOrder(t *testing.T) {
	// Shape mirrors gateway-controller's policyxds/snapshot.go
	// failoverEntryToMap/createRouteConfigResource output exactly.
	data := map[string]interface{}{
		"failover_targets": []interface{}{
			map[string]interface{}{
				"aggregate_cluster": "failover_agg_chat_0",
				"model":             "gpt-4o",
				"chain": []interface{}{
					map[string]interface{}{
						"cluster_name": "openai-provider-cluster",
						"url":          "https://api.openai.com/v1",
						"base_path":    "/v1",
						"model":        "gpt-4o",
						"provider":     "openai-provider",
					},
					map[string]interface{}{
						"cluster_name": "anthropic-upstream-cluster",
						"url":          "https://api.anthropic.com/v1",
						"base_path":    "/v1",
						"model":        "claude-3-5-sonnet-20241022",
						"provider":     "anthropic-upstream",
					},
				},
			},
		},
	}

	targets := parseFailoverTargets(data)

	require.Len(t, targets, 1)
	target := targets[0]
	assert.Equal(t, "failover_agg_chat_0", target.AggregateCluster)
	assert.Equal(t, "gpt-4o", target.Model)
	require.Len(t, target.Chain, 2)

	assert.Equal(t, "gpt-4o", target.Chain[0].Model)
	assert.Equal(t, "openai-provider", target.Chain[0].Provider)
	assert.Equal(t, "openai-provider-cluster", target.Chain[0].Upstream.ClusterName)
	assert.Equal(t, "https://api.openai.com/v1", target.Chain[0].Upstream.URL)

	assert.Equal(t, "claude-3-5-sonnet-20241022", target.Chain[1].Model)
	assert.Equal(t, "anthropic-upstream", target.Chain[1].Provider)
	assert.Equal(t, "anthropic-upstream-cluster", target.Chain[1].Upstream.ClusterName)
}

func TestParseFailoverTargets_MultipleTargetsEachOwnChain(t *testing.T) {
	data := map[string]interface{}{
		"failover_targets": []interface{}{
			map[string]interface{}{
				"aggregate_cluster": "failover_agg_chat_0",
				"model":             "gpt-4o",
				"chain": []interface{}{
					map[string]interface{}{"cluster_name": "openai-cluster", "model": "gpt-4o", "provider": "openai-provider"},
					map[string]interface{}{"cluster_name": "anthropic-cluster", "model": "claude-3-5-sonnet-20241022", "provider": "anthropic-upstream"},
				},
			},
			map[string]interface{}{
				"aggregate_cluster": "failover_agg_chat_1",
				"model":             "gpt-4o-mini",
				"chain": []interface{}{
					map[string]interface{}{"cluster_name": "openai-cluster", "model": "gpt-3.5-turbo", "provider": "openai-provider"},
					map[string]interface{}{"cluster_name": "anthropic-cluster", "model": "claude-3-5-haiku-20241022", "provider": "anthropic-upstream"},
				},
			},
		},
	}

	targets := parseFailoverTargets(data)

	require.Len(t, targets, 2)
	assert.Equal(t, "gpt-4o", targets[0].Model)
	assert.Equal(t, "gpt-4o-mini", targets[1].Model)
	assert.Equal(t, "gpt-3.5-turbo", targets[1].Chain[0].Model)
}

func TestParseFailoverTargets_MalformedEntrySkipped(t *testing.T) {
	data := map[string]interface{}{
		"failover_targets": []interface{}{
			"not-a-map",
			map[string]interface{}{"aggregate_cluster": "failover_agg_chat_0"}, // no "chain" key
			map[string]interface{}{
				"aggregate_cluster": "failover_agg_chat_1",
				"model":             "gpt-4o",
				"chain": []interface{}{
					map[string]interface{}{"cluster_name": "openai-cluster", "model": "gpt-4o", "provider": "openai-provider"},
				},
			},
		},
	}

	targets := parseFailoverTargets(data)

	require.Len(t, targets, 1)
	assert.Equal(t, "failover_agg_chat_1", targets[0].AggregateCluster)
}

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

	"github.com/stretchr/testify/assert"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

func TestClusterNeedsUpstreamPolicyFilter_NoRoutesReferenceCluster(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		Routes:       map[string]*models.Route{},
		PolicyChains: map[string]*models.PolicyChain{},
	}

	assert.False(t, clusterNeedsUpstreamPolicyFilter("main", rdc))
}

func TestClusterNeedsUpstreamPolicyFilter_RouteWithoutUpstreamPhasePolicy(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		Routes: map[string]*models.Route{
			"route-1": {Upstream: models.RouteUpstream{ClusterKey: "main"}},
		},
		PolicyChains: map[string]*models.PolicyChain{
			"route-1": {Policies: []models.Policy{{Name: "prompt-decorator"}}},
		},
	}

	assert.False(t, clusterNeedsUpstreamPolicyFilter("main", rdc))
}

func TestClusterNeedsUpstreamPolicyFilter_RouteWithUpstreamPhasePolicy(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		Routes: map[string]*models.Route{
			"route-1": {Upstream: models.RouteUpstream{ClusterKey: "bedrock-fallback"}},
		},
		PolicyChains: map[string]*models.PolicyChain{
			"route-1": {Policies: []models.Policy{{Name: "aws-authentication"}}},
		},
	}

	assert.True(t, clusterNeedsUpstreamPolicyFilter("bedrock-fallback", rdc))
}

func TestClusterNeedsUpstreamPolicyFilter_UsesCanonicalChainKeyWhenSet(t *testing.T) {
	rdc := &models.RuntimeDeployConfig{
		Routes: map[string]*models.Route{
			"route-1": {
				Upstream:          models.RouteUpstream{ClusterKey: "bedrock-fallback"},
				CanonicalChainKey: "shared-chain",
			},
		},
		PolicyChains: map[string]*models.PolicyChain{
			"shared-chain": {Policies: []models.Policy{{Name: "aws-authentication"}}},
			// A stale entry keyed by the route itself must NOT be consulted
			// once CanonicalChainKey redirects elsewhere.
			"route-1": {Policies: []models.Policy{{Name: "prompt-decorator"}}},
		},
	}

	assert.True(t, clusterNeedsUpstreamPolicyFilter("bedrock-fallback", rdc))
}

func TestClusterNeedsUpstreamPolicyFilter_ORsAcrossSharedCluster(t *testing.T) {
	// Two routes share one cluster (deduped, per the KB precedent on shared
	// clusters); only one of them has the upstream-phase policy attached.
	// The cluster must still get the filter — a per-cluster attachment can't
	// be scoped tighter than "any route that might land here needs it".
	rdc := &models.RuntimeDeployConfig{
		Routes: map[string]*models.Route{
			"route-1": {Upstream: models.RouteUpstream{ClusterKey: "shared-cluster"}},
			"route-2": {Upstream: models.RouteUpstream{ClusterKey: "shared-cluster"}},
		},
		PolicyChains: map[string]*models.PolicyChain{
			"route-1": {Policies: []models.Policy{{Name: "prompt-decorator"}}},
			"route-2": {Policies: []models.Policy{{Name: "aws-authentication"}}},
		},
	}

	assert.True(t, clusterNeedsUpstreamPolicyFilter("shared-cluster", rdc))
}

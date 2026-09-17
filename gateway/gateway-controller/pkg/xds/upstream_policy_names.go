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

import "github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"

// upstreamPhasePolicyNames lists policies that implement the upstream-attempt
// SDK interfaces (policy.UpstreamRequestPolicy / UpstreamResponsePolicy) and
// therefore need the per-cluster upstream ext_proc filter attached to any
// backend cluster they can run against.
//
// gateway-controller does not load policy Go implementations (that's
// gateway-runtime's job), so it cannot ask a policy's Mode() directly — this
// static list is the only signal available here, and it is a deliberate,
// narrow allowlist rather than every policy that plausibly could opt in.
//
// Keep this in lockstep with which policies actually implement the upstream
// interfaces: add a name here in the same change that migrates that policy
// (see aws-authentication.OnUpstreamRequestBody for the first one), not ahead
// of it — an entry here for a policy that hasn't migrated yet would attach an
// upstream filter that finds nothing to run, which is harmless but pointless.
var upstreamPhasePolicyNames = map[string]bool{
	"aws-authentication": true,
}

// isUpstreamPhasePolicy reports whether name is known to implement an
// upstream-attempt SDK interface.
func isUpstreamPhasePolicy(name string) bool {
	return upstreamPhasePolicyNames[name]
}

// resolveChainForRoute returns the PolicyChain a route actually uses:
// CanonicalChainKey when set (a route pointed at a composed/shared operation
// chain), otherwise the route's own key.
func resolveChainForRoute(routeKey string, r *models.Route, rdc *models.RuntimeDeployConfig) *models.PolicyChain {
	chainKey := routeKey
	if r.CanonicalChainKey != "" {
		chainKey = r.CanonicalChainKey
	}
	return rdc.PolicyChains[chainKey]
}

// clusterNeedsUpstreamPolicyFilter reports whether ANY route that can
// dispatch to clusterName has an upstream-phase policy in its chain. Clusters
// are shared/deduped across routes and APIs, so this is an OR across every
// referencing route — a per-cluster attachment can't be scoped any tighter
// than "some route landing here might need it".
func clusterNeedsUpstreamPolicyFilter(clusterName string, rdc *models.RuntimeDeployConfig) bool {
	for routeKey, r := range rdc.Routes {
		if r.Upstream.ClusterKey != clusterName {
			continue
		}
		chain := resolveChainForRoute(routeKey, r, rdc)
		if chain == nil {
			continue
		}
		for _, p := range chain.Policies {
			if isUpstreamPhasePolicy(p.Name) {
				return true
			}
		}
	}
	return false
}

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

// upstreamPhasePolicyNames is a legacy allowlist from when these policies
// self-declared upstream-attempt participation natively, without an
// explicit upstreamPolicies: attachment (see the LlmProvider/LlmProxy
// schema). That native-participation mechanism has been removed from
// gateway-runtime — a policy now runs per upstream attempt only when
// attached via upstreamPolicies: (which sets Policy.Upstream, already
// checked by clusterNeedsUpstreamPolicyFilter's caller independent of this
// list). Matching a name here now only ever attaches an upstream filter that
// finds nothing to run — harmless, but this list has no live purpose and is
// kept only until it's removed along with its call site.
var upstreamPhasePolicyNames = map[string]bool{
	"aws-authentication":              true,
	"openai-to-anthropic-transformer": true,
	"oauth2-generator":                true,
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
//
// A route's failover chain member clusters (RouteFailover.Targets[].Target/
// Fallbacks[].ClusterKey) are checked here too, not just the route's own
// default Upstream.ClusterKey — confirmed live: when the primary attempt is
// currently suspended, the model-failover policy's downstream OnRequestBody
// dispatches straight to a fallback's own real cluster, bypassing the
// aggregate entirely. buildFailoverAggregateClusters unconditionally
// attaches the filter to the AGGREGATE cluster for the normal retry-via-
// aggregate path, but that attachment covers only requests that actually go
// through the aggregate — the direct-bypass dispatch needs the SAME member
// cluster to carry its own attachment too, or the transformer/auth policies
// silently never run for it.
func clusterNeedsUpstreamPolicyFilter(clusterName string, rdc *models.RuntimeDeployConfig) bool {
	for routeKey, r := range rdc.Routes {
		matchesRoute := r.Upstream.ClusterKey == clusterName
		if !matchesRoute && r.Upstream.Failover != nil {
			for _, target := range r.Upstream.Failover.Targets {
				if target.Target.ClusterKey == clusterName {
					matchesRoute = true
					break
				}
				for _, fb := range target.Fallbacks {
					if fb.ClusterKey == clusterName {
						matchesRoute = true
						break
					}
				}
				if matchesRoute {
					break
				}
			}
		}
		if !matchesRoute {
			continue
		}
		chain := resolveChainForRoute(routeKey, r, rdc)
		if chain == nil {
			continue
		}
		for _, p := range chain.Policies {
			// p.Upstream: attached via upstreamPolicies, so it runs upstream regardless of
			// whether its name is in the native-upstream allowlist.
			if p.Upstream || isUpstreamPhasePolicy(p.Name) {
				return true
			}
		}
	}
	return false
}

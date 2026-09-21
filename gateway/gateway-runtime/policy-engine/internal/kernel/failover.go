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

package kernel

import policyenginev1 "github.com/wso2/api-platform/sdk/core/policyengine"

// FailoverChainEntry is one resolvable slot in a failover chain: either the
// declared target itself or one of its ordered fallbacks. Mirrors
// gateway-controller's RouteFailoverEntry — the shape synced over xDS as one
// element of a "chain" array (see policyxds/snapshot.go's failoverEntryToMap).
type FailoverChainEntry struct {
	// Model is the model name this attempt should send upstream — not
	// necessarily the client's originally-requested model (a fallback may
	// target a different model on a different provider).
	Model string

	// Provider identifies which provider this entry belongs to: the
	// LlmProxy's primary provider id, or an additionalProviders[].as/.id.
	// Never empty — gateway-controller always resolves an omitted `provider`
	// to the primary's own id before this ships over xDS.
	Provider string

	// Upstream is this entry's real backend: cluster name, URL, base path.
	Upstream policyenginev1.UpstreamInfo
}

// FailoverTarget is one client-requested model's own failover chain, as
// declared under one resilience.failover.targets[] entry. AggregateCluster is
// the envoy.clusters.aggregate cluster gateway-controller built for this
// entry — Envoy reports this same name via xds.cluster_name on every attempt
// against it, regardless of which real member it actually dialed (confirmed
// live this session), which is what makes it the correct lookup key.
type FailoverTarget struct {
	AggregateCluster string
	Model            string

	// Chain holds the target followed by its fallbacks, in priority order:
	// Chain[0] is the declared target (attempt 1 / x-envoy-attempt-count
	// missing), Chain[1:] are the fallbacks (attempt 2, 3, ...).
	Chain []FailoverChainEntry
}

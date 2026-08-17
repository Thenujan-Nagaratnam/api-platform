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

package policyv1alpha2

import (
	"strings"
	"time"
)

// RetryTarget is one ordered destination within a single RetryGroup.
// UpstreamDefinitionName must resolve to a registered upstreamDefinition on
// the resource the declaring policy is attached to — the same primitive
// plain upstreamDefinitions already provide, not a new one. An empty
// UpstreamDefinitionName means "this API's own main upstream", matching the
// existing convention plain upstreamDefinition-based routing already uses.
type RetryTarget struct {
	UpstreamDefinitionName string
}

// RetryGroup is one independently-selectable failover chain within a route
// (e.g. model-failover: one per client-selectable model, chosen at runtime
// by the declaring policy's own request-body matching logic). Key is an
// opaque, policy-chosen discriminator used only for deterministic cluster
// naming via RetrySourceUpstreamName below — translator.go never interprets
// it. Key must be stable across deploys and unique within one
// RetrySourceDeclaration's Groups.
type RetryGroup struct {
	Key string

	// OrderedTargets: index 0 is tried first, index 1 on the first failure,
	// and so on. Must have at least 2 entries for gateway-controller to
	// build an aggregate cluster for this group — a single-entry group is
	// legal (means "route this target, no failover") but produces no
	// aggregate cluster, matching today's zero-fallback model-failover
	// target-group behavior.
	OrderedTargets []RetryTarget
}

// RetrySourceDeclaration is the generic contract gateway-controller builds
// one aggregate cluster PER Group from, regardless of which policy declared
// it. Which group applies to a given request is entirely the declaring
// policy's own runtime decision (e.g. its own OnRequestBody setting
// UpstreamName via RetrySourceUpstreamName) — gateway-controller never
// needs to know why there are multiple groups, or how one gets selected,
// only that there are some.
type RetrySourceDeclaration struct {
	// Groups must be non-empty.
	Groups []RetryGroup

	// PerAttemptTimeout bounds a single attempt; nil uses the route's
	// existing default. A retry-source policy's own status-code/On/etc.
	// contribution is NOT a field here — it flows through the exact same
	// x-wso2-retry-conditions declaration path every other policy uses
	// (see gateway-controller's resolveRetryDeclarations, which parses a
	// chain member's retry-source metadata and its retry-conditions
	// metadata independently, in the same loop pass). A dedicated
	// Conditions field on this type would just be a second, redundant path
	// to the same merged result.
	PerAttemptTimeout *time.Duration
}

// retrySourceTargetPrefix marks a logical upstream name as belonging to
// this mechanism, reserved-looking so it can't collide with a real,
// operator-declared UpstreamDefinition.Name that happens to equal a group's
// own Key (e.g. an upstreamDefinition literally named "gpt-4o").
const retrySourceTargetPrefix = "__retry_source_target__"

// RetrySourceUpstreamName is the canonical formula for a RetryGroup's
// logical upstream name — the single source of truth both
// gateway-controller (building the aggregate cluster, see
// RetrySourceAggregateClusterKey) and a retry-source-capable policy's own
// runtime code (setting UpstreamName at runtime to redirect into that same
// cluster) call identically. Living in the SDK — which every policy
// already imports, including ones in separate repos from
// gateway-controller — replaces what was previously a formula manually
// duplicated across repos with a comment asking developers to keep both
// copies in sync.
//
// routeKey is "METHOD|PATH|VHOST[|DISCRIMINATOR]" and may contain "|",
// which is not valid in an Envoy cluster name component; it's replaced
// with "_" here. An empty routeKey (a policy that doesn't need per-route
// disambiguation) omits the routeKey segment entirely rather than leaving
// a stray separator.
func RetrySourceUpstreamName(routeKey, groupKey string) string {
	if routeKey == "" {
		return retrySourceTargetPrefix + groupKey
	}
	safeRouteKey := strings.ReplaceAll(routeKey, "|", "_")
	return retrySourceTargetPrefix + safeRouteKey + "__" + groupKey
}

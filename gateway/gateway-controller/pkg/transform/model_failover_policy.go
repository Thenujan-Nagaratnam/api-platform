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
	"encoding/json"
	"fmt"
	"strings"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/xds"
)

const modelFailoverPolicyName = "model-failover"

// modelFailoverTarget/modelFailoverTargetEntry/modelFailoverParams mirror the
// policy's own ModelFailoverParams/FailoverTargetEntry/FailoverTarget shape
// (gateway/dev-policies/model-failover) — hand-written here because
// gateway-controller cannot import the policy's Go module. Keep field names
// and JSON tags in lockstep with it.
type modelFailoverTarget struct {
	Model    string `json:"model"`
	Provider string `json:"provider,omitempty"`
	// UpstreamDefinition is author-facing: the name of the upstream this
	// member actually dials (a named additionalProviders[].as/.id, or any
	// hand-declared upstreamDefinitions[].name — resolveFailoverEntry's lookup
	// doesn't distinguish their origin). Optional; when empty, the dial target
	// defaults to Provider (today's behavior). Kept separate from Provider so
	// a member's credential/transform identity and its physical backend can
	// differ.
	UpstreamDefinition string `json:"upstreamDefinition,omitempty"`
	// BasePath is injected by the controller (never authored): the resolved
	// upstream base path of the cluster this member dials. Every member of a
	// chain on an LlmProxy is a loopback upstream on the SAME host:port, so
	// auto_host_rewrite makes :authority identical across attempts and the
	// base path in :path is the only thing that distinguishes one provider's
	// loopback route from another's. Any author-supplied value is overwritten
	// by buildRouteFailoverFromPolicy.
	BasePath string `json:"basePath,omitempty"`
	// ClusterName is injected by the controller (never authored): the real
	// Envoy cluster this member dials. It is what the policy compares
	// UpstreamRequestContext.RouteCluster against when an attempt did NOT come
	// through the chain's aggregate — the suspended-primary bypass dispatches
	// straight onto a fallback's own cluster, so Envoy reports that cluster's
	// name and the aggregate-name match finds nothing. Any author-supplied
	// value is overwritten by buildRouteFailoverFromPolicy.
	ClusterName string `json:"clusterName,omitempty"`
}

// modelFailoverTargetEntry embeds modelFailoverTarget (not nested under a
// "target" key) so the entry's own model/provider/upstreamDefinition sit at
// the same JSON level as each entry in Fallbacks.
type modelFailoverTargetEntry struct {
	modelFailoverTarget
	Fallbacks        []modelFailoverTarget `json:"fallbacks"`
	AggregateCluster string                `json:"aggregateCluster,omitempty"`
}

type modelFailoverParams struct {
	Targets         []modelFailoverTargetEntry `json:"targets"`
	SuspendDuration int                        `json:"suspendDuration"`
	// StatusCodes is the set of response status codes that trigger escalation
	// to the next chain member (and Envoy's own retry_policy escalation).
	// Empty/omitted defaults to "any 5xx"; when set, it REPLACES that default
	// rather than extending it.
	StatusCodes []int `json:"statusCodes,omitempty"`
	// PrimaryProvider is injected by the controller so the policy can resolve
	// members authored without `provider:` to the primary provider identity.
	PrimaryProvider string `json:"primaryProvider,omitempty"`
	// OperationPath is injected by the controller (never authored): the
	// route's own operation-relative path. Combined with a member's BasePath
	// it yields that member's correct outbound :path, which the policy uses to
	// re-point a retry that escalated to a different provider's loopback route
	// (Envoy replays the first attempt's :path verbatim on a retry — unlike
	// Host, it is not recomputed per attempt). Any author-supplied value is
	// overwritten by buildRouteFailoverFromPolicy.
	OperationPath string `json:"operationPath,omitempty"`
}

// parseModelFailoverParams parses raw policy params and validates that every
// provider reference is the primary provider or one of availableProviders
// (additionalProviders[].as/.id), so bad config is rejected before any xDS
// generation runs.
func parseModelFailoverParams(raw map[string]interface{}, availableProviders []string, primaryProviderID string) (*modelFailoverParams, error) {
	blob, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal model-failover params: %w", err)
	}
	var params modelFailoverParams
	if err := json.Unmarshal(blob, &params); err != nil {
		return nil, fmt.Errorf("failed to parse model-failover params: %w", err)
	}
	if len(params.Targets) == 0 {
		return nil, fmt.Errorf("model-failover: 'targets' must have at least one entry")
	}

	allowed := make(map[string]bool, len(availableProviders)+1)
	allowed[primaryProviderID] = true
	for _, p := range availableProviders {
		allowed[p] = true
	}

	validateProvider := func(t modelFailoverTarget) error {
		provider := strings.TrimSpace(t.Provider)
		if provider == "" {
			provider = primaryProviderID
		}
		if !allowed[provider] {
			return fmt.Errorf("model-failover: provider %q does not match any additionalProviders or the primary provider", provider)
		}
		return nil
	}

	for _, entry := range params.Targets {
		if err := validateProvider(entry.modelFailoverTarget); err != nil {
			return nil, err
		}
		for _, fb := range entry.Fallbacks {
			if err := validateProvider(fb); err != nil {
				return nil, err
			}
		}
	}

	for _, code := range params.StatusCodes {
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("model-failover: statusCodes entry %d is not a valid HTTP status code", code)
		}
	}

	return &params, nil
}

// buildRouteFailoverFromPolicy resolves a model-failover policy's params into
// the *models.RouteFailover the existing xDS generation consumes (reusing
// resolveFailoverEntry), and returns a copy of params with each entry's
// AggregateCluster set via xds.AggregateClusterName(routeKey, index) — the
// copy the policy instance carries at runtime. The input params are not
// mutated. retryOn defaults to ["5xx"] and RetriableStatusCodes is empty;
// when the author configures statusCodes, retryOn becomes
// ["retriable-status-codes"] and RetriableStatusCodes carries the exact list —
// REPLACING the default, not extending it (design's post-implementation
// correction to §5's original "hardcoded, not configurable" claim).
func buildRouteFailoverFromPolicy(rdc *models.RuntimeDeployConfig, r *models.Route, params *modelFailoverParams, routeKey, primaryProviderID string) (*models.RouteFailover, *modelFailoverParams, error) {
	if params == nil || len(params.Targets) == 0 {
		return nil, nil, fmt.Errorf("model-failover: no targets to build failover from")
	}

	expanded := &modelFailoverParams{
		SuspendDuration: params.SuspendDuration,
		StatusCodes:     params.StatusCodes,
		PrimaryProvider: primaryProviderID,
		OperationPath:   r.OperationPath,
		Targets:         make([]modelFailoverTargetEntry, len(params.Targets)),
	}
	targets := make([]models.RouteFailoverTarget, 0, len(params.Targets))
	for i, entry := range params.Targets {
		targetEntry, err := resolveFailoverEntry(rdc, r, entry.modelFailoverTarget, primaryProviderID)
		if err != nil {
			return nil, nil, fmt.Errorf("route %q: resolving failover target %q: %w", routeKey, entry.Model, err)
		}
		fallbacks := make([]models.RouteFailoverEntry, 0, len(entry.Fallbacks))
		expandedFallbacks := make([]modelFailoverTarget, 0, len(entry.Fallbacks))
		for _, fb := range entry.Fallbacks {
			fbEntry, err := resolveFailoverEntry(rdc, r, fb, primaryProviderID)
			if err != nil {
				return nil, nil, fmt.Errorf("route %q: resolving failover fallback %q: %w", routeKey, fb.Model, err)
			}
			fallbacks = append(fallbacks, fbEntry)
			fb.BasePath = fbEntry.Upstream.BasePath
			fb.ClusterName = fbEntry.Upstream.ClusterName
			expandedFallbacks = append(expandedFallbacks, fb)
		}
		targets = append(targets, models.RouteFailoverTarget{
			Model:     entry.Model,
			Target:    targetEntry,
			Fallbacks: fallbacks,
		})

		expandedTarget := entry.modelFailoverTarget
		expandedTarget.BasePath = targetEntry.Upstream.BasePath
		expandedTarget.ClusterName = targetEntry.Upstream.ClusterName
		expanded.Targets[i] = modelFailoverTargetEntry{
			modelFailoverTarget: expandedTarget,
			Fallbacks:           expandedFallbacks,
			AggregateCluster:    xds.AggregateClusterName(routeKey, i),
		}
	}

	// retryOn/retriableStatusCodes: statusCodes REPLACES the "any 5xx" default
	// rather than extending it — see modelFailoverParams.StatusCodes.
	retryOn := []string{"5xx"}
	var retriableStatusCodes []int
	if len(params.StatusCodes) > 0 {
		retryOn = []string{"retriable-status-codes"}
		retriableStatusCodes = params.StatusCodes
	}

	return &models.RouteFailover{
		SuspendDurationSeconds: params.SuspendDuration,
		Targets:                targets,
		RetryOn:                retryOn,
		RetriableStatusCodes:   retriableStatusCodes,
	}, expanded, nil
}

// llmProxyProviderIdentities returns every provider identity a model-failover
// chain may legally reference on this proxy: each additionalProviders entry's
// `as` (or `id` when `as` is omitted). The primary is added separately by
// parseModelFailoverParams.
func llmProxyProviderIdentities(proxy *api.LLMProxyConfiguration) []string {
	if proxy.Spec.AdditionalProviders == nil {
		return nil
	}
	names := make([]string, 0, len(*proxy.Spec.AdditionalProviders))
	for _, ap := range *proxy.Spec.AdditionalProviders {
		name := ap.Id
		if ap.As != nil && *ap.As != "" {
			name = *ap.As
		}
		names = append(names, name)
	}
	return names
}

// applyModelFailoverPolicyToRoutes resolves every route whose policy chain
// carries a model-failover attachment. It is the one and only trigger for
// failover resolution — a route without the attachment is left untouched.
//
// Two things happen per resolved route, both required by the design's §3/§5:
//
//   - the resolved models.RouteFailover is written onto the route, which is the
//     unchanged trigger for aggregate-cluster/retry_policy xDS generation — the
//     policy attachment replaced the removed resilience.failover schema field as
//     the *trigger*, not the generation itself;
//   - the controller-assigned aggregate cluster name is injected back into every
//     model-failover instance in that route's chain (the downstream one and the
//     synthesized upstream one alike), so the policy only ever string-compares a
//     name it was handed instead of recomputing one.
//
// Params are merged key-wise rather than replaced so keys the chain builder
// added (attachedTo) survive.
func applyModelFailoverPolicyToRoutes(rdc *models.RuntimeDeployConfig, availableProviders []string,
	primaryProviderID string) error {
	for routeKey, r := range rdc.Routes {
		chain := rdc.PolicyChains[rdc.EffectiveCanonicalChainKey(routeKey, r)]
		if chain == nil {
			continue
		}
		var instances []*models.Policy
		for i := range chain.Policies {
			if chain.Policies[i].Name == modelFailoverPolicyName {
				instances = append(instances, &chain.Policies[i])
			}
		}
		if len(instances) == 0 {
			continue
		}

		params, err := parseModelFailoverParams(instances[0].Params, availableProviders, primaryProviderID)
		if err != nil {
			return fmt.Errorf("route %q: %w", routeKey, err)
		}
		failover, expanded, err := buildRouteFailoverFromPolicy(rdc, r, params, routeKey, primaryProviderID)
		if err != nil {
			return err
		}
		expandedParams, err := modelFailoverParamsToMap(expanded)
		if err != nil {
			return fmt.Errorf("route %q: %w", routeKey, err)
		}

		r.Upstream.Failover = failover
		if !r.Upstream.UseClusterHeader {
			r.Upstream.UseClusterHeader = true
			r.Upstream.DefaultCluster = r.Upstream.ClusterKey
		}
		for _, instance := range instances {
			if instance.Params == nil {
				instance.Params = map[string]interface{}{}
			}
			for k, v := range expandedParams {
				instance.Params[k] = v
			}
		}
	}
	return nil
}

// modelFailoverParamsToMap renders the expanded params back into the generic
// map the policy instance carries on the wire. It round-trips through JSON so
// the emitted keys are exactly the policy's own JSON tags — the same contract
// parseModelFailoverParams reads back.
func modelFailoverParamsToMap(params *modelFailoverParams) (map[string]interface{}, error) {
	blob, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal expanded model-failover params: %w", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(blob, &out); err != nil {
		return nil, fmt.Errorf("failed to render expanded model-failover params: %w", err)
	}
	return out, nil
}

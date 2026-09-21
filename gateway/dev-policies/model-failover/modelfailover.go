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

// Package modelfailover implements cross-provider LLM model failover: retry
// a request against a declared fallback chain of {model, provider} targets
// on upstream failure, with correct per-attempt credential injection and
// payload translation. See docs/superpowers/specs/2026-09-21-model-failover-policy-design.md.
package modelfailover

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

const PolicyName = "model-failover"

const (
	selectedProviderMetadataKey = "selected_provider"
	selectedModelMetadataKey    = "selected_model"
)

// ResolvedFailoverProviderHeader mirrors the header name the (now-removed)
// kernel-side mechanism used, so downstream analytics attribution keeps
// working unchanged. See kernel.ResolvedFailoverProviderHeader's own history.
const ResolvedFailoverProviderHeader = "x-wso2-resolved-failover-provider"

var (
	_ policy.Policy        = (*Policy)(nil)
	_ policy.RequestPolicy = (*Policy)(nil)

	_ policy.RequestHeaderPolicy  = (*Policy)(nil)
	_ policy.ResponseHeaderPolicy = (*Policy)(nil)
)

// attemptIndexMetadataKey carries this attempt's 1-based chain index from the
// request-header phase to the response-header phase of the same attempt: the
// response context has no request headers, so x-envoy-attempt-count is not
// otherwise visible there. SharedContext is shared across both phases of one
// attempt.
const attemptIndexMetadataKey = "model_failover_attempt_index"

// FailoverTarget identifies one chain member.
type FailoverTarget struct {
	Model    string `json:"model"`
	Provider string `json:"provider,omitempty"`
}

// FailoverTargetEntry is one client-requested-model's primary + fallback chain.
type FailoverTargetEntry struct {
	Target    FailoverTarget   `json:"target"`
	Fallbacks []FailoverTarget `json:"fallbacks"`
	// AggregateCluster is injected by gateway-controller — see this plan's
	// shared params contract.
	AggregateCluster string `json:"aggregateCluster"`
}

// ModelFailoverParams is the parsed shape of this policy's params.
type ModelFailoverParams struct {
	Targets         []FailoverTargetEntry `json:"targets"`
	SuspendDuration int                   `json:"suspendDuration"`
	// PrimaryProvider is injected by gateway-controller: the identity a member
	// authored without `provider:` resolves to (the primary provider ID). Used
	// for selected_provider metadata and suspension keys; never as a cluster name.
	PrimaryProvider string `json:"primaryProvider,omitempty"`
}

// Policy implements downstream target selection and, per upstream attempt,
// chain-position resolution + provider/model metadata seeding + suspension.
type Policy struct {
	params ModelFailoverParams

	mu               sync.Mutex
	suspendedTargets map[string]time.Time
}

// GetPolicy is the v1alpha2 factory entry point.
func GetPolicy(_ policy.PolicyMetadata, rawParams map[string]interface{}) (policy.Policy, error) {
	params, err := parseParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid params: %w", PolicyName, err)
	}
	return &Policy{params: params, suspendedTargets: make(map[string]time.Time)}, nil
}

func parseParams(raw map[string]interface{}) (ModelFailoverParams, error) {
	var params ModelFailoverParams

	targetsRaw, ok := raw["targets"]
	if !ok {
		return params, fmt.Errorf("'targets' is required")
	}

	// Round-trip through JSON to reuse encoding/json's struct-tag-driven
	// decoding instead of hand-walking map[string]interface{} — the params
	// map already came from a JSON-shaped source (policy config), so this
	// is a safe, standard pattern for this kind of nested parsing.
	blob, err := json.Marshal(map[string]interface{}{"targets": targetsRaw})
	if err != nil {
		return params, fmt.Errorf("failed to marshal 'targets': %w", err)
	}
	if err := json.Unmarshal(blob, &params); err != nil {
		return params, fmt.Errorf("failed to parse 'targets': %w", err)
	}
	if len(params.Targets) == 0 {
		return params, fmt.Errorf("'targets' must have at least one entry")
	}
	if pp, ok := raw["primaryProvider"].(string); ok {
		params.PrimaryProvider = pp
	}

	if suspendRaw, ok := raw["suspendDuration"]; ok {
		switch v := suspendRaw.(type) {
		case float64:
			params.SuspendDuration = int(v)
		case int:
			params.SuspendDuration = v
		default:
			return params, fmt.Errorf("'suspendDuration' must be a number")
		}
	}

	return params, nil
}

// Mode declares participation in both the downstream body phase (target
// selection) and — when this instance is separately attached via
// upstreamPolicies: — the upstream-attempt request/response header phases
// (chain resolution, metadata seeding, suspension recording). See
// docs/superpowers/specs/2026-09-21-upstream-policy-reuse-design.md: there is
// no separate upstream mode/interface — attachment point alone selects the
// phase, and RequestHeaderMode/ResponseHeaderMode below govern whichever
// phase this specific instance actually runs in (Downstream == nil tells the
// two apart inside the handlers).
func (p *Policy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeProcess,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeProcess,
		ResponseBodyMode:   policy.BodyModeSkip,
	}
}

// requestedModel extracts the OpenAI-shaped "model" field from a JSON request body.
func requestedModel(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Model)
}

// findEntry returns the target entry whose Target.Model matches the
// requested model, or nil.
func (p *Policy) findEntry(model string) *FailoverTargetEntry {
	for i := range p.params.Targets {
		if strings.EqualFold(p.params.Targets[i].Target.Model, model) {
			return &p.params.Targets[i]
		}
	}
	return nil
}

// suspensionKey matches model-round-robin's own key shape
// (routeKey+model+provider is unavailable here without threading routeKey
// through GetPolicy — this policy instance is already route-scoped by
// attachment, the same way model-round-robin's own instance is, so the key
// only needs model+provider).
func suspensionKey(model, provider string) string {
	return model + "|" + provider
}

// resolvedProvider returns the member's provider identity, defaulting an
// empty (primary-authored) provider to PrimaryProvider. Do not use for
// cluster-name routing.
func (p *Policy) resolvedProvider(m FailoverTarget) string {
	if m.Provider == "" {
		return p.params.PrimaryProvider
	}
	return m.Provider
}

// isSuspended checks and lazily clears an expired suspension entry — same
// check-and-delete-if-expired pattern model-round-robin uses (no background
// sweep).
func (p *Policy) isSuspended(model, provider string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := suspensionKey(model, provider)
	until, ok := p.suspendedTargets[key]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(p.suspendedTargets, key)
	return false
}

func (p *Policy) suspend(model, provider string) {
	if p.params.SuspendDuration <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.suspendedTargets[suspensionKey(model, provider)] = time.Now().Add(time.Duration(p.params.SuspendDuration) * time.Second)
}

// OnRequestBody parses the client-requested model, matches it against the
// configured chain, and routes to that chain's aggregate cluster — skipping
// straight to the first non-suspended fallback's own single cluster if the
// primary target is currently suspended (bypassing the aggregate for a
// known-bad primary, per the design's §4).
func (p *Policy) OnRequestBody(_ context.Context, reqCtx *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
	if reqCtx.Body == nil || !reqCtx.Body.Present || len(reqCtx.Body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	model := requestedModel(reqCtx.Body.Content)
	if model == "" {
		return policy.UpstreamRequestModifications{}
	}

	entry := p.findEntry(model)
	if entry == nil {
		return policy.UpstreamRequestModifications{}
	}

	// The controller injects AggregateCluster; if it is absent there is no
	// chain to route into, so use normal routing rather than pointing
	// UpstreamName at "".
	if entry.AggregateCluster == "" {
		return policy.UpstreamRequestModifications{}
	}

	if !p.isSuspended(entry.Target.Model, p.resolvedProvider(entry.Target)) {
		cluster := entry.AggregateCluster
		return policy.UpstreamRequestModifications{UpstreamName: &cluster}
	}

	for _, fallback := range entry.Fallbacks {
		if fallback.Provider == "" {
			continue // no addressable upstream for this fallback
		}
		if !p.isSuspended(fallback.Model, p.resolvedProvider(fallback)) {
			// Route directly to the fallback's own upstream, bypassing the
			// aggregate entirely — a known-bad primary is skipped, at the
			// cost of no further in-request retry if this fallback also
			// fails (the aggregate was never entered). See design doc's
			// discussion of this tradeoff (inherited from the superseded
			// design's §5.3/§10).
			upstream := fallback.Provider
			return policy.UpstreamRequestModifications{UpstreamName: &upstream}
		}
	}

	// Every member suspended — fall through to the aggregate anyway (Envoy's
	// own retry exhaustion behavior applies; nothing left to skip to).
	cluster := entry.AggregateCluster
	return policy.UpstreamRequestModifications{UpstreamName: &cluster}
}

// findEntryByAggregateCluster returns the target entry whose AggregateCluster
// matches cluster, or nil.
func (p *Policy) findEntryByAggregateCluster(cluster string) *FailoverTargetEntry {
	if cluster == "" {
		return nil
	}
	for i := range p.params.Targets {
		if p.params.Targets[i].AggregateCluster == cluster {
			return &p.params.Targets[i]
		}
	}
	return nil
}

// attemptCount reads x-envoy-attempt-count, defaulting to 1 (the primary
// attempt) for a missing or unparseable header.
func attemptCount(headers *policy.Headers) int {
	if headers == nil {
		return 1
	}
	values := headers.Get("x-envoy-attempt-count")
	if len(values) == 0 {
		return 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil || n <= 0 {
		return 1
	}
	return n
}

// resolveAttempt returns the chain member for the 1-based attempt index
// (1 = primary target, 2 = Fallbacks[0], ...), or nil past the chain's end.
func resolveAttempt(entry *FailoverTargetEntry, index int) *FailoverTarget {
	if index <= 1 {
		return &entry.Target
	}
	fallbackIdx := index - 2
	if fallbackIdx >= len(entry.Fallbacks) {
		return nil
	}
	return &entry.Fallbacks[fallbackIdx]
}

// OnRequestHeaders is meaningful only for an upstream-attempt invocation on a
// cluster this instance's chain owns; otherwise a no-op. It resolves the chain
// member for this attempt and seeds selected_provider/selected_model metadata
// (plus the attempt index for the response phase).
func (p *Policy) OnRequestHeaders(_ context.Context, reqCtx *policy.RequestHeaderContext, _ map[string]interface{}) policy.RequestHeaderAction {
	if reqCtx.Downstream != nil || reqCtx.Upstream == nil {
		return nil
	}
	entry := p.findEntryByAggregateCluster(reqCtx.Upstream.RouteCluster)
	if entry == nil {
		return nil
	}

	index := attemptCount(reqCtx.Headers)
	member := resolveAttempt(entry, index)
	if member == nil {
		return nil
	}

	if reqCtx.SharedContext.Metadata == nil {
		reqCtx.SharedContext.Metadata = map[string]interface{}{}
	}
	reqCtx.SharedContext.Metadata[selectedModelMetadataKey] = member.Model
	reqCtx.SharedContext.Metadata[selectedProviderMetadataKey] = p.resolvedProvider(*member)
	reqCtx.SharedContext.Metadata[attemptIndexMetadataKey] = index

	return nil
}

// responseAttemptIndex recovers the attempt index in the response phase: from
// metadata written by OnRequestHeaders, else request headers if present, else 1.
func responseAttemptIndex(respCtx *policy.ResponseHeaderContext) int {
	if respCtx.SharedContext != nil {
		if n, ok := respCtx.SharedContext.Metadata[attemptIndexMetadataKey].(int); ok && n > 0 {
			return n
		}
	}
	return attemptCount(respCtx.RequestHeaders)
}

// OnResponseHeaders records suspension for a failing attempt (any 5xx) and,
// for an attempt that escalated past the primary, sets
// ResolvedFailoverProviderHeader for downstream analytics attribution.
func (p *Policy) OnResponseHeaders(_ context.Context, respCtx *policy.ResponseHeaderContext, _ map[string]interface{}) policy.ResponseHeaderAction {
	if respCtx.Downstream != nil || respCtx.Upstream == nil {
		return nil
	}
	entry := p.findEntryByAggregateCluster(respCtx.Upstream.RouteCluster)
	if entry == nil {
		return nil
	}

	index := responseAttemptIndex(respCtx)
	member := resolveAttempt(entry, index)
	if member == nil {
		return nil
	}

	if respCtx.ResponseStatus >= 500 {
		p.suspend(member.Model, p.resolvedProvider(*member))
	}

	if index > 1 {
		return policy.DownstreamResponseHeaderModifications{
			HeadersToSet: map[string]string{ResolvedFailoverProviderHeader: p.resolvedProvider(*member)},
		}
	}
	return nil
}

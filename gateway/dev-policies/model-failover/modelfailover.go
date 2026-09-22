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
	// BasePath is injected by gateway-controller: the resolved upstream base
	// path of the cluster this member dials. Every member of a chain is a
	// loopback upstream on the SAME host:port, so auto_host_rewrite leaves
	// :authority identical across attempts and this base path inside :path is
	// the only thing distinguishing one provider's loopback route from
	// another's. See OnRequestHeaders' per-attempt :path correction.
	BasePath string `json:"basePath,omitempty"`
	// ClusterName is injected by gateway-controller: the real Envoy cluster
	// this member dials. It is the fallback match for an attempt that did NOT
	// arrive through the chain's aggregate — the suspended-primary bypass in
	// OnRequestBody dispatches straight at a fallback's own cluster, so Envoy
	// reports that cluster via xds.cluster_name and the aggregate-name
	// comparison finds nothing. See resolveAttemptForCluster.
	ClusterName string `json:"clusterName,omitempty"`
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
	// OperationPath is injected by gateway-controller: the route's own
	// operation-relative path (e.g. "/chat/completions"). Joined with a
	// member's BasePath it yields that member's correct outbound :path.
	OperationPath string `json:"operationPath,omitempty"`
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
	if op, ok := raw["operationPath"].(string); ok {
		params.OperationPath = op
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
			//
			// Nothing about this choice is seeded into metadata here: the
			// upstream phase gets its own fresh SharedContext, so
			// OnRequestHeaders re-identifies this member from the cluster
			// Envoy reports (resolveAttemptForCluster's member-cluster
			// match) and seeds it there, exactly as for an aggregate-routed
			// attempt. Seeding downstream instead would be both too late for
			// the downstream header-phase credential injection and wrong for
			// the body phase — a provider's translator would then run
			// downstream AND again upstream, on its own output.
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

// chainMatch is one resolved upstream attempt: which chain entry it belongs
// to, which member of that chain it dials, and that member's 1-based position
// in the chain (1 = the entry's own target, 2 = Fallbacks[0], ...).
type chainMatch struct {
	entry  *FailoverTargetEntry
	member *FailoverTarget
	index  int
}

// resolveAttemptForCluster identifies which chain member an upstream attempt
// represents, from the cluster Envoy reported plus that attempt's count.
//
// Two dispatch shapes reach the upstream phase, and they are distinguished by
// the cluster name alone:
//
//   - Through the chain's AGGREGATE cluster (the normal path). Envoy reports
//     the aggregate's own name on every attempt against it, never the real
//     member it dialed, so x-envoy-attempt-count is what selects the member.
//   - Directly onto a MEMBER's own cluster. OnRequestBody does this when the
//     primary is suspended, skipping the aggregate entirely. The cluster name
//     identifies the member exactly, and the attempt count is meaningless here
//     (a retry re-dials that same single cluster), so it is ignored — matching
//     what the superseded kernel-side resolution did for this same case.
//
// Only fallbacks are matched by cluster name. A chain's primary member dials
// the route's own default cluster, which is also where an ordinary request
// whose model matches no chain lands; matching it would mean seeding a chain's
// model/provider onto a request that never entered that chain. The bypass only
// ever dispatches at a fallback, so nothing needs the primary matched this way.
//
// Known limitation: if two entries list the SAME provider as a fallback under
// different models, a bypass onto that shared cluster matches the first
// declared one, so selected_model can name the other entry's model. The
// provider — which is what gates credential/transform policies — is correct
// either way, and the upstream phase has no request body to disambiguate with.
func (p *Policy) resolveAttemptForCluster(cluster string, attempt int) *chainMatch {
	if cluster == "" {
		return nil
	}
	if entry := p.findEntryByAggregateCluster(cluster); entry != nil {
		member := resolveAttempt(entry, attempt)
		if member == nil {
			return nil
		}
		return &chainMatch{entry: entry, member: member, index: attempt}
	}
	for i := range p.params.Targets {
		entry := &p.params.Targets[i]
		for j := range entry.Fallbacks {
			// An empty ClusterName means the controller injected nothing for
			// this member; never let it match an attempt.
			if entry.Fallbacks[j].ClusterName == "" || entry.Fallbacks[j].ClusterName != cluster {
				continue
			}
			return &chainMatch{entry: entry, member: &entry.Fallbacks[j], index: j + 2}
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

// joinBasePathAndOperation combines a chain member's base path with the
// route's operation-relative path (e.g. "/anthropic-provider" +
// "/chat/completions" -> "/anthropic-provider/chat/completions"), normalizing
// the separator so neither a missing nor a doubled slash can occur regardless
// of how either piece was stored (a root base path of "/" or "" must not
// produce "//chat/completions"). Returns "" when operationPath is empty —
// nothing to rewrite to, so the caller leaves :path untouched rather than
// clobbering it. Deliberately identical to the kernel's own helper of the same
// name, which produced these paths before failover moved into this policy.
func joinBasePathAndOperation(basePath, operationPath string) string {
	if operationPath == "" {
		return ""
	}
	base := strings.TrimSuffix(basePath, "/")
	op := operationPath
	if !strings.HasPrefix(op, "/") {
		op = "/" + op
	}
	return base + op
}

// OnRequestHeaders is meaningful only for an upstream-attempt invocation on a
// cluster this instance's chain owns — the chain's aggregate, or a fallback's
// own cluster when the suspended-primary bypass dispatched straight at it;
// otherwise a no-op. It resolves the chain member for this attempt, seeds
// selected_provider/selected_model metadata (plus the chain index for the
// response phase), tells the kernel which base path this attempt actually
// dials, and corrects a stale :path left over from an earlier attempt.
//
// The metadata seeding is what makes the per-provider credential/transform
// policies work: the controller attaches one upstream-attempt instance of each
// per referenced provider, every one gated by a CEL condition of the form
// `'selected_provider' in request.Metadata && request.Metadata['selected_provider'] == '<id>'`
// against this attempt's own SharedContext. Each attempt gets a FRESH
// SharedContext (kernel.NewUpstreamAttemptSharedContext), so nothing the
// downstream phase wrote carries in and this is the only thing that populates
// those keys. Seed nothing and every one of those conditions is false: the
// attempt reaches its provider with no credential injected and an untranslated
// body. Ordering holds because the controller emits this instance ahead of
// them in the upstream chain (llm_transformer.go Step 3.6 / Phase 2 vs 3).
func (p *Policy) OnRequestHeaders(_ context.Context, reqCtx *policy.RequestHeaderContext, _ map[string]interface{}) policy.RequestHeaderAction {
	if reqCtx.Downstream != nil || reqCtx.Upstream == nil {
		return nil
	}
	match := p.resolveAttemptForCluster(reqCtx.Upstream.RouteCluster, attemptCount(reqCtx.Headers))
	if match == nil {
		return nil
	}
	member, index := match.member, match.index

	if reqCtx.SharedContext == nil {
		reqCtx.SharedContext = &policy.SharedContext{}
	}
	if reqCtx.SharedContext.Metadata == nil {
		reqCtx.SharedContext.Metadata = map[string]interface{}{}
	}
	reqCtx.SharedContext.Metadata[selectedModelMetadataKey] = member.Model
	reqCtx.SharedContext.Metadata[selectedProviderMetadataKey] = p.resolvedProvider(*member)
	reqCtx.SharedContext.Metadata[attemptIndexMetadataKey] = index

	// The kernel resolves an attempt's backend by xds.cluster_name, which for
	// an aggregate-routed attempt is the aggregate's own name and therefore
	// resolves nothing. This policy does know which member this attempt is,
	// so hand its base path back: the kernel adopts it for the rest of this
	// attempt, and every later policy sees a correct Upstream.BasePath.
	if member.BasePath != "" {
		reqCtx.Upstream.BasePath = member.BasePath
	}

	// The outbound :path was rewritten exactly once, downstream, before Envoy
	// ever dispatched — using the PRIMARY member's base path. Envoy replays
	// that same :path verbatim on a retry; unlike Host (recomputed per attempt
	// by auto_host_rewrite) it is never recalculated. Since every chain member
	// is a loopback upstream on the same host:port, :path is the ONLY thing
	// that selects one provider's route over another's, so an escalation to a
	// different provider must correct it or the loopback listener sends the
	// retry straight back to the provider that just failed.
	//
	// An empty BasePath means the controller supplied nothing for this member
	// (nothing to correct TO), so :path is left exactly as Envoy delivered it
	// rather than rewritten to a guessed root-relative path.
	if member.BasePath != "" {
		if corrected := joinBasePathAndOperation(member.BasePath, p.params.OperationPath); corrected != "" && corrected != reqCtx.Path {
			return policy.UpstreamRequestHeaderModifications{Path: &corrected}
		}
	}

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
// for an attempt that escalated past the primary — whether by Envoy retrying
// inside the aggregate or by the downstream bypass dispatching straight at a
// fallback — sets ResolvedFailoverProviderHeader for downstream analytics
// attribution.
func (p *Policy) OnResponseHeaders(_ context.Context, respCtx *policy.ResponseHeaderContext, _ map[string]interface{}) policy.ResponseHeaderAction {
	if respCtx.Downstream != nil || respCtx.Upstream == nil {
		return nil
	}
	match := p.resolveAttemptForCluster(respCtx.Upstream.RouteCluster, responseAttemptIndex(respCtx))
	if match == nil {
		return nil
	}
	member, index := match.member, match.index

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

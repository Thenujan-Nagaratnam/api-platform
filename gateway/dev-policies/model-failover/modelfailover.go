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
	"sort"
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
	// UpstreamDefinition is author-facing: the name of the upstream this
	// member actually dials (a named additionalProviders[].as/.id, or any
	// hand-declared upstreamDefinitions[].name — the controller's lookup
	// doesn't distinguish their origin). Optional; when empty, gateway-controller
	// resolves the dial target from Provider instead (today's default
	// behavior — same-named provider and dial target). Decoupled from
	// Provider so a member's credential/transform identity and its physical
	// backend can differ — e.g. reusing one provider's credentials against a
	// differently-named regional/load-balanced upstream.
	UpstreamDefinition string `json:"upstreamDefinition,omitempty"`
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

// FailoverTargetEntry is one client-requested-model's primary + fallback
// chain. FailoverTarget is embedded (not nested under a "target" key) so the
// entry's own model/provider/upstreamDefinition sit at the same JSON level as
// each entry in Fallbacks — the primary slot is authored exactly like a
// fallback slot, just without its own further fallbacks.
type FailoverTargetEntry struct {
	FailoverTarget
	Fallbacks []FailoverTarget `json:"fallbacks"`
	// AggregateCluster is injected by gateway-controller — see this plan's
	// shared params contract.
	AggregateCluster string `json:"aggregateCluster"`
}

// ModelFailoverParams is the parsed shape of this policy's params.
type ModelFailoverParams struct {
	Targets         []FailoverTargetEntry `json:"targets"`
	SuspendDuration int                   `json:"suspendDuration"`
	// StatusCodes is the set of response status codes that trigger escalation
	// to the next chain member and count as this target's failure for
	// suspension purposes. Empty/omitted defaults to "any 5xx" — this
	// policy's original, hardcoded behavior. When set, it REPLACES that
	// default entirely (it is not additive to "any 5xx"): list every code
	// that should trigger failover, 5xx codes included, if you still want them.
	StatusCodes []int `json:"statusCodes,omitempty"`
	// PrimaryProvider is injected by gateway-controller: the identity a member
	// authored without `provider:` resolves to (the primary provider ID). Used
	// for selected_provider metadata and suspension keys; never as a cluster name.
	PrimaryProvider string `json:"primaryProvider,omitempty"`
	// OperationPath is injected by gateway-controller: the route's own
	// operation-relative path (e.g. "/chat/completions"). Joined with a
	// member's BasePath it yields that member's correct outbound :path.
	OperationPath string `json:"operationPath,omitempty"`
	// SuspendAfterFailures requires this many CONSECUTIVE qualifying failures
	// (per isFailureStatus) for the same (model, provider) before suspending
	// it — a single success resets the counter to zero. Omitted/<=0 defaults
	// to 1 (suspend on the very first qualifying failure, this policy's
	// original behavior).
	SuspendAfterFailures int `json:"suspendAfterFailures,omitempty"`
	// MaxSuspendDuration caps exponential backoff (see backoffDuration): each
	// time a target is re-suspended immediately after a previous suspension
	// window expired with no intervening success, its suspend duration
	// doubles, up to this ceiling (seconds). Omitted/<=0 defaults to 8x
	// SuspendDuration.
	MaxSuspendDuration int `json:"maxSuspendDuration,omitempty"`
}

// Policy implements downstream target selection and, per upstream attempt,
// chain-position resolution + provider/model metadata seeding + suspension.
type Policy struct {
	params ModelFailoverParams
	susp   *suspensionState
}

// suspensionState is the in-process suspension bookkeeping for one
// model-failover chain attachment (one client-facing route). It exists
// separately from Policy so it can be SHARED between multiple *Policy
// instances of the same chain — see sharedSuspensionStateFor.
type suspensionState struct {
	mu        sync.Mutex
	suspended map[string]time.Time
	// failureCounts tracks CONSECUTIVE qualifying failures per key since the
	// last success, for SuspendAfterFailures thresholding. A success (or any
	// non-failure outcome) deletes the entry, exactly like isSuspended lazily
	// deletes an expired suspension — no background sweep either way.
	failureCounts map[string]int
	// suspensionStreak counts consecutive suspend-then-immediately-refail
	// cycles per key (a success resets it to zero), driving backoffDuration.
	suspensionStreak map[string]int
}

// sharedSuspensionRegistry maps an aggregate-cluster-derived key to the one
// suspensionState every *Policy instance for that chain shares.
//
// model-failover is attached TWICE per route: once downstream
// (operationPolicies:, decides routing) and once upstream (the
// controller-synthesized copy, resolves chain position and records
// suspension). registry.GetInstance creates a genuinely separate *Policy
// object — with its own state, no built-in caching — for EACH attachment,
// every time the policy chain is (re)built. Without this registry, a
// suspension the upstream instance records is invisible to the downstream
// instance's isSuspended check: the downstream call never bypasses a
// suspended primary at all, because it's asking a different object that
// never saw the failure. Keyed by aggregate cluster name(s) (controller-
// computed via xds.AggregateClusterName, unique per route) rather than by
// caching the whole *Policy: each GetPolicy call still gets fresh params
// (so a redeploy is picked up immediately), while suspension bookkeeping is
// shared with — and outlives — any one instance.
//
// This map is never evicted as routes are deleted; acceptable for now given
// suspension state is already documented as ephemeral/in-process-only (no
// cross-replica sharing), but worth revisiting if proxy churn in a long-
// running process becomes a real concern.
var (
	sharedSuspensionMu       sync.Mutex
	sharedSuspensionRegistry = map[string]*suspensionState{}
)

// sharedSuspensionKey derives the registry key from every target entry's
// AggregateCluster. Empty (no aggregate cluster injected — e.g. a hand-built
// Policy in a unit test) signals "don't share": the caller falls back to a
// private, unshared state instead of bucketing unrelated instances together.
func sharedSuspensionKey(params ModelFailoverParams) string {
	names := make([]string, 0, len(params.Targets))
	for _, t := range params.Targets {
		if t.AggregateCluster != "" {
			names = append(names, t.AggregateCluster)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func newSuspensionState() *suspensionState {
	return &suspensionState{
		suspended:        make(map[string]time.Time),
		failureCounts:    make(map[string]int),
		suspensionStreak: make(map[string]int),
	}
}

func sharedSuspensionStateFor(params ModelFailoverParams) *suspensionState {
	key := sharedSuspensionKey(params)
	if key == "" {
		return newSuspensionState()
	}
	sharedSuspensionMu.Lock()
	defer sharedSuspensionMu.Unlock()
	if s, ok := sharedSuspensionRegistry[key]; ok {
		return s
	}
	s := newSuspensionState()
	sharedSuspensionRegistry[key] = s
	return s
}

// GetPolicy is the v1alpha2 factory entry point.
func GetPolicy(_ policy.PolicyMetadata, rawParams map[string]interface{}) (policy.Policy, error) {
	params, err := parseParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid params: %w", PolicyName, err)
	}
	return &Policy{params: params, susp: sharedSuspensionStateFor(params)}, nil
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

	if params.SuspendDuration, err = parseOptionalInt(raw, "suspendDuration"); err != nil {
		return params, err
	}
	if params.SuspendAfterFailures, err = parseOptionalInt(raw, "suspendAfterFailures"); err != nil {
		return params, err
	}
	if params.MaxSuspendDuration, err = parseOptionalInt(raw, "maxSuspendDuration"); err != nil {
		return params, err
	}

	if codesRaw, ok := raw["statusCodes"]; ok {
		codes, ok := codesRaw.([]interface{})
		if !ok {
			return params, fmt.Errorf("'statusCodes' must be an array of numbers")
		}
		params.StatusCodes = make([]int, 0, len(codes))
		for _, c := range codes {
			n, ok := c.(float64)
			if !ok {
				return params, fmt.Errorf("'statusCodes' entries must be numbers")
			}
			params.StatusCodes = append(params.StatusCodes, int(n))
		}
	}

	return params, nil
}

// parseOptionalInt reads an optional numeric field from raw params, returning
// 0 if absent. Params arrive JSON-shaped, so a present numeric value decodes
// as float64.
func parseOptionalInt(raw map[string]interface{}, key string) (int, error) {
	v, ok := raw[key]
	if !ok {
		return 0, nil
	}
	switch n := v.(type) {
	case float64:
		return int(n), nil
	case int:
		return n, nil
	default:
		return 0, fmt.Errorf("'%s' must be a number", key)
	}
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

// findEntry returns the target entry whose own Model matches the requested
// model, or nil.
func (p *Policy) findEntry(model string) *FailoverTargetEntry {
	for i := range p.params.Targets {
		if strings.EqualFold(p.params.Targets[i].Model, model) {
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

// isFailureStatus reports whether a response status counts as this target's
// failure, for both suspension and (mirrored in gateway-controller's xDS
// generation) Envoy's own escalation decision. Defaults to "any 5xx" when
// StatusCodes is unset; when set, it replaces that default rather than
// extending it.
func (p *Policy) isFailureStatus(status int) bool {
	if len(p.params.StatusCodes) == 0 {
		return status >= 500 && status < 600
	}
	for _, code := range p.params.StatusCodes {
		if code == status {
			return true
		}
	}
	return false
}

// isSuspended checks and lazily clears an expired suspension entry — same
// check-and-delete-if-expired pattern model-round-robin uses (no background
// sweep).
func (p *Policy) isSuspended(model, provider string) bool {
	p.susp.mu.Lock()
	defer p.susp.mu.Unlock()
	key := suspensionKey(model, provider)
	until, ok := p.susp.suspended[key]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(p.susp.suspended, key)
	return false
}

// suspend immediately marks (model, provider) suspended for the base
// SuspendDuration, bypassing the SuspendAfterFailures threshold and backoff
// streak entirely — a direct, unconditional primitive kept for tests and for
// any future caller that wants "suspend this right now" without going
// through recordOutcome's bookkeeping.
func (p *Policy) suspend(model, provider string) {
	if p.params.SuspendDuration <= 0 {
		return
	}
	p.susp.mu.Lock()
	defer p.susp.mu.Unlock()
	p.susp.suspended[suspensionKey(model, provider)] = time.Now().Add(time.Duration(p.params.SuspendDuration) * time.Second)
}

// recordOutcome updates per-(model,provider) failure/backoff bookkeeping for
// one attempt's outcome. A success (failed == false) resets both the
// consecutive-failure counter and the backoff streak — a target that
// recovers even once starts the next incident fresh. A qualifying failure
// increments the counter; once it reaches SuspendAfterFailures (default 1,
// i.e. immediate), the target is suspended for backoffDuration's computed
// window and the streak advances, so a target that keeps failing
// immediately after each suspension expires gets progressively longer
// windows rather than flapping at a fixed interval forever.
func (p *Policy) recordOutcome(model, provider string, failed bool) {
	if p.params.SuspendDuration <= 0 {
		return // suspension disabled entirely; nothing to track
	}
	key := suspensionKey(model, provider)

	p.susp.mu.Lock()
	defer p.susp.mu.Unlock()
	if p.susp.failureCounts == nil {
		p.susp.failureCounts = map[string]int{}
	}
	if p.susp.suspensionStreak == nil {
		p.susp.suspensionStreak = map[string]int{}
	}

	if !failed {
		delete(p.susp.failureCounts, key)
		delete(p.susp.suspensionStreak, key)
		return
	}

	threshold := p.params.SuspendAfterFailures
	if threshold <= 0 {
		threshold = 1
	}
	p.susp.failureCounts[key]++
	if p.susp.failureCounts[key] < threshold {
		return
	}
	p.susp.failureCounts[key] = 0
	p.susp.suspensionStreak[key]++
	p.susp.suspended[key] = time.Now().Add(p.backoffDuration(p.susp.suspensionStreak[key]))
}

// backoffDuration computes the suspend window for the nth consecutive
// suspend-then-immediately-refail cycle of the same target (streak == 1 is
// the first cycle, i.e. exactly the base SuspendDuration). Each further
// cycle doubles the window, capped at MaxSuspendDuration (default 8x the
// base when unset/<=0) so a chronically broken target's window grows but
// never runs away unbounded.
func (p *Policy) backoffDuration(streak int) time.Duration {
	base := time.Duration(p.params.SuspendDuration) * time.Second
	if streak < 1 {
		streak = 1
	}
	shift := streak - 1
	if shift > 20 { // guards against a pathologically long streak overflowing the shift
		shift = 20
	}
	duration := base * time.Duration(int64(1)<<uint(shift))

	cap := time.Duration(p.params.MaxSuspendDuration) * time.Second
	if cap <= 0 {
		cap = base * 8
	}
	if duration > cap {
		duration = cap
	}
	return duration
}

// OnRequestBody's downstream invocation parses the client-requested model,
// matches it against the configured chain, and routes to that chain's
// aggregate cluster — skipping straight to the first non-suspended fallback's
// own single cluster if the primary target is currently suspended (bypassing
// the aggregate for a known-bad primary, per the design's §4). For an
// upstream-attempt invocation (Downstream == nil), it instead delegates to
// onUpstreamAttemptRequestBody — this instance is also attached via
// upstreamPolicies: (see OnRequestHeaders), and Mode()'s RequestBodyMode
// applies to both attachments, so this method is called for every attempt too.
func (p *Policy) OnRequestBody(_ context.Context, reqCtx *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
	if reqCtx.Downstream == nil {
		return p.onUpstreamAttemptRequestBody(reqCtx)
	}

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

	if !p.isSuspended(entry.Model, p.resolvedProvider(entry.FailoverTarget)) {
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

// onUpstreamAttemptRequestBody rewrites the replayed client body's "model"
// field to this attempt's resolved chain member, when that member's model
// differs from what's currently in the body.
//
// Envoy replays attempt 1's original body verbatim on every retry — unlike
// :path (corrected per attempt in OnRequestHeaders), nothing recomputes the
// body by default. A cross-provider fallback gets model translation "for
// free" from that provider's own transformer policy (e.g.
// openai-to-anthropic-transformer, which builds its outbound body from its
// own params.model, not from this policy's selected_model metadata) — but a
// same-provider fallback (no transformer in the chain at all) has nothing
// else to do this, so this policy must do it directly for that case.
//
// This uses the same cluster+attempt-count resolution as OnRequestHeaders
// (resolveAttemptForCluster) rather than reading back the selected_model
// metadata OnRequestHeaders already wrote: OnRequestHeaders and OnRequestBody
// both run once per attempt, but body-phase policies execute before
// header-phase modifications are guaranteed visible to a later body-phase
// call in the same chain, so re-resolving directly is the same pattern
// OnRequestHeaders/OnResponseHeaders already use rather than a new one.
func (p *Policy) onUpstreamAttemptRequestBody(reqCtx *policy.RequestContext) policy.RequestAction {
	if reqCtx.Upstream == nil {
		return policy.UpstreamRequestModifications{}
	}
	match := p.resolveAttemptForCluster(reqCtx.Upstream.RouteCluster, attemptCount(reqCtx.Headers))
	if match == nil {
		return policy.UpstreamRequestModifications{}
	}
	member := match.member
	if member.Model == "" || reqCtx.Body == nil || !reqCtx.Body.Present || len(reqCtx.Body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(reqCtx.Body.Content, &payload); err != nil {
		return policy.UpstreamRequestModifications{}
	}
	if current, _ := payload["model"].(string); current == member.Model {
		return policy.UpstreamRequestModifications{}
	}
	payload["model"] = member.Model

	newBody, err := json.Marshal(payload)
	if err != nil {
		return policy.UpstreamRequestModifications{}
	}
	return policy.UpstreamRequestModifications{Body: newBody}
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
		return &entry.FailoverTarget
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

// OnResponseHeaders records this attempt's outcome (isFailureStatus — any
// 5xx by default, or exactly the configured StatusCodes) via recordOutcome —
// a qualifying failure counts toward SuspendAfterFailures and, once that
// threshold is met, suspends the target for a backoff-computed window; a
// non-qualifying response resets that bookkeeping instead. Separately, for
// an attempt that escalated past the primary — whether by Envoy retrying
// inside the aggregate or by the downstream bypass dispatching straight at a
// fallback — it sets ResolvedFailoverProviderHeader for downstream analytics
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

	p.recordOutcome(member.Model, p.resolvedProvider(*member), p.isFailureStatus(int(respCtx.ResponseStatus)))

	if index > 1 {
		return policy.DownstreamResponseHeaderModifications{
			HeadersToSet: map[string]string{ResolvedFailoverProviderHeader: p.resolvedProvider(*member)},
		}
	}
	return nil
}

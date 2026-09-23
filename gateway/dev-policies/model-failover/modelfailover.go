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
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
	utils "github.com/wso2/api-platform/sdk/core/utils"
)

// circuitLogger returns the logger used for circuit-breaking state-transition
// events (suspend/close, with a "reason" field) — ECI #18469 finding #8. The
// SDK exposes no logger/metrics interface to policies (see
// sdk/core/policy/v1alpha2/context.go — AnalyticsMetadata is the only
// per-request telemetry surface, used separately in OnResponseHeaders), so
// this uses the standard library's default slog handler directly, exactly
// the way any other Go binary would when it isn't handed a logger. Package-
// level (not per-Policy) because the transition itself, not any one Policy
// instance, is what an operator needs to see.
func circuitLogger() *slog.Logger {
	return slog.Default().With("policy", PolicyName)
}

const PolicyName = "model-failover"

const (
	selectedProviderMetadataKey = "selected_provider"
	selectedModelMetadataKey    = "selected_model"
	// attemptStartMetadataKey stashes this attempt's OnRequestHeaders
	// timestamp so OnResponseHeaders can self-measure latency for the
	// latency circuit-breaking rule (LatencyThresholdMs) — the SDK exposes no
	// latency/duration field of its own (see context.go), and SharedContext
	// persists only within one attempt's own request->response round trip
	// (fresh again at the next attempt), so this key never leaks across
	// attempts or into the downstream phase.
	attemptStartMetadataKey = "model_failover_attempt_start"
)

// ResolvedFailoverProviderHeader mirrors the header name the (now-removed)
// kernel-side mechanism used, so downstream analytics attribution keeps
// working unchanged. See kernel.ResolvedFailoverProviderHeader's own history.
const ResolvedFailoverProviderHeader = "x-wso2-resolved-failover-provider"

// requestModel location values — see RequestModelConfig's doc comment.
const (
	requestModelLocationPayload    = "payload"
	requestModelLocationHeader     = "header"
	requestModelLocationQueryParam = "queryParam"
	requestModelLocationPathParam  = "pathParam"
)

// RequestModelConfig says where the client-requested model lives on the way
// in, and where a fallback's own resolved model must be written on the way
// out. Mirrors model-round-robin's own requestModel contract
// (github.com/wso2/gateway-controllers/policies/model-round-robin) field for
// field, rather than this policy's original, hardcoded assumption that the
// model always lives at the OpenAI-shaped top-level "model" JSON field.
type RequestModelConfig struct {
	// Location is one of requestModelLocationPayload (Identifier is a
	// JSONPath into the request body, e.g. "model" or "input.model"),
	// requestModelLocationHeader (Identifier is a header name),
	// requestModelLocationQueryParam (Identifier is a query parameter name),
	// or requestModelLocationPathParam (Identifier is a regexp with one
	// capture group identifying the path segment). Omitted defaults to
	// requestModelLocationPayload.
	Location string `json:"location,omitempty"`
	// Identifier's meaning depends on Location — see its doc comment.
	// Omitted defaults to "model" (only meaningful for the default payload
	// location).
	Identifier string `json:"identifier,omitempty"`
}

var (
	_ policy.Policy        = (*Policy)(nil)
	_ policy.RequestPolicy = (*Policy)(nil)

	_ policy.RequestHeaderPolicy  = (*Policy)(nil)
	_ policy.ResponseHeaderPolicy = (*Policy)(nil)
)

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
	// ClusterName is injected by gateway-controller: this member's own leaf
	// cluster. Once an attempt's entry point is known, the leaf Envoy
	// actually dialed (xds.upstream_host_metadata) is matched against it to
	// tell which member the attempt is — see resolveMemberWithinEntry. It is
	// never a dispatch target itself.
	ClusterName string `json:"clusterName,omitempty"`
	// SuffixCluster is injected by gateway-controller, for fallback members
	// only: a composite cluster covering this member and every member after
	// it (xds.SuffixCompositeClusterName). The suspended-prefix bypass in
	// OnRequestBody dispatches here, so a failure of this member can still
	// retry into the remaining chain within the same request. It is a second
	// valid "entry point" name alongside AggregateCluster — see
	// findEntryPointCluster.
	SuffixCluster string `json:"suffixCluster,omitempty"`
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

	// ─── Aggregate circuit criteria (ECI #18469 asks 3-4) ───
	// Additive to SuspendAfterFailures: any one trigger suspends the target
	// via the same backoffDuration escalation. Both rules evaluate over a
	// fixed 60s rolling window and need circuitMinimumSamples attempts in it
	// before they can fire (see the circuit* constants).

	// FailureRateThresholdPercent (1-100) suspends the target once its failure
	// rate across the rolling window reaches this value. <=0 (default)
	// disables the rate rule.
	FailureRateThresholdPercent int `json:"failureRateThresholdPercent,omitempty"`
	// LatencyThresholdMs suspends the target once the p95 of its attempt
	// latencies (this attempt's OnRequestHeaders to its OnResponseHeaders)
	// across the rolling window reaches this many milliseconds. <=0 (default)
	// disables the latency rule.
	LatencyThresholdMs int `json:"latencyThresholdMs,omitempty"`

	// RequestModel says where the client-requested model lives and where a
	// fallback's own resolved model must be written — see RequestModelConfig.
	// Omitted entirely defaults to {location: "payload", identifier: "model"},
	// this policy's original, unchanged behavior.
	RequestModel RequestModelConfig `json:"requestModel,omitempty"`
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
	// streakExpiresAt bounds how long a streak stays "hot" after its most
	// recent suspension window ends — see recordOutcome's doc comment for
	// why this exists (without it, two qualifying failures for the same key
	// hours apart would incorrectly compound backoff as if they were the
	// same incident).
	streakExpiresAt map[string]time.Time

	// rateWindows/latencyWindows hold the rolling-bucket state for the
	// aggregate/rolling circuit criteria (FailureRateThresholdPercent,
	// LatencyThresholdMs) — populated lazily, only for a key that has
	// actually recorded an outcome while the corresponding rule is enabled.
	rateWindows    map[string]*rollingWindow
	latencyWindows map[string]*rollingWindow
	// probes holds half-open controlled-recovery bookkeeping (see
	// allowRequest/completeProbe) — populated only while a key is actually in
	// a half-open recovery cycle (its suspension window already expired);
	// deleted again the moment that cycle closes or
	// reopens.
	//
	// Every map keyed by target identity in this struct (suspended,
	// failureCounts, ..., rateWindows, latencyWindows, probes) is naturally
	// bounded: keys come from this chain's own configured Targets/Fallbacks —
	// static policy config, not per-request user input — so there is no
	// per-key growth to bound here. The whole *suspensionState itself can
	// still be orphaned (its route deleted/redeployed away), which
	// idleEvictionHorizon/gcRegistryLocked handles at the registry level.
	probes map[string]*probeState

	// lastUsedUnixNano is stamped by every routing decision and every
	// recorded outcome, so registry eviction measures real traffic rather
	// than only redeploys. Atomic so gcRegistryLocked can read it under
	// sharedSuspensionMu without also taking this state's own mu.
	lastUsedUnixNano atomic.Int64
}

func (s *suspensionState) markUsed(now time.Time) {
	s.lastUsedUnixNano.Store(now.UnixNano())
}

// probeState is one target's half-open controlled-recovery bookkeeping:
// whether its single probe is in flight. See allowRequest/completeProbe.
type probeState struct {
	inFlight  bool
	grantedAt time.Time // when the in-flight probe was granted, for circuitProbeLease
}

// Fixed circuit-breaker tuning. Only the thresholds that decide WHEN a target
// is unhealthy (suspendAfterFailures, failureRateThresholdPercent,
// latencyThresholdMs) are author-facing; how the policy measures and recovers
// is not, so a route author never has to fill in a dozen knobs.
const (
	// circuitWindow is the rolling lookback for the rate and latency rules,
	// split into circuitBuckets equal buckets that rotate lazily.
	circuitWindow  = 60 * time.Second
	circuitBuckets = 10
	// circuitMinimumSamples is how many attempts the window needs before
	// either rule is evaluated, so one or two unlucky calls never suspend a
	// target. At 20, p95 is the second-slowest sample, so a single outlier
	// never trips the latency rule on its own.
	circuitMinimumSamples = 20
	// circuitLatencyPercentile is compared against LatencyThresholdMs.
	circuitLatencyPercentile = 95
	// circuitMaxSuspendMultiplier caps backoff at this multiple of SuspendDuration.
	circuitMaxSuspendMultiplier = 8
	// circuitProbeLease frees a half-open probe slot whose outcome never
	// arrived (a transport failure or client cancel never reaches
	// recordOutcome), so a lost probe can't hold a target half-open forever.
	circuitProbeLease = 60 * time.Second
)
const maxLatencySamplesPerBucket = 200 // bounds memory; ample for percentile accuracy at normal traffic

// rollingBucket is one time-slice of a rolling window: attempt/failure counts
// for the failure-rate rule and a bounded latency sample set for the latency
// rule. Both rules share one rolling window per key so a target's failure
// rate and its latency are always sampled from the exact same attempts.
type rollingBucket struct {
	start     time.Time
	attempts  int
	failures  int
	latencies []int // milliseconds
}

// rollingWindow is a ring of fixed-duration buckets, lazily rotated on each
// record — no background sweep, matching this file's existing
// isSuspended/streakExpiresAt lazy-expiry style.
type rollingWindow struct {
	bucketDuration time.Duration
	buckets        []rollingBucket
	cursor         int // index of the current (most recent) bucket
}

func newRollingWindow() *rollingWindow {
	bucketDuration := circuitWindow / circuitBuckets
	now := time.Now()
	buckets := make([]rollingBucket, circuitBuckets)
	for i := range buckets {
		buckets[i].start = now
	}
	return &rollingWindow{bucketDuration: bucketDuration, buckets: buckets}
}

// advance rotates the ring forward to the bucket "now" belongs in, clearing
// every bucket it passes through so a bucket that aged out never carries
// stale counts into the new window.
func (w *rollingWindow) advance(now time.Time) {
	elapsed := now.Sub(w.buckets[w.cursor].start)
	if elapsed < w.bucketDuration {
		return
	}
	steps := int(elapsed / w.bucketDuration)
	if steps > len(w.buckets) {
		steps = len(w.buckets) // never rotate more than one full lap
	}
	for i := 0; i < steps; i++ {
		w.cursor = (w.cursor + 1) % len(w.buckets)
		w.buckets[w.cursor] = rollingBucket{start: now}
	}
}

func (w *rollingWindow) recordOutcome(now time.Time, failed bool) {
	w.advance(now)
	b := &w.buckets[w.cursor]
	b.attempts++
	if failed {
		b.failures++
	}
}

func (w *rollingWindow) recordLatency(now time.Time, latencyMs int) {
	w.advance(now)
	b := &w.buckets[w.cursor]
	if len(b.latencies) < maxLatencySamplesPerBucket {
		b.latencies = append(b.latencies, latencyMs)
	}
}

// totals sums attempts/failures across every LIVE bucket. Callers that need
// this to reflect "now" must call advance(now) first (recordOutcome already
// does, on the same window, immediately before this is read).
func (w *rollingWindow) totals() (attempts, failures int) {
	for _, b := range w.buckets {
		attempts += b.attempts
		failures += b.failures
	}
	return attempts, failures
}

// latencyPercentile returns the given percentile across every LIVE bucket's
// latency samples, and false while the window holds fewer than minSamples.
// Callers that need this to reflect "now" must call advance(now) first
// (recordLatency does).
func (w *rollingWindow) latencyPercentile(percentile, minSamples int) (int, bool) {
	var all []int
	for _, b := range w.buckets {
		all = append(all, b.latencies...)
	}
	if len(all) == 0 || len(all) < minSamples {
		return 0, false
	}
	sort.Ints(all)
	// Nearest-rank: the smallest sample with at least percentile% of samples
	// at or below it.
	idx := (percentile*len(all)+99)/100 - 1
	if idx < 0 {
		idx = 0
	}
	return all[idx], true
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
// Entries are evicted by idle time (see idleEvictionHorizon/gcRegistryLocked)
// rather than precisely on route deletion — this package has no
// route-deletion hook to observe, only GetPolicy calls that BUILD a chain.
var (
	sharedSuspensionMu       sync.Mutex
	sharedSuspensionRegistry = map[string]*registryEntry{}
)

// registryEntry pairs a chain's shared suspensionState with the last time a
// GetPolicy call (a deploy or redeploy) touched it; the state itself carries
// the last time traffic used it. See lastActivity.
type registryEntry struct {
	state       *suspensionState
	lastTouched time.Time
}

// lastActivity is the later of the entry's last redeploy and its last
// routing decision or recorded outcome.
func (e *registryEntry) lastActivity() time.Time {
	last := e.lastTouched
	if used := e.state.lastUsedUnixNano.Load(); used > 0 {
		if t := time.Unix(0, used); t.After(last) {
			last = t
		}
	}
	return last
}

// idleEvictionHorizon bounds how long a chain's shared suspension state may
// sit with neither a redeploy nor any traffic before this registry treats it
// as abandoned and evicts it. There is no route-deletion hook available here
// to trigger eviction precisely, so a deleted route is detected by going
// quiet: it gets no more GetPolicy calls and no more requests. A live route
// under steady traffic is never evicted, however long since its last
// redeploy.
const idleEvictionHorizon = 24 * time.Hour

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
		streakExpiresAt:  make(map[string]time.Time),
		rateWindows:      make(map[string]*rollingWindow),
		latencyWindows:   make(map[string]*rollingWindow),
		probes:           make(map[string]*probeState),
	}
}

func sharedSuspensionStateFor(params ModelFailoverParams) *suspensionState {
	key := sharedSuspensionKey(params)
	if key == "" {
		return newSuspensionState()
	}
	sharedSuspensionMu.Lock()
	defer sharedSuspensionMu.Unlock()

	now := time.Now()
	gcRegistryLocked(now)

	if e, ok := sharedSuspensionRegistry[key]; ok {
		e.lastTouched = now
		return e.state
	}
	e := &registryEntry{state: newSuspensionState(), lastTouched: now}
	sharedSuspensionRegistry[key] = e
	return e.state
}

// gcRegistryLocked evicts every registry entry idle past idleEvictionHorizon.
// Caller must hold sharedSuspensionMu.
func gcRegistryLocked(now time.Time) {
	for key, e := range sharedSuspensionRegistry {
		if now.Sub(e.lastActivity()) > idleEvictionHorizon {
			delete(sharedSuspensionRegistry, key)
		}
	}
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
	if params.FailureRateThresholdPercent, err = parseOptionalInt(raw, "failureRateThresholdPercent"); err != nil {
		return params, err
	}
	if params.LatencyThresholdMs, err = parseOptionalInt(raw, "latencyThresholdMs"); err != nil {
		return params, err
	}

	if requestModelRaw, ok := raw["requestModel"]; ok {
		requestModelMap, ok := requestModelRaw.(map[string]interface{})
		if !ok {
			return params, fmt.Errorf("'requestModel' must be an object")
		}
		if loc, ok := requestModelMap["location"]; ok {
			locStr, ok := loc.(string)
			if !ok {
				return params, fmt.Errorf("'requestModel.location' must be a string")
			}
			params.RequestModel.Location = locStr
		}
		if id, ok := requestModelMap["identifier"]; ok {
			idStr, ok := id.(string)
			if !ok {
				return params, fmt.Errorf("'requestModel.identifier' must be a string")
			}
			params.RequestModel.Identifier = idStr
		}
	}
	if params.RequestModel.Location == "" {
		params.RequestModel.Location = requestModelLocationPayload
	}
	if params.RequestModel.Identifier == "" {
		params.RequestModel.Identifier = "model"
	}
	switch params.RequestModel.Location {
	case requestModelLocationPayload, requestModelLocationHeader, requestModelLocationQueryParam, requestModelLocationPathParam:
	default:
		return params, fmt.Errorf("'requestModel.location' must be one of: payload, header, queryParam, pathParam")
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
// extractRequestedModel reads the client-requested model from wherever
// RequestModel says it lives, mirroring model-round-robin's own requestModel
// contract rather than assuming an OpenAI-shaped top-level "model" JSON
// field. For header/queryParam/pathParam it reads the DOWNSTREAM snapshot
// (DownstreamRequest) — the client's actual original request — never a
// possibly-already-mutated live value.
func (p *Policy) extractRequestedModel(reqCtx *policy.RequestContext) string {
	switch p.requestModelLocation() {
	case requestModelLocationHeader:
		headers := reqCtx.DownstreamHeaders()
		if headers == nil {
			return ""
		}
		values := headers.Get(p.requestModelIdentifier())
		if len(values) == 0 {
			return ""
		}
		return strings.TrimSpace(values[0])
	case requestModelLocationQueryParam:
		return extractQueryParam(reqCtx.DownstreamRequest().Path, p.requestModelIdentifier())
	case requestModelLocationPathParam:
		return extractPathParam(reqCtx.DownstreamRequest().Path, p.requestModelIdentifier())
	default: // requestModelLocationPayload
		if reqCtx.Body == nil || !reqCtx.Body.Present || len(reqCtx.Body.Content) == 0 {
			return ""
		}
		value, err := utils.ExtractStringValueFromJsonpath(reqCtx.Body.Content, p.requestModelIdentifier())
		if err != nil {
			return ""
		}
		return strings.TrimSpace(value)
	}
}

// extractQueryParam reads paramName's value from rawPath's query string.
func extractQueryParam(rawPath, paramName string) string {
	if rawPath == "" {
		return ""
	}
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		decoded = rawPath
	}
	_, query, ok := strings.Cut(decoded, "?")
	if !ok {
		return ""
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return ""
	}
	return values.Get(paramName)
}

// extractPathParam reads regexPattern's first capture group matched against
// rawPath's path portion (query string excluded) — the read counterpart of
// replacePathParam, same regex convention as model-round-robin's
// modifyPathParamInPath (one capture group identifying the path segment).
func extractPathParam(rawPath, regexPattern string) string {
	if rawPath == "" {
		return ""
	}
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		decoded = rawPath
	}
	pathOnly, _, _ := strings.Cut(decoded, "?")
	re, err := regexp.Compile(regexPattern)
	if err != nil {
		return ""
	}
	match := re.FindStringSubmatch(pathOnly)
	if len(match) < 2 {
		return ""
	}
	return match[1]
}

// replacePathParam substitutes regexPattern's first capture group within
// rawPath's path portion with newValue, preserving the query string —
// mirrors model-round-robin's modifyPathParamInPath. Returns rawPath
// unchanged (byte-for-byte, so a caller can compare by equality) if the
// pattern doesn't match or already captures newValue.
func replacePathParam(rawPath, regexPattern, newValue string) string {
	if rawPath == "" {
		return rawPath
	}
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		return rawPath
	}
	pathOnly, query, hasQuery := strings.Cut(decoded, "?")
	re, err := regexp.Compile(regexPattern)
	if err != nil {
		return rawPath
	}
	matchIndices := re.FindStringSubmatchIndex(pathOnly)
	if len(matchIndices) < 4 || matchIndices[2] == -1 || matchIndices[3] == -1 {
		return rawPath
	}
	updated := pathOnly[:matchIndices[2]] + newValue + pathOnly[matchIndices[3]:]
	if hasQuery {
		return updated + "?" + query
	}
	return updated
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
// suspensionKey includes upstreamDefinition alongside model+provider: two
// entry/fallback members can author the identical (model, provider) pair yet
// dial genuinely different physical backends via a distinct
// UpstreamDefinition (a named additionalProviders[].as/.id, or a hand-declared
// upstreamDefinitions[].name — see FailoverTarget.UpstreamDefinition's doc
// comment), and validation-time duplicate-member detection
// (gateway-controller's canonicalMember) already keys on all three. A
// model+provider-only key here would incorrectly pool two distinct physical
// targets' health under one shared suspension entry.
func suspensionKey(model, provider, upstreamDefinition string) string {
	return model + "|" + provider + "|" + upstreamDefinition
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

// requestModelLocation/requestModelIdentifier default an unset RequestModel
// to {payload, model} at the point of use — the same use-site defaulting
// convention this file already applies to SuspendAfterFailures
// — rather than relying solely on parseParams having run. This keeps a
// hand-built Policy{params: ModelFailoverParams{...}} (as unit tests
// construct directly, without going through parseParams) behaved identically
// to a parsed one.
func (p *Policy) requestModelLocation() string {
	if p.params.RequestModel.Location == "" {
		return requestModelLocationPayload
	}
	return p.params.RequestModel.Location
}

func (p *Policy) requestModelIdentifier() string {
	if p.params.RequestModel.Identifier == "" {
		return "model"
	}
	return p.params.RequestModel.Identifier
}

// keyFor derives a target's full suspension/circuit key, resolving its
// provider identity exactly the way every other member-identity decision in
// this file does.
func (p *Policy) keyFor(m FailoverTarget) string {
	return suspensionKey(m.Model, p.resolvedProvider(m), m.UpstreamDefinition)
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
// sweep). Side-effect-free (besides that lazy clear) — safe to call from
// anywhere just to inspect state. The actual dispatch decision must use
// allowRequest instead, which additionally accounts for half-open recovery.
func (p *Policy) isSuspended(model, provider, upstreamDefinition string) bool {
	p.susp.mu.Lock()
	defer p.susp.mu.Unlock()
	key := suspensionKey(model, provider, upstreamDefinition)
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

// suspend immediately marks (model, provider, upstreamDefinition) suspended
// for the base SuspendDuration, bypassing the SuspendAfterFailures threshold
// and backoff streak entirely — a direct, unconditional primitive kept for
// tests and for any future caller that wants "suspend this right now" without
// going through recordOutcome's bookkeeping.
func (p *Policy) suspend(model, provider, upstreamDefinition string) {
	if p.params.SuspendDuration <= 0 {
		return
	}
	p.susp.mu.Lock()
	defer p.susp.mu.Unlock()
	p.susp.suspended[suspensionKey(model, provider, upstreamDefinition)] = time.Now().Add(time.Duration(p.params.SuspendDuration) * time.Second)
}

// allowRequest is the ROUTING decision for whether target may be dispatched
// to right now — isSuspended's dispatch-path counterpart, adding half-open
// controlled recovery: once a suspension window expires, instead of
// immediately treating the target as fully healthy again, exactly one request
// at a time is let through as a probe while it's otherwise still treated as
// suspended. One successful probe closes the circuit; one failed probe
// re-suspends it with its backoff window doubled.
//
// This MUTATES probe accounting when it grants a probe slot (increments
// active), so it must only be called at an actual dispatch decision, never
// from a test/assertion context that isn't going to act on the result —
// isSuspended stays available, side-effect-free, for that. completeProbe
// (called from recordOutcome) is what releases the slot again.
func (p *Policy) allowRequest(target FailoverTarget) bool {
	key := p.keyFor(target)
	p.susp.markUsed(time.Now())

	p.susp.mu.Lock()
	defer p.susp.mu.Unlock()

	until, suspended := p.susp.suspended[key]
	if !suspended {
		return true
	}
	if time.Now().Before(until) {
		return false
	}

	// Half-open: the window is expired, but recovery is gated by a single
	// in-flight probe rather than opening unconditionally. The
	// suspended timestamp is deliberately left in place — from here on it
	// means "in half-open recovery" (distinguished from "actively blocked"
	// purely by already being in the past), until completeProbe closes or
	// reopens the circuit.
	if p.susp.probes == nil {
		p.susp.probes = map[string]*probeState{}
	}
	ps, ok := p.susp.probes[key]
	if !ok {
		ps = &probeState{}
		p.susp.probes[key] = ps
	}
	now := time.Now()
	if ps.inFlight && now.Sub(ps.grantedAt) < circuitProbeLease {
		return false // a probe is already in flight — still treated as blocked
	}
	ps.inFlight = true
	ps.grantedAt = now
	circuitLogger().Debug("model-failover half-open probe granted", "target", key)
	return true
}

// completeProbe records one half-open probe's outcome for key and decides
// whether to close (fully reopen, target healthy again) or reopen (re-suspend
// via the same backoff escalation as any other trigger) the circuit. Called
// from recordOutcome whenever key currently has an active half-open cycle —
// the only way a response can exist for a suspended key at all is that
// allowRequest granted it as a probe, so any recorded outcome for such a key
// is necessarily a probe outcome. Caller must hold p.susp.mu.
func (p *Policy) completeProbe(key string, failed bool, now time.Time) {
	if _, ok := p.susp.probes[key]; !ok {
		return
	}
	delete(p.susp.probes, key)
	if failed {
		p.triggerSuspensionLocked(key, now, "half_open_probe_failed")
		return
	}
	delete(p.susp.suspended, key)
	circuitLogger().Info("model-failover circuit closed", "target", key, "reason", "half_open_probe_succeeded")
}

// triggerSuspensionLocked applies the shared backoff-streak escalation
// (identical to recordOutcome's original inline logic) to suspend key right
// now — the common suspension primitive every trigger (consecutive-failure
// count, rolling failure-rate, latency, a reopened half-open probe) funnels
// through, so they all escalate backoff consistently rather than each
// re-implementing their own window math. Caller must hold p.susp.mu. reason
// is purely for observability (ECI #18469 finding #8) — it never affects
// behavior.
func (p *Policy) triggerSuspensionLocked(key string, now time.Time, reason string) {
	if expiresAt, ok := p.susp.streakExpiresAt[key]; !ok || now.After(expiresAt) {
		p.susp.suspensionStreak[key] = 0 // no hot streak to continue — this is a fresh incident
	}
	p.susp.suspensionStreak[key]++
	duration := p.backoffDuration(p.susp.suspensionStreak[key])
	p.susp.suspended[key] = now.Add(duration)

	base := time.Duration(p.params.SuspendDuration) * time.Second
	p.susp.streakExpiresAt[key] = now.Add(duration + base)

	// Judge the target afresh once it recovers: samples from before this
	// suspension would otherwise re-trip the rate/latency rule on the very
	// first attempt after it closes.
	delete(p.susp.rateWindows, key)
	delete(p.susp.latencyWindows, key)

	circuitLogger().Warn("model-failover circuit suspended", "target", key, "reason", reason,
		"streak", p.susp.suspensionStreak[key], "durationSeconds", duration.Seconds())
}

// rateWindowFor/latencyWindowFor lazily create the rolling window for key.
// Caller must hold p.susp.mu.
func (p *Policy) rateWindowFor(key string) *rollingWindow {
	if w, ok := p.susp.rateWindows[key]; ok {
		return w
	}
	w := newRollingWindow()
	p.susp.rateWindows[key] = w
	return w
}

func (p *Policy) latencyWindowFor(key string) *rollingWindow {
	if w, ok := p.susp.latencyWindows[key]; ok {
		return w
	}
	w := newRollingWindow()
	p.susp.latencyWindows[key] = w
	return w
}

// recordOutcome updates every configured circuit-breaking signal for one
// attempt's outcome against target: the original consecutive-qualifying-
// failure counter (SuspendAfterFailures), the rolling failure-rate rule
// (FailureRateThresholdPercent), and the latency rule (LatencyThresholdMs)
// — any one of them independently suspends the target via the shared
// triggerSuspensionLocked escalation. hasLatency/latencyMs come from this
// attempt's own OnRequestHeaders->OnResponseHeaders elapsed time (see
// OnRequestHeaders' SharedContext stash); hasLatency is false when that stash
// is unavailable (e.g. OnRequestHeaders never ran for this attempt).
//
// A success (failed == false) resets the consecutive-failure counter and the
// backoff streak — a target that recovers even once starts the next incident
// fresh — but still feeds the rolling rate/latency windows: a rate rule needs
// TOTAL attempts, not just failures, and a slow success is still slow.
//
// If target is currently in a half-open recovery cycle (completeProbe would
// find an active probes[] entry), this delegates to completeProbe instead of
// the ordinary counters — a single probe outcome closes or reopens the
// circuit, regardless of SuspendAfterFailures.
func (p *Policy) recordOutcome(target FailoverTarget, failed bool, hasLatency bool, latencyMs int) {
	if p.params.SuspendDuration <= 0 {
		return // suspension disabled entirely; nothing to track
	}
	key := p.keyFor(target)
	now := time.Now()
	p.susp.markUsed(now)

	p.susp.mu.Lock()
	defer p.susp.mu.Unlock()
	if p.susp.failureCounts == nil {
		p.susp.failureCounts = map[string]int{}
	}
	if p.susp.suspensionStreak == nil {
		p.susp.suspensionStreak = map[string]int{}
	}
	if p.susp.streakExpiresAt == nil {
		p.susp.streakExpiresAt = map[string]time.Time{}
	}
	if p.susp.rateWindows == nil {
		p.susp.rateWindows = map[string]*rollingWindow{}
	}
	if p.susp.latencyWindows == nil {
		p.susp.latencyWindows = map[string]*rollingWindow{}
	}
	if p.susp.probes == nil {
		p.susp.probes = map[string]*probeState{}
	}

	if _, inHalfOpen := p.susp.probes[key]; inHalfOpen {
		p.completeProbe(key, failed, now)
		return
	}

	if !failed {
		delete(p.susp.failureCounts, key)
		delete(p.susp.suspensionStreak, key)
		delete(p.susp.streakExpiresAt, key)
	} else {
		threshold := p.params.SuspendAfterFailures
		if threshold <= 0 {
			threshold = 1
		}
		p.susp.failureCounts[key]++
		if p.susp.failureCounts[key] >= threshold {
			p.susp.failureCounts[key] = 0
			p.triggerSuspensionLocked(key, now, "consecutive_failures")
		}
	}

	// Rolling failure-rate rule — additive to the consecutive-failure trigger
	// above; either can independently suspend the target.
	if p.params.FailureRateThresholdPercent > 0 {
		w := p.rateWindowFor(key)
		w.recordOutcome(now, failed)
		attempts, failures := w.totals()
		if attempts >= circuitMinimumSamples && failures*100/attempts >= p.params.FailureRateThresholdPercent {
			p.triggerSuspensionLocked(key, now, "failure_rate_threshold")
		}
	}

	// Latency rule — every attempt (success or failure) has a latency, so
	// this is tracked regardless of the failed/success branch above.
	if hasLatency && p.params.LatencyThresholdMs > 0 {
		w := p.latencyWindowFor(key)
		w.recordLatency(now, latencyMs)
		if pct, ok := w.latencyPercentile(circuitLatencyPercentile, circuitMinimumSamples); ok && pct >= p.params.LatencyThresholdMs {
			p.triggerSuspensionLocked(key, now, "latency_threshold")
		}
	}
}

// backoffDuration computes the suspend window for the nth consecutive
// suspend-then-immediately-refail cycle of the same target (streak == 1 is
// the first cycle, i.e. exactly the base SuspendDuration). Each further
// cycle doubles the window, capped at circuitMaxSuspendMultiplier times the
// base so a chronically broken target's window grows but
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

	cap := base * circuitMaxSuspendMultiplier
	if duration > cap {
		duration = cap
	}
	return duration
}

// OnRequestBody's downstream invocation parses the client-requested model,
// matches it against the configured chain, and always routes to that chain's
// composite cluster. Envoy owns target health and skips ejected leaf clusters;
// the downstream policy never bypasses the chain. For an
// upstream-attempt invocation (Downstream == nil), it instead delegates to
// onUpstreamAttemptRequestBody — this instance is also attached via
// upstreamPolicies: (see OnRequestHeaders), and Mode()'s RequestBodyMode
// applies to both attachments, so this method is called for every attempt too.
func (p *Policy) OnRequestBody(_ context.Context, reqCtx *policy.RequestContext, _ map[string]interface{}) policy.RequestAction {
	if reqCtx.Downstream == nil {
		return p.onUpstreamAttemptRequestBody(reqCtx)
	}

	model := p.extractRequestedModel(reqCtx)
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

	// KNOWN LIMITATION (ECI #18469 finding #1): this only checks whether the
	// ENTRY (primary) is blocked — never whether some LATER member inside the
	// composite/suffix this dispatches into is ALSO currently suspended. Once
	// a request enters a composite cluster, envoy.clusters.composite selects
	// its next member purely by retry-ATTEMPT-COUNT, never by this policy's
	// own suspension state (confirmed via that cluster type's own doc comment
	// and via live testing — see this plan's outlier-detection investigation);
	// there is no hook in the stock Envoy retry path or in this SDK's
	// ImmediateResponse (which terminates the WHOLE client request, not just
	// "skip this member" — see action.go) that lets a policy skip an
	// individual already-known-bad member WITHIN an attempt that's already
	// in flight inside a multi-member composite. The suffix-composite
	// mechanism (SuffixCluster) mitigates this for the common case — a
	// SUSPENDED PREFIX is skipped entirely by choosing a later entry point —
	// but can't skip a suspended member in the MIDDLE of an otherwise-healthy
	// suffix without combinatorial suffix generation (rejected: exponential
	// cluster count) or a custom Envoy extension (see
	// docs/superpowers/specs/2026-09-23-llm-model-failover-implementation-spec.md's
	// proposal, not implemented here). Genuinely fixing this requires one of
	// those two, not a change to this file.
	if p.allowRequest(entry.FailoverTarget) {
		cluster := entry.AggregateCluster
		return policy.UpstreamRequestModifications{
			UpstreamName: &cluster,
			HeadersToSet: map[string]string{"x-envoy-max-retries": strconv.Itoa(len(entry.Fallbacks))},
		}
	}

	for j, fallback := range entry.Fallbacks {
		// The controller injects a SuffixCluster for every fallback; one
		// without it has nowhere to be dispatched, so it counts as blocked.
		if fallback.SuffixCluster == "" || !p.allowRequest(fallback) {
			continue
		}
		// Route at a SUFFIX COMPOSITE covering this fallback and every member
		// after it, bypassing only the SUSPENDED prefix of the chain — a
		// failure of THIS fallback can still retry into the remaining chain
		// within the same request, exactly like entering the full chain at
		// position 0 does (see xds.SuffixCompositeClusterName).
		//
		// The route's RetryPolicy is sized for the DEEPEST target's
		// full-length chain (translator.go) and would give this shorter suffix
		// too many retries, masking a genuine exhaustion as still-retriable.
		// x-envoy-max-retries is Envoy's per-request override, corrected here
		// to exactly this suffix's remaining member count.
		//
		// Nothing is seeded into metadata here: the upstream phase gets its
		// own fresh SharedContext, and OnRequestHeaders re-identifies the
		// member from the cluster Envoy reports.
		cluster := fallback.SuffixCluster
		return policy.UpstreamRequestModifications{
			UpstreamName: &cluster,
			HeadersToSet: map[string]string{"x-envoy-max-retries": strconv.Itoa(len(entry.Fallbacks) - j - 1)},
		}
	}

	// Every member is blocked (suspended, or in half-open
	// recovery with its probe budget already spent) — nothing left to bypass
	// to. Previously this fell through to the full composite with a full
	// retry budget, silently spending an entire retry chain against members
	// already known to be failing (ECI #18469 finding #2). Fail fast instead:
	// short-circuit the WHOLE policy chain and return the exhaustion response
	// directly to the client, without dispatching to Envoy's router at all —
	// this is exactly ImmediateResponse's documented use from the downstream
	// phase (see action.go; it is NOT usable this way from an upstream-attempt
	// phase, which is what makes finding #1 above structurally different).
	circuitLogger().Warn("model-failover chain exhausted — every member suspended", "entry_model", entry.Model)
	return policy.ImmediateResponse{
		StatusCode: 503,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       []byte(`{"error":{"message":"all failover targets are currently suspended","type":"service_unavailable","code":"failover_exhausted"}}`),
	}
}

// onUpstreamAttemptRequestBody rewrites the replayed client body to carry
// this attempt's resolved chain member's model, when RequestModel says the
// model lives in the payload (header/queryParam/pathParam are rewritten in
// OnRequestHeaders instead, since headers/path are available there and don't
// need to wait for the body) and that member's model differs from what's
// currently at RequestModel.Identifier's JSONPath.
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
	if reqCtx.Upstream == nil || p.requestModelLocation() != requestModelLocationPayload {
		return policy.UpstreamRequestModifications{}
	}
	match := p.resolveAttemptForCluster(reqCtx.Upstream.RouteCluster, reqCtx.Upstream.MemberClusterName, attemptCount(reqCtx.Headers))
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
	if current, err := utils.ExtractValueFromJsonpath(payload, p.requestModelIdentifier()); err == nil {
		if currentStr, ok := current.(string); ok && currentStr == member.Model {
			return policy.UpstreamRequestModifications{}
		}
	}
	if err := utils.SetValueAtJSONPath(payload, p.requestModelIdentifier(), member.Model); err != nil {
		return policy.UpstreamRequestModifications{} // identifier's parent path doesn't exist in this body shape — nothing safe to rewrite
	}

	newBody, err := json.Marshal(payload)
	if err != nil {
		return policy.UpstreamRequestModifications{}
	}
	return policy.UpstreamRequestModifications{Body: newBody}
}

// findEntryPointCluster resolves routeCluster (xds.cluster_name) to the chain
// entry it belongs to, when routeCluster names a cluster the downstream
// policy dispatches an ATTEMPT 1 at directly — the chain's own
// AggregateCluster (the normal path) or one of its fallbacks' SuffixCluster
// (the suspended-prefix bypass — see OnRequestBody). Either way this only
// identifies WHICH ENTRY; resolveMemberWithinEntry still resolves which
// member within it from memberClusterName.
func (p *Policy) findEntryPointCluster(cluster string) *FailoverTargetEntry {
	if cluster == "" {
		return nil
	}
	for i := range p.params.Targets {
		entry := &p.params.Targets[i]
		if entry.AggregateCluster == cluster {
			return entry
		}
		for j := range entry.Fallbacks {
			if entry.Fallbacks[j].SuffixCluster != "" && entry.Fallbacks[j].SuffixCluster == cluster {
				return entry
			}
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
// represents. memberClusterName is the REAL cluster Envoy actually dialed,
// read from that host's own declared identity metadata
// (UpstreamRequestContext.MemberClusterName) — dial-accurate for every
// attempt, unlike x-envoy-attempt-count, which counts dials rather than
// priorities and so can't be trusted to identify a member ACROSS different
// clusters/priorities once Envoy's load balancer can skip a priority due to
// host health (e.g. outlier_detection ejecting the primary) without that
// being a retry. attempt is used only as a narrow tiebreaker — see
// resolveMemberWithinEntry.
//
// Every attempt reaches the upstream phase through an ENTRY-POINT cluster —
// the chain's own AggregateCluster (the normal path) or a fallback's
// SuffixCluster (the suspended-prefix bypass, see OnRequestBody).
// routeCluster, from xds.cluster_name, is that entry-point cluster's own
// name, and findEntryPointCluster identifies WHICH ENTRY unambiguously, since
// every AggregateCluster and SuffixCluster is uniquely named. Matching is
// then scoped to just that entry's own primary and fallbacks. This scoping
// matters: a proxy's targets[] entries typically share the SAME primary
// provider, so matching a member's identity across EVERY entry — instead of
// just the one routeCluster identified — would be ambiguous whenever two
// entries share a member cluster. A routeCluster that is neither is not this
// chain's attempt at all.
func (p *Policy) resolveAttemptForCluster(routeCluster, memberClusterName string, attempt int) *chainMatch {
	entry := p.findEntryPointCluster(routeCluster)
	if entry == nil {
		return nil
	}
	return resolveMemberWithinEntry(entry, memberClusterName, attempt)
}

// resolveMemberWithinEntry finds which of entry's own members (its primary,
// or one of its fallbacks) memberClusterName identifies. Usually exactly one
// candidate matches. A same-provider fallback (author omits provider:, or
// sets it to the primary's own identity) has no distinct physical cluster of
// its own — it dials the IDENTICAL real cluster as the primary or an earlier
// same-provider fallback, differing only in which model it requests — so
// cluster identity alone can tie two or more members together.
//
// attempt (x-envoy-attempt-count) breaks that tie. This is safe specifically
// because tied members share a host: Envoy's own priority-health-based
// skipping — the reason attempt count can't be trusted for resolution across
// DIFFERENT clusters/priorities — can never separate them, since ejecting a
// shared host ejects every priority it appears at together. A tie that
// attempt doesn't resolve (e.g. header missing/unparseable) falls back to
// the first declared candidate, the same fail-safe default this policy
// already uses elsewhere.
func resolveMemberWithinEntry(entry *FailoverTargetEntry, memberClusterName string, attempt int) *chainMatch {
	if memberClusterName == "" {
		return nil
	}
	var candidates []*chainMatch
	if entry.ClusterName != "" && entry.ClusterName == memberClusterName {
		candidates = append(candidates, &chainMatch{entry: entry, member: &entry.FailoverTarget, index: 1})
	}
	for j := range entry.Fallbacks {
		if entry.Fallbacks[j].ClusterName == "" || entry.Fallbacks[j].ClusterName != memberClusterName {
			continue
		}
		candidates = append(candidates, &chainMatch{entry: entry, member: &entry.Fallbacks[j], index: j + 2})
	}
	switch len(candidates) {
	case 0:
		return nil
	case 1:
		return candidates[0]
	default:
		for _, c := range candidates {
			if c.index == attempt {
				return c
			}
		}
		return candidates[0]
	}
}

// attemptCount reads x-envoy-attempt-count, defaulting to 1 (the primary
// attempt) for a missing or unparseable header. Used only as
// resolveMemberWithinEntry's same-cluster tiebreaker — never as the primary
// signal for identifying a member across different clusters.
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

// chainBasePaths lists every base path a member of entry dials — the prefixes
// a replayed attempt's :path can start with.
func chainBasePaths(entry *FailoverTargetEntry) []string {
	bases := []string{entry.BasePath}
	for _, fb := range entry.Fallbacks {
		bases = append(bases, fb.BasePath)
	}
	return bases
}

// rebaseAttemptPath swaps whichever chain member's base path currently
// prefixes currentPath for newBase, keeping the rest of the path and the query
// string exactly as the client sent them. The longest matching base wins, on a
// segment boundary, so "/openai" never matches "/openai-backup/...". When no
// known base prefixes the path (not expected — the downstream rewrite always
// uses one of them), it falls back to newBase + operationPath, which is exact
// for a route whose operation path has no parameters and a no-op when
// operationPath is empty.
func rebaseAttemptPath(currentPath, newBase string, knownBases []string, operationPath string) string {
	pathOnly, query, hasQuery := strings.Cut(currentPath, "?")

	matched := ""
	found := false
	for _, b := range knownBases {
		b = strings.TrimSuffix(b, "/")
		if b == "" || len(b) <= len(matched) {
			continue
		}
		if pathOnly == b || strings.HasPrefix(pathOnly, b+"/") {
			matched, found = b, true
		}
	}
	if !found {
		corrected := joinBasePathAndOperation(newBase, operationPath)
		if corrected == "" {
			return currentPath
		}
		return withQueryFrom(currentPath, corrected)
	}

	rebased := strings.TrimSuffix(newBase, "/") + pathOnly[len(matched):]
	if rebased == "" {
		rebased = "/"
	}
	if hasQuery {
		return rebased + "?" + query
	}
	return rebased
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
// suffix composite when the suspended-prefix bypass dispatched at it;
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
	match := p.resolveAttemptForCluster(reqCtx.Upstream.RouteCluster, reqCtx.Upstream.MemberClusterName, attemptCount(reqCtx.Headers))
	if match == nil {
		return nil
	}
	member := match.member

	if reqCtx.SharedContext == nil {
		reqCtx.SharedContext = &policy.SharedContext{}
	}
	if reqCtx.SharedContext.Metadata == nil {
		reqCtx.SharedContext.Metadata = map[string]interface{}{}
	}
	reqCtx.SharedContext.Metadata[selectedModelMetadataKey] = member.Model
	reqCtx.SharedContext.Metadata[selectedProviderMetadataKey] = p.resolvedProvider(*member)
	reqCtx.SharedContext.Metadata[attemptStartMetadataKey] = time.Now()

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
	// Only the base-path PREFIX is swapped: the rest of the replayed path is
	// the client's concrete request (e.g. "/models/gemini-pro:generateContent"
	// for a path-param provider), which OperationPath — the route TEMPLATE,
	// "/models/{model}:generateContent" — can't reproduce. An empty BasePath
	// means the controller supplied nothing for this member (nothing to
	// correct TO), so :path is left exactly as Envoy delivered it.
	newPath := reqCtx.Path
	if member.BasePath != "" {
		newPath = rebaseAttemptPath(reqCtx.Path, member.BasePath, chainBasePaths(match.entry), p.params.OperationPath)
	}

	// RequestModel's header/queryParam/pathParam locations are rewritten here
	// (payload is rewritten in onUpstreamAttemptRequestBody instead, since the
	// body isn't available yet at this phase) — composed onto newPath above
	// so only ONE Path mutation is ever returned even when both the base-path
	// correction and a pathParam/queryParam model rewrite apply to the same
	// attempt.
	mods := policy.UpstreamRequestHeaderModifications{}
	changed := false
	if member.Model != "" {
		switch p.requestModelLocation() {
		case requestModelLocationHeader:
			current := ""
			if reqCtx.Headers != nil {
				if values := reqCtx.Headers.Get(p.requestModelIdentifier()); len(values) > 0 {
					current = values[0]
				}
			}
			if current != member.Model {
				mods.HeadersToSet = map[string]string{p.requestModelIdentifier(): member.Model}
				changed = true
			}
		case requestModelLocationQueryParam:
			if extractQueryParam(newPath, p.requestModelIdentifier()) != member.Model {
				mods.QueryParametersToAdd = map[string][]string{p.requestModelIdentifier(): {member.Model}}
				changed = true
			}
		case requestModelLocationPathParam:
			newPath = replacePathParam(newPath, p.requestModelIdentifier(), member.Model)
		}
	}

	if newPath != reqCtx.Path {
		mods.Path = &newPath
		changed = true
	}
	if changed {
		return mods
	}
	return nil
}

// withQueryFrom reattaches stalePath's query string (everything from the
// first "?" onward) onto newPath. Envoy's ":path" pseudo-header is
// path+query as one string, and the kernel replays attempt 1's ":path"
// verbatim on every retry — so a stale query string is still sitting in
// reqCtx.Path when a later attempt corrects the path portion for a different
// provider's base path. Rebuilding the path from only basePath+operationPath
// (both static, query-free configuration values) would otherwise silently
// drop it: "?stream=true" on the client's original request must survive
// every attempt, not just the first.
func withQueryFrom(stalePath, newPath string) string {
	if _, query, ok := strings.Cut(stalePath, "?"); ok && query != "" {
		return newPath + "?" + query
	}
	return newPath
}

// OnResponseHeaders records this attempt's outcome (isFailureStatus — any
// 5xx by default, or exactly the configured StatusCodes) via recordOutcome —
// this is the SOLE cross-request routing-health signal: downstream's
// OnRequestBody suspension bypass reads exactly this state (Envoy's own
// composite-cluster selection has no host-health awareness of its own — see
// the outlier-detection investigation this design's history records — so
// there is no second, Envoy-native mechanism to fall back on). A qualifying
// failure counts toward SuspendAfterFailures/the rolling failure-rate/latency
// rules and, once any of them trips, suspends the target for a
// backoff-computed window; a non-qualifying response resets the
// consecutive-failure bookkeeping instead (see recordOutcome).
//
// KNOWN LIMITATION (ECI #18469 finding #3): this is the ONLY place an outcome
// is ever recorded, and it only runs when Envoy actually delivers a real
// upstream response through the ext_proc filter's response-header phase — a
// connection failure, timeout, reset, or other local-reply/transport error
// never reaches here at all. Confirmed structurally, not just unimplemented:
// this go-control-plane build's ExternalProcessor proto has no
// process_on_local_reply-equivalent field to opt a local reply into ext_proc
// processing (checked the full field list), and Envoy's documented default is
// that ext_proc, like other HTTP filters, is not invoked for local replies.
// Fixing this needs an Envoy/go-control-plane capability this deployment
// doesn't have, not a change to this file.
//
// Separately, for an attempt that escalated past the primary — whether by
// Envoy retrying inside the aggregate or by the downstream bypass entering at
// a fallback's suffix composite — this sets ResolvedFailoverProviderHeader for
// downstream analytics attribution.
func (p *Policy) OnResponseHeaders(_ context.Context, respCtx *policy.ResponseHeaderContext, _ map[string]interface{}) policy.ResponseHeaderAction {
	if respCtx.Downstream != nil || respCtx.Upstream == nil {
		return nil
	}
	match := p.resolveAttemptForCluster(respCtx.Upstream.RouteCluster, respCtx.Upstream.MemberClusterName, attemptCount(respCtx.RequestHeaders))
	if match == nil {
		return nil
	}
	member, index := match.member, match.index

	hasLatency, latencyMs := false, 0
	if respCtx.SharedContext != nil && respCtx.SharedContext.Metadata != nil {
		if startedAt, ok := respCtx.SharedContext.Metadata[attemptStartMetadataKey].(time.Time); ok {
			hasLatency = true
			latencyMs = int(time.Since(startedAt).Milliseconds())
		}
	}
	p.recordOutcome(*member, p.isFailureStatus(int(respCtx.ResponseStatus)), hasLatency, latencyMs)

	if index > 1 {
		return policy.DownstreamResponseHeaderModifications{
			HeadersToSet: map[string]string{ResolvedFailoverProviderHeader: p.resolvedProvider(*member)},
			// Per-request circuit-breaking observability (ECI #18469 finding
			// #8): which chain position actually served this request. There is
			// no first-class metrics API to emit a counter from directly (see
			// circuitLogger's doc comment), so this rides the one per-request
			// telemetry surface the SDK does expose.
			AnalyticsMetadata: map[string]any{
				"model_failover_chain_position": index,
				"model_failover_resolved_model": member.Model,
			},
		}
	}
	return nil
}

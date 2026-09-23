package modelfailover

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func TestParseParams_ValidTargets(t *testing.T) {
	raw := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"model": "gpt-4o",
				"fallbacks": []interface{}{
					map[string]interface{}{"model": "claude-sonnet-4-5-20250929", "provider": "anthropic-upstream"},
				},
				"aggregateCluster": "failover_agg_chat_0",
			},
		},
		"suspendDuration": float64(900),
	}

	params, err := parseParams(raw)

	require.NoError(t, err)
	require.Len(t, params.Targets, 1)
	assert.Equal(t, "gpt-4o", params.Targets[0].Model)
	assert.Equal(t, "failover_agg_chat_0", params.Targets[0].AggregateCluster)
	require.Len(t, params.Targets[0].Fallbacks, 1)
	assert.Equal(t, "anthropic-upstream", params.Targets[0].Fallbacks[0].Provider)
	assert.Equal(t, 900, params.SuspendDuration)
}

func TestParseParams_MissingTargets(t *testing.T) {
	_, err := parseParams(map[string]interface{}{})
	require.Error(t, err)
}

func TestOnRequestBody_RoutesToMatchedTargetAggregate(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{
				{
					FailoverTarget:   FailoverTarget{Model: "gpt-4o"},
					Fallbacks:        []FailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
					AggregateCluster: "failover_agg_chat_0",
				},
			},
		},
		susp: &suspensionState{suspended: make(map[string]time.Time)},
	}
	reqCtx := &policy.RequestContext{
		Downstream: &policy.DownstreamContext{},
		Body:       &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true},
	}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "failover_agg_chat_0", *mods.UpstreamName)
}

func TestOnRequestBody_NoMatchIsNoop(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{Targets: []FailoverTargetEntry{{FailoverTarget: FailoverTarget{Model: "gpt-4o"}}}},
		susp:   &suspensionState{suspended: make(map[string]time.Time)},
	}
	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"some-other-model"}`), Present: true}}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	assert.Nil(t, mods.UpstreamName)
}

func TestOnRequestBody_SuspendedPrimaryBypassesToOnlyFallbacksSuffix(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{{
				FailoverTarget: FailoverTarget{Model: "gpt-4o"},
				Fallbacks: []FailoverTarget{{
					Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream",
					ClusterName: "leaf_1", SuffixCluster: "failover_suffix_1",
				}},
				AggregateCluster: "failover_agg_chat_0",
			}},
		},
		susp: &suspensionState{suspended: map[string]time.Time{
			suspensionKey("gpt-4o", "", ""): time.Now().Add(time.Hour),
		}},
	}
	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

	mods, ok := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)

	require.True(t, ok)
	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "failover_suffix_1", *mods.UpstreamName)
	assert.Equal(t, "0", mods.HeadersToSet["x-envoy-max-retries"], "last member of the chain: nothing after it to retry into")
}

func TestOnRequestBody_SuspendedPrimaryBypassesToSameProviderFallbackSuffix(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{{
				FailoverTarget: FailoverTarget{Model: "gpt-4o"},
				Fallbacks: []FailoverTarget{
					{Model: "same-provider-model", ClusterName: "leaf_1", SuffixCluster: "failover_suffix_1"},
					{Model: "claude", Provider: "anthropic-upstream", ClusterName: "leaf_2", SuffixCluster: "failover_suffix_2"},
				},
				AggregateCluster: "failover_agg_chat_0",
			}},
		},
		susp: &suspensionState{suspended: map[string]time.Time{suspensionKey("gpt-4o", "", ""): time.Now().Add(time.Hour)}},
	}
	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

	mods := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)

	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "failover_suffix_1", *mods.UpstreamName,
		"a same-provider fallback is its own member (different model), so the primary's suspension doesn't cover it")
	assert.Equal(t, "1", mods.HeadersToSet["x-envoy-max-retries"])
}

func TestOnRequestBody_FallbackWithoutSuffixClusterCountsAsBlocked(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{{
				FailoverTarget:   FailoverTarget{Model: "gpt-4o"},
				Fallbacks:        []FailoverTarget{{Model: "same-provider-model", ClusterName: "leaf_1"}},
				AggregateCluster: "failover_agg_chat_0",
			}},
		},
		susp: &suspensionState{suspended: map[string]time.Time{suspensionKey("gpt-4o", "", ""): time.Now().Add(time.Hour)}},
	}
	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

	resp, ok := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.ImmediateResponse)

	require.True(t, ok, "with no suffix composite to enter, a suspended primary must not fall back to the full composite")
	assert.Equal(t, 503, resp.StatusCode)
}

// TestOnRequestBody_MultipleTargetsSelectsMatchingEntryAndOwnRetryCount pins
// that with two independently-configured targets[] entries, the requested
// model selects the RIGHT entry's own AggregateCluster and its own
// x-envoy-max-retries — never the other entry's chain depth, even though
// both entries are evaluated by the same findEntry loop.
func TestOnRequestBody_MultipleTargetsSelectsMatchingEntryAndOwnRetryCount(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{
				{
					FailoverTarget:   FailoverTarget{Model: "gpt-4o"},
					Fallbacks:        []FailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
					AggregateCluster: "failover_agg_chat_0",
				},
				{
					FailoverTarget: FailoverTarget{Model: "gpt-4o-mini"},
					Fallbacks: []FailoverTarget{
						{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"},
						{Model: "command-r-plus", Provider: "cohere-upstream"},
					},
					AggregateCluster: "failover_agg_chat_1",
				},
			},
		},
		susp: &suspensionState{suspended: make(map[string]time.Time)},
	}

	reqCtx0 := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}
	mods0 := p.OnRequestBody(context.Background(), reqCtx0, nil).(policy.UpstreamRequestModifications)
	require.NotNil(t, mods0.UpstreamName)
	assert.Equal(t, "failover_agg_chat_0", *mods0.UpstreamName)
	assert.Equal(t, "1", mods0.HeadersToSet["x-envoy-max-retries"], "entry 0 has exactly one fallback")

	reqCtx1 := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o-mini"}`), Present: true}}
	mods1 := p.OnRequestBody(context.Background(), reqCtx1, nil).(policy.UpstreamRequestModifications)
	require.NotNil(t, mods1.UpstreamName)
	assert.Equal(t, "failover_agg_chat_1", *mods1.UpstreamName)
	assert.Equal(t, "2", mods1.HeadersToSet["x-envoy-max-retries"], "entry 1 has two fallbacks — must not inherit entry 0's count")
}

func TestOnRequestBody_EmptyAggregateClusterIsNoop(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{Targets: []FailoverTargetEntry{{FailoverTarget: FailoverTarget{Model: "gpt-4o"}}}},
		susp:   &suspensionState{suspended: make(map[string]time.Time)},
	}
	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

	mods := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)

	assert.Nil(t, mods.UpstreamName, "no injected aggregate cluster means normal routing, never a pointer to an empty string")
}

func TestSuspension_ConcurrentAccess(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{SuspendDuration: 60},
		susp:   &suspensionState{suspended: make(map[string]time.Time)},
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); p.suspend("gpt-4o", "", "") }()
		go func() { defer wg.Done(); _ = p.isSuspended("gpt-4o", "", "") }()
	}
	wg.Wait()
	assert.True(t, p.isSuspended("gpt-4o", "", ""))
}

// ─── Upstream-attempt member resolution (host-metadata based) ───────────────

// chainPolicy's primary and fallback each carry a distinct controller-
// injected ClusterName, as the controller always sets for every real
// cluster — never left empty — so member-cluster-name matching has
// something to match against.
func chainPolicy() *Policy {
	return &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{{
				FailoverTarget: FailoverTarget{Model: "gpt-4o", ClusterName: "cluster_openai_primary"},
				Fallbacks: []FailoverTarget{
					{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream", ClusterName: "cluster_anthropic_upstream", SuffixCluster: "failover_suffix_chat_0_1"},
				},
				AggregateCluster: "failover_agg_chat_0",
			}},
			SuspendDuration: 900,
		},
		susp: &suspensionState{suspended: make(map[string]time.Time)},
	}
}

// memberReqCtx builds an upstream-attempt RequestHeaderContext directly from
// the two signals resolveAttemptForCluster actually uses: routeCluster (from
// xds.cluster_name) and memberClusterName (from xds.upstream_host_metadata,
// the real host's own declared identity) — no x-envoy-attempt-count header,
// since resolution no longer depends on it except as resolveMemberWithinEntry's
// narrow same-cluster tiebreaker (see attemptReqCtxTiebreak for that case).
func memberReqCtx(routeCluster, memberClusterName string) *policy.RequestHeaderContext {
	return &policy.RequestHeaderContext{
		SharedContext: &policy.SharedContext{Metadata: map[string]interface{}{}},
		Headers:       policy.NewHeaders(nil),
		Upstream:      &policy.UpstreamRequestContext{RouteCluster: routeCluster, MemberClusterName: memberClusterName},
	}
}

func TestOnRequestHeaders_ResolvesPrimaryMemberAndSeedsMetadata(t *testing.T) {
	reqCtx := memberReqCtx("failover_agg_chat_0", "cluster_openai_primary")
	chainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "gpt-4o", reqCtx.SharedContext.Metadata[selectedModelMetadataKey])
	provider, present := reqCtx.SharedContext.Metadata[selectedProviderMetadataKey]
	assert.True(t, present)
	assert.Equal(t, "", provider)
}

func TestOnRequestHeaders_ResolvesFallbackMember(t *testing.T) {
	reqCtx := memberReqCtx("failover_agg_chat_0", "cluster_anthropic_upstream")
	chainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "claude-sonnet-4-5-20250929", reqCtx.SharedContext.Metadata[selectedModelMetadataKey])
	assert.Equal(t, "anthropic-upstream", reqCtx.SharedContext.Metadata[selectedProviderMetadataKey])
}

func TestOnRequestHeaders_EmptyMemberClusterNameOnAggregateMatchIsNoop(t *testing.T) {
	reqCtx := memberReqCtx("failover_agg_chat_0", "")
	chainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Empty(t, reqCtx.SharedContext.Metadata)
}

func TestOnRequestHeaders_MemberClusterNameNotInThisEntryIsNoop(t *testing.T) {
	reqCtx := memberReqCtx("failover_agg_chat_0", "cluster_belongs_to_no_member")
	chainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Empty(t, reqCtx.SharedContext.Metadata)
}

func TestOnRequestHeaders_UnknownRouteClusterIsNoop(t *testing.T) {
	reqCtx := memberReqCtx("some-unrelated-cluster", "cluster_openai_primary")
	chainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Empty(t, reqCtx.SharedContext.Metadata)
}

func TestOnRequestHeaders_DownstreamInvocationIsNoop(t *testing.T) {
	reqCtx := memberReqCtx("failover_agg_chat_0", "cluster_openai_primary")
	reqCtx.Downstream = &policy.DownstreamContext{}
	chainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Empty(t, reqCtx.SharedContext.Metadata)
}

func TestOnResponseHeaders_RecordsSuspensionOn5xx(t *testing.T) {
	p := chainPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		ResponseStatus: 500,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_openai_primary"},
	}

	action := p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.Nil(t, action, "primary attempt sets no resolved-provider header")
	assert.True(t, p.isSuspended("gpt-4o", "", ""))
}

func TestOnResponseHeaders_FallbackMemberSetsResolvedProviderHeader(t *testing.T) {
	p := chainPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		ResponseStatus: 200,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_anthropic_upstream"},
	}

	action := p.OnResponseHeaders(context.Background(), respCtx, nil)

	mods, ok := action.(policy.DownstreamResponseHeaderModifications)
	require.True(t, ok)
	assert.Equal(t, "anthropic-upstream", mods.HeadersToSet[ResolvedFailoverProviderHeader])
	assert.False(t, p.isSuspended("claude-sonnet-4-5-20250929", "anthropic-upstream", ""))
}

func TestOnResponseHeaders_UnknownClusterIsNoop(t *testing.T) {
	p := chainPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		ResponseStatus: 500,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "other"},
	}

	assert.Nil(t, p.OnResponseHeaders(context.Background(), respCtx, nil))
	assert.False(t, p.isSuspended("gpt-4o", "", ""))
}

func primaryResolvingPolicy() *Policy {
	p := chainPolicy()
	p.params.PrimaryProvider = "openai-primary"
	return p
}

func TestParseParams_CarriesPrimaryProvider(t *testing.T) {
	params, err := parseParams(map[string]interface{}{
		"targets":         []interface{}{map[string]interface{}{"model": "gpt-4o", "fallbacks": []interface{}{}}},
		"primaryProvider": "openai-primary",
	})
	require.NoError(t, err)
	assert.Equal(t, "openai-primary", params.PrimaryProvider)
}

func TestOnRequestHeaders_EmptyProviderTargetSeedsPrimaryProvider(t *testing.T) {
	reqCtx := memberReqCtx("failover_agg_chat_0", "cluster_openai_primary")
	primaryResolvingPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "openai-primary", reqCtx.SharedContext.Metadata[selectedProviderMetadataKey])
}

func TestOnResponseHeaders_5xxSuspendsUnderResolvedKeyAndDownstreamSeesIt(t *testing.T) {
	p := primaryResolvingPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		ResponseStatus: 503,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_openai_primary"},
	}
	p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.True(t, p.isSuspended("gpt-4o", "openai-primary", ""))
	assert.False(t, p.isSuspended("gpt-4o", "", ""), "must not be recorded under the empty key")

	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}
	mods, ok := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)
	require.True(t, ok)
	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "failover_suffix_chat_0_1", *mods.UpstreamName, "primary is suspended; downstream must enter at the first non-suspended fallback's suffix composite")
	assert.Equal(t, "0", mods.HeadersToSet["x-envoy-max-retries"], "the fallback is the last member, so the suffix leaves nothing to retry into")
}

// threeMemberChainPolicy has a primary and TWO fallbacks, each fallback
// carrying a SuffixCluster, as gateway-controller populates via
// xds.SuffixCompositeClusterName.
func threeMemberChainPolicy() *Policy {
	return &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{{
				FailoverTarget: FailoverTarget{Model: "gpt-4o", ClusterName: "cluster_openai_primary"},
				Fallbacks: []FailoverTarget{
					{Model: "gpt-4o-mini", ClusterName: "cluster_openai_mini", SuffixCluster: "failover_suffix_from_1"},
					{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream", ClusterName: "cluster_anthropic_upstream", SuffixCluster: "failover_suffix_from_2"},
				},
				AggregateCluster: "failover_agg_chat_0",
			}},
			SuspendDuration: 900,
		},
		susp: &suspensionState{suspended: make(map[string]time.Time)},
	}
}

func TestOnRequestBody_SuspendedPrimaryBypassesToFirstFallbacksSuffix(t *testing.T) {
	p := threeMemberChainPolicy()
	p.susp.suspended[suspensionKey("gpt-4o", "", "")] = time.Now().Add(time.Hour)

	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}
	mods, ok := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)
	require.True(t, ok)
	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "failover_suffix_from_1", *mods.UpstreamName,
		"must dispatch at the suffix composite covering fallback 1 onward, so a failure of fallback 1 can still retry into fallback 2 within this request")
	assert.Equal(t, "1", mods.HeadersToSet["x-envoy-max-retries"], "one member (fallback 2) remains after fallback 1 in this suffix")
}

func TestOnRequestBody_TwoSuspendedMembersBypassesToLastMembersOwnSuffix(t *testing.T) {
	p := threeMemberChainPolicy()
	p.susp.suspended[suspensionKey("gpt-4o", "", "")] = time.Now().Add(time.Hour)
	p.susp.suspended[suspensionKey("gpt-4o-mini", "", "")] = time.Now().Add(time.Hour)

	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}
	mods, ok := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)
	require.True(t, ok)
	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "failover_suffix_from_2", *mods.UpstreamName, "must skip both suspended members and dispatch at fallback 2's own suffix")
	assert.Equal(t, "0", mods.HeadersToSet["x-envoy-max-retries"], "fallback 2 is the last chain member; its own single-member suffix has nothing left to retry into")
}

// ─── Suspension gating ───────────────────────────────────────────────────────

func TestOnResponseHeaders_4xxDoesNotSuspend(t *testing.T) {
	p := chainPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		ResponseStatus: 429,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_openai_primary"},
	}

	p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.False(t, p.isSuspended("gpt-4o", "", ""), "a 4xx is the client's problem, not a failing target")
}

func TestOnResponseHeaders_ZeroSuspendDurationDoesNotSuspend(t *testing.T) {
	p := chainPolicy()
	p.params.SuspendDuration = 0
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		ResponseStatus: 503,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_openai_primary"},
	}

	p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.False(t, p.isSuspended("gpt-4o", "", ""), "suspendDuration 0 disables suspension entirely")
}

// ─── Per-attempt :path correction ────────────────────────────────────────────

// pathChainPolicy carries the basePath/operationPath the controller injects, so
// each member's correct outbound :path is computable.
func pathChainPolicy() *Policy {
	p := chainPolicy()
	p.params.OperationPath = "/chat/completions"
	p.params.Targets[0].BasePath = "/openai-provider"
	p.params.Targets[0].Fallbacks[0].BasePath = "/anthropic-provider"
	return p
}

func memberReqCtxWithPath(routeCluster, memberClusterName, path string) *policy.RequestHeaderContext {
	reqCtx := memberReqCtx(routeCluster, memberClusterName)
	reqCtx.Path = path
	return reqCtx
}

// TestOnRequestHeaders_FallbackRewritesStalePathToMembersBasePath is the
// regression test for a bug caught by live e2e verification: the downstream
// phase rewrites :path exactly once, before Envoy's first dispatch, using the
// PRIMARY member's base path. Envoy replays that same :path verbatim on a
// retry — unlike Host, it is not recomputed per attempt via auto_host_rewrite.
// Every chain member being a loopback upstream on the same host:port, an
// uncorrected :path sends the retry straight back to the provider that just
// failed, regardless of which real cluster Envoy dialed.
func TestOnRequestHeaders_FallbackRewritesStalePathToMembersBasePath(t *testing.T) {
	reqCtx := memberReqCtxWithPath("failover_agg_chat_0", "cluster_anthropic_upstream", "/openai-provider/chat/completions")

	action := pathChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestHeaderModifications)
	require.True(t, ok, "an escalated attempt on a different provider's base path must correct :path")
	require.NotNil(t, mods.Path)
	assert.Equal(t, "/anthropic-provider/chat/completions", *mods.Path)
}

// TestOnRequestHeaders_FallbackRewriteToPreservesOriginalQueryString is the
// regression test for the design doc's "Per-attempt path reconstruction does
// not preserve the original query string" finding: joinBasePathAndOperation
// builds the corrected path from only basePath+operationPath (both
// query-free), so a stale ":path" carrying "?stream=true" from an earlier
// attempt must have that query string reattached, not dropped.
func TestOnRequestHeaders_FallbackRewriteToPreservesOriginalQueryString(t *testing.T) {
	reqCtx := memberReqCtxWithPath("failover_agg_chat_0", "cluster_anthropic_upstream", "/openai-provider/chat/completions?stream=true")

	action := pathChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestHeaderModifications)
	require.True(t, ok, "an escalated attempt must still correct :path when a query string is present")
	require.NotNil(t, mods.Path)
	assert.Equal(t, "/anthropic-provider/chat/completions?stream=true", *mods.Path)
}

func TestOnRequestHeaders_PrimaryNoPathMutationWhenAlreadyCorrect(t *testing.T) {
	reqCtx := memberReqCtxWithPath("failover_agg_chat_0", "cluster_openai_primary", "/openai-provider/chat/completions")

	action := pathChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Nil(t, action, "the primary attempt's :path is already this member's own — nothing to correct")
}

func TestOnRequestHeaders_EmptyBasePathLeavesPathUntouched(t *testing.T) {
	p := pathChainPolicy()
	p.params.Targets[0].Fallbacks[0].BasePath = ""
	reqCtx := memberReqCtxWithPath("failover_agg_chat_0", "cluster_anthropic_upstream", "/openai-provider/chat/completions")

	action := p.OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Nil(t, action, "no injected basePath means nothing to correct TO — never guess a root-relative path")
	assert.Equal(t, "/openai-provider/chat/completions", reqCtx.Path)
}

func TestOnRequestHeaders_RebasesWithoutNeedingOperationPath(t *testing.T) {
	p := pathChainPolicy()
	p.params.OperationPath = ""
	reqCtx := memberReqCtxWithPath("failover_agg_chat_0", "cluster_anthropic_upstream", "/openai-provider/chat/completions")

	mods, ok := p.OnRequestHeaders(context.Background(), reqCtx, nil).(policy.UpstreamRequestHeaderModifications)

	require.True(t, ok)
	require.NotNil(t, mods.Path)
	assert.Equal(t, "/anthropic-provider/chat/completions", *mods.Path, "only the base-path prefix is swapped; the rest comes from the replayed path")
}

func TestOnRequestHeaders_UnknownPrefixAndNoOperationPathLeavesPathUntouched(t *testing.T) {
	p := pathChainPolicy()
	p.params.OperationPath = ""
	reqCtx := memberReqCtxWithPath("failover_agg_chat_0", "cluster_anthropic_upstream", "/somewhere-else/chat/completions")

	assert.Nil(t, p.OnRequestHeaders(context.Background(), reqCtx, nil))
}

// Gemini/Bedrock templates put the model in the path. The route's
// OperationPath is a TEMPLATE there, so rebuilding :path from it would send
// "{model}" upstream; swapping only the base-path prefix keeps the client's
// concrete segment, which the pathParam model rewrite then replaces.
func TestOnRequestHeaders_PathParamProviderKeepsConcretePathAndRewritesModel(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{
		OperationPath: "/models/{model}:generateContent",
		RequestModel:  RequestModelConfig{Location: requestModelLocationPathParam, Identifier: `models/([a-zA-Z0-9.\-]+)`},
		Targets: []FailoverTargetEntry{{
			FailoverTarget: FailoverTarget{Model: "gemini-1.5-pro", ClusterName: "leaf_0", BasePath: "/gemini-primary"},
			Fallbacks: []FailoverTarget{
				{Model: "gemini-1.5-flash", Provider: "gemini-backup", ClusterName: "leaf_1", SuffixCluster: "suffix_1", BasePath: "/gemini-backup"},
			},
			AggregateCluster: "agg_0",
		}},
	}, susp: newSuspensionState()}
	reqCtx := memberReqCtxWithPath("agg_0", "leaf_1", "/gemini-primary/models/gemini-1.5-pro:generateContent?alt=sse")

	mods, ok := p.OnRequestHeaders(context.Background(), reqCtx, nil).(policy.UpstreamRequestHeaderModifications)

	require.True(t, ok)
	require.NotNil(t, mods.Path)
	assert.Equal(t, "/gemini-backup/models/gemini-1.5-flash:generateContent?alt=sse", *mods.Path)
}

func TestRebaseAttemptPath(t *testing.T) {
	bases := []string{"/openai", "/openai-backup", "/anthropic/"}
	cases := []struct{ name, current, newBase, want string }{
		{"swaps the matching prefix", "/openai/chat/completions", "/anthropic", "/anthropic/chat/completions"},
		{"keeps the query string", "/openai/chat/completions?stream=true", "/anthropic", "/anthropic/chat/completions?stream=true"},
		{"matches on a segment boundary, longest wins", "/openai-backup/chat", "/anthropic", "/anthropic/chat"},
		{"trailing-slash base still matches", "/anthropic/v1/messages", "/openai", "/openai/v1/messages"},
		{"base alone", "/openai", "/anthropic", "/anthropic"},
		{"root new base", "/openai/chat", "/", "/chat"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, rebaseAttemptPath(c.current, c.newBase, bases, "/unused"))
		})
	}
	assert.Equal(t, "/anthropic/chat/completions?x=1", rebaseAttemptPath("/elsewhere/chat?x=1", "/anthropic", bases, "/chat/completions"),
		"no known prefix falls back to newBase + operationPath")
}

// TestOnRequestHeaders_SetsUpstreamBasePathForThisAttempt pins the other half
// of the aggregate-attempt gap: the kernel resolves a backend by
// xds.cluster_name, which for an aggregate attempt is the aggregate's own name
// and resolves nothing, so BasePath would otherwise stay empty for every later
// policy in the attempt.
func TestOnRequestHeaders_SetsUpstreamBasePathForThisAttempt(t *testing.T) {
	reqCtx := memberReqCtxWithPath("failover_agg_chat_0", "cluster_anthropic_upstream", "/openai-provider/chat/completions")

	pathChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "/anthropic-provider", reqCtx.Upstream.BasePath)
}

func TestParseParams_CarriesInjectedBasePathAndOperationPath(t *testing.T) {
	params, err := parseParams(map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"model":    "gpt-4o",
				"basePath": "/openai-provider",
				"fallbacks": []interface{}{
					map[string]interface{}{"model": "claude", "provider": "anthropic-upstream", "basePath": "/anthropic-provider"},
				},
				"aggregateCluster": "failover_agg_chat_0",
			},
		},
		"operationPath": "/chat/completions",
	})

	require.NoError(t, err)
	assert.Equal(t, "/chat/completions", params.OperationPath)
	assert.Equal(t, "/openai-provider", params.Targets[0].BasePath)
	assert.Equal(t, "/anthropic-provider", params.Targets[0].Fallbacks[0].BasePath)
}

func TestJoinBasePathAndOperation(t *testing.T) {
	assert.Equal(t, "/p/chat", joinBasePathAndOperation("/p", "/chat"))
	assert.Equal(t, "/p/chat", joinBasePathAndOperation("/p/", "/chat"))
	assert.Equal(t, "/p/chat", joinBasePathAndOperation("/p", "chat"))
	assert.Equal(t, "/chat", joinBasePathAndOperation("/", "/chat"))
	assert.Equal(t, "/chat", joinBasePathAndOperation("", "/chat"))
	assert.Equal(t, "", joinBasePathAndOperation("/p", ""))
}

// ─── Suspended-primary bypass: upstream-phase identification ─────────────────

// bypassChainPolicy is pathChainPolicy plus the controller-injected cluster
// names for each member. A suspended-prefix bypass enters at the fallback's
// suffix composite, so an attempt it produces reports that suffix as
// xds.cluster_name and the fallback's own leaf as its host-metadata identity.
func bypassChainPolicy() *Policy {
	p := pathChainPolicy()
	p.params.PrimaryProvider = "openai-upstream"
	p.params.Targets[0].ClusterName = "upstream_LlmProxy_abc_openai-upstream"
	p.params.Targets[0].Fallbacks[0].ClusterName = "upstream_LlmProxy_abc_anthropic-upstream"
	p.params.Targets[0].Fallbacks[0].SuffixCluster = bypassSuffixCluster
	return p
}

const bypassSuffixCluster = "failover_suffix_chat_0_1"

// TestOnRequestHeaders_BypassClusterSeedsSelectedProviderAndModel is the
// regression test for the suspended-prefix bypass: OnRequestBody dispatches at
// a fallback's suffix composite, so RouteCluster is that suffix and never the
// aggregate. Without the suffix entry-point match nothing seeds
// selected_provider, every provider-scoped upstream attachment's CEL gate is
// false, and the fallback is dialed with no credential injected and an
// untranslated body.
func TestOnRequestHeaders_BypassClusterSeedsSelectedProviderAndModel(t *testing.T) {
	reqCtx := memberReqCtxWithPath(bypassSuffixCluster, "upstream_LlmProxy_abc_anthropic-upstream",
		"/anthropic-provider/chat/completions")

	bypassChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "anthropic-upstream", reqCtx.SharedContext.Metadata[selectedProviderMetadataKey])
	assert.Equal(t, "claude-sonnet-4-5-20250929", reqCtx.SharedContext.Metadata[selectedModelMetadataKey])
}

func TestOnRequestHeaders_BypassClusterSetsUpstreamBasePath(t *testing.T) {
	reqCtx := memberReqCtxWithPath(bypassSuffixCluster, "upstream_LlmProxy_abc_anthropic-upstream",
		"/anthropic-provider/chat/completions")

	action := bypassChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "/anthropic-provider", reqCtx.Upstream.BasePath)
	assert.Nil(t, action, "the downstream redirect already rewrote :path to this member's base path")
}

// The primary member's cluster is the route's OWN default cluster, which an
// ordinary request whose model matches no chain also lands on. Only an
// entry-point cluster (aggregate or suffix) identifies a chain attempt, so a
// bare member cluster as xds.cluster_name must never seed a chain's
// model/provider onto a request that never entered the chain.
func TestOnRequestHeaders_PrimaryMemberClusterNotMatchedOutsideAggregate(t *testing.T) {
	reqCtx := memberReqCtxWithPath("upstream_LlmProxy_abc_openai-upstream", "upstream_LlmProxy_abc_openai-upstream",
		"/openai-provider/chat/completions")

	bypassChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Empty(t, reqCtx.SharedContext.Metadata)
}

// A member with no controller-injected clusterName must never match — an empty
// RouteCluster is already rejected, but an empty stored name must not match an
// arbitrary cluster either.
func TestOnRequestHeaders_EmptyMemberClusterNameNeverMatches(t *testing.T) {
	p := bypassChainPolicy()
	p.params.Targets[0].Fallbacks[0].ClusterName = ""
	reqCtx := memberReqCtxWithPath("some-unrelated-cluster", "some-unrelated-cluster", "/x")

	p.OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Empty(t, reqCtx.SharedContext.Metadata)
}

func TestOnRequestHeaders_NilSharedContextIsCreated(t *testing.T) {
	reqCtx := memberReqCtxWithPath(bypassSuffixCluster, "upstream_LlmProxy_abc_anthropic-upstream",
		"/anthropic-provider/chat/completions")
	reqCtx.SharedContext = nil

	bypassChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	require.NotNil(t, reqCtx.SharedContext)
	assert.Equal(t, "anthropic-upstream", reqCtx.SharedContext.Metadata[selectedProviderMetadataKey])
}

func TestOnRequestHeaders_NilMetadataMapIsCreated(t *testing.T) {
	reqCtx := memberReqCtxWithPath(bypassSuffixCluster, "upstream_LlmProxy_abc_anthropic-upstream",
		"/anthropic-provider/chat/completions")
	reqCtx.SharedContext = &policy.SharedContext{}

	bypassChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "anthropic-upstream", reqCtx.SharedContext.Metadata[selectedProviderMetadataKey])
}

func TestOnResponseHeaders_BypassClusterSuspendsAndAttributes(t *testing.T) {
	p := bypassChainPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		ResponseStatus: 503,
		Upstream: &policy.UpstreamResponseContext{
			RouteCluster:      bypassSuffixCluster,
			MemberClusterName: "upstream_LlmProxy_abc_anthropic-upstream",
		},
	}

	action := p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.True(t, p.isSuspended("claude-sonnet-4-5-20250929", "anthropic-upstream", ""))
	mods, ok := action.(policy.DownstreamResponseHeaderModifications)
	require.True(t, ok, "a bypass dispatch IS an escalation past the primary")
	assert.Equal(t, "anthropic-upstream", mods.HeadersToSet[ResolvedFailoverProviderHeader])
}

func TestParseParams_CarriesInjectedClusterName(t *testing.T) {
	params, err := parseParams(map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"model":       "gpt-4o",
				"clusterName": "upstream_x_primary",
				"fallbacks": []interface{}{
					map[string]interface{}{"model": "claude", "provider": "anthropic-upstream", "clusterName": "upstream_x_anthropic"},
				},
				"aggregateCluster": "failover_agg_chat_0",
			},
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "upstream_x_primary", params.Targets[0].ClusterName)
	assert.Equal(t, "upstream_x_anthropic", params.Targets[0].Fallbacks[0].ClusterName)
}

// An aggregate route-cluster match scopes resolution to just that entry's own
// members — even if (pathologically) a fallback's own ClusterName happened to
// collide with the aggregate's name, routeCluster alone decides which entry
// is in play, and memberClusterName is only ever compared against THAT
// entry's own primary/fallbacks.
func TestResolveAttemptForCluster_AggregateRouteClusterScopesResolutionToItsOwnEntry(t *testing.T) {
	p := bypassChainPolicy()

	match := p.resolveAttemptForCluster("failover_agg_chat_0", "upstream_LlmProxy_abc_openai-upstream", 1)

	require.NotNil(t, match)
	assert.Equal(t, "gpt-4o", match.member.Model)
	assert.Equal(t, 1, match.index)
}

// ─── Same-cluster tiebreak: a same-provider fallback shares its primary's cluster ─

// sameClusterChainPolicy has a fallback with NO distinct physical cluster of
// its own (empty Provider defaults to the primary's identity, and — since
// nothing gives it a different upstream — the controller injects the SAME
// ClusterName as the primary). Only the model differs. This is the
// "same-provider, same-backend, cheaper-model" case.
func sameClusterChainPolicy() *Policy {
	return &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{{
				FailoverTarget: FailoverTarget{Model: "gpt-4o", ClusterName: "cluster_openai_primary"},
				Fallbacks: []FailoverTarget{
					{Model: "gpt-4o-mini", ClusterName: "cluster_openai_primary"},
					{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream", ClusterName: "cluster_anthropic_upstream"},
				},
				AggregateCluster: "failover_agg_chat_0",
			}},
			SuspendDuration: 900,
		},
		susp: &suspensionState{suspended: make(map[string]time.Time)},
	}
}

func TestResolveMemberWithinEntry_SameClusterTieBreaksToAttemptOneAsPrimary(t *testing.T) {
	p := sameClusterChainPolicy()

	match := p.resolveAttemptForCluster("failover_agg_chat_0", "cluster_openai_primary", 1)

	require.NotNil(t, match)
	assert.Equal(t, "gpt-4o", match.member.Model, "attempt 1 on the shared cluster is the primary")
	assert.Equal(t, 1, match.index)
}

func TestResolveMemberWithinEntry_SameClusterTieBreaksToAttemptTwoAsSameProviderFallback(t *testing.T) {
	p := sameClusterChainPolicy()

	match := p.resolveAttemptForCluster("failover_agg_chat_0", "cluster_openai_primary", 2)

	require.NotNil(t, match)
	assert.Equal(t, "gpt-4o-mini", match.member.Model, "attempt 2 on the SAME shared cluster is the same-provider fallback, not the primary")
	assert.Equal(t, 2, match.index)
}

func TestResolveMemberWithinEntry_SameClusterTieDefaultsToFirstCandidateWhenAttemptUnresolved(t *testing.T) {
	p := sameClusterChainPolicy()

	// attempt 99 matches neither tied candidate's own index (1 or 2) — falls
	// back to the first declared candidate (the primary) rather than
	// resolving to nothing.
	match := p.resolveAttemptForCluster("failover_agg_chat_0", "cluster_openai_primary", 99)

	require.NotNil(t, match)
	assert.Equal(t, "gpt-4o", match.member.Model)
}

func TestResolveMemberWithinEntry_DistinctClusterFallbackIsUnaffectedByTiebreak(t *testing.T) {
	p := sameClusterChainPolicy()

	// The cross-provider fallback has its OWN distinct cluster — exactly one
	// candidate, so the attempt argument is irrelevant to it.
	match := p.resolveAttemptForCluster("failover_agg_chat_0", "cluster_anthropic_upstream", 1)

	require.NotNil(t, match)
	assert.Equal(t, "claude-sonnet-4-5-20250929", match.member.Model)
	assert.Equal(t, 3, match.index)
}

// ─── Configurable statusCodes ────────────────────────────────────────────────

func TestParseParams_CarriesStatusCodes(t *testing.T) {
	params, err := parseParams(map[string]interface{}{
		"targets":     []interface{}{map[string]interface{}{"model": "gpt-4o", "fallbacks": []interface{}{}}},
		"statusCodes": []interface{}{float64(500), float64(502), float64(429)},
	})

	require.NoError(t, err)
	assert.Equal(t, []int{500, 502, 429}, params.StatusCodes)
}

func TestParseParams_StatusCodesOmittedIsNilNotEmptySlice(t *testing.T) {
	params, err := parseParams(map[string]interface{}{
		"targets": []interface{}{map[string]interface{}{"model": "gpt-4o", "fallbacks": []interface{}{}}},
	})

	require.NoError(t, err)
	assert.Nil(t, params.StatusCodes, "omitted statusCodes must default via isFailureStatus, not an explicit empty list")
}

func TestParseParams_StatusCodesRejectsNonNumberEntries(t *testing.T) {
	_, err := parseParams(map[string]interface{}{
		"targets":     []interface{}{map[string]interface{}{"model": "gpt-4o", "fallbacks": []interface{}{}}},
		"statusCodes": []interface{}{"500"},
	})

	require.Error(t, err)
}

func TestIsFailureStatus_DefaultsToAny5xx(t *testing.T) {
	p := chainPolicy()

	assert.True(t, p.isFailureStatus(500))
	assert.True(t, p.isFailureStatus(503))
	assert.True(t, p.isFailureStatus(599))
	assert.False(t, p.isFailureStatus(429), "no configured statusCodes: a 4xx is the client's problem, not a failing target")
	assert.False(t, p.isFailureStatus(200))
}

func TestIsFailureStatus_ConfiguredCodesReplaceNotExtendTheDefault(t *testing.T) {
	p := chainPolicy()
	p.params.StatusCodes = []int{429, 502}

	assert.True(t, p.isFailureStatus(429), "explicitly configured code")
	assert.True(t, p.isFailureStatus(502), "explicitly configured code")
	assert.False(t, p.isFailureStatus(500), "500 is not in the configured list, so it no longer counts as a failure")
	assert.False(t, p.isFailureStatus(503))
}

func TestOnResponseHeaders_ConfiguredStatusCodeSuspendsANonDefault4xx(t *testing.T) {
	p := chainPolicy()
	p.params.StatusCodes = []int{429}
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		ResponseStatus: 429,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_openai_primary"},
	}

	p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.True(t, p.isSuspended("gpt-4o", "", ""), "429 is configured as a failure status for this chain")
}

func TestOnResponseHeaders_ConfiguredStatusCodesExcludeDefault5xx(t *testing.T) {
	p := chainPolicy()
	p.params.StatusCodes = []int{429}
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		ResponseStatus: 500,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_openai_primary"},
	}

	p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.False(t, p.isSuspended("gpt-4o", "", ""), "500 was replaced out of the trigger set by an explicit statusCodes list")
}

// ─── UpstreamDefinition (author-facing dial-target override) ────────────────

func TestParseParams_CarriesUpstreamDefinition(t *testing.T) {
	params, err := parseParams(map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"model": "gpt-4o",
				"fallbacks": []interface{}{
					map[string]interface{}{
						"model":              "claude",
						"provider":           "anthropic-upstream",
						"upstreamDefinition": "anthropic-eu-west",
					},
				},
			},
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "anthropic-eu-west", params.Targets[0].Fallbacks[0].UpstreamDefinition)
	// Provider stays the credential/transform identity, independent of which
	// physical upstream was named — the whole point of the split.
	assert.Equal(t, "anthropic-upstream", params.Targets[0].Fallbacks[0].Provider)
}

// ─── Suspension state shared across GetPolicy() calls for the same chain ────

// TestGetPolicy_SharesSuspensionStateAcrossInstancesOfTheSameChain is the
// regression test for a real, live-e2e-caught bug: model-failover is
// attached twice per route (once downstream via operationPolicies:, once
// upstream via the controller's synthesized copy), and
// registry.GetInstance creates a genuinely separate *Policy object for each
// attachment — no built-in instance caching. Before sharedSuspensionStateFor
// existed, each *Policy had its own private suspendedTargets map, so a
// suspension the upstream instance recorded (OnResponseHeaders, on a 5xx)
// was invisible to a DIFFERENT *Policy instance's isSuspended check
// (OnRequestBody) — the downstream routing decision never saw it, and a
// known-bad primary was never actually bypassed. Reproduced live: two
// requests through the real gateway, back to back, both hit the primary
// even after the first one's 500 should have suspended it.
func TestGetPolicy_SharesSuspensionStateAcrossInstancesOfTheSameChain(t *testing.T) {
	raw := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"model":            "gpt-4o",
				"aggregateCluster": "failover_agg_shared_test_0",
				"fallbacks": []interface{}{
					map[string]interface{}{"model": "claude", "provider": "anthropic-upstream"},
				},
			},
		},
		"suspendDuration": float64(900),
	}

	downstreamInstance, err := GetPolicy(policy.PolicyMetadata{}, raw)
	require.NoError(t, err)
	upstreamInstance, err := GetPolicy(policy.PolicyMetadata{}, raw)
	require.NoError(t, err)

	downstreamPolicy := downstreamInstance.(*Policy)
	upstreamPolicy := upstreamInstance.(*Policy)
	require.NotSame(t, downstreamPolicy, upstreamPolicy,
		"GetPolicy must still return a distinct *Policy per call (fresh params on redeploy) — only suspension state is shared")

	// The upstream instance records a suspension (as OnResponseHeaders would
	// on a 5xx)...
	upstreamPolicy.suspend("gpt-4o", "", "")

	// ...and the DOWNSTREAM instance — a different Go object — must see it.
	assert.True(t, downstreamPolicy.isSuspended("gpt-4o", "", ""),
		"suspension recorded by one *Policy instance of a chain must be visible to another instance of the SAME chain")
}

// A *Policy built without an injected aggregateCluster (e.g. directly, as
// every other test in this file does) must not share state with anything —
// sharedSuspensionKey returns "" and each instance gets its own private map.
func TestGetPolicy_NoAggregateClusterMeansPrivateUnsharedState(t *testing.T) {
	raw := map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{"model": "gpt-4o", "fallbacks": []interface{}{}},
		},
	}

	a, err := GetPolicy(policy.PolicyMetadata{}, raw)
	require.NoError(t, err)
	b, err := GetPolicy(policy.PolicyMetadata{}, raw)
	require.NoError(t, err)

	aPolicy, bPolicy := a.(*Policy), b.(*Policy)
	aPolicy.params.SuspendDuration = 900
	aPolicy.suspend("gpt-4o", "", "")

	assert.False(t, bPolicy.isSuspended("gpt-4o", "", ""),
		"without an aggregate cluster key, instances must not accidentally share suspension state")
}

// ─── Upstream-attempt body rewrite (model translation with no provider change) ─

// samePlusCrossProviderChainPolicy has a same-provider fallback (empty
// Provider, cheaper model, same physical cluster as the primary) AND a
// cross-provider fallback, on the same target entry, so tests can exercise
// both without duplicating the chain shape.
func samePlusCrossProviderChainPolicy() *Policy {
	return &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{{
				FailoverTarget: FailoverTarget{Model: "gpt-4o", ClusterName: "cluster_openai_primary"},
				Fallbacks: []FailoverTarget{
					{Model: "gpt-4o-mini", ClusterName: "cluster_openai_primary"}, // same provider (empty -> primary), same cluster
					{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream", ClusterName: "cluster_anthropic_upstream"},
				},
				AggregateCluster: "failover_agg_chat_0",
			}},
		},
		susp: &suspensionState{suspended: make(map[string]time.Time)},
	}
}

func attemptHeaders(n string) *policy.Headers {
	return policy.NewHeaders(map[string][]string{"x-envoy-attempt-count": {n}})
}

func TestOnRequestBody_UpstreamInvocationRewritesModelForSameProviderFallback(t *testing.T) {
	p := samePlusCrossProviderChainPolicy()
	reqCtx := &policy.RequestContext{
		// Downstream == nil is the upstream-attempt signal. The attempt
		// header is needed here specifically to break the same-cluster tie
		// between the primary and this same-provider fallback.
		Headers:  attemptHeaders("2"),
		Upstream: &policy.UpstreamRequestContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_openai_primary"},
		Body:     &policy.Body{Content: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`), Present: true},
	}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	require.NotNil(t, mods.Body, "must rewrite the replayed body since Envoy never recomputes it per attempt")
	assert.Nil(t, mods.UpstreamName, "an upstream-attempt invocation must never re-target the upstream cluster")

	var rewritten map[string]interface{}
	require.NoError(t, json.Unmarshal(mods.Body, &rewritten))
	assert.Equal(t, "gpt-4o-mini", rewritten["model"])
	assert.NotNil(t, rewritten["messages"], "only the model field changes, the rest of the payload survives")
}

func TestOnRequestBody_UpstreamInvocationNoRewriteWhenModelAlreadyMatches(t *testing.T) {
	p := samePlusCrossProviderChainPolicy()
	reqCtx := &policy.RequestContext{
		Headers:  attemptHeaders("1"),
		Upstream: &policy.UpstreamRequestContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_openai_primary"},
		Body:     &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true},
	}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	assert.Nil(t, mods.Body, "attempt 1 already carries the primary's own model — nothing to rewrite")
}

func TestOnRequestBody_UpstreamInvocationAlsoRewritesModelForCrossProviderFallback(t *testing.T) {
	p := samePlusCrossProviderChainPolicy()
	reqCtx := &policy.RequestContext{
		Upstream: &policy.UpstreamRequestContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_anthropic_upstream"},
		Body:     &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true},
	}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	require.NotNil(t, mods.Body)
	var rewritten map[string]interface{}
	require.NoError(t, json.Unmarshal(mods.Body, &rewritten))
	assert.Equal(t, "claude-sonnet-4-5-20250929", rewritten["model"],
		"harmless even here: openai-to-anthropic-transformer rebuilds the body from its own params.model anyway")
}

func TestOnRequestBody_UpstreamInvocationUnknownClusterIsNoop(t *testing.T) {
	p := samePlusCrossProviderChainPolicy()
	reqCtx := &policy.RequestContext{
		Upstream: &policy.UpstreamRequestContext{RouteCluster: "some-unrelated-cluster"},
		Body:     &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true},
	}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	assert.Nil(t, mods.Body)
}

func TestOnRequestBody_UpstreamInvocationMalformedBodyIsNoop(t *testing.T) {
	p := samePlusCrossProviderChainPolicy()
	reqCtx := &policy.RequestContext{
		Upstream: &policy.UpstreamRequestContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_anthropic_upstream"},
		Body:     &policy.Body{Content: []byte(`not json`), Present: true},
	}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	assert.Nil(t, mods.Body)
}

func TestOnRequestBody_UpstreamInvocationNilUpstreamIsNoop(t *testing.T) {
	p := samePlusCrossProviderChainPolicy()
	reqCtx := &policy.RequestContext{
		Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true},
	}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	assert.Nil(t, mods.Body)
	assert.Nil(t, mods.UpstreamName)
}

// ─── Consecutive-failure threshold and exponential backoff ─────────────────

func TestParseParams_CarriesCircuitThresholds(t *testing.T) {
	params, err := parseParams(map[string]interface{}{
		"targets":                     []interface{}{map[string]interface{}{"model": "gpt-4o", "fallbacks": []interface{}{}}},
		"suspendAfterFailures":        float64(3),
		"failureRateThresholdPercent": float64(50),
		"latencyThresholdMs":          float64(2000),
	})
	require.NoError(t, err)
	assert.Equal(t, 3, params.SuspendAfterFailures)
	assert.Equal(t, 50, params.FailureRateThresholdPercent)
	assert.Equal(t, 2000, params.LatencyThresholdMs)
}

func TestParseParams_SuspendAfterFailuresRejectsNonNumber(t *testing.T) {
	_, err := parseParams(map[string]interface{}{
		"targets":              []interface{}{map[string]interface{}{"model": "gpt-4o", "fallbacks": []interface{}{}}},
		"suspendAfterFailures": "three",
	})
	require.Error(t, err)
}

func TestRecordOutcome_DefaultThresholdSuspendsOnFirstFailure(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60}, susp: newSuspensionState()}

	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0)

	assert.True(t, p.isSuspended("gpt-4o", "", ""), "SuspendAfterFailures unset must default to 1 (today's original behavior)")
}

func TestRecordOutcome_ConfiguredThresholdRequiresConsecutiveFailures(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60, SuspendAfterFailures: 3}, susp: newSuspensionState()}

	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0)
	assert.False(t, p.isSuspended("gpt-4o", "", ""), "1 of 3 required failures must not suspend yet")

	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0)
	assert.False(t, p.isSuspended("gpt-4o", "", ""), "2 of 3 required failures must not suspend yet")

	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0)
	assert.True(t, p.isSuspended("gpt-4o", "", ""), "the 3rd consecutive failure must suspend")
}

func TestRecordOutcome_SuccessResetsTheConsecutiveFailureCounter(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60, SuspendAfterFailures: 3}, susp: newSuspensionState()}

	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0)
	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0)
	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, false, false, 0) // success — counter must reset to zero
	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0)

	assert.False(t, p.isSuspended("gpt-4o", "", ""), "only 1 consecutive failure since the last success — must not suspend")
}

func TestRecordOutcome_ZeroSuspendDurationNeverTracksOrSuspends(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 0, SuspendAfterFailures: 1}, susp: newSuspensionState()}

	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0)

	assert.False(t, p.isSuspended("gpt-4o", "", ""))
	assert.Empty(t, p.susp.failureCounts, "suspension disabled entirely means no bookkeeping at all, not just no suspension")
}

func TestBackoffDuration_FirstStreakIsExactlyTheBaseDuration(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60}}
	assert.Equal(t, 60*time.Second, p.backoffDuration(1))
}

func TestBackoffDuration_DoublesEachStreak(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60}}
	assert.Equal(t, 60*time.Second, p.backoffDuration(1))
	assert.Equal(t, 120*time.Second, p.backoffDuration(2))
	assert.Equal(t, 240*time.Second, p.backoffDuration(3))
	assert.Equal(t, 480*time.Second, p.backoffDuration(4))
}

func TestBackoffDuration_CapIsEightTimesBase(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60}}

	// streak 4 (8x) is exactly the cap; streak 5+ (16x, uncapped) must clamp
	// down to that same 8x ceiling.
	assert.Equal(t, 480*time.Second, p.backoffDuration(4))
	assert.Equal(t, 480*time.Second, p.backoffDuration(5))
	assert.Equal(t, 480*time.Second, p.backoffDuration(10))
}

func TestRecordOutcome_ImmediateResuspensionAfterExpiryDoublesTheWindow(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60}, susp: newSuspensionState()}

	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0) // 1st suspension: streak 1, ~60s window
	require.True(t, p.isSuspended("gpt-4o", "", ""))
	firstUntil := p.susp.suspended[suspensionKey("gpt-4o", "", "")]
	assert.WithinDuration(t, time.Now().Add(60*time.Second), firstUntil, 2*time.Second)

	// Force the window to have already expired (isSuspended's lazy-expiry
	// pattern), then fail again immediately — simulating "still broken the
	// moment it came back".
	p.susp.mu.Lock()
	p.susp.suspended[suspensionKey("gpt-4o", "", "")] = time.Now().Add(-time.Second)
	p.susp.mu.Unlock()

	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0) // 2nd consecutive cycle: streak 2, ~120s window
	require.True(t, p.isSuspended("gpt-4o", "", ""))
	secondUntil := p.susp.suspended[suspensionKey("gpt-4o", "", "")]
	assert.WithinDuration(t, time.Now().Add(120*time.Second), secondUntil, 2*time.Second,
		"a target that fails again immediately after its window expires must get a longer window, not the same flat one")
}

func TestRecordOutcome_SuccessResetsTheBackoffStreak(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60}, susp: newSuspensionState()}

	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0) // streak 1
	p.susp.mu.Lock()
	p.susp.suspended[suspensionKey("gpt-4o", "", "")] = time.Now().Add(-time.Second) // force-expire
	p.susp.mu.Unlock()
	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, false, false, 0) // success in between — must reset the streak, not just the counter

	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0) // a fresh incident: must be back to streak 1, not streak 2
	until := p.susp.suspended[suspensionKey("gpt-4o", "", "")]
	assert.WithinDuration(t, time.Now().Add(60*time.Second), until, 2*time.Second,
		"an intervening success must reset backoff back to the base duration for the next incident")
}

// TestRecordOutcome_LongGapWithNoSuccessAlsoResetsTheStreak is the regression
// test for a real live-e2e-caught bug: a target that hits recordOutcome
// repeatedly across unrelated callers or test scenarios, with no SUCCESS
// call in between for this key — but with real wall-clock time passing
// between each failure — kept incorrectly compounding backoff (5s -> 10s ->
// 20s -> ...) as if it were one continuously-failing incident. A suspend
// window's caller has no way to observe "the target came back healthy" if
// nothing ever calls it successfully; a long enough gap with no activity at
// all must reset the streak too, not just an explicit success.
func TestRecordOutcome_LongGapWithNoSuccessAlsoResetsTheStreak(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 5}, susp: newSuspensionState()}

	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0) // streak 1, ~5s window
	firstUntil := p.susp.suspended[suspensionKey("gpt-4o", "", "")]
	assert.WithinDuration(t, time.Now().Add(5*time.Second), firstUntil, time.Second)

	// Simulate real time passing well beyond the streak's hot window (base +
	// duration) with NO success and NO further failures for this key — e.g.
	// an unrelated test scenario ran in between, never touching this target.
	p.susp.mu.Lock()
	p.susp.suspended[suspensionKey("gpt-4o", "", "")] = time.Now().Add(-time.Hour)
	p.susp.streakExpiresAt[suspensionKey("gpt-4o", "", "")] = time.Now().Add(-time.Hour)
	p.susp.mu.Unlock()

	p.recordOutcome(FailoverTarget{Model: "gpt-4o"}, true, false, 0) // a genuinely new, unrelated incident
	secondUntil := p.susp.suspended[suspensionKey("gpt-4o", "", "")]
	assert.WithinDuration(t, time.Now().Add(5*time.Second), secondUntil, time.Second,
		"a failure long after the streak went cold must get the base window, not a compounded one")
}

// ─── ECI #18469 finding #6: suspension key includes upstreamDefinition ──────

func TestSuspensionKey_DistinguishesUpstreamDefinition(t *testing.T) {
	assert.NotEqual(t,
		suspensionKey("gpt-4o", "openai", "us-east"),
		suspensionKey("gpt-4o", "openai", "us-west"),
		"identical model+provider but different physical upstream must not share a suspension entry")
}

func TestRecordOutcome_SuspendsOnlyTheFailingUpstreamDefinitionNotItsSibling(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 30}, susp: newSuspensionState()}
	failing := FailoverTarget{Model: "gpt-4o", Provider: "openai", UpstreamDefinition: "us-east"}
	sibling := FailoverTarget{Model: "gpt-4o", Provider: "openai", UpstreamDefinition: "us-west"}

	p.recordOutcome(failing, true, false, 0)

	assert.True(t, p.isSuspended(failing.Model, failing.Provider, failing.UpstreamDefinition))
	assert.False(t, p.isSuspended(sibling.Model, sibling.Provider, sibling.UpstreamDefinition),
		"a distinct upstreamDefinition is a distinct physical target — its health must be independent")
}

// ─── ECI #18469 finding #2: all-suspended returns ImmediateResponse ─────────

func TestOnRequestBody_AllMembersSuspendedReturnsImmediateResponseExhaustion(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			SuspendDuration: 30,
			Targets: []FailoverTargetEntry{{
				FailoverTarget: FailoverTarget{Model: "gpt-4o"},
				Fallbacks: []FailoverTarget{
					{Model: "claude", Provider: "anthropic-upstream", ClusterName: "anthropic_leaf", SuffixCluster: "failover_suffix_1"},
				},
				AggregateCluster: "failover_agg_chat_0",
			}},
		},
		susp: &suspensionState{suspended: map[string]time.Time{
			suspensionKey("gpt-4o", "", ""):                   time.Now().Add(time.Hour),
			suspensionKey("claude", "anthropic-upstream", ""): time.Now().Add(time.Hour),
		}},
	}
	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	resp, ok := action.(policy.ImmediateResponse)
	require.True(t, ok, "every member suspended must short-circuit the chain, not dispatch to the full composite")
	assert.Equal(t, 503, resp.StatusCode)
	assert.NotEmpty(t, resp.Body)
}

// ─── ECI #18469 finding #4: rolling failure-rate rule ───────────────────────

// rateOnlyPolicy disables the consecutive-count trigger so only the rolling
// rules are under test.
func rateOnlyPolicy(params ModelFailoverParams) *Policy {
	params.SuspendDuration = 30
	params.SuspendAfterFailures = 1000
	return &Policy{params: params, susp: newSuspensionState()}
}

func TestRecordOutcome_RollingFailureRateSuspendsOnceThresholdAndMinimumSamplesAreMet(t *testing.T) {
	p := rateOnlyPolicy(ModelFailoverParams{FailureRateThresholdPercent: 50})
	target := FailoverTarget{Model: "gpt-4o"}

	for i := 0; i < circuitMinimumSamples-1; i++ {
		p.recordOutcome(target, i%2 == 0, false, 0) // more than half failing, but below the minimum samples
	}
	assert.False(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition), "below minimum samples — must not evaluate the rate yet")

	p.recordOutcome(target, true, false, 0) // minimum samples met, rate over 50%
	assert.True(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition))
}

func TestRecordOutcome_RollingFailureRateBelowThresholdDoesNotSuspend(t *testing.T) {
	p := rateOnlyPolicy(ModelFailoverParams{FailureRateThresholdPercent: 50})
	target := FailoverTarget{Model: "gpt-4o"}

	for i := 0; i < circuitMinimumSamples*2; i++ {
		p.recordOutcome(target, i%4 == 0, false, 0) // 25% failing
	}
	assert.False(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition))
}

func TestRecordOutcome_RollingFailureRateDisabledWhenThresholdUnset(t *testing.T) {
	p := rateOnlyPolicy(ModelFailoverParams{})
	target := FailoverTarget{Model: "gpt-4o"}

	for i := 0; i < circuitMinimumSamples*2; i++ {
		p.recordOutcome(target, true, false, 0)
	}

	assert.False(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition),
		"FailureRateThresholdPercent<=0 must disable the rate rule entirely")
	assert.Empty(t, p.susp.rateWindows, "a disabled rule keeps no window at all")
}

func TestRecordOutcome_SuspensionClearsTheWindowsSoRecoveryIsJudgedAfresh(t *testing.T) {
	p := rateOnlyPolicy(ModelFailoverParams{FailureRateThresholdPercent: 50})
	target := FailoverTarget{Model: "gpt-4o"}
	key := p.keyFor(target)

	for i := 0; i < circuitMinimumSamples; i++ {
		p.recordOutcome(target, true, false, 0)
	}
	require.True(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition))

	// The window expires and the half-open probe succeeds, closing the circuit.
	p.susp.suspended[key] = time.Now().Add(-time.Second)
	require.True(t, p.allowRequest(target))
	p.recordOutcome(target, false, false, 0)
	require.False(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition))

	// One more success must not re-trip the rule off the pre-suspension failures.
	p.recordOutcome(target, false, false, 0)
	assert.False(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition),
		"failures from before the suspension must not count against the recovered target")
}

// ─── ECI #18469 finding #4: latency rule ────────────────────────────────────

func TestRecordOutcome_LatencySuspendsOnceP95BreachesAcrossTheWindow(t *testing.T) {
	p := rateOnlyPolicy(ModelFailoverParams{LatencyThresholdMs: 100})
	target := FailoverTarget{Model: "gpt-4o"}

	for i := 0; i < circuitMinimumSamples-1; i++ {
		p.recordOutcome(target, false, true, 200) // slow, but below the minimum samples
	}
	assert.False(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition), "below minimum samples — must not evaluate p95 yet")

	p.recordOutcome(target, false, true, 200)
	assert.True(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition),
		"p95 of ten 200ms samples breaches the 100ms threshold — a slow success still suspends")
}

func TestRecordOutcome_LatencyOccasionalSlowAttemptDoesNotSuspend(t *testing.T) {
	p := rateOnlyPolicy(ModelFailoverParams{LatencyThresholdMs: 100})
	target := FailoverTarget{Model: "gpt-4o"}

	p.recordOutcome(target, false, true, 5000) // one outlier
	for i := 0; i < 30; i++ {
		p.recordOutcome(target, false, true, 50)
	}
	assert.False(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition),
		"one slow attempt out of 31 sits above p95 — it must not suspend the target")
}

func TestRecordOutcome_NoLatencySampleNeverFeedsTheLatencyRule(t *testing.T) {
	p := rateOnlyPolicy(ModelFailoverParams{LatencyThresholdMs: 1})
	target := FailoverTarget{Model: "gpt-4o"}

	for i := 0; i < circuitMinimumSamples; i++ {
		p.recordOutcome(target, false, false, 0) // hasLatency=false — must be ignored entirely
	}

	assert.False(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition))
}

// ─── ECI #18469 finding #5: half-open controlled recovery ──────────────────

func TestAllowRequest_HalfOpenAllowsOneProbeAndClosesOnSuccess(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 30}, susp: newSuspensionState()}
	target := FailoverTarget{Model: "gpt-4o"}
	key := p.keyFor(target)
	p.susp.suspended[key] = time.Now().Add(-time.Second) // already expired -> half-open

	assert.True(t, p.allowRequest(target), "first probe must be granted")
	assert.False(t, p.allowRequest(target), "a probe is already in flight — a second concurrent request must be blocked")

	p.recordOutcome(target, false, false, 0) // the in-flight probe succeeds
	assert.False(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition), "a successful probe must close the circuit")
	assert.True(t, p.allowRequest(target), "circuit is closed — no longer gated by the probe")
	assert.True(t, p.allowRequest(target))
}

func TestAllowRequest_HalfOpenReopensOnFailedProbe(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 30}, susp: newSuspensionState()}
	target := FailoverTarget{Model: "gpt-4o"}
	key := p.keyFor(target)
	p.susp.suspended[key] = time.Now().Add(-time.Second) // already expired -> half-open

	require.True(t, p.allowRequest(target))
	p.recordOutcome(target, true, false, 0) // the probe fails

	assert.True(t, p.isSuspended(target.Model, target.Provider, target.UpstreamDefinition), "a failed probe must reopen (re-suspend) the circuit")
	assert.False(t, p.allowRequest(target), "freshly reopened — the new suspension window has not expired yet")
}

func TestAllowRequest_LostProbeIsReleasedAfterItsLease(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 30}, susp: newSuspensionState()}
	target := FailoverTarget{Model: "gpt-4o"}
	key := p.keyFor(target)
	p.susp.suspended[key] = time.Now().Add(-time.Second)

	require.True(t, p.allowRequest(target))
	require.False(t, p.allowRequest(target))

	// The probe's outcome never arrives (transport failure, client cancel).
	p.susp.probes[key].grantedAt = time.Now().Add(-circuitProbeLease - time.Second)

	assert.True(t, p.allowRequest(target), "a probe lost past its lease must not hold the target half-open forever")
}

// ─── ECI #18469 finding #7: idle registry eviction ──────────────────────────

func TestSharedSuspensionStateFor_EvictsRegistryEntriesIdlePastTheHorizon(t *testing.T) {
	params := ModelFailoverParams{Targets: []FailoverTargetEntry{{AggregateCluster: "failover_agg_evict_test"}}}

	first := sharedSuspensionStateFor(params)
	first.suspended["marker"] = time.Now().Add(time.Hour) // distinguishes this instance from a freshly-created one

	sharedSuspensionMu.Lock()
	key := sharedSuspensionKey(params)
	sharedSuspensionRegistry[key].lastTouched = time.Now().Add(-idleEvictionHorizon - time.Minute)
	sharedSuspensionMu.Unlock()

	second := sharedSuspensionStateFor(params)

	_, stillHasMarker := second.suspended["marker"]
	assert.False(t, stillHasMarker, "an idle-past-horizon entry must be evicted and replaced with a fresh state")
}

func TestSharedSuspensionStateFor_RecentTrafficKeepsAnUnredeployedEntryAlive(t *testing.T) {
	params := ModelFailoverParams{
		SuspendDuration: 30,
		Targets:         []FailoverTargetEntry{{FailoverTarget: FailoverTarget{Model: "gpt-4o"}, AggregateCluster: "failover_agg_traffic_test"}},
	}

	first := sharedSuspensionStateFor(params)
	first.suspended["marker"] = time.Now().Add(time.Hour)

	sharedSuspensionMu.Lock()
	sharedSuspensionRegistry[sharedSuspensionKey(params)].lastTouched = time.Now().Add(-idleEvictionHorizon - time.Minute)
	sharedSuspensionMu.Unlock()

	// Traffic after the stale redeploy timestamp: one routing decision.
	(&Policy{params: params, susp: first}).allowRequest(FailoverTarget{Model: "gpt-4o"})

	second := sharedSuspensionStateFor(params)

	_, stillHasMarker := second.suspended["marker"]
	assert.True(t, stillHasMarker, "a route with live traffic must not be evicted just because it hasn't been redeployed in 24h")
}

// ─── requestModel locations (from the provider template) ───────────────────

func requestModelPolicy(rm RequestModelConfig) *Policy {
	p := chainPolicy()
	p.params.RequestModel = rm
	return p
}

func downstreamCtx(path string, headers map[string][]string, body string) *policy.RequestContext {
	ctx := &policy.RequestContext{
		Path:       path,
		Downstream: &policy.DownstreamContext{Request: &policy.DownstreamRequest{Path: path, Headers: policy.NewHeaders(headers)}},
	}
	if body != "" {
		ctx.Body = &policy.Body{Content: []byte(body), Present: true}
	}
	return ctx
}

func TestOnRequestBody_MatchesTheModelWhereverTheTemplateSaysItLives(t *testing.T) {
	cases := []struct {
		name string
		rm   RequestModelConfig
		ctx  *policy.RequestContext
	}{
		{"payload, template JSONPath form", RequestModelConfig{Location: requestModelLocationPayload, Identifier: "$.model"},
			downstreamCtx("/p/chat", nil, `{"model":"gpt-4o"}`)},
		{"nested payload", RequestModelConfig{Location: requestModelLocationPayload, Identifier: "$.input.model"},
			downstreamCtx("/p/chat", nil, `{"input":{"model":"gpt-4o"}}`)},
		{"header", RequestModelConfig{Location: requestModelLocationHeader, Identifier: "x-model"},
			downstreamCtx("/p/chat", map[string][]string{"X-Model": {"gpt-4o"}}, "")},
		{"query param", RequestModelConfig{Location: requestModelLocationQueryParam, Identifier: "model"},
			downstreamCtx("/p/chat?model=gpt-4o&stream=true", nil, "")},
		{"path param", RequestModelConfig{Location: requestModelLocationPathParam, Identifier: `models/([a-zA-Z0-9.\-]+)`},
			downstreamCtx("/p/models/gpt-4o:generateContent", nil, "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mods, ok := requestModelPolicy(c.rm).OnRequestBody(context.Background(), c.ctx, nil).(policy.UpstreamRequestModifications)
			require.True(t, ok)
			require.NotNil(t, mods.UpstreamName, "the requested model must be found at %s", c.rm.Location)
			assert.Equal(t, "failover_agg_chat_0", *mods.UpstreamName)
		})
	}
}

func TestOnRequestBody_ModelAtADifferentLocationIsNotMatched(t *testing.T) {
	p := requestModelPolicy(RequestModelConfig{Location: requestModelLocationHeader, Identifier: "x-model"})

	mods := p.OnRequestBody(context.Background(), downstreamCtx("/p/chat", nil, `{"model":"gpt-4o"}`), nil).(policy.UpstreamRequestModifications)

	assert.Nil(t, mods.UpstreamName, "a header-located template must not fall back to reading the body")
}

func TestOnRequestHeaders_RewritesModelHeaderForTheFallback(t *testing.T) {
	p := requestModelPolicy(RequestModelConfig{Location: requestModelLocationHeader, Identifier: "x-model"})
	reqCtx := memberReqCtx("failover_agg_chat_0", "cluster_anthropic_upstream")
	reqCtx.Headers = policy.NewHeaders(map[string][]string{"x-model": {"gpt-4o"}})

	mods, ok := p.OnRequestHeaders(context.Background(), reqCtx, nil).(policy.UpstreamRequestHeaderModifications)

	require.True(t, ok)
	assert.Equal(t, "claude-sonnet-4-5-20250929", mods.HeadersToSet["x-model"])
}

func TestOnRequestHeaders_RewritesModelQueryParamForTheFallback(t *testing.T) {
	p := requestModelPolicy(RequestModelConfig{Location: requestModelLocationQueryParam, Identifier: "model"})
	reqCtx := memberReqCtxWithPath("failover_agg_chat_0", "cluster_anthropic_upstream", "/chat?model=gpt-4o")

	mods, ok := p.OnRequestHeaders(context.Background(), reqCtx, nil).(policy.UpstreamRequestHeaderModifications)

	require.True(t, ok)
	assert.Equal(t, []string{"claude-sonnet-4-5-20250929"}, mods.QueryParametersToAdd["model"])
}

func TestOnRequestHeaders_PrimaryAttemptLeavesAlreadyCorrectModelAlone(t *testing.T) {
	p := requestModelPolicy(RequestModelConfig{Location: requestModelLocationHeader, Identifier: "x-model"})
	reqCtx := memberReqCtx("failover_agg_chat_0", "cluster_openai_primary")
	reqCtx.Headers = policy.NewHeaders(map[string][]string{"x-model": {"gpt-4o"}})

	assert.Nil(t, p.OnRequestHeaders(context.Background(), reqCtx, nil))
}

func TestOnRequestBody_UpstreamRewritesNestedPayloadModel(t *testing.T) {
	p := requestModelPolicy(RequestModelConfig{Location: requestModelLocationPayload, Identifier: "$.input.model"})
	reqCtx := &policy.RequestContext{
		Headers:  policy.NewHeaders(nil),
		Body:     &policy.Body{Content: []byte(`{"input":{"model":"gpt-4o"},"model":"untouched"}`), Present: true},
		Upstream: &policy.UpstreamRequestContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_anthropic_upstream"},
	}

	mods, ok := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)

	require.True(t, ok)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(mods.Body, &body))
	assert.Equal(t, "claude-sonnet-4-5-20250929", body["input"].(map[string]interface{})["model"])
	assert.Equal(t, "untouched", body["model"], "only the template's own location is rewritten")
}

func TestOnRequestBody_UpstreamSkipsBodyForNonPayloadLocations(t *testing.T) {
	p := requestModelPolicy(RequestModelConfig{Location: requestModelLocationHeader, Identifier: "x-model"})
	reqCtx := &policy.RequestContext{
		Headers:  policy.NewHeaders(nil),
		Body:     &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true},
		Upstream: &policy.UpstreamRequestContext{RouteCluster: "failover_agg_chat_0", MemberClusterName: "cluster_anthropic_upstream"},
	}

	mods := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)

	assert.Nil(t, mods.Body, "the model lives in a header here; the body is not the policy's to touch")
}

func TestParseParams_ReadsAndDefaultsRequestModel(t *testing.T) {
	targets := []interface{}{map[string]interface{}{"model": "gpt-4o"}}

	params, err := parseParams(map[string]interface{}{"targets": targets})
	require.NoError(t, err)
	assert.Equal(t, RequestModelConfig{Location: requestModelLocationPayload, Identifier: "model"}, params.RequestModel)

	params, err = parseParams(map[string]interface{}{"targets": targets,
		"requestModel": map[string]interface{}{"location": "pathParam", "identifier": "models/([a-z]+)"}})
	require.NoError(t, err)
	assert.Equal(t, RequestModelConfig{Location: requestModelLocationPathParam, Identifier: "models/([a-z]+)"}, params.RequestModel)

	_, err = parseParams(map[string]interface{}{"targets": targets,
		"requestModel": map[string]interface{}{"location": "cookie", "identifier": "m"}})
	assert.Error(t, err)
}

// ─── rolling window bucket rotation ────────────────────────────────────────

func TestRollingWindow_AgedOutBucketsStopCounting(t *testing.T) {
	w := newRollingWindow()
	now := w.buckets[0].start
	bucket := circuitWindow / circuitBuckets

	w.recordOutcome(now, true)
	w.recordOutcome(now.Add(bucket+time.Second), false) // next bucket
	attempts, failures := w.totals()
	assert.Equal(t, 2, attempts)
	assert.Equal(t, 1, failures)

	w.recordOutcome(now.Add(circuitWindow+2*bucket), false) // a full lap later: everything older is gone
	attempts, failures = w.totals()
	assert.Equal(t, 1, attempts)
	assert.Equal(t, 0, failures, "a failure from outside the window must not keep counting toward the rate")
}

func TestRollingWindow_LatencyPercentileSpansTheWholeWindow(t *testing.T) {
	w := newRollingWindow()
	now := w.buckets[0].start
	bucket := circuitWindow / circuitBuckets

	for i := 0; i < 20; i++ {
		w.recordLatency(now.Add(time.Duration(i%circuitBuckets)*bucket), 100*(i+1)) // spread over every bucket
	}
	pct, ok := w.latencyPercentile(95, 20)
	require.True(t, ok)
	assert.Equal(t, 1900, pct, "nearest-rank p95 of 100..2000 is the 19th sample")

	_, ok = w.latencyPercentile(95, 21)
	assert.False(t, ok, "too few samples must not be evaluated")

	w.recordLatency(now.Add(circuitWindow+circuitWindow), 50) // everything older aged out
	_, ok = w.latencyPercentile(95, 20)
	assert.False(t, ok)
}

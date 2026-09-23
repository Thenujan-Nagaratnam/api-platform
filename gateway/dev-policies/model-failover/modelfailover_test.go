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

func TestOnRequestBody_SuspendedTargetSkipsToFallback(t *testing.T) {
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
		susp: &suspensionState{suspended: map[string]time.Time{
			suspensionKey("gpt-4o", ""): time.Now().Add(time.Hour),
		}},
	}
	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "anthropic-upstream", *mods.UpstreamName, "must bypass the aggregate and target the fallback's own upstream directly")
}

func TestOnRequestBody_SkipsFallbackWithEmptyProvider(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{{
				FailoverTarget: FailoverTarget{Model: "gpt-4o"},
				Fallbacks: []FailoverTarget{
					{Model: "same-provider-model"},
					{Model: "claude", Provider: "anthropic-upstream"},
				},
				AggregateCluster: "failover_agg_chat_0",
			}},
		},
		susp: &suspensionState{suspended: map[string]time.Time{suspensionKey("gpt-4o", ""): time.Now().Add(time.Hour)}},
	}
	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

	mods := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)

	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "anthropic-upstream", *mods.UpstreamName)
}

func TestOnRequestBody_OnlyEmptyProviderFallbacksRoutesToAggregate(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{{
				FailoverTarget:   FailoverTarget{Model: "gpt-4o"},
				Fallbacks:        []FailoverTarget{{Model: "same-provider-model"}},
				AggregateCluster: "failover_agg_chat_0",
			}},
		},
		susp: &suspensionState{suspended: map[string]time.Time{suspensionKey("gpt-4o", ""): time.Now().Add(time.Hour)}},
	}
	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

	mods := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)

	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "failover_agg_chat_0", *mods.UpstreamName)
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
		go func() { defer wg.Done(); p.suspend("gpt-4o", "") }()
		go func() { defer wg.Done(); _ = p.isSuspended("gpt-4o", "") }()
	}
	wg.Wait()
	assert.True(t, p.isSuspended("gpt-4o", ""))
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
					{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream", ClusterName: "cluster_anthropic_upstream"},
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
	assert.True(t, p.isSuspended("gpt-4o", ""))
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
	assert.False(t, p.isSuspended("claude-sonnet-4-5-20250929", "anthropic-upstream"))
}

func TestOnResponseHeaders_UnknownClusterIsNoop(t *testing.T) {
	p := chainPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		ResponseStatus: 500,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "other"},
	}

	assert.Nil(t, p.OnResponseHeaders(context.Background(), respCtx, nil))
	assert.False(t, p.isSuspended("gpt-4o", ""))
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

	assert.True(t, p.isSuspended("gpt-4o", "openai-primary"))
	assert.False(t, p.isSuspended("gpt-4o", ""), "must not be recorded under the empty key")

	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}
	mods, ok := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)
	require.True(t, ok)
	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "anthropic-upstream", *mods.UpstreamName, "downstream must see the primary as suspended")
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

	assert.False(t, p.isSuspended("gpt-4o", ""), "a 4xx is the client's problem, not a failing target")
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

	assert.False(t, p.isSuspended("gpt-4o", ""), "suspendDuration 0 disables suspension entirely")
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

func TestOnRequestHeaders_MissingOperationPathLeavesPathUntouched(t *testing.T) {
	p := pathChainPolicy()
	p.params.OperationPath = ""
	reqCtx := memberReqCtxWithPath("failover_agg_chat_0", "cluster_anthropic_upstream", "/openai-provider/chat/completions")

	assert.Nil(t, p.OnRequestHeaders(context.Background(), reqCtx, nil))
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

// bypassChainPolicy is pathChainPolicy plus the controller-injected real Envoy
// cluster name for each member — what an attempt dispatched straight onto a
// fallback (rather than through the aggregate) reports both as
// xds.cluster_name AND as its own host-metadata identity (both signals point
// at the same real cluster for a non-aggregate dispatch).
func bypassChainPolicy() *Policy {
	p := pathChainPolicy()
	p.params.PrimaryProvider = "openai-upstream"
	p.params.Targets[0].ClusterName = "upstream_LlmProxy_abc_openai-upstream"
	p.params.Targets[0].Fallbacks[0].ClusterName = "upstream_LlmProxy_abc_anthropic-upstream"
	return p
}

// TestOnRequestHeaders_BypassClusterSeedsSelectedProviderAndModel is the
// regression test for the suspended-primary bypass: OnRequestBody dispatches
// at a fallback's OWN cluster, so RouteCluster is that cluster and never the
// aggregate. Without the member-cluster match nothing seeds selected_provider,
// every provider-scoped upstream attachment's CEL gate is false, and the
// fallback is dialed with no credential injected and an untranslated body.
func TestOnRequestHeaders_BypassClusterSeedsSelectedProviderAndModel(t *testing.T) {
	reqCtx := memberReqCtxWithPath("upstream_LlmProxy_abc_anthropic-upstream", "upstream_LlmProxy_abc_anthropic-upstream",
		"/anthropic-provider/chat/completions")

	bypassChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "anthropic-upstream", reqCtx.SharedContext.Metadata[selectedProviderMetadataKey])
	assert.Equal(t, "claude-sonnet-4-5-20250929", reqCtx.SharedContext.Metadata[selectedModelMetadataKey])
}

func TestOnRequestHeaders_BypassClusterSetsUpstreamBasePath(t *testing.T) {
	reqCtx := memberReqCtxWithPath("upstream_LlmProxy_abc_anthropic-upstream", "upstream_LlmProxy_abc_anthropic-upstream",
		"/anthropic-provider/chat/completions")

	action := bypassChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "/anthropic-provider", reqCtx.Upstream.BasePath)
	assert.Nil(t, action, "the downstream redirect already rewrote :path to this member's base path")
}

// The primary member's cluster is the route's OWN default cluster, which an
// ordinary request whose model matches no chain also lands on. Matching it
// outside the aggregate would seed a chain's model/provider onto a request
// that never entered the chain, so the non-aggregate (bypass) pass only ever
// matches fallbacks.
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
	reqCtx := memberReqCtxWithPath("upstream_LlmProxy_abc_anthropic-upstream", "upstream_LlmProxy_abc_anthropic-upstream",
		"/anthropic-provider/chat/completions")
	reqCtx.SharedContext = nil

	bypassChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	require.NotNil(t, reqCtx.SharedContext)
	assert.Equal(t, "anthropic-upstream", reqCtx.SharedContext.Metadata[selectedProviderMetadataKey])
}

func TestOnRequestHeaders_NilMetadataMapIsCreated(t *testing.T) {
	reqCtx := memberReqCtxWithPath("upstream_LlmProxy_abc_anthropic-upstream", "upstream_LlmProxy_abc_anthropic-upstream",
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
			RouteCluster:      "upstream_LlmProxy_abc_anthropic-upstream",
			MemberClusterName: "upstream_LlmProxy_abc_anthropic-upstream",
		},
	}

	action := p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.True(t, p.isSuspended("claude-sonnet-4-5-20250929", "anthropic-upstream"))
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

	assert.True(t, p.isSuspended("gpt-4o", ""), "429 is configured as a failure status for this chain")
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

	assert.False(t, p.isSuspended("gpt-4o", ""), "500 was replaced out of the trigger set by an explicit statusCodes list")
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
	upstreamPolicy.suspend("gpt-4o", "")

	// ...and the DOWNSTREAM instance — a different Go object — must see it.
	assert.True(t, downstreamPolicy.isSuspended("gpt-4o", ""),
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
	aPolicy.suspend("gpt-4o", "")

	assert.False(t, bPolicy.isSuspended("gpt-4o", ""),
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

func TestParseParams_CarriesSuspendAfterFailuresAndMaxSuspendDuration(t *testing.T) {
	params, err := parseParams(map[string]interface{}{
		"targets":              []interface{}{map[string]interface{}{"model": "gpt-4o", "fallbacks": []interface{}{}}},
		"suspendAfterFailures": float64(3),
		"maxSuspendDuration":   float64(600),
	})
	require.NoError(t, err)
	assert.Equal(t, 3, params.SuspendAfterFailures)
	assert.Equal(t, 600, params.MaxSuspendDuration)
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

	p.recordOutcome("gpt-4o", "", true)

	assert.True(t, p.isSuspended("gpt-4o", ""), "SuspendAfterFailures unset must default to 1 (today's original behavior)")
}

func TestRecordOutcome_ConfiguredThresholdRequiresConsecutiveFailures(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60, SuspendAfterFailures: 3}, susp: newSuspensionState()}

	p.recordOutcome("gpt-4o", "", true)
	assert.False(t, p.isSuspended("gpt-4o", ""), "1 of 3 required failures must not suspend yet")

	p.recordOutcome("gpt-4o", "", true)
	assert.False(t, p.isSuspended("gpt-4o", ""), "2 of 3 required failures must not suspend yet")

	p.recordOutcome("gpt-4o", "", true)
	assert.True(t, p.isSuspended("gpt-4o", ""), "the 3rd consecutive failure must suspend")
}

func TestRecordOutcome_SuccessResetsTheConsecutiveFailureCounter(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60, SuspendAfterFailures: 3}, susp: newSuspensionState()}

	p.recordOutcome("gpt-4o", "", true)
	p.recordOutcome("gpt-4o", "", true)
	p.recordOutcome("gpt-4o", "", false) // success — counter must reset to zero
	p.recordOutcome("gpt-4o", "", true)

	assert.False(t, p.isSuspended("gpt-4o", ""), "only 1 consecutive failure since the last success — must not suspend")
}

func TestRecordOutcome_ZeroSuspendDurationNeverTracksOrSuspends(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 0, SuspendAfterFailures: 1}, susp: newSuspensionState()}

	p.recordOutcome("gpt-4o", "", true)

	assert.False(t, p.isSuspended("gpt-4o", ""))
	assert.Empty(t, p.susp.failureCounts, "suspension disabled entirely means no bookkeeping at all, not just no suspension")
}

func TestBackoffDuration_FirstStreakIsExactlyTheBaseDuration(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60}}
	assert.Equal(t, 60*time.Second, p.backoffDuration(1))
}

func TestBackoffDuration_DoublesEachStreak(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60, MaxSuspendDuration: 100000}}
	assert.Equal(t, 60*time.Second, p.backoffDuration(1))
	assert.Equal(t, 120*time.Second, p.backoffDuration(2))
	assert.Equal(t, 240*time.Second, p.backoffDuration(3))
	assert.Equal(t, 480*time.Second, p.backoffDuration(4))
}

func TestBackoffDuration_DefaultCapIsEightTimesBase(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60}} // MaxSuspendDuration unset

	// streak 4 (8x) is exactly the default cap; streak 5+ (16x, uncapped)
	// must clamp down to that same 8x ceiling.
	assert.Equal(t, 480*time.Second, p.backoffDuration(4))
	assert.Equal(t, 480*time.Second, p.backoffDuration(5))
	assert.Equal(t, 480*time.Second, p.backoffDuration(10))
}

func TestBackoffDuration_RespectsConfiguredMaxSuspendDuration(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60, MaxSuspendDuration: 150}}

	assert.Equal(t, 120*time.Second, p.backoffDuration(2))
	assert.Equal(t, 150*time.Second, p.backoffDuration(3), "240s would exceed the configured 150s cap")
	assert.Equal(t, 150*time.Second, p.backoffDuration(10))
}

func TestRecordOutcome_ImmediateResuspensionAfterExpiryDoublesTheWindow(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60, MaxSuspendDuration: 100000}, susp: newSuspensionState()}

	p.recordOutcome("gpt-4o", "", true) // 1st suspension: streak 1, ~60s window
	require.True(t, p.isSuspended("gpt-4o", ""))
	firstUntil := p.susp.suspended[suspensionKey("gpt-4o", "")]
	assert.WithinDuration(t, time.Now().Add(60*time.Second), firstUntil, 2*time.Second)

	// Force the window to have already expired (isSuspended's lazy-expiry
	// pattern), then fail again immediately — simulating "still broken the
	// moment it came back".
	p.susp.mu.Lock()
	p.susp.suspended[suspensionKey("gpt-4o", "")] = time.Now().Add(-time.Second)
	p.susp.mu.Unlock()

	p.recordOutcome("gpt-4o", "", true) // 2nd consecutive cycle: streak 2, ~120s window
	require.True(t, p.isSuspended("gpt-4o", ""))
	secondUntil := p.susp.suspended[suspensionKey("gpt-4o", "")]
	assert.WithinDuration(t, time.Now().Add(120*time.Second), secondUntil, 2*time.Second,
		"a target that fails again immediately after its window expires must get a longer window, not the same flat one")
}

func TestRecordOutcome_SuccessResetsTheBackoffStreak(t *testing.T) {
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 60, MaxSuspendDuration: 100000}, susp: newSuspensionState()}

	p.recordOutcome("gpt-4o", "", true) // streak 1
	p.susp.mu.Lock()
	p.susp.suspended[suspensionKey("gpt-4o", "")] = time.Now().Add(-time.Second) // force-expire
	p.susp.mu.Unlock()
	p.recordOutcome("gpt-4o", "", false) // success in between — must reset the streak, not just the counter

	p.recordOutcome("gpt-4o", "", true) // a fresh incident: must be back to streak 1, not streak 2
	until := p.susp.suspended[suspensionKey("gpt-4o", "")]
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
	p := &Policy{params: ModelFailoverParams{SuspendDuration: 5, MaxSuspendDuration: 100000}, susp: newSuspensionState()}

	p.recordOutcome("gpt-4o", "", true) // streak 1, ~5s window
	firstUntil := p.susp.suspended[suspensionKey("gpt-4o", "")]
	assert.WithinDuration(t, time.Now().Add(5*time.Second), firstUntil, time.Second)

	// Simulate real time passing well beyond the streak's hot window (base +
	// duration) with NO success and NO further failures for this key — e.g.
	// an unrelated test scenario ran in between, never touching this target.
	p.susp.mu.Lock()
	p.susp.suspended[suspensionKey("gpt-4o", "")] = time.Now().Add(-time.Hour)
	p.susp.streakExpiresAt[suspensionKey("gpt-4o", "")] = time.Now().Add(-time.Hour)
	p.susp.mu.Unlock()

	p.recordOutcome("gpt-4o", "", true) // a genuinely new, unrelated incident
	secondUntil := p.susp.suspended[suspensionKey("gpt-4o", "")]
	assert.WithinDuration(t, time.Now().Add(5*time.Second), secondUntil, time.Second,
		"a failure long after the streak went cold must get the base window, not a compounded one")
}

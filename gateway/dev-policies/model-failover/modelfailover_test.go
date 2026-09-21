package modelfailover

import (
	"context"
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
				"target": map[string]interface{}{"model": "gpt-4o"},
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
	assert.Equal(t, "gpt-4o", params.Targets[0].Target.Model)
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
					Target:           FailoverTarget{Model: "gpt-4o"},
					Fallbacks:        []FailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
					AggregateCluster: "failover_agg_chat_0",
				},
			},
		},
		suspendedTargets: make(map[string]time.Time),
	}
	reqCtx := &policy.RequestContext{
		Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true},
	}

	action := p.OnRequestBody(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestModifications)
	require.True(t, ok)
	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "failover_agg_chat_0", *mods.UpstreamName)
}

func TestOnRequestBody_NoMatchIsNoop(t *testing.T) {
	p := &Policy{
		params:           ModelFailoverParams{Targets: []FailoverTargetEntry{{Target: FailoverTarget{Model: "gpt-4o"}}}},
		suspendedTargets: make(map[string]time.Time),
	}
	reqCtx := &policy.RequestContext{Body: &policy.Body{Content: []byte(`{"model":"some-other-model"}`), Present: true}}

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
					Target:           FailoverTarget{Model: "gpt-4o"},
					Fallbacks:        []FailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
					AggregateCluster: "failover_agg_chat_0",
				},
			},
		},
		suspendedTargets: map[string]time.Time{
			suspensionKey("gpt-4o", ""): time.Now().Add(time.Hour),
		},
	}
	reqCtx := &policy.RequestContext{Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

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
				Target: FailoverTarget{Model: "gpt-4o"},
				Fallbacks: []FailoverTarget{
					{Model: "same-provider-model"},
					{Model: "claude", Provider: "anthropic-upstream"},
				},
				AggregateCluster: "failover_agg_chat_0",
			}},
		},
		suspendedTargets: map[string]time.Time{suspensionKey("gpt-4o", ""): time.Now().Add(time.Hour)},
	}
	reqCtx := &policy.RequestContext{Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

	mods := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)

	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "anthropic-upstream", *mods.UpstreamName)
}

func TestOnRequestBody_OnlyEmptyProviderFallbacksRoutesToAggregate(t *testing.T) {
	p := &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{{
				Target:           FailoverTarget{Model: "gpt-4o"},
				Fallbacks:        []FailoverTarget{{Model: "same-provider-model"}},
				AggregateCluster: "failover_agg_chat_0",
			}},
		},
		suspendedTargets: map[string]time.Time{suspensionKey("gpt-4o", ""): time.Now().Add(time.Hour)},
	}
	reqCtx := &policy.RequestContext{Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

	mods := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)

	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "failover_agg_chat_0", *mods.UpstreamName)
}

func TestOnRequestBody_EmptyAggregateClusterIsNoop(t *testing.T) {
	p := &Policy{
		params:           ModelFailoverParams{Targets: []FailoverTargetEntry{{Target: FailoverTarget{Model: "gpt-4o"}}}},
		suspendedTargets: make(map[string]time.Time),
	}
	reqCtx := &policy.RequestContext{Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}

	mods := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)

	assert.Nil(t, mods.UpstreamName, "no injected aggregate cluster means normal routing, never a pointer to an empty string")
}

func TestSuspension_ConcurrentAccess(t *testing.T) {
	p := &Policy{
		params:           ModelFailoverParams{SuspendDuration: 60},
		suspendedTargets: make(map[string]time.Time),
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

func chainPolicy() *Policy {
	return &Policy{
		params: ModelFailoverParams{
			Targets: []FailoverTargetEntry{{
				Target:           FailoverTarget{Model: "gpt-4o"},
				Fallbacks:        []FailoverTarget{{Model: "claude-sonnet-4-5-20250929", Provider: "anthropic-upstream"}},
				AggregateCluster: "failover_agg_chat_0",
			}},
			SuspendDuration: 900,
		},
		suspendedTargets: make(map[string]time.Time),
	}
}

func attemptReqCtx(cluster, attempt string) *policy.RequestHeaderContext {
	h := map[string][]string{}
	if attempt != "" {
		h["x-envoy-attempt-count"] = []string{attempt}
	}
	return &policy.RequestHeaderContext{
		SharedContext: &policy.SharedContext{Metadata: map[string]interface{}{}},
		Headers:       policy.NewHeaders(h),
		Upstream:      &policy.UpstreamRequestContext{RouteCluster: cluster},
	}
}

func TestOnRequestHeaders_ResolvesTargetAttemptAndSeedsMetadata(t *testing.T) {
	reqCtx := attemptReqCtx("failover_agg_chat_0", "1")
	chainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "gpt-4o", reqCtx.SharedContext.Metadata[selectedModelMetadataKey])
	provider, present := reqCtx.SharedContext.Metadata[selectedProviderMetadataKey]
	assert.True(t, present)
	assert.Equal(t, "", provider)
}

func TestOnRequestHeaders_ResolvesFallbackAttempt(t *testing.T) {
	reqCtx := attemptReqCtx("failover_agg_chat_0", "2")
	chainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "claude-sonnet-4-5-20250929", reqCtx.SharedContext.Metadata[selectedModelMetadataKey])
	assert.Equal(t, "anthropic-upstream", reqCtx.SharedContext.Metadata[selectedProviderMetadataKey])
}

func TestOnRequestHeaders_MissingAttemptHeaderIsPrimary(t *testing.T) {
	reqCtx := attemptReqCtx("failover_agg_chat_0", "")
	chainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "gpt-4o", reqCtx.SharedContext.Metadata[selectedModelMetadataKey])
}

func TestOnRequestHeaders_PastChainEndIsNoop(t *testing.T) {
	reqCtx := attemptReqCtx("failover_agg_chat_0", "5")
	chainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Empty(t, reqCtx.SharedContext.Metadata)
}

func TestOnRequestHeaders_UnknownClusterIsNoop(t *testing.T) {
	reqCtx := attemptReqCtx("some-unrelated-cluster", "1")
	chainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Empty(t, reqCtx.SharedContext.Metadata)
}

func TestOnRequestHeaders_DownstreamInvocationIsNoop(t *testing.T) {
	reqCtx := attemptReqCtx("failover_agg_chat_0", "1")
	reqCtx.Downstream = &policy.DownstreamContext{}
	chainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Empty(t, reqCtx.SharedContext.Metadata)
}

func TestOnResponseHeaders_RecordsSuspensionOn5xx(t *testing.T) {
	p := chainPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{attemptIndexMetadataKey: 1}},
		ResponseStatus: 500,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0"},
	}

	action := p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.Nil(t, action, "primary attempt sets no resolved-provider header")
	assert.True(t, p.isSuspended("gpt-4o", ""))
}

func TestOnResponseHeaders_FallbackAttemptSetsResolvedProviderHeader(t *testing.T) {
	p := chainPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{attemptIndexMetadataKey: 2}},
		ResponseStatus: 200,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0"},
	}

	action := p.OnResponseHeaders(context.Background(), respCtx, nil)

	mods, ok := action.(policy.DownstreamResponseHeaderModifications)
	require.True(t, ok)
	assert.Equal(t, "anthropic-upstream", mods.HeadersToSet[ResolvedFailoverProviderHeader])
	assert.False(t, p.isSuspended("claude-sonnet-4-5-20250929", "anthropic-upstream"))
}

func TestOnResponseHeaders_FallsBackToRequestHeadersWhenNoMetadata(t *testing.T) {
	p := chainPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		RequestHeaders: policy.NewHeaders(map[string][]string{"x-envoy-attempt-count": {"2"}}),
		ResponseStatus: 503,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0"},
	}

	p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.True(t, p.isSuspended("claude-sonnet-4-5-20250929", "anthropic-upstream"))
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
		"targets":         []interface{}{map[string]interface{}{"target": map[string]interface{}{"model": "gpt-4o"}, "fallbacks": []interface{}{}}},
		"primaryProvider": "openai-primary",
	})
	require.NoError(t, err)
	assert.Equal(t, "openai-primary", params.PrimaryProvider)
}

func TestOnRequestHeaders_EmptyProviderTargetSeedsPrimaryProvider(t *testing.T) {
	reqCtx := attemptReqCtx("failover_agg_chat_0", "1")
	primaryResolvingPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "openai-primary", reqCtx.SharedContext.Metadata[selectedProviderMetadataKey])
}

func TestOnResponseHeaders_5xxSuspendsUnderResolvedKeyAndDownstreamSeesIt(t *testing.T) {
	p := primaryResolvingPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{attemptIndexMetadataKey: 1}},
		ResponseStatus: 503,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0"},
	}
	p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.True(t, p.isSuspended("gpt-4o", "openai-primary"))
	assert.False(t, p.isSuspended("gpt-4o", ""), "must not be recorded under the empty key")

	reqCtx := &policy.RequestContext{Body: &policy.Body{Content: []byte(`{"model":"gpt-4o"}`), Present: true}}
	mods, ok := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)
	require.True(t, ok)
	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "anthropic-upstream", *mods.UpstreamName, "downstream must see the primary as suspended")
}

// ─── Suspension gating ───────────────────────────────────────────────────────

func TestOnResponseHeaders_4xxDoesNotSuspend(t *testing.T) {
	p := chainPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{attemptIndexMetadataKey: 1}},
		ResponseStatus: 429,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0"},
	}

	p.OnResponseHeaders(context.Background(), respCtx, nil)

	assert.False(t, p.isSuspended("gpt-4o", ""), "a 4xx is the client's problem, not a failing target")
}

func TestOnResponseHeaders_ZeroSuspendDurationDoesNotSuspend(t *testing.T) {
	p := chainPolicy()
	p.params.SuspendDuration = 0
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{attemptIndexMetadataKey: 1}},
		ResponseStatus: 503,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_chat_0"},
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
	p.params.Targets[0].Target.BasePath = "/openai-provider"
	p.params.Targets[0].Fallbacks[0].BasePath = "/anthropic-provider"
	return p
}

func attemptReqCtxWithPath(cluster, attempt, path string) *policy.RequestHeaderContext {
	reqCtx := attemptReqCtx(cluster, attempt)
	reqCtx.Path = path
	return reqCtx
}

// TestOnRequestHeaders_Attempt2RewritesStalePathToMembersBasePath is the
// regression test for a bug caught by live e2e verification: the downstream
// phase rewrites :path exactly once, before Envoy's first dispatch, using the
// PRIMARY member's base path. Envoy replays that same :path verbatim on a
// retry — unlike Host, it is not recomputed per attempt via auto_host_rewrite.
// Every chain member being a loopback upstream on the same host:port, an
// uncorrected :path sends the retry straight back to the provider that just
// failed, regardless of which real cluster Envoy dialed.
func TestOnRequestHeaders_Attempt2RewritesStalePathToMembersBasePath(t *testing.T) {
	reqCtx := attemptReqCtxWithPath("failover_agg_chat_0", "2", "/openai-provider/chat/completions")

	action := pathChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	mods, ok := action.(policy.UpstreamRequestHeaderModifications)
	require.True(t, ok, "an escalated attempt on a different provider's base path must correct :path")
	require.NotNil(t, mods.Path)
	assert.Equal(t, "/anthropic-provider/chat/completions", *mods.Path)
}

func TestOnRequestHeaders_Attempt1NoPathMutationWhenAlreadyCorrect(t *testing.T) {
	reqCtx := attemptReqCtxWithPath("failover_agg_chat_0", "1", "/openai-provider/chat/completions")

	action := pathChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Nil(t, action, "the primary attempt's :path is already this member's own — nothing to correct")
}

func TestOnRequestHeaders_EmptyBasePathLeavesPathUntouched(t *testing.T) {
	p := pathChainPolicy()
	p.params.Targets[0].Fallbacks[0].BasePath = ""
	reqCtx := attemptReqCtxWithPath("failover_agg_chat_0", "2", "/openai-provider/chat/completions")

	action := p.OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Nil(t, action, "no injected basePath means nothing to correct TO — never guess a root-relative path")
	assert.Equal(t, "/openai-provider/chat/completions", reqCtx.Path)
}

func TestOnRequestHeaders_MissingOperationPathLeavesPathUntouched(t *testing.T) {
	p := pathChainPolicy()
	p.params.OperationPath = ""
	reqCtx := attemptReqCtxWithPath("failover_agg_chat_0", "2", "/openai-provider/chat/completions")

	assert.Nil(t, p.OnRequestHeaders(context.Background(), reqCtx, nil))
}

// TestOnRequestHeaders_SetsUpstreamBasePathForThisAttempt pins the other half
// of the aggregate-attempt gap: the kernel resolves a backend by
// xds.cluster_name, which for an aggregate attempt is the aggregate's own name
// and resolves nothing, so BasePath would otherwise stay empty for every later
// policy in the attempt.
func TestOnRequestHeaders_SetsUpstreamBasePathForThisAttempt(t *testing.T) {
	reqCtx := attemptReqCtxWithPath("failover_agg_chat_0", "2", "/openai-provider/chat/completions")

	pathChainPolicy().OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Equal(t, "/anthropic-provider", reqCtx.Upstream.BasePath)
}

func TestParseParams_CarriesInjectedBasePathAndOperationPath(t *testing.T) {
	params, err := parseParams(map[string]interface{}{
		"targets": []interface{}{
			map[string]interface{}{
				"target": map[string]interface{}{"model": "gpt-4o", "basePath": "/openai-provider"},
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
	assert.Equal(t, "/openai-provider", params.Targets[0].Target.BasePath)
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

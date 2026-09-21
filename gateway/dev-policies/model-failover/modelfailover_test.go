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

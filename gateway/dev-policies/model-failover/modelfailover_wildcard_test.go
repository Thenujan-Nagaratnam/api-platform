package modelfailover

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// wildcardChainPolicy declares the catch-all entry FIRST, ahead of an exact
// gpt-4o entry, so tests prove the exact match wins regardless of order. The
// catch-all fails over to the same model on a backup provider.
func wildcardChainPolicy() *Policy {
	return &Policy{
		params: ModelFailoverParams{
			PrimaryProvider: "openai-provider",
			Targets: []FailoverTargetEntry{
				{
					FailoverTarget: FailoverTarget{Model: wildcardModel, ClusterName: "leaf_any_primary"},
					Fallbacks: []FailoverTarget{{
						Model: wildcardModel, Provider: "openai-backup",
						ClusterName: "leaf_any_backup", SuffixCluster: "failover_suffix_any_1",
					}},
					AggregateCluster: "failover_agg_any",
				},
				{
					FailoverTarget: FailoverTarget{Model: "gpt-4o", ClusterName: "leaf_4o_primary"},
					Fallbacks: []FailoverTarget{{
						Model: "gpt-4o-mini", ClusterName: "leaf_4o_mini", SuffixCluster: "failover_suffix_4o_1",
					}},
					AggregateCluster: "failover_agg_4o",
				},
			},
		},
		susp: &suspensionState{suspended: make(map[string]time.Time)},
	}
}

func dispatchFor(t *testing.T, p *Policy, body string) policy.UpstreamRequestModifications {
	t.Helper()
	reqCtx := &policy.RequestContext{Downstream: &policy.DownstreamContext{}, Body: &policy.Body{Content: []byte(body), Present: true}}
	mods, ok := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)
	require.True(t, ok)
	return mods
}

func TestOnRequestBody_WildcardEntryCatchesAnUnlistedModel(t *testing.T) {
	mods := dispatchFor(t, wildcardChainPolicy(), `{"model":"gpt-4.1"}`)

	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "failover_agg_any", *mods.UpstreamName)
	assert.Equal(t, "1", mods.HeadersToSet["x-envoy-max-retries"])
}

func TestOnRequestBody_ExactEntryWinsOverAnEarlierWildcard(t *testing.T) {
	mods := dispatchFor(t, wildcardChainPolicy(), `{"model":"GPT-4o"}`)

	require.NotNil(t, mods.UpstreamName)
	assert.Equal(t, "failover_agg_4o", *mods.UpstreamName)
}

func TestOnRequestBody_RequestWithoutAModelIsNotCaughtByTheWildcard(t *testing.T) {
	mods := dispatchFor(t, wildcardChainPolicy(), `{"messages":[]}`)

	assert.Nil(t, mods.UpstreamName, "nothing to match on, so normal routing applies")
}

func TestOnRequestBody_SuspendedWildcardPrimaryBypassesToTheBackupForEveryModel(t *testing.T) {
	p := wildcardChainPolicy()
	// One circuit per wildcard member: the primary region's health is shared
	// by every model the catch-all serves.
	p.susp.suspended[suspensionKey(wildcardModel, "openai-provider")] = time.Now().Add(time.Hour)

	for _, model := range []string{"gpt-4.1", "o3-mini"} {
		mods := dispatchFor(t, p, `{"model":"`+model+`"}`)
		require.NotNil(t, mods.UpstreamName, model)
		assert.Equal(t, "failover_suffix_any_1", *mods.UpstreamName, model)
	}
	mods := dispatchFor(t, p, `{"model":"gpt-4o"}`)
	assert.Equal(t, "failover_agg_4o", *mods.UpstreamName, "an exact entry's primary has its own circuit")
}

func TestOnRequestBody_UpstreamAttemptOnAWildcardMemberKeepsTheClientsModel(t *testing.T) {
	p := wildcardChainPolicy()
	for _, attempt := range []struct{ n, leaf string }{{"1", "leaf_any_primary"}, {"2", "leaf_any_backup"}} {
		reqCtx := &policy.RequestContext{
			Headers:  attemptHeaders(attempt.n),
			Upstream: &policy.UpstreamRequestContext{RouteCluster: "failover_agg_any", MemberClusterName: attempt.leaf},
			Body:     &policy.Body{Content: []byte(`{"model":"gpt-4.1"}`), Present: true},
		}

		mods, ok := p.OnRequestBody(context.Background(), reqCtx, nil).(policy.UpstreamRequestModifications)

		require.True(t, ok)
		assert.Nil(t, mods.Body, "attempt %s must not write \"*\" into the client's model", attempt.n)
	}
}

func TestOnRequestHeaders_WildcardMemberSeedsProviderButNoModel(t *testing.T) {
	p := wildcardChainPolicy()
	p.params.RequestModel = RequestModelConfig{Location: requestModelLocationHeader, Identifier: "x-model"}
	reqCtx := memberReqCtx("failover_agg_any", "leaf_any_backup")
	reqCtx.Headers = policy.NewHeaders(map[string][]string{"x-model": {"gpt-4.1"}, "x-envoy-attempt-count": {"2"}})

	action := p.OnRequestHeaders(context.Background(), reqCtx, nil)

	assert.Nil(t, action, "no header rewrite: the client's model passes through")
	assert.Equal(t, "openai-backup", reqCtx.SharedContext.Metadata[selectedProviderMetadataKey])
	_, seeded := reqCtx.SharedContext.Metadata[selectedModelMetadataKey]
	assert.False(t, seeded, "\"*\" is not a model name to hand to later policies")
}

func TestOnResponseHeaders_WildcardMemberRecordsNoResolvedModel(t *testing.T) {
	p := wildcardChainPolicy()
	respCtx := &policy.ResponseHeaderContext{
		SharedContext:  &policy.SharedContext{Metadata: map[string]interface{}{}},
		RequestHeaders: attemptHeaders("2"),
		ResponseStatus: 200,
		Upstream:       &policy.UpstreamResponseContext{RouteCluster: "failover_agg_any", MemberClusterName: "leaf_any_backup"},
	}

	mods, ok := p.OnResponseHeaders(context.Background(), respCtx, nil).(policy.DownstreamResponseHeaderModifications)

	require.True(t, ok)
	assert.Equal(t, 2, mods.AnalyticsMetadata["model_failover_chain_position"])
	_, recorded := mods.AnalyticsMetadata["model_failover_resolved_model"]
	assert.False(t, recorded)
}

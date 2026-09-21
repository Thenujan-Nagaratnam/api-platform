package modelfailover

import (
	"context"
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

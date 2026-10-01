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

package xds

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

func TestMergeUpstreamRetryPolicy_EmptyInputs_ReturnsNil(t *testing.T) {
	got := mergeUpstreamRetryPolicy(nil, nil)
	assert.Nil(t, got)
}

func TestMergeUpstreamRetryPolicy_OneContributionAlone_TranslatesMaxAttemptsToNumRetries(t *testing.T) {
	contributions := []models.UpstreamRetryContribution{
		{PolicyName: "oauth2-generator", StatusCodes: []int{401}, MaxAttempts: 2},
	}

	got := mergeUpstreamRetryPolicy(contributions, nil)

	require.NotNil(t, got)
	assert.Contains(t, got.RetryOn, "retriable-status-codes")
	assert.Equal(t, []uint32{401}, got.RetriableStatusCodes)
	// MaxAttempts: 2 (total sends) translates to Envoy's NumRetries: 1.
	assert.Equal(t, uint32(1), got.NumRetries.GetValue())
}

func TestMergeUpstreamRetryPolicy_FailoverAlone_DefaultRetryOn_MatchesPreFeatureBehavior(t *testing.T) {
	// buildRouteFailoverFromPolicy never pairs a bare "5xx" RetryOn with a
	// non-empty RetriableStatusCodes (RetriableStatusCodes only takes
	// effect alongside "retriable-status-codes") — this fixture is the
	// unconfigured-statusCodes default case.
	failover := &models.RouteFailover{
		RetryOn:         []string{"5xx", "connect-failure", "reset", "reset-before-request"},
		PerTryTimeoutMs: 1500,
		Targets: []models.RouteFailoverTarget{
			{
				Model:     "gpt-4",
				Fallbacks: []models.RouteFailoverEntry{{}, {}}, // 2 fallbacks -> maxFallbackDepth 2
			},
		},
	}

	got := mergeUpstreamRetryPolicy(nil, failover)

	require.NotNil(t, got)
	// failover.RetryOn is reused verbatim (same order, same content) for a
	// failover-only route — this is the byte-for-byte regression guard.
	assert.Equal(t, "5xx,connect-failure,reset,reset-before-request", got.RetryOn)
	assert.Empty(t, got.RetriableStatusCodes)
	// maxFallbackDepth (2) + 1 primary attempt = 3 total attempts -> NumRetries 2.
	assert.Equal(t, uint32(2), got.NumRetries.GetValue())
	assert.Equal(t, 1500*time.Millisecond, got.PerTryTimeout.AsDuration())
}

func TestMergeUpstreamRetryPolicy_FailoverAlone_ExplicitStatusCodes_MatchesPreFeatureBehavior(t *testing.T) {
	// The explicit-statusCodes case: buildRouteFailoverFromPolicy sets
	// RetryOn to ["retriable-status-codes", ...] exactly when
	// RetriableStatusCodes is non-empty.
	failover := &models.RouteFailover{
		RetryOn:              []string{"retriable-status-codes", "connect-failure", "reset", "reset-before-request"},
		RetriableStatusCodes: []int{429},
		Targets: []models.RouteFailoverTarget{
			{Model: "gpt-4", Fallbacks: []models.RouteFailoverEntry{{}, {}}}, // 2 fallbacks -> 3 total attempts
		},
	}

	got := mergeUpstreamRetryPolicy(nil, failover)

	require.NotNil(t, got)
	assert.Equal(t, "retriable-status-codes,connect-failure,reset,reset-before-request", got.RetryOn)
	assert.Equal(t, []uint32{429}, got.RetriableStatusCodes)
	assert.Equal(t, uint32(2), got.NumRetries.GetValue())
}

func TestMergeUpstreamRetryPolicy_ContributionAndFailover_UnionNotOverride(t *testing.T) {
	failover := &models.RouteFailover{
		RetryOn:              []string{"retriable-status-codes", "connect-failure", "reset", "reset-before-request"},
		RetriableStatusCodes: []int{429},
		Targets: []models.RouteFailoverTarget{
			{Model: "gpt-4", Fallbacks: []models.RouteFailoverEntry{{}}}, // 1 fallback -> 2 total attempts
		},
	}
	contributions := []models.UpstreamRetryContribution{
		{PolicyName: "oauth2-generator", StatusCodes: []int{401}, MaxAttempts: 4},
	}

	got := mergeUpstreamRetryPolicy(contributions, failover)

	require.NotNil(t, got)
	assert.ElementsMatch(t, []uint32{429, 401}, got.RetriableStatusCodes)
	// max(failover's 2 total attempts, contribution's 4) - 1 = 3.
	assert.Equal(t, uint32(3), got.NumRetries.GetValue())
}

func TestMergeUpstreamRetryPolicy_TwoContributions_UnionOfCodesMaxOfAttemptsMinusOne(t *testing.T) {
	contributions := []models.UpstreamRetryContribution{
		{PolicyName: "policy-a", StatusCodes: []int{401}, MaxAttempts: 2},
		{PolicyName: "policy-b", StatusCodes: []int{429}, MaxAttempts: 3},
	}

	got := mergeUpstreamRetryPolicy(contributions, nil)

	require.NotNil(t, got)
	assert.ElementsMatch(t, []uint32{401, 429}, got.RetriableStatusCodes)
	assert.Equal(t, uint32(2), got.NumRetries.GetValue()) // max(2,3) - 1
}

func TestMergeUpstreamRetryPolicy_PerAttemptTimeout_TightestWins(t *testing.T) {
	contributions := []models.UpstreamRetryContribution{
		{PolicyName: "slow-policy", StatusCodes: []int{401}, MaxAttempts: 2, PerAttemptTimeoutMs: 2000},
		{PolicyName: "fast-policy", StatusCodes: []int{429}, MaxAttempts: 2, PerAttemptTimeoutMs: 500},
	}

	got := mergeUpstreamRetryPolicy(contributions, nil)

	require.NotNil(t, got)
	require.NotNil(t, got.PerTryTimeout)
	assert.Equal(t, 500*time.Millisecond, got.PerTryTimeout.AsDuration())
}

func TestMergeUpstreamRetryPolicy_PerAttemptTimeout_UnsetContributorDoesNotParticipate(t *testing.T) {
	contributions := []models.UpstreamRetryContribution{
		{PolicyName: "no-opinion", StatusCodes: []int{401}, MaxAttempts: 2}, // PerAttemptTimeoutMs left 0
		{PolicyName: "has-opinion", StatusCodes: []int{429}, MaxAttempts: 2, PerAttemptTimeoutMs: 800},
	}

	got := mergeUpstreamRetryPolicy(contributions, nil)

	require.NotNil(t, got)
	require.NotNil(t, got.PerTryTimeout)
	assert.Equal(t, 800*time.Millisecond, got.PerTryTimeout.AsDuration())
}

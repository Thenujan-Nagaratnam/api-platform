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

package attempts

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

func intp(n int) *int { return &n }

// defs: a guardrail (per request), a signer (every attempt), an oauth-like
// retrier (2 attempts, switched by a param), a failover-like retrier (longest
// list + 1, on both routes) and a header router it can't share with.
var testDefs = map[string]models.PolicyDefinition{
	"guard|v1.0.0": {Name: "guard", Version: "v1.0.0"},
	"sign|v1.0.0":  {Name: "sign", Version: "v1.0.0", RetryBehavior: &models.RetryBehavior{Runs: models.RunsOnEveryAttempt}},
	"oauth|v1.0.0": {Name: "oauth", Version: "v1.0.0", RetryBehavior: &models.RetryBehavior{
		Runs:     models.RunsOnEveryAttempt,
		CanRetry: &models.CanRetry{EnabledByParam: "retryOnUnauthorized", MaxAttempts: intp(2)},
	}},
	"failover|v0.1.0": {Name: "failover", Version: "v0.1.0", RetryBehavior: &models.RetryBehavior{
		Runs: models.RunsOnBoth,
		CanRetry: &models.CanRetry{
			MaxAttemptsFromLongestList: &models.LongestList{Path: "chains[].fallbacks", Plus: 1},
			PerAttemptTimeoutFromParam: "perAttemptTimeout",
			SeesConnectionFailures:     true,
		},
		SameOperation: []models.SameOperationRule{
			{Policy: "header-router", Allowed: models.AllowedNever},
			{Policy: "round-robin", Allowed: models.AllowedOnlyIf, OnlyIf: &models.OnlyIf{RunsBeforeThisPolicy: true, ParamNotSet: "models[].provider"}},
		},
	}},
	"keys|v1.0.0": {Name: "keys", Version: "v1.0.0", RetryBehavior: &models.RetryBehavior{
		Runs:     models.RunsOnEveryAttempt,
		CanRetry: &models.CanRetry{MaxAttemptsFromParam: "maxAttempts", PerAttemptTimeout: "5s"},
	}},
	"header-router|v0.1.0": {Name: "header-router", Version: "v0.1.0"},
	"round-robin|v1.0.0":   {Name: "round-robin", Version: "v1.0.0"},
}

func pol(name, version string, params map[string]interface{}) models.Policy {
	return models.Policy{Name: name, Version: version, Params: params}
}

func plan(t *testing.T, chain ...models.Policy) (Plan, error) {
	t.Helper()
	return PlanChain(chain, FromMap(testDefs), DefaultLimits, func(n string) bool { return strings.HasPrefix(n, "wso2_apip_sys_") })
}

func failoverParams(fallbacks ...int) map[string]interface{} {
	var chains []interface{}
	for _, n := range fallbacks {
		fb := make([]interface{}, n)
		for i := range fb {
			fb[i] = map[string]interface{}{"model": "m"}
		}
		chains = append(chains, map[string]interface{}{"primary": map[string]interface{}{"model": "p"}, "fallbacks": fb})
	}
	return map[string]interface{}{"chains": chains, "perAttemptTimeout": "10s"}
}

func TestNoRetryingPolicyMeansNoSplit(t *testing.T) {
	p, err := plan(t, pol("guard", "v1.0.0", nil), pol("sign", "v1.0.0", nil))
	require.NoError(t, err)
	assert.False(t, p.Split, "a per-attempt policy alone doesn't split")

	p, err = plan(t, pol("oauth", "v1.0.0", map[string]interface{}{"retryOnUnauthorized": false}))
	require.NoError(t, err)
	assert.False(t, p.Split, "a retrier switched off doesn't split")

	p, err = plan(t, pol("oauth", "v1.0.0", nil))
	require.NoError(t, err)
	assert.False(t, p.Split, "an absent switch param means off")
}

func TestPlacementAndSizing(t *testing.T) {
	chain := []models.Policy{
		pol("wso2_apip_sys_analytics", "v1.0.0", nil),
		pol("guard", "v1.0.0", nil),
		pol("oauth", "v1.0.0", map[string]interface{}{"retryOnUnauthorized": true}),
		pol("sign", "v1.0.0", nil),
	}
	p, err := plan(t, chain...)
	require.NoError(t, err)
	require.True(t, p.Split)
	assert.Equal(t, []int{0, 1}, p.FrontIndexes, "system and per-request policies stay in front")
	assert.Equal(t, []int{2, 3}, p.AttemptIndexes)
	assert.Equal(t, map[string]int{"oauth": 2}, p.Allowances)
	assert.Equal(t, 1, p.NumRetries)
	assert.Equal(t, 30*time.Second, p.PerTryTimeout, "gateway default")
	assert.Equal(t, 62*time.Second, p.RouteTimeout)
	assert.Equal(t, "retriable-headers", p.RetryOn)
}

func TestLongestListParamAndBothPlacement(t *testing.T) {
	p, err := plan(t,
		pol("failover", "v0.1.0", failoverParams(1, 3)),
		pol("keys", "v1.0.0", map[string]interface{}{"maxAttempts": float64(3)}),
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"failover": 4, "keys": 3}, p.Allowances, "longest chain (3) + 1; param 3")
	assert.Equal(t, 5, p.NumRetries, "(4-1) + (3-1)")
	assert.Equal(t, 5*time.Second, p.PerTryTimeout, "tightest declared timeout wins")
	assert.Equal(t, "retriable-headers,connect-failure,reset", p.RetryOn)
	assert.Equal(t, []int{0}, p.FrontIndexes)
	assert.Equal(t, []int{0, 1}, p.AttemptIndexes)
	assert.True(t, p.BothIndexes[0])
}

func TestRejections(t *testing.T) {
	cases := map[string][]models.Policy{
		"over the cap":          {pol("failover", "v0.1.0", failoverParams(9)), pol("keys", "v1.0.0", map[string]interface{}{"maxAttempts": float64(3)})},
		"switch not a boolean":  {pol("oauth", "v1.0.0", map[string]interface{}{"retryOnUnauthorized": "yes"})},
		"param not a number":    {pol("keys", "v1.0.0", map[string]interface{}{"maxAttempts": "three"})},
		"param missing":         {pol("keys", "v1.0.0", nil)},
		"no list at path":       {pol("failover", "v0.1.0", map[string]interface{}{})},
		"bad timeout param":     {pol("failover", "v0.1.0", map[string]interface{}{"chains": failoverParams(1)["chains"], "perAttemptTimeout": "soon"})},
		"never shares":          {pol("failover", "v0.1.0", failoverParams(1)), pol("header-router", "v0.1.0", nil)},
		"must run before":       {pol("failover", "v0.1.0", failoverParams(1)), pol("round-robin", "v1.0.0", nil)},
		"param must not be set": {pol("round-robin", "v1.0.0", map[string]interface{}{"models": []interface{}{map[string]interface{}{"model": "m", "provider": "x"}}}), pol("failover", "v0.1.0", failoverParams(1))},
	}
	for name, chain := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := plan(t, chain...)
			assert.Error(t, err)
		})
	}
	p, err := plan(t, pol("round-robin", "v1.0.0", map[string]interface{}{"models": []interface{}{map[string]interface{}{"model": "m"}}}), pol("failover", "v0.1.0", failoverParams(1)))
	require.NoError(t, err, "round-robin before failover without providers is allowed")
	assert.True(t, p.Split)
}

func TestValidateRetryBehavior(t *testing.T) {
	bad := map[string]*models.RetryBehavior{
		"unknown runs":        {Runs: "sometimes"},
		"retry per request":   {CanRetry: &models.CanRetry{MaxAttempts: intp(2)}},
		"no max source":       {Runs: models.RunsOnEveryAttempt, CanRetry: &models.CanRetry{}},
		"two max sources":     {Runs: models.RunsOnEveryAttempt, CanRetry: &models.CanRetry{MaxAttempts: intp(2), MaxAttemptsFromParam: "n"}},
		"max below 1":         {Runs: models.RunsOnEveryAttempt, CanRetry: &models.CanRetry{MaxAttempts: intp(0)}},
		"two timeouts":        {Runs: models.RunsOnEveryAttempt, CanRetry: &models.CanRetry{MaxAttempts: intp(2), PerAttemptTimeout: "1s", PerAttemptTimeoutFromParam: "t"}},
		"bad fixed timeout":   {Runs: models.RunsOnEveryAttempt, CanRetry: &models.CanRetry{MaxAttempts: intp(2), PerAttemptTimeout: "later"}},
		"unknown allowed":     {SameOperation: []models.SameOperationRule{{Policy: "x", Allowed: "sometimes"}}},
		"onlyIf without cond": {SameOperation: []models.SameOperationRule{{Policy: "x", Allowed: models.AllowedOnlyIf, OnlyIf: &models.OnlyIf{}}}},
		"never with onlyIf":   {SameOperation: []models.SameOperationRule{{Policy: "x", Allowed: models.AllowedNever, OnlyIf: &models.OnlyIf{RunsBeforeThisPolicy: true}}}},
	}
	for name, b := range bad {
		assert.Error(t, b.Validate(), name)
	}
	for _, d := range testDefs {
		assert.NoError(t, d.RetryBehavior.Validate(), d.Name)
	}
	var none *models.RetryBehavior
	assert.NoError(t, none.Validate())
	assert.Equal(t, models.RunsOnClientRequest, none.EffectiveRuns())
}

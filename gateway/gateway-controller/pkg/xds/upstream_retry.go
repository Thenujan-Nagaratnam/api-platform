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
	"strings"
	"time"

	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	durationpb "google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// mergeUpstreamRetryPolicy folds every policy's resolved retry contribution
// on a route, plus the route's model-failover chain (if any), into a single
// Envoy RetryPolicy. See
// specs/001-upstream-retry-contribution/data-model.md "Merged Route Retry
// Policy" for the exact rules this implements; the short version:
//
//   - RetryOn / RetriableStatusCodes: union — broadest wins.
//   - NumRetries: max(every contributor's MaxAttempts, failover's own
//     derived total attempts) − 1. MaxAttempts is total sends (first
//     attempt included); Envoy's NumRetries excludes the first send. The
//     −1 translation happens exactly once, here.
//   - PerTryTimeout: the *tightest* (minimum) resolved value wins, not the
//     loosest — see research.md Decision 7 for why.
//
// Returns nil if there is nothing to retry on at all (no contributions, no
// failover chain) — callers must not set routeAction.Route.RetryPolicy in
// that case, exactly as before this function existed.
func mergeUpstreamRetryPolicy(contributions []models.UpstreamRetryContribution, failover *models.RouteFailover) *route.RetryPolicy {
	if len(contributions) == 0 && failover == nil {
		return nil
	}

	// retryOn is built as an ordered, deduplicated list rather than through
	// map iteration so a failover-only route (no contributions) reproduces
	// its pre-existing RetryOn byte-for-byte: failover.RetryOn is already
	// correct and complete (computed once in pkg/transform, see
	// buildRouteFailoverFromPolicy), so it seeds the list verbatim and
	// nothing is re-derived for it.
	var retryOn []string
	seenCondition := make(map[string]bool)
	appendCondition := func(cond string) {
		if cond == "" || seenCondition[cond] {
			return
		}
		seenCondition[cond] = true
		retryOn = append(retryOn, cond)
	}

	var retriableStatusCodes []uint32
	seenCode := make(map[int]bool)
	appendCode := func(code int) {
		if seenCode[code] {
			return
		}
		seenCode[code] = true
		retriableStatusCodes = append(retriableStatusCodes, uint32(code))
	}

	maxTotalAttempts := 0
	tightestPerAttemptTimeoutMs := 0 // 0 means "no contributor has an opinion yet"
	alsoRetryOnConnectionFailure := false

	if failover != nil {
		// RetryOn is expected non-empty by the time it reaches here —
		// buildRouteFailoverFromPolicy (pkg/transform/model_failover_policy.go)
		// defaults it to ["5xx", "connect-failure", "reset",
		// "reset-before-request"] — but default defensively here too (this
		// exact fallback predates this function; preserved verbatim), so a
		// RouteFailover built any other way never produces an empty
		// retry_on, which Envoy treats as "never retry", silently defeating
		// the whole feature.
		failoverRetryOn := failover.RetryOn
		if len(failoverRetryOn) == 0 {
			failoverRetryOn = []string{"5xx", "connect-failure", "reset", "reset-before-request"}
		}
		for _, cond := range failoverRetryOn {
			appendCondition(strings.TrimSpace(cond))
		}
		for _, code := range failover.RetriableStatusCodes {
			appendCode(code)
		}
		// failover's own attempt budget is already expressed in Envoy's
		// "retries excluding the first send" convention (one retry per
		// fallback position) — add 1 to compare it on equal footing with
		// contributors' MaxAttempts (total sends, first included) before
		// taking the overall max. This mirrors the maxFallbackDepth
		// computation translator.go used before this function existed.
		maxFallbackDepth := 0
		for _, target := range failover.Targets {
			if len(target.Fallbacks) > maxFallbackDepth {
				maxFallbackDepth = len(target.Fallbacks)
			}
		}
		if failoverTotalAttempts := maxFallbackDepth + 1; failoverTotalAttempts > maxTotalAttempts {
			maxTotalAttempts = failoverTotalAttempts
		}
		if failover.PerTryTimeoutMs > 0 {
			tightestPerAttemptTimeoutMs = failover.PerTryTimeoutMs
		}
	}

	for _, c := range contributions {
		for _, code := range c.StatusCodes {
			appendCode(code)
		}
		if c.MaxAttempts > maxTotalAttempts {
			maxTotalAttempts = c.MaxAttempts
		}
		if c.AlsoRetryOnConnectionFailure {
			alsoRetryOnConnectionFailure = true
		}
		if c.PerAttemptTimeoutMs > 0 {
			if tightestPerAttemptTimeoutMs == 0 || c.PerAttemptTimeoutMs < tightestPerAttemptTimeoutMs {
				tightestPerAttemptTimeoutMs = c.PerAttemptTimeoutMs
			}
		}
	}

	if len(retriableStatusCodes) > 0 {
		appendCondition("retriable-status-codes")
	}
	if alsoRetryOnConnectionFailure {
		appendCondition("connect-failure")
		appendCondition("reset")
		appendCondition("reset-before-request")
	}
	if len(retryOn) == 0 {
		// Defensive fallback only: policy-load-time validation requires
		// every Retry block to declare status codes, and a failover chain
		// always seeds at least one condition above, so this should be
		// unreachable in practice.
		appendCondition("5xx")
	}

	if maxTotalAttempts == 0 {
		// Same defensive reasoning as the RetryOn fallback above — a
		// non-empty RetryOn paired with NumRetries: 0 is silently "never
		// retry" in Envoy, which would defeat the contribution entirely.
		maxTotalAttempts = 2
	}

	retryPolicy := &route.RetryPolicy{
		RetryOn:    strings.Join(retryOn, ","),
		NumRetries: wrapperspb.UInt32(uint32(maxTotalAttempts - 1)),
	}
	if len(retriableStatusCodes) > 0 {
		retryPolicy.RetriableStatusCodes = retriableStatusCodes
	}
	if tightestPerAttemptTimeoutMs > 0 {
		retryPolicy.PerTryTimeout = durationpb.New(time.Duration(tightestPerAttemptTimeoutMs) * time.Millisecond)
	}
	return retryPolicy
}

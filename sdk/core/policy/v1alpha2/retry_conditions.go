/*
 *  Copyright (c) 2026, WSO2 LLC. (http://www.wso2.com) All Rights Reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 */

package policyv1alpha2

import "time"

// RetryConditions is the shared vocabulary a contributor — an operator's
// resilience.retry or a retry-source policy — uses to describe what it
// wants from Envoy's RouteAction.RetryPolicy. Multiple contributors on one
// route compose into a single RetryConditions via field-by-field merge
// rules (gateway-controller's MergeRetryConditions) — see
// docs/superpowers/specs/2026-08-17-generalized-retry-conditions-design.md.
type RetryConditions struct {
	// On lists Envoy RetryOn conditions this contributor wants active:
	// "5xx", "gateway-error", "reset", "connect-failure", "refused-stream",
	// "envoy-ratelimited", "retriable-4xx", "retriable-status-codes",
	// "retriable-headers". Composes by union across contributors.
	On []string

	// StatusCodes are retriable response status codes, used when On
	// includes "retriable-status-codes" (implied automatically if
	// StatusCodes is non-empty and On doesn't already list it). Composes
	// by union.
	StatusCodes []int

	// Headers are retriable response header matchers, used when On
	// includes "retriable-headers" (implied automatically if Headers is
	// non-empty). Composes by union.
	Headers []RetriableHeader

	// NumRetries is an exact retry-count request. At most one DISTINCT
	// value may be set across all contributors on a route — a second
	// contributor setting a different explicit value is a conflict,
	// detected at translation time — the affected route is left with no
	// retry policy rather than silently picking a winner (see
	// MergeRetryConditions). Identical values from multiple contributors
	// are fine.
	NumRetries *int

	// MinAttempts is "I need at least N total attempts to get value from
	// retrying" — distinct from NumRetries. Composes as a floor: only ever
	// raised (max across contributors), never lowered.
	MinAttempts *int

	// PerTryTimeout bounds a single attempt. Composes as a ceiling: only
	// ever tightened (min across contributors), never widened.
	PerTryTimeout *time.Duration

	// BackOff configures retry pacing. At most one contributor on a route
	// may set this — a second contributor also setting it is a conflict,
	// detected at translation time, even with identical values — the
	// affected route is left with no retry policy rather than silently
	// picking a winner (see MergeRetryConditions).
	BackOff *RetryBackOff

	// AvoidPreviousHosts maps to Envoy's "previous_hosts" RetryHostPredicate.
	// Composes by OR: any contributor wanting the safer behavior turns it
	// on for every contributor on the route.
	AvoidPreviousHosts bool
}

// RetriableHeader is one retriable-response-header matcher. Exact-match
// only for now — extend to regex/presence-only matching if a real consumer
// needs it; not speculatively built ahead of a need.
type RetriableHeader struct {
	Name  string
	Exact string
}

// RetryBackOff configures Envoy's retry pacing between attempts.
type RetryBackOff struct {
	BaseInterval time.Duration
	MaxInterval  *time.Duration
}

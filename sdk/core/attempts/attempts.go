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

// Package attempts lets a policy take part in the gateway's per-attempt
// retry hop without the gateway knowing anything about the policy.
//
// When at least one policy on an operation declares `retryBehavior.canRetry`
// in its definition, the gateway splits the operation in two: a front route
// that runs once per client request and holds Envoy's retry policy, and a
// per-attempt route that Envoy enters once per attempt. Policies declared
// `runs: onEveryAttempt` run on the per-attempt route, so they run again for
// every attempt.
//
// A gateway system policy starts a scope for each client request on the front
// route and admits each attempt on the per-attempt route. Policies then use
// this package to:
//
//   - read the current attempt (Current): its number, which policy asked for
//     it and why, and whether the previous attempt timed out;
//   - keep their own state across the attempts of one request (Get/Set);
//   - ask for another attempt from the response phase (Retry);
//   - read a transport failure the gateway reported (TransportFailure).
//
// The store is process-wide: every policy compiled into the policy engine
// shares it, because each attempt is a separate ext_proc stream with its own
// SharedContext.
package attempts

import (
	"strings"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// Internal headers. None of them reach a client or a backend: the gateway
// strips them on the way in and on the way out.
const (
	HeaderPrefix = "x-wso2-attempt-"
	// HeaderScope carries the request's scope id from the front route to
	// every attempt.
	HeaderScope = "x-wso2-attempt-scope"
	// HeaderRetry on an attempt's response makes Envoy send another attempt.
	HeaderRetry = "x-wso2-attempt-retry"
	// HeaderHop carries the per-boot hop secret so the per-attempt route's
	// local-reply mapper labels transport failures for this request only.
	HeaderHop = "x-wso2-attempt-hop"
	// HeaderUpstreamFailure holds Envoy's response flags on a local reply for
	// a transport failure (connect failure, reset, timeout).
	HeaderUpstreamFailure = "x-wso2-upstream-failure"
)

// Transport failure reasons reported by TransportFailure.
const (
	ConnectFailure = "connect_failure"
	Reset          = "reset"
	Timeout        = "timeout"
)

// metadataKey is where the system policy leaves the current attempt in the
// per-stream SharedContext.
const metadataKey = "wso2.attempts.current"

// Current returns the attempt this stream belongs to, or nil when the
// operation is not split (no policy on it can retry). It is available in
// every phase of a per-attempt route, for any policy after the gateway's
// system policy.
func Current(shared *policy.SharedContext) *Attempt {
	if shared == nil || shared.Metadata == nil {
		return nil
	}
	a, _ := shared.Metadata[metadataKey].(*Attempt)
	return a
}

// Bind records a in shared. Only the gateway's system policy calls it.
func Bind(shared *policy.SharedContext, a *Attempt) {
	if shared == nil || a == nil {
		return
	}
	if shared.Metadata == nil {
		shared.Metadata = map[string]interface{}{}
	}
	shared.Metadata[metadataKey] = a
}

// Retry asks for another attempt, on behalf of policyName, from a response
// phase. It returns modifications that tag the response for Envoy to retry.
//
// If policyName has used up the attempts it declared (canRetry.maxAttempts),
// no retry is requested: Retry returns giveUp when it is non-nil, and a plain
// pass otherwise. giveUp is also kept as the answer the client gets if the
// gateway runs out of attempts for another reason before this retry happens.
func Retry(shared *policy.SharedContext, policyName, reason string, giveUp *policy.ImmediateResponse) policy.ResponseHeaderAction {
	a := Current(shared)
	if a == nil || !a.reg.requestRetry(a.scopeID, policyName, reason, giveUp) {
		if giveUp != nil {
			return *giveUp
		}
		return policy.DownstreamResponseHeaderModifications{}
	}
	if reason == "" {
		reason = policyName
	}
	return policy.DownstreamResponseHeaderModifications{
		HeadersToSet: map[string]string{HeaderRetry: reason},
	}
}

// TransportFailure returns ConnectFailure, Reset or Timeout when the attempt
// failed at the transport level and the gateway answered locally, or "" for a
// response from the backend. Only policies that declared
// canRetry.seesConnectionFailures get these local replies retried.
func TransportFailure(respHeaders *policy.Headers) string {
	if respHeaders == nil {
		return ""
	}
	v := respHeaders.Get(HeaderUpstreamFailure)
	if len(v) == 0 {
		return ""
	}
	reason := ""
	for _, f := range strings.Split(v[0], ",") {
		switch strings.TrimSpace(f) {
		case "UT":
			return Timeout
		case "UC", "DC", "UR":
			reason = Reset
		case "UF", "URX", "UH", "UO", "LR":
			if reason == "" {
				reason = ConnectFailure
			}
		}
	}
	return reason
}

// IsInternalHeader reports whether name is one of the gateway's attempt
// headers, which a client may never set.
func IsInternalHeader(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasPrefix(lower, HeaderPrefix) || lower == HeaderUpstreamFailure
}

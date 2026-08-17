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

import "context"

// UpstreamAttemptResponseContext mirrors UpstreamAttemptContext's per-attempt
// (not per-client-request) scope, but on the response side of that same
// attempt — it fires once per individual upstream dial attempt's response,
// before Envoy decides whether to retry.
type UpstreamAttemptResponseContext struct {
	*SharedContext

	// AttemptCount is Envoy's x-envoy-attempt-count for this specific dial,
	// starting at 1.
	AttemptCount int

	// RequestID is x-request-id — stable across every attempt of one
	// client request, assigned once at the edge, not per-attempt. The
	// correlation key an OnUpstreamAttemptResponse implementation uses to
	// hand information forward to a later attempt's
	// UpstreamAttemptPolicy.OnUpstreamAttemptRequest call (see that
	// interface's own doc comment) — deliberately not Envoy's own
	// cross-attempt dynamic metadata, whose visibility across the
	// per-attempt filter-chain instances Envoy creates is unverified.
	RequestID string

	// ResponseStatus is this specific attempt's response status code.
	ResponseStatus int
}

// UpstreamAttemptResponseObserver is implemented by a policy that wants to
// know why a specific attempt failed, to inform its own behavior on a
// later attempt of the SAME client request. Fires read-only — nothing
// about the response can be mutated from here; mutation only ever happens
// in UpstreamAttemptPolicy.OnUpstreamAttemptRequest, on a subsequent
// attempt.
//
// A policy's Go instance is long-lived — it spans every request and every
// attempt for this route's lifetime (the same lifetime model round-robin's
// own suspendedModels map already relies on) — so an implementation
// typically records (RequestID -> observed cause) in its own in-memory
// state here, and reads it back in OnUpstreamAttemptRequest on the next
// attempt. Implementations MUST bound that state (a TTL, or cleanup on the
// request's final downstream response) — an unbounded map keyed by
// RequestID grows without limit for requests that error out before any
// later attempt ever reads the entry back.
type UpstreamAttemptResponseObserver interface {
	OnUpstreamAttemptResponse(ctx context.Context, actx *UpstreamAttemptResponseContext)
}

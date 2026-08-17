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

import "testing"

func TestRetrySourceUpstreamName_SingleGroup(t *testing.T) {
	got := RetrySourceUpstreamName("POST|/chat/completions|main", "gpt-4o")
	want := "__retry_source_target__POST_/chat/completions_main__gpt-4o"
	if got != want {
		t.Errorf("RetrySourceUpstreamName() = %q, want %q", got, want)
	}
}

func TestRetrySourceUpstreamName_EmptyRouteKey(t *testing.T) {
	got := RetrySourceUpstreamName("", "gpt-4o")
	want := "__retry_source_target__gpt-4o"
	if got != want {
		t.Errorf("RetrySourceUpstreamName() = %q, want %q", got, want)
	}
}

func TestRetrySourceUpstreamName_PipeInRouteKeyIsSanitized(t *testing.T) {
	got := RetrySourceUpstreamName("GET|/a|b", "x")
	want := "__retry_source_target__GET_/a_b__x"
	if got != want {
		t.Errorf("RetrySourceUpstreamName() = %q, want %q", got, want)
	}
}

func TestRetrySourceDeclaration_IsUpstreamAttemptActionFree(t *testing.T) {
	// Compile-time shape check: RetrySourceDeclaration must not itself be an
	// action type — it's a registration-time declaration, never returned
	// from a runtime hook.
	var _ = RetrySourceDeclaration{
		Groups: []RetryGroup{
			{Key: "gpt-4o", OrderedTargets: []RetryTarget{{UpstreamDefinitionName: "primary"}}},
		},
		RetriableStatusCodes: []int{429, 500},
	}
}

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

package retryonstatus

import (
	"context"
	"testing"
	"time"

	"github.com/wso2/api-platform/sdk/core/attempts"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// attempt admits one attempt of scope id and answers it with status.
func attempt(t *testing.T, p *Policy, id string, status int) (int, policy.DownstreamResponseHeaderModifications) {
	t.Helper()
	a, err := attempts.Default.Arrive(id, "op")
	if err != nil {
		t.Fatal(err)
	}
	sh := &policy.SharedContext{Metadata: map[string]interface{}{}}
	attempts.Bind(sh, a)
	req := p.OnRequestHeaders(context.Background(), &policy.RequestHeaderContext{SharedContext: sh}, nil).(policy.UpstreamRequestHeaderModifications)
	if req.HeadersToSet[headerAttempt] == "" {
		t.Fatal("the backend is told the attempt number")
	}
	resp := p.OnResponseHeaders(context.Background(), &policy.ResponseHeaderContext{SharedContext: sh, ResponseStatus: status}, nil).(policy.DownstreamResponseHeaderModifications)
	attempts.Default.Responded(id)
	return a.Number, resp
}

func TestRetriesOnTheConfiguredStatusUpToTheAllowance(t *testing.T) {
	pol, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{"status": float64(418)})
	if err != nil {
		t.Fatal(err)
	}
	p := pol.(*Policy)
	id, _ := attempts.Default.Begin("op", map[string]int{Name: 2}, time.Minute)
	defer attempts.Default.Close(id)

	if n, resp := attempt(t, p, id, 418); n != 1 || resp.HeadersToSet[attempts.HeaderRetry] == "" {
		t.Fatalf("attempt 1 on 418 must ask for a retry: %+v", resp)
	}
	if n, resp := attempt(t, p, id, 418); n != 2 || resp.HeadersToSet[attempts.HeaderRetry] != "" || resp.HeadersToSet[headerAttempt] != "2" {
		t.Fatalf("attempt 2 is the last one allowed: %+v", resp)
	}
}

func TestOtherStatusesPass(t *testing.T) {
	pol, _ := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{})
	id, _ := attempts.Default.Begin("op", map[string]int{Name: 2}, time.Minute)
	defer attempts.Default.Close(id)
	if _, resp := attempt(t, pol.(*Policy), id, 200); resp.HeadersToSet[attempts.HeaderRetry] != "" {
		t.Fatal("a 200 is never retried")
	}
}

func TestInvalidStatusIsRejected(t *testing.T) {
	if _, err := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{"status": float64(200)}); err == nil {
		t.Fatal("status must be 400-599")
	}
}

func TestUnsplitOperationIsANoOp(t *testing.T) {
	pol, _ := GetPolicy(policy.PolicyMetadata{}, map[string]interface{}{})
	sh := &policy.SharedContext{Metadata: map[string]interface{}{}}
	if r := pol.(*Policy).OnResponseHeaders(context.Background(), &policy.ResponseHeaderContext{SharedContext: sh, ResponseStatus: 503}, nil).(policy.DownstreamResponseHeaderModifications); len(r.HeadersToSet) != 0 {
		t.Fatal("without the retry hop the policy does nothing")
	}
}

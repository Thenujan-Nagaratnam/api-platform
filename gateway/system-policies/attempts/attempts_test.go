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

package attemptspolicy

import (
	"context"
	"testing"

	"github.com/wso2/api-platform/sdk/core/attempts"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

func pair(reg *attempts.Registry, allowances map[string]any) (front, attempt *Policy) {
	front = newPolicy(map[string]any{ParamRole: "front", ParamScope: "op-1", ParamTTL: "10s", ParamAllowances: allowances}, reg)
	attempt = newPolicy(map[string]any{ParamRole: "attempt", ParamScope: "op-1", ParamHopSecret: "s3cret"}, reg)
	return front, attempt
}

func newShared() *policy.SharedContext {
	return &policy.SharedContext{Metadata: map[string]any{}}
}

func TestFrontStartsAScopeAndStripsClientHeaders(t *testing.T) {
	reg := attempts.NewRegistry(nil, 0)
	front, _ := pair(reg, map[string]any{"p": float64(2)})
	sh := newShared()
	act := front.OnRequestHeaders(context.Background(), &policy.RequestHeaderContext{
		SharedContext: sh,
		Headers:       policy.NewHeaders(map[string][]string{attempts.HeaderRetry: {"x"}, attempts.HeaderScope: {"forged"}, "authorization": {"k"}}),
	}, nil)
	m := act.(policy.UpstreamRequestHeaderModifications)
	id := m.HeadersToSet[attempts.HeaderScope]
	if len(id) != 32 || id == "forged" {
		t.Fatalf("front must mint the scope id, got %q", id)
	}
	if len(m.HeadersToRemove) != 1 || m.HeadersToRemove[0] != attempts.HeaderRetry {
		t.Fatalf("only client attempt headers are removed: %v", m.HeadersToRemove)
	}
	if reg.Len() != 1 {
		t.Fatal("a scope is open")
	}
}

// run drives one client request through front → attempts → front, with each
// attempt answering status and the policy under test asking for a retry when
// retryOn is true.
func TestRetriedRequestFlow(t *testing.T) {
	reg := attempts.NewRegistry(nil, 0)
	front, gate := pair(reg, map[string]any{"p": float64(2)})
	fsh := newShared()
	id := front.OnRequestHeaders(context.Background(), &policy.RequestHeaderContext{SharedContext: fsh, Headers: policy.NewHeaders(nil)}, nil).(policy.UpstreamRequestHeaderModifications).HeadersToSet[attempts.HeaderScope]

	attempt := func(retry bool) (*attempts.Attempt, policy.ResponseHeaderAction) {
		sh := newShared()
		m := gate.OnRequestHeaders(context.Background(), &policy.RequestHeaderContext{SharedContext: sh, Headers: policy.NewHeaders(map[string][]string{attempts.HeaderScope: {id}})}, nil).(policy.UpstreamRequestHeaderModifications)
		if m.HeadersToSet[attempts.HeaderHop] != "s3cret" || m.HeadersToRemove[0] != attempts.HeaderScope {
			t.Fatalf("gate must send the hop secret and drop the scope header: %+v", m)
		}
		a := attempts.Current(sh)
		var act policy.ResponseHeaderAction = policy.DownstreamResponseHeaderModifications{}
		if retry {
			act = attempts.Retry(sh, "p", "status", nil)
		}
		gate.OnResponseHeaders(context.Background(), &policy.ResponseHeaderContext{SharedContext: sh, ResponseHeaders: policy.NewHeaders(nil)}, nil)
		return a, act
	}
	a1, act := attempt(true)
	if a1.Number != 1 || act.(policy.DownstreamResponseHeaderModifications).HeadersToSet[attempts.HeaderRetry] == "" {
		t.Fatal("first attempt tagged for retry")
	}
	a2, _ := attempt(false)
	if a2.Number != 2 || a2.RequestedBy != "p" {
		t.Fatalf("second attempt: %+v", a2)
	}
	final := front.OnResponseHeaders(context.Background(), &policy.ResponseHeaderContext{SharedContext: fsh, ResponseHeaders: policy.NewHeaders(nil)}, nil).(policy.DownstreamResponseHeaderModifications)
	if len(final.HeadersToRemove) != 3 || reg.Len() != 0 {
		t.Fatalf("front strips tags and closes the scope: %+v, %d open", final, reg.Len())
	}
}

func TestGiveUpAnswerWhenStillTagged(t *testing.T) {
	reg := attempts.NewRegistry(nil, 0)
	front, gate := pair(reg, map[string]any{"p": float64(3)})
	fsh := newShared()
	id := front.OnRequestHeaders(context.Background(), &policy.RequestHeaderContext{SharedContext: fsh, Headers: policy.NewHeaders(nil)}, nil).(policy.UpstreamRequestHeaderModifications).HeadersToSet[attempts.HeaderScope]
	sh := newShared()
	gate.OnRequestHeaders(context.Background(), &policy.RequestHeaderContext{SharedContext: sh, Headers: policy.NewHeaders(map[string][]string{attempts.HeaderScope: {id}})}, nil)
	attempts.Retry(sh, "p", "x", &policy.ImmediateResponse{StatusCode: 502})
	// Envoy ran out of retries (for example per-attempt timeouts used them up):
	// the final response still carries the tag.
	act := front.OnResponseHeaders(context.Background(), &policy.ResponseHeaderContext{SharedContext: fsh, ResponseHeaders: policy.NewHeaders(map[string][]string{attempts.HeaderRetry: {"x"}})}, nil)
	if ir, ok := act.(policy.ImmediateResponse); !ok || ir.StatusCode != 502 {
		t.Fatalf("the policy's give-up answer must reach the client, got %#v", act)
	}
}

func TestForgedScopeIsRejectedWithoutRetry(t *testing.T) {
	reg := attempts.NewRegistry(nil, 0)
	_, gate := pair(reg, nil)
	act := gate.OnRequestHeaders(context.Background(), &policy.RequestHeaderContext{SharedContext: newShared(), Headers: policy.NewHeaders(map[string][]string{attempts.HeaderScope: {"forged"}})}, nil)
	ir, ok := act.(policy.ImmediateResponse)
	if !ok || ir.StatusCode != 500 || ir.Headers[attempts.HeaderRetry] != "" {
		t.Fatalf("expected an untagged 500, got %#v", act)
	}
}

func TestInvalidParamsFailClosed(t *testing.T) {
	for name, params := range map[string]map[string]any{
		"no role":       {ParamScope: "op"},
		"no scope":      {ParamRole: "attempt", ParamHopSecret: "s"},
		"bad ttl":       {ParamRole: "front", ParamScope: "op", ParamTTL: "soon"},
		"bad allowance": {ParamRole: "front", ParamScope: "op", ParamTTL: "1s", ParamAllowances: map[string]any{"p": float64(0)}},
		"no hop secret": {ParamRole: "attempt", ParamScope: "op"},
	} {
		p := newPolicy(params, attempts.NewRegistry(nil, 0))
		act := p.OnRequestHeaders(context.Background(), &policy.RequestHeaderContext{SharedContext: newShared(), Headers: policy.NewHeaders(nil)}, nil)
		if ir, ok := act.(policy.ImmediateResponse); !ok || ir.StatusCode != 500 {
			t.Errorf("%s: expected 500, got %#v", name, act)
		}
	}
}

func TestFullStoreAnswers503(t *testing.T) {
	reg := attempts.NewRegistry(nil, 1)
	front, _ := pair(reg, nil)
	call := func() policy.RequestHeaderAction {
		return front.OnRequestHeaders(context.Background(), &policy.RequestHeaderContext{SharedContext: newShared(), Headers: policy.NewHeaders(nil)}, nil)
	}
	call()
	if ir, ok := call().(policy.ImmediateResponse); !ok || ir.StatusCode != 503 {
		t.Fatal("a full store must answer 503")
	}
}

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
	"errors"
	"sync"
	"testing"
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestRegistry() (*Registry, *clock) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	return NewRegistry(c.now, 3), c
}

// shared binds an attempt to a fresh SharedContext, as the gateway's system
// policy does for each per-attempt stream.
func shared(a *Attempt) *policy.SharedContext {
	s := &policy.SharedContext{Metadata: map[string]interface{}{}}
	Bind(s, a)
	return s
}

func TestAttemptsAreNumberedAndCarryTheRequester(t *testing.T) {
	r, _ := newTestRegistry()
	id, err := r.Begin("op", map[string]int{"oauth2": 2}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := r.Arrive(id, "op")
	if err != nil || a1.Number != 1 || a1.RequestedBy != "" || a1.PreviousTimedOut {
		t.Fatalf("first attempt: %+v %v", a1, err)
	}
	act := Retry(shared(a1), "oauth2", "stale_token", nil)
	if m, ok := act.(policy.DownstreamResponseHeaderModifications); !ok || m.HeadersToSet[HeaderRetry] != "stale_token" {
		t.Fatalf("retry must tag the response, got %#v", act)
	}
	r.Responded(id)
	a2, _ := r.Arrive(id, "op")
	if a2.Number != 2 || a2.RequestedBy != "oauth2" || a2.Reason != "stale_token" || a2.PreviousTimedOut {
		t.Fatalf("second attempt: %+v", a2)
	}
}

func TestRetryIsBoundedByTheDeclaredAllowance(t *testing.T) {
	r, _ := newTestRegistry()
	id, _ := r.Begin("op", map[string]int{"p": 2}, time.Minute)
	a1, _ := r.Arrive(id, "op")
	Retry(shared(a1), "p", "x", nil)
	a2, _ := r.Arrive(id, "op")
	giveUp := &policy.ImmediateResponse{StatusCode: 502}
	act := Retry(shared(a2), "p", "x", giveUp)
	if ir, ok := act.(policy.ImmediateResponse); !ok || ir.StatusCode != 502 {
		t.Fatalf("a retry beyond the allowance must give up, got %#v", act)
	}
	if m, ok := Retry(shared(a2), "undeclared", "x", nil).(policy.DownstreamResponseHeaderModifications); !ok || len(m.HeadersToSet) != 0 {
		t.Fatal("a policy that declared no retries can never tag")
	}
}

func TestTimeoutIsInferredWhenTheNextAttemptArrives(t *testing.T) {
	r, _ := newTestRegistry()
	id, _ := r.Begin("op", map[string]int{"p": 3}, time.Minute)
	r.Arrive(id, "op") // never responds: Envoy abandoned it
	a2, _ := r.Arrive(id, "op")
	if !a2.PreviousTimedOut {
		t.Fatal("an attempt without a response before the next one arrived timed out")
	}
	out, ok := r.Close(id)
	if !ok || out.Attempts != 2 || out.LastResponded {
		t.Fatalf("outcome: %+v", out)
	}
}

func TestStateIsPerPolicyAndSurvivesAttempts(t *testing.T) {
	r, _ := newTestRegistry()
	id, _ := r.Begin("op", map[string]int{"p": 2}, time.Minute)
	a1, _ := r.Arrive(id, "op")
	a1.Set("p", "token", "T1")
	a2, _ := r.Arrive(id, "op")
	if v, ok := a2.Get("p", "token"); !ok || v != "T1" {
		t.Fatalf("state must survive attempts, got %v %v", v, ok)
	}
	if _, ok := a2.Get("other", "token"); ok {
		t.Fatal("state is per policy")
	}
}

func TestForgedMismatchedAndExpiredScopesAreRejected(t *testing.T) {
	r, c := newTestRegistry()
	if _, err := r.Arrive("forged", "op"); !errors.Is(err, ErrUnknownScope) {
		t.Fatalf("forged: %v", err)
	}
	id, _ := r.Begin("op", nil, time.Second)
	if _, err := r.Arrive(id, "other-op"); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	c.advance(2 * time.Second)
	if _, err := r.Arrive(id, "op"); !errors.Is(err, ErrUnknownScope) {
		t.Fatalf("expired: %v", err)
	}
	r.Sweep()
	if r.Len() != 0 {
		t.Fatal("sweep must remove expired scopes")
	}
}

func TestTheStoreIsBounded(t *testing.T) {
	r, _ := newTestRegistry() // max 3
	for i := 0; i < 3; i++ {
		if _, err := r.Begin("op", nil, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Begin("op", nil, time.Minute); !errors.Is(err, ErrTooManyScopes) {
		t.Fatalf("a full store must refuse new scopes, got %v", err)
	}
}

func TestGiveUpAnswerIsKeptForTheFrontRoute(t *testing.T) {
	r, _ := newTestRegistry()
	id, _ := r.Begin("op", map[string]int{"p": 2}, time.Minute)
	a1, _ := r.Arrive(id, "op")
	Retry(shared(a1), "p", "x", &policy.ImmediateResponse{StatusCode: 502})
	out, _ := r.Close(id)
	if out.GiveUp == nil || out.GiveUp.StatusCode != 502 {
		t.Fatalf("give-up answer: %+v", out.GiveUp)
	}
	if _, ok := r.Close(id); ok {
		t.Fatal("closing twice reports nothing the second time")
	}
}

func TestCurrentIsNilOnUnsplitOperations(t *testing.T) {
	if Current(&policy.SharedContext{Metadata: map[string]interface{}{}}) != nil || Current(nil) != nil {
		t.Fatal("no attempt without the system policy")
	}
	act := Retry(&policy.SharedContext{}, "p", "x", nil)
	if m, ok := act.(policy.DownstreamResponseHeaderModifications); !ok || len(m.HeadersToSet) != 0 {
		t.Fatal("retry on an unsplit operation is a no-op")
	}
}

func TestTransportFailure(t *testing.T) {
	for flags, want := range map[string]string{"UF": ConnectFailure, "UC": Reset, "UT": Timeout, "UF,UT": Timeout, "": ""} {
		h := policy.NewHeaders(map[string][]string{})
		if flags != "" {
			h = policy.NewHeaders(map[string][]string{HeaderUpstreamFailure: {flags}})
		}
		if got := TransportFailure(h); got != want {
			t.Errorf("%q: got %q want %q", flags, got, want)
		}
	}
	if !IsInternalHeader("X-WSO2-Attempt-Retry") || !IsInternalHeader("x-wso2-upstream-failure") || IsInternalHeader("authorization") {
		t.Fatal("internal header detection")
	}
}

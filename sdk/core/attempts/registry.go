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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// DefaultMaxScopes bounds how many client requests can be in flight on split
// operations in one policy engine process. Beyond it, new requests are
// refused rather than growing the store without limit.
const DefaultMaxScopes = 100000

var (
	// ErrUnknownScope means the scope id is missing, expired or was never
	// issued: the request did not come through the front route.
	ErrUnknownScope = errors.New("unknown or expired attempt scope")
	// ErrScopeMismatch means the scope belongs to another operation.
	ErrScopeMismatch = errors.New("attempt scope belongs to another operation")
	// ErrTooManyScopes means the store is full.
	ErrTooManyScopes = errors.New("too many requests in flight on split operations")
)

// Attempt is one attempt of a client request, as seen by a policy on the
// per-attempt route.
type Attempt struct {
	// Number is 1 for the first attempt.
	Number int
	// RequestedBy is the policy that asked for this attempt, "" for the first.
	RequestedBy string
	// Reason is what RequestedBy gave when asking.
	Reason string
	// PreviousTimedOut is true when the previous attempt never produced a
	// response: Envoy abandoned it on its per-attempt timeout.
	PreviousTimedOut bool

	scopeID string
	reg     *Registry
}

// ScopeID is the request's scope id, stable across its attempts.
func (a *Attempt) ScopeID() string { return a.scopeID }

// Get returns the value policyName stored under key for this client request.
func (a *Attempt) Get(policyName, key string) (interface{}, bool) {
	return a.reg.get(a.scopeID, policyName, key)
}

// Set stores a value for policyName under key; it is visible to that policy
// on every later attempt of the same client request.
func (a *Attempt) Set(policyName, key string, value interface{}) {
	a.reg.set(a.scopeID, policyName, key, value)
}

type scope struct {
	mu        sync.Mutex
	operation string
	expires   time.Time

	attempt   int
	responded bool // the current attempt produced a response

	requester string
	reason    string

	// allowances is each retrying policy's declared maximum attempts;
	// used counts the retries each has asked for.
	allowances map[string]int
	used       map[string]int

	giveUp *policy.ImmediateResponse
	state  map[string]map[string]interface{}
}

// Registry holds the scopes of in-flight client requests.
type Registry struct {
	mu        sync.Mutex
	scopes    map[string]*scope
	now       func() time.Time
	maxScopes int
	sweepOnce sync.Once
}

// NewRegistry returns an empty registry. now may be nil for time.Now.
func NewRegistry(now func() time.Time, maxScopes int) *Registry {
	if now == nil {
		now = time.Now
	}
	if maxScopes <= 0 {
		maxScopes = DefaultMaxScopes
	}
	return &Registry{scopes: map[string]*scope{}, now: now, maxScopes: maxScopes}
}

// Default is the process-wide registry shared by the gateway's system policy
// and every policy using this package.
var Default = NewRegistry(nil, DefaultMaxScopes)

// Begin opens a scope for one client request on operation. allowances maps
// each retrying policy to the most attempts it declared. ttl bounds how long
// the scope lives if Close is never called.
func (r *Registry) Begin(operation string, allowances map[string]int, ttl time.Duration) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	s := &scope{
		operation:  operation,
		expires:    r.now().Add(ttl),
		allowances: allowances,
		used:       map[string]int{},
		state:      map[string]map[string]interface{}{},
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.scopes) >= r.maxScopes {
		return "", ErrTooManyScopes
	}
	r.scopes[id] = s
	return id, nil
}

func (r *Registry) lookup(id string) *scope {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.scopes[id]
	if s == nil || !r.now().Before(s.expires) {
		return nil
	}
	return s
}

// Arrive admits the next attempt of scope id on operation.
func (r *Registry) Arrive(id, operation string) (*Attempt, error) {
	s := r.lookup(id)
	if s == nil {
		return nil, ErrUnknownScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.operation != operation {
		return nil, ErrScopeMismatch
	}
	a := &Attempt{scopeID: id, reg: r}
	if s.attempt > 0 {
		a.RequestedBy, a.Reason = s.requester, s.reason
		a.PreviousTimedOut = !s.responded
	}
	s.attempt++
	s.responded = false
	s.requester, s.reason = "", ""
	a.Number = s.attempt
	return a, nil
}

// Responded marks that the current attempt of scope id produced a response.
func (r *Registry) Responded(id string) {
	if s := r.lookup(id); s != nil {
		s.mu.Lock()
		s.responded = true
		s.mu.Unlock()
	}
}

// Outcome is what the front route needs when the client request finishes.
type Outcome struct {
	// Attempts is how many attempts were admitted.
	Attempts int
	// LastResponded is false when the last attempt was abandoned on timeout.
	LastResponded bool
	// GiveUp is the answer a policy asked for if no attempt was left.
	GiveUp *policy.ImmediateResponse
}

// Close removes scope id and returns its outcome. It is safe to call twice.
func (r *Registry) Close(id string) (Outcome, bool) {
	r.mu.Lock()
	s := r.scopes[id]
	delete(r.scopes, id)
	r.mu.Unlock()
	if s == nil {
		return Outcome{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Outcome{Attempts: s.attempt, LastResponded: s.responded, GiveUp: s.giveUp}, true
}

func (r *Registry) requestRetry(id, policyName, reason string, giveUp *policy.ImmediateResponse) bool {
	s := r.lookup(id)
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	max, declared := s.allowances[policyName]
	if !declared || s.used[policyName] >= max-1 {
		return false
	}
	s.used[policyName]++
	s.requester, s.reason = policyName, reason
	if giveUp != nil {
		g := *giveUp
		s.giveUp = &g
	}
	return true
}

func (r *Registry) get(id, policyName, key string) (interface{}, bool) {
	s := r.lookup(id)
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.state[policyName][key]
	return v, ok
}

func (r *Registry) set(id, policyName, key string, value interface{}) {
	s := r.lookup(id)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.state[policyName]
	if m == nil {
		m = map[string]interface{}{}
		s.state[policyName] = m
	}
	m[key] = value
}

// Sweep removes expired scopes.
func (r *Registry) Sweep() {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.scopes {
		if !now.Before(s.expires) {
			delete(r.scopes, id)
		}
	}
}

// StartSweeper sweeps every interval for the life of the process. Calling it
// more than once has no further effect.
func (r *Registry) StartSweeper(interval time.Duration) {
	r.sweepOnce.Do(func() {
		go func() {
			t := time.NewTicker(interval)
			defer t.Stop()
			for range t.C {
				r.Sweep()
			}
		}()
	})
}

// Len is the number of live scopes, for tests and metrics.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.scopes)
}

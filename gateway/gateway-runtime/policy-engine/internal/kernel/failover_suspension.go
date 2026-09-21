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

package kernel

import (
	"sync"
	"time"
)

// suspensionTracker records which failover targets recently failed, so the
// downstream phase can pre-emptively skip a known-bad target for new
// requests rather than dispatching one that's very likely to fail again.
//
// In-memory, per policy-engine replica — the same consistency level
// model-round-robin's own suspension tracking already has today. A
// Redis-backed shared tracker is a future hardening step, not part of this
// mechanism.
type suspensionTracker struct {
	mu    sync.Mutex
	until map[string]time.Time
}

// newSuspensionTracker constructs an empty tracker.
func newSuspensionTracker() *suspensionTracker {
	return &suspensionTracker{until: make(map[string]time.Time)}
}

// suspensionKey builds the tracker key for one failover chain entry. Shared
// by every reader/writer (the downstream phase's IsSuspended check now; a
// later upstream-phase Suspend call recording a failure) so both sides agree
// on identity without either needing to know the other's key format.
func suspensionKey(routeKey, model, provider string) string {
	return routeKey + "|" + model + "|" + provider
}

// IsSuspended reports whether key is currently suspended, lazily clearing an
// expired entry on read — check-and-delete-if-expired, no background sweep,
// mirroring model-round-robin's existing suspendedModels pattern.
func (t *suspensionTracker) IsSuspended(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	until, ok := t.until[key]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(t.until, key)
		return false
	}
	return true
}

// Suspend marks key suspended until now+duration. duration <= 0 is a
// no-op — this is what makes RouteFailover.SuspendDurationSeconds == 0
// disable suspension tracking entirely, per the config's own documented
// default.
func (t *suspensionTracker) Suspend(key string, duration time.Duration) {
	if duration <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.until[key] = time.Now().Add(duration)
}

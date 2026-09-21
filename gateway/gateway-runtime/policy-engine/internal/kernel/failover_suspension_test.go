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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSuspensionTracker_UnsuspendedKey_IsNotSuspended(t *testing.T) {
	tracker := newSuspensionTracker()
	assert.False(t, tracker.IsSuspended("route|gpt-4o|openai-provider"))
}

func TestSuspensionTracker_SuspendThenCheck_IsSuspended(t *testing.T) {
	tracker := newSuspensionTracker()
	key := suspensionKey("chat-route", "gpt-4o", "openai-provider")

	tracker.Suspend(key, time.Minute)

	assert.True(t, tracker.IsSuspended(key))
}

func TestSuspensionTracker_ExpiredSuspension_ClearsOnRead(t *testing.T) {
	tracker := newSuspensionTracker()
	key := suspensionKey("chat-route", "gpt-4o", "openai-provider")

	tracker.Suspend(key, -time.Second) // already expired

	assert.False(t, tracker.IsSuspended(key))
	// Lazily cleared — a second read finds no stale entry either.
	assert.False(t, tracker.IsSuspended(key))
	tracker.mu.Lock()
	_, stillPresent := tracker.until[key]
	tracker.mu.Unlock()
	assert.False(t, stillPresent, "expired entry must be deleted on read, not just reported false")
}

func TestSuspensionTracker_ZeroOrNegativeDuration_Noop(t *testing.T) {
	tracker := newSuspensionTracker()
	key := suspensionKey("chat-route", "gpt-4o", "openai-provider")

	tracker.Suspend(key, 0)
	assert.False(t, tracker.IsSuspended(key), "suspendDuration: 0 must disable suspension tracking entirely")
}

func TestSuspensionTracker_DistinctKeysIndependent(t *testing.T) {
	tracker := newSuspensionTracker()
	tracker.Suspend(suspensionKey("chat-route", "gpt-4o", "openai-provider"), time.Minute)

	assert.True(t, tracker.IsSuspended(suspensionKey("chat-route", "gpt-4o", "openai-provider")))
	assert.False(t, tracker.IsSuspended(suspensionKey("chat-route", "gpt-4o-mini", "openai-provider")), "different model")
	assert.False(t, tracker.IsSuspended(suspensionKey("chat-route", "gpt-4o", "anthropic-upstream")), "different provider")
	assert.False(t, tracker.IsSuspended(suspensionKey("other-route", "gpt-4o", "openai-provider")), "different route")
}

func TestSuspensionKey_DistinctInputsProduceDistinctKeys(t *testing.T) {
	a := suspensionKey("route-a", "gpt-4o", "openai-provider")
	b := suspensionKey("route-b", "gpt-4o", "openai-provider")
	assert.NotEqual(t, a, b)
}

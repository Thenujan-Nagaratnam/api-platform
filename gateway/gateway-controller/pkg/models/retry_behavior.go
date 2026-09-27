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

package models

import (
	"fmt"
	"strings"
	"time"
)

// When a policy runs if the gateway may send a request more than once.
const (
	RunsOnClientRequest = "onClientRequest"
	RunsOnEveryAttempt  = "onEveryAttempt"
	RunsOnBoth          = "onBoth"
)

// Values of SameOperationRule.Allowed.
const (
	AllowedNever  = "never"
	AllowedOnlyIf = "onlyIf"
)

// RetryBehavior is a policy definition's `retryBehavior` block: how the policy
// takes part in the gateway's per-attempt retry hop. The gateway reads it
// without knowing what the policy does.
type RetryBehavior struct {
	// Runs is when the policy runs on an operation the gateway splits:
	// onClientRequest (default), onEveryAttempt or onBoth.
	Runs string `json:"runs,omitempty" yaml:"runs,omitempty"`
	// CanRetry is present only if the policy can ask for another attempt.
	CanRetry *CanRetry `json:"canRetry,omitempty" yaml:"canRetry,omitempty"`
	// SameOperation lists rules for other policies attached to the same
	// operation, checked at registration.
	SameOperation []SameOperationRule `json:"sameOperation,omitempty" yaml:"sameOperation,omitempty"`
}

// CanRetry describes a policy that can ask the gateway to send the request
// again.
type CanRetry struct {
	// EnabledByParam is the boolean param that turns retrying on. Empty means
	// always on.
	EnabledByParam string `json:"enabledByParam,omitempty" yaml:"enabledByParam,omitempty"`

	// The most sends this policy can cause, the first included. Exactly one
	// of the three is set.
	MaxAttempts                *int         `json:"maxAttempts,omitempty" yaml:"maxAttempts,omitempty"`
	MaxAttemptsFromParam       string       `json:"maxAttemptsFromParam,omitempty" yaml:"maxAttemptsFromParam,omitempty"`
	MaxAttemptsFromLongestList *LongestList `json:"maxAttemptsFromLongestList,omitempty" yaml:"maxAttemptsFromLongestList,omitempty"`

	// How long one attempt may wait for the backend to start answering. At
	// most one is set; neither means the gateway default.
	PerAttemptTimeout          string `json:"perAttemptTimeout,omitempty" yaml:"perAttemptTimeout,omitempty"`
	PerAttemptTimeoutFromParam string `json:"perAttemptTimeoutFromParam,omitempty" yaml:"perAttemptTimeoutFromParam,omitempty"`

	// SeesConnectionFailures also reports connection failures, resets and
	// timeouts to the policy, and makes Envoy retry them.
	SeesConnectionFailures bool `json:"seesConnectionFailures,omitempty" yaml:"seesConnectionFailures,omitempty"`
}

// LongestList is "the longest list at Path in the params, plus Plus".
// Path segments are separated by dots; a segment ending in [] walks every
// element of that list.
type LongestList struct {
	Path string `json:"path" yaml:"path"`
	Plus int    `json:"plus" yaml:"plus"`
}

// SameOperationRule is one rule for another policy on the same operation.
type SameOperationRule struct {
	Policy  string  `json:"policy" yaml:"policy"`
	Allowed string  `json:"allowed" yaml:"allowed"`
	OnlyIf  *OnlyIf `json:"onlyIf,omitempty" yaml:"onlyIf,omitempty"`
}

// OnlyIf is the closed set of conditions an `allowed: onlyIf` rule can use.
type OnlyIf struct {
	// RunsBeforeThisPolicy requires the other policy earlier in the chain.
	RunsBeforeThisPolicy bool `json:"runsBeforeThisPolicy,omitempty" yaml:"runsBeforeThisPolicy,omitempty"`
	// ParamNotSet requires that the other policy's params have nothing at
	// this path (same path syntax as LongestList).
	ParamNotSet string `json:"paramNotSet,omitempty" yaml:"paramNotSet,omitempty"`
}

// EffectiveRuns returns Runs, defaulting to onClientRequest.
func (b *RetryBehavior) EffectiveRuns() string {
	if b == nil || b.Runs == "" {
		return RunsOnClientRequest
	}
	return b.Runs
}

// Validate checks the block's shape when a definition is loaded.
func (b *RetryBehavior) Validate() error {
	if b == nil {
		return nil
	}
	switch b.EffectiveRuns() {
	case RunsOnClientRequest, RunsOnEveryAttempt, RunsOnBoth:
	default:
		return fmt.Errorf("retryBehavior.runs must be %s, %s or %s, got %q", RunsOnClientRequest, RunsOnEveryAttempt, RunsOnBoth, b.Runs)
	}
	if c := b.CanRetry; c != nil {
		if b.EffectiveRuns() == RunsOnClientRequest {
			return fmt.Errorf("retryBehavior.canRetry needs runs: %s or %s, since the policy must see each attempt's response", RunsOnEveryAttempt, RunsOnBoth)
		}
		sources := 0
		if c.MaxAttempts != nil {
			sources++
			if *c.MaxAttempts < 1 {
				return fmt.Errorf("retryBehavior.canRetry.maxAttempts must be at least 1")
			}
		}
		if c.MaxAttemptsFromParam != "" {
			sources++
		}
		if c.MaxAttemptsFromLongestList != nil {
			sources++
			if strings.TrimSpace(c.MaxAttemptsFromLongestList.Path) == "" {
				return fmt.Errorf("retryBehavior.canRetry.maxAttemptsFromLongestList.path is required")
			}
		}
		if sources != 1 {
			return fmt.Errorf("retryBehavior.canRetry needs exactly one of maxAttempts, maxAttemptsFromParam, maxAttemptsFromLongestList")
		}
		if c.PerAttemptTimeout != "" && c.PerAttemptTimeoutFromParam != "" {
			return fmt.Errorf("retryBehavior.canRetry takes at most one of perAttemptTimeout, perAttemptTimeoutFromParam")
		}
		if c.PerAttemptTimeout != "" {
			if d, err := time.ParseDuration(c.PerAttemptTimeout); err != nil || d <= 0 {
				return fmt.Errorf("retryBehavior.canRetry.perAttemptTimeout must be a positive duration, got %q", c.PerAttemptTimeout)
			}
		}
	}
	for i, r := range b.SameOperation {
		if r.Policy == "" {
			return fmt.Errorf("retryBehavior.sameOperation[%d].policy is required", i)
		}
		switch r.Allowed {
		case AllowedNever:
			if r.OnlyIf != nil {
				return fmt.Errorf("retryBehavior.sameOperation[%d]: onlyIf is only used with allowed: %s", i, AllowedOnlyIf)
			}
		case AllowedOnlyIf:
			if r.OnlyIf == nil || (!r.OnlyIf.RunsBeforeThisPolicy && r.OnlyIf.ParamNotSet == "") {
				return fmt.Errorf("retryBehavior.sameOperation[%d]: allowed: %s needs at least one onlyIf condition", i, AllowedOnlyIf)
			}
		default:
			return fmt.Errorf("retryBehavior.sameOperation[%d].allowed must be %s or %s, got %q", i, AllowedNever, AllowedOnlyIf, r.Allowed)
		}
	}
	return nil
}

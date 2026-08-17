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

package config

import (
	"fmt"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// MergeRetryConditions combines every contributor's RetryConditions on one
// route into a single merged value, per the composition table in
// docs/superpowers/specs/2026-08-17-generalized-retry-conditions-design.md
// Component 2. Field-by-field: On/StatusCodes/Headers/AvoidPreviousHosts
// union safely (see this function); NumRetries/BackOff reject on conflict
// (see mergeSingleOwnerFields in the next task).
func MergeRetryConditions(contributions []policy.RetryConditions) (policy.RetryConditions, error) {
	var merged policy.RetryConditions

	onSet := map[string]struct{}{}
	codeSet := map[int]struct{}{}
	headerSet := map[string]policy.RetriableHeader{}

	for _, c := range contributions {
		for _, on := range c.On {
			onSet[on] = struct{}{}
		}
		for _, code := range c.StatusCodes {
			codeSet[code] = struct{}{}
		}
		for _, h := range c.Headers {
			headerSet[h.Name+"\x00"+h.Exact] = h
		}
		if c.AvoidPreviousHosts {
			merged.AvoidPreviousHosts = true
		}
		if c.MinAttempts != nil && (merged.MinAttempts == nil || *c.MinAttempts > *merged.MinAttempts) {
			v := *c.MinAttempts
			merged.MinAttempts = &v
		}
		if c.PerTryTimeout != nil && (merged.PerTryTimeout == nil || *c.PerTryTimeout < *merged.PerTryTimeout) {
			v := *c.PerTryTimeout
			merged.PerTryTimeout = &v
		}
	}

	for code := range codeSet {
		merged.StatusCodes = append(merged.StatusCodes, code)
	}
	for _, h := range headerSet {
		merged.Headers = append(merged.Headers, h)
	}
	if len(merged.StatusCodes) > 0 {
		onSet["retriable-status-codes"] = struct{}{}
	}
	if len(merged.Headers) > 0 {
		onSet["retriable-headers"] = struct{}{}
	}
	for on := range onSet {
		merged.On = append(merged.On, on)
	}

	numRetries, backOff, err := mergeSingleOwnerFields(contributions)
	if err != nil {
		return policy.RetryConditions{}, err
	}
	merged.NumRetries = numRetries
	merged.BackOff = backOff
	if merged.NumRetries == nil && merged.MinAttempts != nil {
		derived := *merged.MinAttempts - 1
		merged.NumRetries = &derived
	}

	return merged, nil
}

// mergeSingleOwnerFields enforces the two single-owner composition rules:
// NumRetries allows multiple contributors only if they all agree on the
// same value; BackOff allows at most one contributor, full stop, even with
// identical values (ownership ambiguity is the problem, not the value).
func mergeSingleOwnerFields(contributions []policy.RetryConditions) (*int, *policy.RetryBackOff, error) {
	var numRetries *int
	var backOffOwners int
	var backOff *policy.RetryBackOff

	for _, c := range contributions {
		if c.NumRetries != nil {
			if numRetries != nil && *numRetries != *c.NumRetries {
				return nil, nil, fmt.Errorf(
					"conflicting NumRetries: %d and %d declared by different contributors on the same route",
					*numRetries, *c.NumRetries)
			}
			numRetries = c.NumRetries
		}
		if c.BackOff != nil {
			backOffOwners++
			backOff = c.BackOff
		}
	}
	if backOffOwners > 1 {
		return nil, nil, fmt.Errorf(
			"BackOff declared by %d contributors on the same route — at most one is allowed, even with identical values",
			backOffOwners)
	}

	return numRetries, backOff, nil
}

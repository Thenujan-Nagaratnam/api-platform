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

// PolicyDefinition represents the definition/schema of a policy
type PolicyDefinition struct {
	Name             string                  `json:"name" yaml:"name"`
	Version          string                  `json:"version" yaml:"version"`
	DisplayName      string                  `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	Description      *string                 `json:"description,omitempty" yaml:"description,omitempty"`
	Parameters       *map[string]interface{} `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	SystemParameters *map[string]interface{} `json:"systemParameters,omitempty" yaml:"systemParameters,omitempty"`
	ManagedBy        string                  `json:"managedBy" yaml:"managedBy,omitempty"`
	Retry            *Retry                  `json:"retry,omitempty" yaml:"retry,omitempty"`
}

// LongestListAttempts derives a MaxAttempts value from the length of a
// list-shaped param on the same policy, plus a constant (e.g. one attempt
// per entry in a fallback-target list, plus the primary attempt).
type LongestListAttempts struct {
	// ParamPath is a dot-separated path into this policy's own Parameters.
	ParamPath string `json:"paramPath" yaml:"paramPath"`
	Plus      int    `json:"plus,omitempty" yaml:"plus,omitempty"`
}

// Retry declares, once per policy definition, which backend response or
// connection conditions justify the gateway retrying a request against the
// same upstream target, and how many times. Never set per-attachment; see
// specs/001-upstream-retry-contribution/data-model.md for the full contract,
// including the fixed-vs-"...FromParam" resolution rules.
type Retry struct {
	// EnabledByParam names a boolean param on this same policy that turns
	// the whole contribution on or off for a given attachment. Empty means
	// always on.
	EnabledByParam string `json:"enabledByParam,omitempty" yaml:"enabledByParam,omitempty"`

	StatusCodes          []int  `json:"statusCodes,omitempty" yaml:"statusCodes,omitempty"`
	StatusCodesFromParam string `json:"statusCodesFromParam,omitempty" yaml:"statusCodesFromParam,omitempty"`

	// MaxAttempts is the total number of sends, the first attempt included
	// — not Envoy's "retries excluding the first" convention. Exactly one
	// of MaxAttempts / MaxAttemptsFromParam / MaxAttemptsFromLongestList
	// must be set.
	MaxAttempts                int                  `json:"maxAttempts,omitempty" yaml:"maxAttempts,omitempty"`
	MaxAttemptsFromParam       string               `json:"maxAttemptsFromParam,omitempty" yaml:"maxAttemptsFromParam,omitempty"`
	MaxAttemptsFromLongestList *LongestListAttempts `json:"maxAttemptsFromLongestList,omitempty" yaml:"maxAttemptsFromLongestList,omitempty"`

	PerAttemptTimeoutMs          int    `json:"perAttemptTimeoutMs,omitempty" yaml:"perAttemptTimeoutMs,omitempty"`
	PerAttemptTimeoutMsFromParam string `json:"perAttemptTimeoutMsFromParam,omitempty" yaml:"perAttemptTimeoutMsFromParam,omitempty"`

	// AlsoRetryOnConnectionFailure opts into Envoy's connect-failure/reset/
	// reset-before-request retry conditions, not only a response status.
	AlsoRetryOnConnectionFailure          bool   `json:"alsoRetryOnConnectionFailure,omitempty" yaml:"alsoRetryOnConnectionFailure,omitempty"`
	AlsoRetryOnConnectionFailureFromParam string `json:"alsoRetryOnConnectionFailureFromParam,omitempty" yaml:"alsoRetryOnConnectionFailureFromParam,omitempty"`
}

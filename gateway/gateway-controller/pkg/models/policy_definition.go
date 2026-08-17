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

// RetrySourceMetadata is a policy-definition.yaml's x-wso2-retry-source
// declaration: this policy's params describe one or more
// independently-selectable failover chains, in the fixed structural shape
// ParseRetrySourceParams understands (targets: [{<GroupKeyField>: string,
// upstreamDefinition: string, fallbacks: [{upstreamDefinition: string}]}]).
// The target-list field name defaults to "targets" but is overridable via
// TargetsField below. WHAT to retry on (status codes, etc.) is a separate
// concern, declared via this policy's own x-wso2-retry-conditions block —
// RetrySourceMetadata never describes retry conditions itself.
type RetrySourceMetadata struct {
	// GroupKeyField names which field in each targets[] entry
	// gateway-controller treats as the group's opaque discriminator (e.g.
	// "model" for model-failover). Required when RetrySource is non-nil.
	GroupKeyField string `json:"groupKeyField" yaml:"groupKeyField"`

	// TargetsField names which top-level field in this policy's params
	// holds the ordered target list. Defaults to "targets" when empty —
	// see ParseRetrySourceParams.
	TargetsField string `json:"targetsField,omitempty" yaml:"targetsField,omitempty"`
}

// PolicyDefinition represents the definition/schema of a policy
type PolicyDefinition struct {
	Name             string                  `json:"name" yaml:"name"`
	Version          string                  `json:"version" yaml:"version"`
	DisplayName      string                  `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	Description      *string                 `json:"description,omitempty" yaml:"description,omitempty"`
	Parameters       *map[string]interface{} `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	SystemParameters *map[string]interface{} `json:"systemParameters,omitempty" yaml:"systemParameters,omitempty"`
	ManagedBy        string                  `json:"managedBy" yaml:"managedBy,omitempty"`

	// RetrySource is non-nil when this policy's policy-definition.yaml
	// declares x-wso2-retry-source.
	RetrySource *RetrySourceMetadata `json:"x-wso2-retry-source,omitempty" yaml:"x-wso2-retry-source,omitempty"`

	// RetryConditions is this policy's x-wso2-retry-conditions declaration
	// (see docs/superpowers/specs/2026-08-17-generalized-retry-conditions-design.md) —
	// a raw map because its values are heterogeneous: each key is either a
	// literal or a {fromParam: "<name>"} pointer, resolved generically by
	// config.ParseRetryConditions, never unmarshaled into a typed struct.
	RetryConditions *map[string]interface{} `json:"x-wso2-retry-conditions,omitempty" yaml:"x-wso2-retry-conditions,omitempty"`

	// UpstreamResponseObserver is true when this policy's
	// policy-definition.yaml declares x-wso2-upstream-response-observer:
	// true — see Task 8.
	UpstreamResponseObserver bool `json:"x-wso2-upstream-response-observer,omitempty" yaml:"x-wso2-upstream-response-observer,omitempty"`
}

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
// upstreamDefinition: string, fallbacks: [{upstreamDefinition: string}]}],
// statusCodes: [int]).
type RetrySourceMetadata struct {
	// GroupKeyField names which field in each targets[] entry
	// gateway-controller treats as the group's opaque discriminator (e.g.
	// "model" for model-failover). Required when RetrySource is non-nil.
	GroupKeyField string `json:"groupKeyField" yaml:"groupKeyField"`
}

// RetryTriggerMetadata is a policy-definition.yaml's x-wso2-retry-trigger
// declaration: this policy's params contribute retry conditions without
// owning destination selection.
type RetryTriggerMetadata struct {
	// StatusCodesField names which top-level field in this policy's params
	// holds the array of retriable status codes (e.g.
	// "tokenPurgeStatusCodes" for oauth2-generator).
	StatusCodesField string `json:"statusCodesField" yaml:"statusCodesField"`

	// MinAttempts is a fixed constant (not read from params) — the minimum
	// total attempts this policy needs to get value from retrying.
	MinAttempts int `json:"minAttempts" yaml:"minAttempts"`
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

	// RetryTrigger is non-nil when this policy's policy-definition.yaml
	// declares x-wso2-retry-trigger.
	RetryTrigger *RetryTriggerMetadata `json:"x-wso2-retry-trigger,omitempty" yaml:"x-wso2-retry-trigger,omitempty"`

	// UpstreamResponseObserver is true when this policy's
	// policy-definition.yaml declares x-wso2-upstream-response-observer:
	// true — see Task 8.
	UpstreamResponseObserver bool `json:"x-wso2-upstream-response-observer,omitempty" yaml:"x-wso2-upstream-response-observer,omitempty"`
}

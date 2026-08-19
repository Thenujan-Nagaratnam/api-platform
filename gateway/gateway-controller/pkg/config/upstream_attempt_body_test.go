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
	"testing"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

func attemptBodyTestDefinitions() map[string]models.PolicyDefinition {
	return map[string]models.PolicyDefinition{
		"body-policy|v1.0.0": {
			Name:            "body-policy",
			Version:         "v1.0.0",
			UpstreamAttempt: &models.UpstreamAttemptMetadata{Body: true},
		},
		"headers-only-policy|v1.0.0": {
			Name:            "headers-only-policy",
			Version:         "v1.0.0",
			UpstreamAttempt: &models.UpstreamAttemptMetadata{Body: false},
		},
		"inert-policy|v1.0.0": {
			Name:    "inert-policy",
			Version: "v1.0.0",
		},
	}
}

func TestLookupUpstreamAttemptBody_TrueForDeclaringPolicy(t *testing.T) {
	defs := attemptBodyTestDefinitions()
	latest := BuildLatestVersionIndex(defs)
	if !LookupUpstreamAttemptBody(defs, latest, "body-policy", "v1") {
		t.Error("expected true for a policy declaring x-wso2-upstream-attempt: {body: true}")
	}
}

func TestLookupUpstreamAttemptBody_FalseForPolicyDeclaringBodyFalse(t *testing.T) {
	defs := attemptBodyTestDefinitions()
	latest := BuildLatestVersionIndex(defs)
	if LookupUpstreamAttemptBody(defs, latest, "headers-only-policy", "v1") {
		t.Error("expected false for a policy that explicitly declares body: false")
	}
}

func TestLookupUpstreamAttemptBody_FalseForNonDeclaringPolicy(t *testing.T) {
	defs := attemptBodyTestDefinitions()
	latest := BuildLatestVersionIndex(defs)
	if LookupUpstreamAttemptBody(defs, latest, "inert-policy", "v1") {
		t.Error("expected false for a policy with no x-wso2-upstream-attempt block at all")
	}
}

func TestLookupUpstreamAttemptBody_FalseForUnresolvablePolicy(t *testing.T) {
	defs := attemptBodyTestDefinitions()
	latest := BuildLatestVersionIndex(defs)
	if LookupUpstreamAttemptBody(defs, latest, "does-not-exist", "v1") {
		t.Error("expected false for a policy name/version that doesn't resolve, not an error")
	}
}

func TestLookupUpstreamAttemptBody_FalseForEmptyDefinitions(t *testing.T) {
	if LookupUpstreamAttemptBody(nil, nil, "body-policy", "v1") {
		t.Error("expected false when no policy definitions are loaded")
	}
}

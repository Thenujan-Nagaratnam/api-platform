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

import "github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"

// LookupUpstreamAttemptBody resolves ONE policy reference (name plus a possibly major-only
// version, the form a models.PolicyChain carries) against the loaded policy definitions and
// reports whether that policy's own policy-definition.yaml declares
// x-wso2-upstream-attempt: {body: true}. false covers both "this policy declares no such
// thing" and "the name/version doesn't resolve at all" — an unresolvable reference is already
// rejected far earlier by PolicyValidator and must not turn into a translation-time error
// here.
//
// Uses the exact same resolution mechanics as LookupUpstreamResponseObserver/
// LookupRetryMetadata (major-only version resolved via ResolvePolicyVersion, then keyed by
// "name|resolved"), so this and their lookups can never disagree about the same policy chain.
// gateway-controller never imports or instantiates any policy implementation package — this
// reads cached YAML metadata only.
func LookupUpstreamAttemptBody(
	definitions map[string]models.PolicyDefinition,
	latestVersions map[string]string,
	name, version string,
) bool {
	if len(definitions) == 0 {
		return false
	}
	resolved, err := ResolvePolicyVersion(definitions, latestVersions, name, version)
	if err != nil {
		return false
	}
	def, ok := definitions[name+"|"+resolved]
	if !ok {
		return false
	}
	return def.UpstreamAttempt != nil && def.UpstreamAttempt.Body
}

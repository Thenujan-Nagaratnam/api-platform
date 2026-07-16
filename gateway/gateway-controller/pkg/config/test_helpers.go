/*
 * Copyright (c) 2025, WSO2 LLC. (https://www.wso2.com).
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

import "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"

// stringPtr is a helper function that returns a pointer to the provided string.
// This is used in tests to create string pointers for optional fields.
func stringPtr(s string) *string {
	return &s
}

// oauth2ClientAuthMethodPtr returns a pointer to the given
// LLMProviderConfigDataUpstreamAuthOauth2ClientAuthMethod value, for use in
// oauth2 upstream auth test fixtures (client_secret_basic/client_secret_post).
func oauth2ClientAuthMethodPtr(v string) *management.LLMProviderConfigDataUpstreamAuthOauth2ClientAuthMethod {
	m := management.LLMProviderConfigDataUpstreamAuthOauth2ClientAuthMethod(v)
	return &m
}

// oauth2GrantTypePtr returns a pointer to the given
// LLMProviderConfigDataUpstreamAuthOauth2GrantType value, for use in oauth2
// upstream auth test fixtures (client_credentials/password).
func oauth2GrantTypePtr(v string) *management.LLMProviderConfigDataUpstreamAuthOauth2GrantType {
	g := management.LLMProviderConfigDataUpstreamAuthOauth2GrantType(v)
	return &g
}

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

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/constants"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// TestNewLLMValidator tests the constructor
func TestNewLLMValidator(t *testing.T) {
	validator := NewLLMValidator()

	assert.NotNil(t, validator, "Validator should not be nil")
	assert.NotNil(t, validator.versionRegex, "Version regex should be initialized")
	assert.NotNil(t, validator.metadataNameRegex, "URL friendly metadata.name regex should be initialized")
}

// ============================================================================
// LLM Provider Template Validation Tests
// ============================================================================

// TestValidateLLMProviderTemplate_Valid tests validation of valid templates
func TestValidateLLMProviderTemplate_Valid(t *testing.T) {
	tests := []struct {
		name     string
		template api.LLMProviderTemplate
	}{
		{
			name: "full template with all fields",
			template: api.LLMProviderTemplate{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderTemplateKindLlmProviderTemplate,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderTemplateData{
					DisplayName: "openai",
					PromptTokens: &api.ExtractionIdentifier{
						Location:   api.Payload,
						Identifier: "$.usage.prompt_tokens",
					},
					CompletionTokens: &api.ExtractionIdentifier{
						Location:   api.Payload,
						Identifier: "$.usage.completion_tokens",
					},
					TotalTokens: &api.ExtractionIdentifier{
						Location:   api.Payload,
						Identifier: "$.usage.total_tokens",
					},
					RequestModel: &api.ExtractionIdentifier{
						Location:   api.Payload,
						Identifier: "$.model",
					},
				},
			},
		},
		{
			name: "minimal template",
			template: api.LLMProviderTemplate{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderTemplateKindLlmProviderTemplate,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderTemplateData{
					DisplayName: "minimal",
				},
			},
		},
		{
			name: "template with header extraction",
			template: api.LLMProviderTemplate{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderTemplateKindLlmProviderTemplate,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderTemplateData{
					DisplayName: "custom-llm",
					PromptTokens: &api.ExtractionIdentifier{
						Location:   api.Header,
						Identifier: "X-Prompt-Tokens",
					},
				},
			},
		},
		{
			name: "template with various name formats",
			template: api.LLMProviderTemplate{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderTemplateKindLlmProviderTemplate,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderTemplateData{
					DisplayName: "llm-provider_1.0",
				},
			},
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errors := validator.Validate(&tt.template)
			assert.Empty(t, errors, "Valid template should not produce validation errors")
		})
	}
}

// TestValidateLLMProviderTemplate_InvalidKind tests kind validation
func TestValidateLLMProviderTemplate_InvalidKind(t *testing.T) {
	tests := []struct {
		name        string
		kind        string
		expectError bool
	}{
		{
			name:        "invalid kind - llm/template",
			kind:        "llm/template",
			expectError: true,
		},
		{
			name:        "invalid kind - provider-template",
			kind:        "provider-template",
			expectError: true,
		},
		{
			name:        "invalid kind - empty",
			kind:        "",
			expectError: true,
		},
		{
			name:        "valid kind",
			kind:        string(api.LLMProviderTemplateKindLlmProviderTemplate),
			expectError: false,
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			template := api.LLMProviderTemplate{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderTemplateKind(tt.kind),
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderTemplateData{
					DisplayName: "test",
				},
			}

			errors := validator.Validate(&template)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == "kind" {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for kind field")
			} else {
				assert.Empty(t, errors, "Should not have validation errors")
			}
		})
	}
}

// TestValidateLLMProviderTemplate_InvalidName tests name validation
func TestValidateLLMProviderTemplate_InvalidName(t *testing.T) {
	tests := []struct {
		name         string
		templateName string
		expectError  bool
		errorPart    string
	}{
		{
			name:         "invalid name - empty",
			templateName: "",
			expectError:  true,
			errorPart:    "spec.displayName",
		},
		{
			name:         "invalid name - too large",
			templateName: "a123456789b123456789c123456789d123456789e123456789f123456789g123456789h123456789i123456789j123456789k123456789l123456789m123456789n123456789o123456789p123456789q123456789r123456789s123456789t123456789u123456789v123456789w123456789x123456789y123456789z12345",
			expectError:  true,
			errorPart:    "spec.displayName",
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			template := api.LLMProviderTemplate{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderTemplateKindLlmProviderTemplate,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderTemplateData{
					DisplayName: tt.templateName,
				},
			}

			errors := validator.Validate(&template)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == "spec.displayName" && strings.Contains(err.Message, tt.errorPart) {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for spec.name with message containing %s", tt.errorPart)
			} else {
				assert.Empty(t, errors, "Should not have validation errors")
			}
		})
	}
}

// TestValidateLLMProviderTemplate_InvalidMetadataName tests metadata.name validation
func TestValidateLLMProviderTemplate_InvalidMetadataName(t *testing.T) {
	tests := []struct {
		name         string
		metadataName string
		expectError  bool
		errorPart    string
	}{
		{
			name:         "invalid spec.displayName - spaces",
			metadataName: "my provider config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - special chars @",
			metadataName: "provider@config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - special chars #",
			metadataName: "provider#config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - slash",
			metadataName: "provider/config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - brackets",
			metadataName: "provider[1]",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - parentheses",
			metadataName: "provider(v1)",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - percent",
			metadataName: "provider%20config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - ampersand",
			metadataName: "provider&config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - plus",
			metadataName: "provider+config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - equals",
			metadataName: "provider=config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - question mark",
			metadataName: "provider?config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - colon",
			metadataName: "provider:config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - semicolon",
			metadataName: "provider;config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - empty string",
			metadataName: "",
			expectError:  true,
			errorPart:    "required",
		},
		{
			name:         "invalid metadata.name - only spaces",
			metadataName: "   ",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - starts with hyphen",
			metadataName: "-provider",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - starts with dot",
			metadataName: ".provider",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - starts with underscore",
			metadataName: "_provider",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - ends with hyphen",
			metadataName: "provider-",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - ends with dot",
			metadataName: "provider.",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - consecutive hyphens",
			metadataName: "provider--config",
			expectError:  false,
		},
		{
			name:         "invalid metadata.name - consecutive dots",
			metadataName: "provider..config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "valid metadata.name - letters only",
			metadataName: "provider",
			expectError:  false,
		},
		{
			name:         "valid metadata.name - with hyphen",
			metadataName: "my-provider",
			expectError:  false,
		},
		{
			name:         "valid metadata.name - with underscore",
			metadataName: "my_provider",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "valid metadata.name - with dot",
			metadataName: "my.provider",
			expectError:  false,
		},
		{
			name:         "valid metadata.name - with numbers",
			metadataName: "provider-v1",
			expectError:  false,
		},
		{
			name:         "valid metadata.name - complex",
			metadataName: "my-llm_provider.v1",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "valid metadata.name - all alphanumeric",
			metadataName: "provider123",
			expectError:  false,
		},
		{
			name:         "valid metadata.name - mixed case",
			metadataName: "MyProvider-Config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "valid metadata.name - starts with number",
			metadataName: "1provider",
			expectError:  false,
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: tt.metadataName},
				Spec: api.LLMProviderConfigData{
					DisplayName: "test-display-name",
					Version:     "v1.0",
					Template:    "openai",
					Upstream: api.LLMProviderConfigData_Upstream{
						Url: stringPtr("https://api.example.com"),
					},
					AccessControl: api.LLMAccessControl{
						Mode: api.AllowAll,
					},
				},
			}

			errors := validator.Validate(&provider)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == "metadata.name" && strings.Contains(err.Message, tt.errorPart) {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for metadata.name with message containing %s", tt.errorPart)
			} else {
				assert.Empty(t, errors, "Should not have validation errors for valid metadata.name")
			}
		})
	}
}

// TestValidateLLMProviderTemplate_ExtractionIdentifier tests extraction identifier validation
func TestValidateLLMProviderTemplate_ExtractionIdentifier(t *testing.T) {
	tests := []struct {
		name        string
		identifier  *api.ExtractionIdentifier
		fieldPrefix string
		expectError bool
		errorField  string
		errorPart   string
	}{
		{
			name: "invalid location - body",
			identifier: &api.ExtractionIdentifier{
				Location:   "body",
				Identifier: "$.tokens",
			},
			fieldPrefix: "spec.promptTokens",
			expectError: true,
			errorField:  "spec.promptTokens.location",
			errorPart:   "payload' or 'header",
		},
		{
			name: "invalid location - empty",
			identifier: &api.ExtractionIdentifier{
				Location:   "",
				Identifier: "$.tokens",
			},
			fieldPrefix: "spec.promptTokens",
			expectError: true,
			errorField:  "spec.promptTokens.location",
		},
		{
			name: "missing identifier",
			identifier: &api.ExtractionIdentifier{
				Location:   api.Payload,
				Identifier: "",
			},
			fieldPrefix: "spec.promptTokens",
			expectError: true,
			errorField:  "spec.promptTokens.identifier",
			errorPart:   "required",
		},
		{
			name: "valid payload location",
			identifier: &api.ExtractionIdentifier{
				Location:   api.Payload,
				Identifier: "$.usage.tokens",
			},
			fieldPrefix: "spec.promptTokens",
			expectError: false,
		},
		{
			name: "valid header location",
			identifier: &api.ExtractionIdentifier{
				Location:   api.Header,
				Identifier: "X-Token-Count",
			},
			fieldPrefix: "spec.promptTokens",
			expectError: false,
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			template := api.LLMProviderTemplate{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderTemplateKindLlmProviderTemplate,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderTemplateData{
					DisplayName:  "test",
					PromptTokens: tt.identifier,
				},
			}

			errors := validator.Validate(&template)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == tt.errorField && (tt.errorPart == "" || strings.Contains(err.Message, tt.errorPart)) {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for field %s", tt.errorField)
			} else {
				assert.Empty(t, errors, "Should not have validation errors")
			}
		})
	}
}

// ============================================================================
// LLM Provider Configuration Validation Tests
// ============================================================================

// TestValidateLLMProvider_Valid tests validation of valid provider configurations
func TestValidateLLMProvider_Valid(t *testing.T) {
	tests := []struct {
		name     string
		provider api.LLMProviderConfiguration
	}{
		{
			name: "full provider with all fields",
			provider: api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderConfigData{
					DisplayName: "my-provider",
					Version:     "v1.0",
					Context:     stringPtr("/openai"),
					Vhost:       stringPtr("api.openai.com"),
					Template:    "openai",
					Upstream: api.LLMProviderConfigData_Upstream{
						Url: stringPtr("https://api.openai.com"),
						Auth: &struct {
							Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
							PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
							PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
							PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
							Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
							Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
						}{
							Type:   api.LLMProviderConfigDataUpstreamAuthTypeApiKey,
							Header: stringPtr("Authorization"),
							Value:  stringPtr("Bearer sk-test"),
						},
					},
					AccessControl: api.LLMAccessControl{
						Mode: api.AllowAll,
					},
				},
			},
		},
		{
			name: "minimal provider",
			provider: api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderConfigData{
					DisplayName: "minimal",
					Version:     "v1.0",
					Template:    "openai",
					Upstream: api.LLMProviderConfigData_Upstream{
						Url: stringPtr("https://api.example.com"),
					},
					AccessControl: api.LLMAccessControl{
						Mode: api.AllowAll,
					},
				},
			},
		},
		{
			name: "provider with deny_all mode",
			provider: api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderConfigData{
					DisplayName: "restricted",
					Version:     "v1.0",
					Template:    "openai",
					Upstream: api.LLMProviderConfigData_Upstream{
						Url: stringPtr("https://api.example.com"),
					},
					AccessControl: api.LLMAccessControl{
						Mode: api.DenyAll,
						Exceptions: &[]api.RouteException{
							{
								Path:    "/v1/chat/completions",
								Methods: []api.RouteExceptionMethods{api.RouteExceptionMethodsPOST},
							},
						},
					},
				},
			},
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errors := validator.Validate(&tt.provider)
			assert.Empty(t, errors, "Valid provider should not produce validation errors")
		})
	}
}

// TestValidateLLMProvider_InvalidKind tests provider kind validation
func TestValidateLLMProvider_InvalidKind(t *testing.T) {
	tests := []struct {
		name        string
		kind        string
		expectError bool
	}{
		{
			name:        "invalid kind - llm/proxy",
			kind:        "llm/proxy",
			expectError: true,
		},
		{
			name:        "invalid kind - provider",
			kind:        "provider",
			expectError: true,
		},
		{
			name:        "valid kind",
			kind:        string(api.LLMProviderConfigurationKindLlmProvider),
			expectError: false,
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKind(tt.kind),
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderConfigData{
					DisplayName: "test",
					Version:     "v1.0",
					Template:    "openai",
					Upstream: api.LLMProviderConfigData_Upstream{
						Url: stringPtr("https://api.example.com"),
					},
					AccessControl: api.LLMAccessControl{
						Mode: api.AllowAll,
					},
				},
			}

			errors := validator.Validate(&provider)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == "kind" {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for kind field")
			} else {
				assert.Empty(t, errors, "Should not have validation errors")
			}
		})
	}
}

// TestValidateLLMProvider_Name tests provider name validation
func TestValidateLLMProvider_Name(t *testing.T) {
	tests := []struct {
		name         string
		providerName string
		expectError  bool
		errorPart    string
	}{
		{
			name:         "invalid name - empty",
			providerName: "",
			expectError:  true,
			errorPart:    "spec.displayName",
		},
		{
			name:         "invalid name - too large",
			providerName: "a123456789b123456789c123456789d123456789e123456789f123456789g123456789h123456789i123456789j123456789k123456789l123456789m123456789n123456789o123456789p123456789q123456789r123456789s123456789t123456789u123456789v123456789w123456789x123456789y123456789z12345",
			expectError:  true,
			errorPart:    "spec.displayName",
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderConfigData{
					DisplayName: tt.providerName,
					Version:     "v1.0",
					Template:    "openai",
					Upstream: api.LLMProviderConfigData_Upstream{
						Url: stringPtr("https://api.example.com"),
					},
					AccessControl: api.LLMAccessControl{
						Mode: api.AllowAll,
					},
				},
			}

			errors := validator.Validate(&provider)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == "spec.displayName" && strings.Contains(err.Message, tt.errorPart) {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for spec.name field")
			} else {
				assert.Empty(t, errors, "Should not have validation errors")
			}
		})
	}
}

// TestValidateLLMProvider_InvalidMetadataName tests metadata.name validation for LLM Provider
func TestValidateLLMProvider_InvalidMetadataName(t *testing.T) {
	tests := []struct {
		name         string
		metadataName string
		expectError  bool
		errorPart    string
	}{
		{
			name:         "invalid metadata.name - spaces",
			metadataName: "my provider config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - special chars @",
			metadataName: "provider@config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - special chars #",
			metadataName: "provider#config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - slash",
			metadataName: "provider/config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - brackets",
			metadataName: "provider[1]",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - parentheses",
			metadataName: "provider(v1)",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - percent",
			metadataName: "provider%20config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - ampersand",
			metadataName: "provider&config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - plus",
			metadataName: "provider+config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - equals",
			metadataName: "provider=config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - question mark",
			metadataName: "provider?config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - colon",
			metadataName: "provider:config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - semicolon",
			metadataName: "provider;config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - empty string",
			metadataName: "",
			expectError:  true,
			errorPart:    "required",
		},
		{
			name:         "invalid metadata.name - only spaces",
			metadataName: "   ",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - starts with hyphen",
			metadataName: "-provider",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - starts with dot",
			metadataName: ".provider",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - starts with underscore",
			metadataName: "_provider",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - ends with hyphen",
			metadataName: "provider-",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "invalid metadata.name - ends with dot",
			metadataName: "provider.",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "valid metadata.name - consecutive hyphens",
			metadataName: "provider--config",
			expectError:  false,
		},
		{
			name:         "invalid metadata.name - consecutive dots",
			metadataName: "provider..config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "valid metadata.name - letters only",
			metadataName: "provider",
			expectError:  false,
		},
		{
			name:         "valid metadata.name - with hyphen",
			metadataName: "my-provider",
			expectError:  false,
		},
		{
			name:         "invalid metadata.name - with underscore",
			metadataName: "my_provider",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "valid metadata.name - with dot",
			metadataName: "my.provider",
			expectError:  false,
		},
		{
			name:         "valid metadata.name - with numbers",
			metadataName: "provider-v1",
			expectError:  false,
		},
		{
			name:         "invalid metadata.name - complex",
			metadataName: "my-llm_provider.v1",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "valid metadata.name - all alphanumeric",
			metadataName: "provider123",
			expectError:  false,
		},
		{
			name:         "invalid metadata.name - mixed case",
			metadataName: "MyProvider-Config",
			expectError:  true,
			errorPart:    "metadata.name must consist of",
		},
		{
			name:         "valid metadata.name - starts with number",
			metadataName: "1provider",
			expectError:  false,
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: tt.metadataName},
				Spec: api.LLMProviderConfigData{
					DisplayName: "test-display-name",
					Version:     "v1.0",
					Template:    "openai",
					Upstream: api.LLMProviderConfigData_Upstream{
						Url: stringPtr("https://api.example.com"),
					},
					AccessControl: api.LLMAccessControl{
						Mode: api.AllowAll,
					},
				},
			}

			errors := validator.Validate(&provider)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == "metadata.name" && strings.Contains(err.Message, tt.errorPart) {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for metadata.name with message containing %s", tt.errorPart)
			} else {
				assert.Empty(t, errors, "Should not have validation errors for valid metadata.name")
			}
		})
	}
}

// TestValidateLLMProvider_Version tests provider version field validation
func TestValidateLLMProvider_ProviderVersion(t *testing.T) {
	tests := []struct {
		name            string
		providerVersion string
		expectError     bool
	}{
		{
			name:            "empty version",
			providerVersion: "",
			expectError:     false,
		},
		{
			name:            "valid version - v1.0",
			providerVersion: "v1.0",
			expectError:     false,
		},
		{
			name:            "valid version - v1.0.0",
			providerVersion: "v1.0.0",
			expectError:     false,
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderConfigData{
					DisplayName: "test",
					Version:     tt.providerVersion,
					Template:    "openai",
					Upstream: api.LLMProviderConfigData_Upstream{
						Url: stringPtr("https://api.example.com"),
					},
					AccessControl: api.LLMAccessControl{
						Mode: api.AllowAll,
					},
				},
			}

			errors := validator.Validate(&provider)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == "spec.version" {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for spec.version field")
			} else {
				assert.Empty(t, errors, "Should not have validation errors")
			}
		})
	}
}

// TestValidateLLMProvider_Template tests template reference validation
func TestValidateLLMProvider_Template(t *testing.T) {
	tests := []struct {
		name        string
		template    string
		expectError bool
	}{
		{
			name:        "empty template",
			template:    "",
			expectError: true,
		},
		{
			name:        "valid template",
			template:    "openai",
			expectError: false,
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderConfigData{
					DisplayName: "test",
					Version:     "v1.0",
					Template:    tt.template,
					Upstream: api.LLMProviderConfigData_Upstream{
						Url: stringPtr("https://api.example.com"),
					},
					AccessControl: api.LLMAccessControl{
						Mode: api.AllowAll,
					},
				},
			}

			errors := validator.Validate(&provider)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == "spec.template" {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for spec.template field")
			} else {
				assert.Empty(t, errors, "Should not have validation errors")
			}
		})
	}
}

// TestValidateLLMProvider_Upstream tests upstream validation
func TestValidateLLMProvider_Upstream(t *testing.T) {
	tests := []struct {
		name        string
		upstream    api.LLMProviderConfigData_Upstream
		expectError bool
		errorField  string
		errorPart   string
	}{
		{
			name: "missing URL",
			upstream: api.LLMProviderConfigData_Upstream{
				Url: nil,
			},
			expectError: true,
			errorField:  "spec.upstream",
			errorPart:   "either 'url' or 'ref'",
		},
		{
			name: "empty URL",
			upstream: api.LLMProviderConfigData_Upstream{
				Url: stringPtr(""),
			},
			expectError: true,
			errorField:  "spec.upstream",
		},
		{
			name: "invalid URL - no protocol",
			upstream: api.LLMProviderConfigData_Upstream{
				Url: stringPtr("api.example.com"),
			},
			expectError: true,
			errorField:  "spec.upstream.url",
			errorPart:   "http",
		},
		{
			name: "invalid URL - ftp protocol",
			upstream: api.LLMProviderConfigData_Upstream{
				Url: stringPtr("ftp://api.example.com"),
			},
			expectError: true,
			errorField:  "spec.upstream.url",
			errorPart:   "http",
		},
		{
			name: "valid URL - http",
			upstream: api.LLMProviderConfigData_Upstream{
				Url: stringPtr("http://api.example.com"),
			},
			expectError: false,
		},
		{
			name: "valid URL - https",
			upstream: api.LLMProviderConfigData_Upstream{
				Url: stringPtr("https://api.example.com"),
			},
			expectError: false,
		},
		{
			name: "valid URL - https with port",
			upstream: api.LLMProviderConfigData_Upstream{
				Url: stringPtr("https://api.example.com:8443"),
			},
			expectError: false,
		},
		{
			name: "valid URL - https with path",
			upstream: api.LLMProviderConfigData_Upstream{
				Url: stringPtr("https://api.example.com/v1"),
			},
			expectError: false,
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderConfigData{
					DisplayName: "test",
					Version:     "v1.0",
					Template:    "openai",
					Upstream:    tt.upstream,
					AccessControl: api.LLMAccessControl{
						Mode: api.AllowAll,
					},
				},
			}

			errors := validator.Validate(&provider)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == tt.errorField && (tt.errorPart == "" || strings.Contains(err.Message, tt.errorPart)) {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for field %s with message containing %s", tt.errorField, tt.errorPart)
			} else {
				assert.Empty(t, errors, "Should not have validation errors")
			}
		})
	}
}

// TestValidateLLMProvider_UpstreamAuth tests upstream authentication validation
func TestValidateLLMProvider_UpstreamAuth(t *testing.T) {
	tests := []struct {
		name string
		auth *struct {
			Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
			PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
			PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
			PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
			Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
			Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
		}
		expectError bool
		errorField  string
		errorPart   string
	}{
		{
			name: "missing auth type",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:   "",
				Header: stringPtr("Authorization"),
				Value:  stringPtr("Bearer sk-test"),
			},
			expectError: true,
			errorField:  "spec.upstream.auth.type",
			errorPart:   "required",
		},
		{
			name: "invalid auth type",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:   "bearer",
				Header: stringPtr("Authorization"),
				Value:  stringPtr("Bearer sk-test"),
			},
			expectError: true,
			errorField:  "spec.upstream.auth.type",
			errorPart:   "api-key",
		},
		{
			name: "api-key without header",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:  api.LLMProviderConfigDataUpstreamAuthTypeApiKey,
				Value: stringPtr("sk-test"),
			},
			expectError: true,
			errorField:  "spec.upstream.auth.header",
			errorPart:   "required",
		},
		{
			name: "api-key with empty header",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:   api.LLMProviderConfigDataUpstreamAuthTypeApiKey,
				Header: stringPtr(""),
				Value:  stringPtr("sk-test"),
			},
			expectError: true,
			errorField:  "spec.upstream.auth.header",
			errorPart:   "required",
		},
		{
			name: "api-key without value",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:   api.LLMProviderConfigDataUpstreamAuthTypeApiKey,
				Header: stringPtr("Authorization"),
			},
			expectError: true,
			errorField:  "spec.upstream.auth.value",
			errorPart:   "required",
		},
		{
			name: "api-key with empty value",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:   api.LLMProviderConfigDataUpstreamAuthTypeApiKey,
				Header: stringPtr("Authorization"),
				Value:  stringPtr(""),
			},
			expectError: true,
			errorField:  "spec.upstream.auth.value",
			errorPart:   "required",
		},
		{
			name: "valid api-key auth",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:   api.LLMProviderConfigDataUpstreamAuthTypeApiKey,
				Header: stringPtr("Authorization"),
				Value:  stringPtr("Bearer sk-test"),
			},
			expectError: false,
		},
		{
			name: "oauth2 without policyParams",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type: api.LLMProviderConfigDataUpstreamAuthTypeOauth2,
			},
			expectError: true,
			errorField:  "spec.upstream.auth.policyParams",
			errorPart:   "required",
		},
		{
			name: "valid oauth2 auth via policyParams",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type: api.LLMProviderConfigDataUpstreamAuthTypeOauth2,
				PolicyParams: &map[string]interface{}{
					"tokenEndpoint": "https://idp.example.com/oauth2/token",
					"clientId":      "client-id",
					"clientSecret":  "client-secret",
				},
			},
			expectError: false,
		},
		{
			name: "oauth2 with policyName override and policyVersion",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:          api.LLMProviderConfigDataUpstreamAuthTypeOauth2,
				PolicyName:    stringPtr("my-oauth2-fork"),
				PolicyVersion: stringPtr("v2"),
				PolicyParams: &map[string]interface{}{
					"bearerToken": "static-token",
				},
			},
			expectError: false,
		},
		{
			name: "valid none auth without header or value",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type: api.LLMProviderConfigDataUpstreamAuthTypeNone,
			},
			expectError: false,
		},
		{
			name: "invalid policyVersion format",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:          api.LLMProviderConfigDataUpstreamAuthTypeOauth2,
				PolicyVersion: stringPtr("v1.0.0"),
				PolicyParams: &map[string]interface{}{
					"bearerToken": "static-token",
				},
			},
			expectError: true,
			errorField:  "spec.upstream.auth.policyVersion",
			errorPart:   "major-only",
		},
		{
			name: "api-key with both header/value and policyParams",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:   api.LLMProviderConfigDataUpstreamAuthTypeApiKey,
				Header: stringPtr("Authorization"),
				Value:  stringPtr("Bearer sk-test"),
				PolicyParams: &map[string]interface{}{
					"request": map[string]interface{}{"headers": []interface{}{}},
				},
			},
			expectError: true,
			errorField:  "spec.upstream.auth.policyParams",
			errorPart:   "cannot be combined",
		},
		{
			name: "other without policyName",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:         api.LLMProviderConfigDataUpstreamAuthTypeOther,
				PolicyParams: &map[string]interface{}{"foo": "bar"},
			},
			expectError: true,
			errorField:  "spec.upstream.auth.policyName",
			errorPart:   "required",
		},
		{
			name: "other without policyParams",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:       api.LLMProviderConfigDataUpstreamAuthTypeOther,
				PolicyName: stringPtr("my-custom-auth-policy"),
			},
			expectError: true,
			errorField:  "spec.upstream.auth.policyParams",
			errorPart:   "required",
		},
		{
			name: "valid other auth",
			auth: &struct {
				Header        *string                                   `json:"header,omitempty" yaml:"header,omitempty"`
				PolicyName    *string                                   `json:"policyName,omitempty" yaml:"policyName,omitempty"`
				PolicyParams  *map[string]interface{}                   `json:"policyParams,omitempty" yaml:"policyParams,omitempty"`
				PolicyVersion *string                                   `json:"policyVersion,omitempty" yaml:"policyVersion,omitempty"`
				Type          api.LLMProviderConfigDataUpstreamAuthType `json:"type" yaml:"type"`
				Value         *string                                   `json:"value,omitempty" yaml:"value,omitempty"`
			}{
				Type:         api.LLMProviderConfigDataUpstreamAuthTypeOther,
				PolicyName:   stringPtr("my-custom-auth-policy"),
				PolicyParams: &map[string]interface{}{"foo": "bar"},
			},
			expectError: false,
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderConfigData{
					DisplayName: "test",
					Version:     "v1.0",
					Template:    "openai",
					Upstream: api.LLMProviderConfigData_Upstream{
						Url:  stringPtr("https://api.example.com"),
						Auth: tt.auth,
					},
					AccessControl: api.LLMAccessControl{
						Mode: api.AllowAll,
					},
				},
			}

			errors := validator.Validate(&provider)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == tt.errorField && (tt.errorPart == "" || strings.Contains(err.Message, tt.errorPart)) {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for field %s", tt.errorField)
			} else {
				assert.Empty(t, errors, "Should not have validation errors")
			}
		})
	}
}

// TestValidateLLMProvider_AccessControl tests access control validation
func TestValidateLLMProvider_AccessControl(t *testing.T) {
	tests := []struct {
		name          string
		accessControl api.LLMAccessControl
		expectError   bool
		errorField    string
		errorPart     string
	}{
		{
			name: "invalid mode",
			accessControl: api.LLMAccessControl{
				Mode: "allow_some",
			},
			expectError: true,
			errorField:  "spec.accessControl.mode",
			errorPart:   "allow_all' or 'deny_all",
		},
		{
			name: "empty mode",
			accessControl: api.LLMAccessControl{
				Mode: "",
			},
			expectError: true,
			errorField:  "spec.accessControl.mode",
		},
		{
			name: "valid allow_all",
			accessControl: api.LLMAccessControl{
				Mode: api.AllowAll,
			},
			expectError: false,
		},
		{
			name: "valid deny_all",
			accessControl: api.LLMAccessControl{
				Mode: api.DenyAll,
			},
			expectError: false,
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderConfigData{
					DisplayName: "test",
					Version:     "v1.0",
					Template:    "openai",
					Upstream: api.LLMProviderConfigData_Upstream{
						Url: stringPtr("https://api.example.com"),
					},
					AccessControl: tt.accessControl,
				},
			}

			errors := validator.Validate(&provider)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == tt.errorField && (tt.errorPart == "" || strings.Contains(err.Message, tt.errorPart)) {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for field %s", tt.errorField)
			} else {
				assert.Empty(t, errors, "Should not have validation errors")
			}
		})
	}
}

// TestValidateLLMProvider_AccessControlExceptions tests exception validation
func TestValidateLLMProvider_AccessControlExceptions(t *testing.T) {
	tests := []struct {
		name        string
		exceptions  []api.RouteException
		expectError bool
		errorField  string
		errorPart   string
	}{
		{
			name: "exception with empty path",
			exceptions: []api.RouteException{
				{
					Path:    "",
					Methods: []api.RouteExceptionMethods{api.RouteExceptionMethodsGET},
				},
			},
			expectError: true,
			errorField:  "spec.accessControl.exceptions[0].path",
			errorPart:   "required",
		},
		{
			name: "exception with empty methods",
			exceptions: []api.RouteException{
				{
					Path:    "/admin",
					Methods: []api.RouteExceptionMethods{},
				},
			},
			expectError: true,
			errorField:  "spec.accessControl.exceptions[0].methods",
			errorPart:   "At least one method",
		},
		{
			name: "valid single exception",
			exceptions: []api.RouteException{
				{
					Path:    "/admin",
					Methods: []api.RouteExceptionMethods{api.RouteExceptionMethodsGET, api.RouteExceptionMethodsPOST},
				},
			},
			expectError: false,
		},
		{
			name: "valid multiple exceptions",
			exceptions: []api.RouteException{
				{
					Path:    "/admin",
					Methods: []api.RouteExceptionMethods{api.RouteExceptionMethodsGET},
				},
				{
					Path:    "/internal/metrics",
					Methods: []api.RouteExceptionMethods{api.RouteExceptionMethodsGET, api.RouteExceptionMethodsPOST, api.RouteExceptionMethodsDELETE},
				},
			},
			expectError: false,
		},
		{
			name: "second exception invalid",
			exceptions: []api.RouteException{
				{
					Path:    "/admin",
					Methods: []api.RouteExceptionMethods{api.RouteExceptionMethodsGET},
				},
				{
					Path:    "",
					Methods: []api.RouteExceptionMethods{api.RouteExceptionMethodsPOST},
				},
			},
			expectError: true,
			errorField:  "spec.accessControl.exceptions[1].path",
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderConfigData{
					DisplayName: "test",
					Version:     "v1.0",
					Template:    "openai",
					Upstream: api.LLMProviderConfigData_Upstream{
						Url: stringPtr("https://api.example.com"),
					},
					AccessControl: api.LLMAccessControl{
						Mode:       api.AllowAll,
						Exceptions: &tt.exceptions,
					},
				},
			}

			errors := validator.Validate(&provider)

			if tt.expectError {
				require.NotEmpty(t, errors, "Should have validation errors")
				found := false
				for _, err := range errors {
					if err.Field == tt.errorField && (tt.errorPart == "" || strings.Contains(err.Message, tt.errorPart)) {
						found = true
						break
					}
				}
				assert.True(t, found, "Should find error for field %s", tt.errorField)
			} else {
				assert.Empty(t, errors, "Should not have validation errors")
			}
		})
	}
}

// ============================================================================
// Edge Cases and Security Tests
// ============================================================================

// TestValidateLLMProvider_NilSpec tests validation with nil spec
func TestValidateLLMProvider_NilSpec(t *testing.T) {
	validator := NewLLMValidator()

	provider := api.LLMProviderConfiguration{
		ApiVersion: "gateway.api-platform.wso2.com/v1",
		Kind:       api.LLMProviderConfigurationKindLlmProvider,
		// Spec is nil/zero value
	}

	errors := validator.Validate(&provider)
	// Should have validation errors for missing required fields
	assert.NotEmpty(t, errors, "Should have validation errors for nil spec")
}

// TestValidateLLMProvider_ExtremelyLongInputs tests handling of very long inputs
func TestValidateLLMProvider_ExtremelyLongInputs(t *testing.T) {
	validator := NewLLMValidator()

	// Create extremely long name (>1000 characters)
	longName := ""
	for i := 0; i < 1000; i++ {
		longName += "a"
	}

	provider := api.LLMProviderConfiguration{
		ApiVersion: "gateway.api-platform.wso2.com/v1",
		Kind:       api.LLMProviderConfigurationKindLlmProvider,
		Metadata:   api.Metadata{Name: "openai"},
		Spec: api.LLMProviderConfigData{
			DisplayName: longName,
			Version:     "v1.0",
			Template:    "openai",
			Upstream: api.LLMProviderConfigData_Upstream{
				Url: stringPtr("https://api.example.com"),
			},
			AccessControl: api.LLMAccessControl{
				Mode: api.AllowAll,
			},
		},
	}

	errors := validator.Validate(&provider)
	// Validation should complete without crashing
	// May or may not have errors depending on length limits
	assert.NotNil(t, errors, "Validator should handle extremely long inputs")
}

// TestValidateLLMProvider_URLValidation tests various URL formats
func TestValidateLLMProvider_URLValidation(t *testing.T) {
	tests := []struct {
		name        string
		url         string
		expectError bool
	}{
		// Valid URLs
		{
			name:        "valid https with subdomain",
			url:         "https://api.openai.com",
			expectError: false,
		},
		{
			name:        "valid https with port",
			url:         "https://api.example.com:8443",
			expectError: false,
		},
		{
			name:        "valid https with path",
			url:         "https://api.example.com/v1/llm",
			expectError: false,
		},
		{
			name:        "valid http localhost",
			url:         "http://localhost:8080",
			expectError: false,
		},
		{
			name:        "valid IP address",
			url:         "https://192.168.1.1:8080",
			expectError: false,
		},
		// Invalid URLs
		{
			name:        "javascript protocol",
			url:         "javascript:alert('XSS')",
			expectError: true,
		},
		{
			name:        "file protocol",
			url:         "file:///etc/passwd",
			expectError: true,
		},
		{
			name:        "data URI",
			url:         "data:text/html,<script>alert('XSS')</script>",
			expectError: true,
		},
		{
			name:        "ftp protocol",
			url:         "ftp://example.com",
			expectError: true,
		},
		{
			name:        "no protocol",
			url:         "example.com",
			expectError: true,
		},
		{
			name:        "malformed URL",
			url:         "https://",
			expectError: true,
		},
	}

	validator := NewLLMValidator()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := api.LLMProviderConfiguration{
				ApiVersion: "gateway.api-platform.wso2.com/v1",
				Kind:       api.LLMProviderConfigurationKindLlmProvider,
				Metadata:   api.Metadata{Name: "openai"},
				Spec: api.LLMProviderConfigData{
					DisplayName: "test",
					Version:     "v1.0",
					Template:    "openai",
					Upstream: api.LLMProviderConfigData_Upstream{
						Url: stringPtr(tt.url),
					},
					AccessControl: api.LLMAccessControl{
						Mode: api.AllowAll,
					},
				},
			}

			errors := validator.Validate(&provider)

			if tt.expectError {
				assert.NotEmpty(t, errors, "URL should fail validation: %s", tt.url)
			} else {
				assert.Empty(t, errors, "URL should pass validation: %s", tt.url)
			}
		})
	}
}

// TestValidate_UnsupportedConfigType tests validation with unsupported config type
func TestValidate_UnsupportedConfigType(t *testing.T) {
	validator := NewLLMValidator()

	type UnsupportedConfig struct {
		Name string
	}

	unsupported := &UnsupportedConfig{Name: "test"}
	errors := validator.Validate(unsupported)

	require.NotEmpty(t, errors, "Should have validation errors for unsupported config type")
	assert.Equal(t, "config", errors[0].Field)
	assert.Contains(t, errors[0].Message, "Unsupported configuration type")
}

// assertHasFieldError fails unless errs contains a validation error on the given field. Shared by
// the resilience and upstream-ref tests across the LLM and MCP validators (same package).
func assertHasFieldError(t *testing.T, errs []ValidationError, field string) {
	t.Helper()
	for _, e := range errs {
		if e.Field == field {
			return
		}
	}
	t.Fatalf("expected a validation error on field %q, got %+v", field, errs)
}

// upstreamDef builds a valid upstream definition (one host-only target) with an optional connect
// timeout. connect == "" leaves the timeout unset. Shared by the LLM and MCP upstream-ref tests.
func upstreamDef(name, connect string) api.UpstreamDefinition {
	def := api.UpstreamDefinition{
		Name: name,
		Upstreams: []struct {
			Url    string `json:"url" yaml:"url"`
			Weight *int   `json:"weight,omitempty" yaml:"weight,omitempty"`
		}{
			{Url: "http://backend:8080"},
		},
	}
	if connect != "" {
		def.Timeout = &api.UpstreamTimeout{Connect: stringPtr(connect)}
	}
	return def
}

// ============================================================================
// Resilience validation
// ============================================================================

func validProviderWithResilience(r *api.Resilience) api.LLMProviderConfiguration {
	return api.LLMProviderConfiguration{
		ApiVersion: "gateway.api-platform.wso2.com/v1",
		Kind:       api.LLMProviderConfigurationKindLlmProvider,
		Metadata:   api.Metadata{Name: "openai"},
		Spec: api.LLMProviderConfigData{
			DisplayName:   "my-provider",
			Version:       "v1.0",
			Template:      "openai",
			Upstream:      api.LLMProviderConfigData_Upstream{Url: stringPtr("https://api.openai.com")},
			AccessControl: api.LLMAccessControl{Mode: api.AllowAll},
			Resilience:    r,
		},
	}
}

func validProxyWithResilience(r *api.Resilience) api.LLMProxyConfiguration {
	return api.LLMProxyConfiguration{
		ApiVersion: api.LLMProxyConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProxyConfigurationKindLlmProxy,
		Metadata:   api.Metadata{Name: "openai-proxy"},
		Spec: api.LLMProxyConfigData{
			DisplayName: "my-proxy",
			Version:     "v1.0",
			Provider:    api.LLMProxyProvider{Id: "openai"},
			Resilience:  r,
		},
	}
}

// The reserved gateway health-check namespace (constants.GatewayHealthPathPrefix)
// must be checked against an LLMProvider's spec.context — unlike RestAPI, an
// LLMProvider has no per-operation path list to combine it with.
func TestValidateLLMProvider_ReservedHealthPath(t *testing.T) {
	validator := NewLLMValidator()

	newProvider := func(context *string) api.LLMProviderConfiguration {
		return api.LLMProviderConfiguration{
			ApiVersion: "gateway.api-platform.wso2.com/v1",
			Kind:       api.LLMProviderConfigurationKindLlmProvider,
			Metadata:   api.Metadata{Name: "openai"},
			Spec: api.LLMProviderConfigData{
				DisplayName:   "my-provider",
				Version:       "v1.0",
				Template:      "openai",
				Context:       context,
				Upstream:      api.LLMProviderConfigData_Upstream{Url: stringPtr("https://api.openai.com")},
				AccessControl: api.LLMAccessControl{Mode: api.AllowAll},
			},
		}
	}

	t.Run("context under the reserved namespace is rejected", func(t *testing.T) {
		provider := newProvider(stringPtr(constants.GatewayHealthPathPrefix + "/ready"))
		errs := validator.Validate(&provider)
		assertHasFieldError(t, errs, "spec.context")
	})

	t.Run("context equal to the reserved prefix itself is rejected", func(t *testing.T) {
		provider := newProvider(stringPtr(constants.GatewayHealthPathPrefix))
		errs := validator.Validate(&provider)
		assertHasFieldError(t, errs, "spec.context")
	})

	t.Run("unrelated context is unaffected", func(t *testing.T) {
		provider := newProvider(stringPtr("/openai"))
		errs := validator.Validate(&provider)
		assert.Empty(t, errs)
	})

	t.Run("nil context is unaffected", func(t *testing.T) {
		provider := newProvider(nil)
		errs := validator.Validate(&provider)
		assert.Empty(t, errs)
	})

	t.Run("dot-segment context resolving into the reserved namespace is rejected", func(t *testing.T) {
		provider := newProvider(stringPtr("/foo/.." + constants.GatewayHealthPathPrefix + "/ready"))
		errs := validator.Validate(&provider)
		assertHasFieldError(t, errs, "spec.context")
	})

	t.Run("dot-segment context resolving to an unrelated namespace is unaffected", func(t *testing.T) {
		provider := newProvider(stringPtr("/foo/../bar"))
		errs := validator.Validate(&provider)
		assert.Empty(t, errs)
	})
}

// Same reservation, checked against an LLMProxy's spec.context.
func TestValidateLLMProxy_ReservedHealthPath(t *testing.T) {
	validator := NewLLMValidator()

	newProxy := func(context *string) api.LLMProxyConfiguration {
		return api.LLMProxyConfiguration{
			ApiVersion: api.LLMProxyConfigurationApiVersionGatewayApiPlatformWso2Comv1,
			Kind:       api.LLMProxyConfigurationKindLlmProxy,
			Metadata:   api.Metadata{Name: "openai-proxy"},
			Spec: api.LLMProxyConfigData{
				DisplayName: "my-proxy",
				Version:     "v1.0",
				Context:     context,
				Provider:    api.LLMProxyProvider{Id: "openai"},
			},
		}
	}

	t.Run("context under the reserved namespace is rejected", func(t *testing.T) {
		proxy := newProxy(stringPtr(constants.GatewayHealthPathPrefix + "/healthy"))
		errs := validator.Validate(&proxy)
		assertHasFieldError(t, errs, "spec.context")
	})

	t.Run("unrelated context is unaffected", func(t *testing.T) {
		proxy := newProxy(stringPtr("/openai-proxy"))
		errs := validator.Validate(&proxy)
		assert.Empty(t, errs)
	})

	t.Run("nil context is unaffected", func(t *testing.T) {
		proxy := newProxy(nil)
		errs := validator.Validate(&proxy)
		assert.Empty(t, errs)
	})

	t.Run("dot-segment context resolving into the reserved namespace is rejected", func(t *testing.T) {
		proxy := newProxy(stringPtr("/foo/.." + constants.GatewayHealthPathPrefix + "/healthy"))
		errs := validator.Validate(&proxy)
		assertHasFieldError(t, errs, "spec.context")
	})
}

func TestValidateLLMProvider_Resilience(t *testing.T) {
	validator := NewLLMValidator()

	t.Run("valid timeout and idleTimeout", func(t *testing.T) {
		errs := validator.Validate(validProviderWithResilience(&api.Resilience{
			Timeout:     stringPtr("30s"),
			IdleTimeout: stringPtr("0s"),
		}))
		assert.Empty(t, errs)
	})

	t.Run("nil resilience is fine", func(t *testing.T) {
		errs := validator.Validate(validProviderWithResilience(nil))
		assert.Empty(t, errs)
	})

	t.Run("malformed timeout is rejected", func(t *testing.T) {
		errs := validator.Validate(validProviderWithResilience(&api.Resilience{Timeout: stringPtr("30")}))
		assertHasFieldError(t, errs, "spec.resilience.timeout")
	})

	t.Run("compound timeout is rejected (must match CRD pattern)", func(t *testing.T) {
		errs := validator.Validate(validProviderWithResilience(&api.Resilience{Timeout: stringPtr("1h30m")}))
		assertHasFieldError(t, errs, "spec.resilience.timeout")
	})

	t.Run("0s is accepted (disables)", func(t *testing.T) {
		errs := validator.Validate(validProviderWithResilience(&api.Resilience{Timeout: stringPtr("0s")}))
		assert.Empty(t, errs)
	})

	t.Run("negative timeout is rejected", func(t *testing.T) {
		errs := validator.Validate(validProviderWithResilience(&api.Resilience{Timeout: stringPtr("-5s")}))
		assertHasFieldError(t, errs, "spec.resilience.timeout")
	})

	t.Run("malformed idleTimeout is rejected", func(t *testing.T) {
		errs := validator.Validate(validProviderWithResilience(&api.Resilience{IdleTimeout: stringPtr("abc")}))
		assertHasFieldError(t, errs, "spec.resilience.idleTimeout")
	})

	t.Run("retry with empty statusCodes is rejected", func(t *testing.T) {
		errs := validator.Validate(validProviderWithResilience(&api.Resilience{
			Retry: &api.Retry{StatusCodes: []int{}},
		}))
		assertHasFieldError(t, errs, "spec.resilience.retry.statusCodes")
	})

	t.Run("retry with valid statusCodes and numRetries is accepted", func(t *testing.T) {
		numRetries := 2
		errs := validator.Validate(validProviderWithResilience(&api.Resilience{
			Retry: &api.Retry{StatusCodes: []int{401, 503}, NumRetries: &numRetries},
		}))
		assert.Empty(t, errs)
	})
}

func TestValidateLLMProxy_Resilience(t *testing.T) {
	validator := NewLLMValidator()

	t.Run("valid timeout", func(t *testing.T) {
		errs := validator.Validate(validProxyWithResilience(&api.Resilience{Timeout: stringPtr("75s")}))
		assert.Empty(t, errs)
	})

	t.Run("nil resilience is fine", func(t *testing.T) {
		errs := validator.Validate(validProxyWithResilience(nil))
		assert.Empty(t, errs)
	})

	t.Run("malformed timeout is rejected", func(t *testing.T) {
		errs := validator.Validate(validProxyWithResilience(&api.Resilience{Timeout: stringPtr("fast")}))
		assertHasFieldError(t, errs, "spec.resilience.timeout")
	})

	t.Run("negative idleTimeout is rejected", func(t *testing.T) {
		errs := validator.Validate(validProxyWithResilience(&api.Resilience{IdleTimeout: stringPtr("-1s")}))
		assertHasFieldError(t, errs, "spec.resilience.idleTimeout")
	})

	t.Run("retry with empty statusCodes is rejected", func(t *testing.T) {
		errs := validator.Validate(validProxyWithResilience(&api.Resilience{
			Retry: &api.Retry{StatusCodes: []int{}},
		}))
		assertHasFieldError(t, errs, "spec.resilience.retry.statusCodes")
	})

	t.Run("retry with valid statusCodes and numRetries is accepted", func(t *testing.T) {
		numRetries := 2
		errs := validator.Validate(validProxyWithResilience(&api.Resilience{
			Retry: &api.Retry{StatusCodes: []int{401, 503}, NumRetries: &numRetries},
		}))
		assert.Empty(t, errs)
	})
}

// validProxyWithAuth builds an LlmProxy whose primary provider.auth is set - the
// shared entry point for both validateLLMUpstreamAuth call sites is exercised
// separately below via additionalProviders.
func validProxyWithAuth(auth *api.LLMUpstreamAuth) api.LLMProxyConfiguration {
	return api.LLMProxyConfiguration{
		ApiVersion: api.LLMProxyConfigurationApiVersionGatewayApiPlatformWso2Comv1,
		Kind:       api.LLMProxyConfigurationKindLlmProxy,
		Metadata:   api.Metadata{Name: "openai-proxy"},
		Spec: api.LLMProxyConfigData{
			DisplayName: "my-proxy",
			Version:     "v1.0",
			Provider:    api.LLMProxyProvider{Id: "openai", Auth: auth},
		},
	}
}

// TestValidateLLMProxy_ProviderAuth covers validateLLMUpstreamAuth, the
// LlmProxy-side counterpart to validateUpstreamWithAuth's LlmProvider
// upstream.auth handling - previously exercised by no test at all (0%
// coverage), even though it shares validateUpstreamAuthFields with the
// LlmProvider/Mcp paths that were already covered.
func TestValidateLLMProxy_ProviderAuth(t *testing.T) {
	validator := NewLLMValidator()

	t.Run("oauth2 with policyParams is valid", func(t *testing.T) {
		errs := validator.Validate(validProxyWithAuth(&api.LLMUpstreamAuth{
			Type:         "oauth2",
			PolicyParams: &map[string]interface{}{"tokenEndpoint": "https://idp.example.com/token"},
		}))
		assert.Empty(t, errs)
	})

	t.Run("oauth2 without policyParams is rejected", func(t *testing.T) {
		errs := validator.Validate(validProxyWithAuth(&api.LLMUpstreamAuth{Type: "oauth2"}))
		assertHasFieldError(t, errs, "spec.provider.auth.policyParams")
	})

	t.Run("other without policyName is rejected", func(t *testing.T) {
		errs := validator.Validate(validProxyWithAuth(&api.LLMUpstreamAuth{
			Type:         "other",
			PolicyParams: &map[string]interface{}{"foo": "bar"},
		}))
		assertHasFieldError(t, errs, "spec.provider.auth.policyName")
	})

	t.Run("nil auth is fine", func(t *testing.T) {
		errs := validator.Validate(validProxyWithAuth(nil))
		assert.Empty(t, errs)
	})
}

// TestValidateLLMProxy_AdditionalProviderAuth covers the second
// validateLLMUpstreamAuth call site (spec.additionalProviders[].auth),
// distinct from the primary provider's - also previously untested.
func TestValidateLLMProxy_AdditionalProviderAuth(t *testing.T) {
	validator := NewLLMValidator()

	proxyWithAdditional := func(auth *api.LLMUpstreamAuth) api.LLMProxyConfiguration {
		p := validProxyWithAuth(nil)
		p.Spec.AdditionalProviders = &[]api.LLMProxyAdditionalProvider{
			{Id: "anthropic", Auth: auth},
		}
		return p
	}

	t.Run("oauth2 with policyParams is valid", func(t *testing.T) {
		errs := validator.Validate(proxyWithAdditional(&api.LLMUpstreamAuth{
			Type:         "oauth2",
			PolicyParams: &map[string]interface{}{"tokenEndpoint": "https://idp.example.com/token"},
		}))
		assert.Empty(t, errs)
	})

	t.Run("oauth2 without policyParams is rejected on the additional provider's own field path", func(t *testing.T) {
		errs := validator.Validate(proxyWithAdditional(&api.LLMUpstreamAuth{Type: "oauth2"}))
		assertHasFieldError(t, errs, "spec.additionalProviders[0].auth.policyParams")
	})
}

// ============================================================================
// Upstream ref validation
// ============================================================================

func providerWithUpstream(defs *[]api.UpstreamDefinition, up api.LLMProviderConfigData_Upstream) api.LLMProviderConfiguration {
	return api.LLMProviderConfiguration{
		ApiVersion: "gateway.api-platform.wso2.com/v1",
		Kind:       api.LLMProviderConfigurationKindLlmProvider,
		Metadata:   api.Metadata{Name: "openai"},
		Spec: api.LLMProviderConfigData{
			DisplayName:         "my-provider",
			Version:             "v1.0",
			Template:            "openai",
			UpstreamDefinitions: defs,
			Upstream:            up,
			AccessControl:       api.LLMAccessControl{Mode: api.AllowAll},
		},
	}
}

func TestValidateLLMProvider_UpstreamRef(t *testing.T) {
	validator := NewLLMValidator()

	t.Run("valid ref resolves to a definition", func(t *testing.T) {
		defs := &[]api.UpstreamDefinition{upstreamDef("openai-backend", "6s")}
		errs := validator.Validate(providerWithUpstream(defs, api.LLMProviderConfigData_Upstream{Ref: stringPtr("openai-backend")}))
		assert.Empty(t, errs)
	})

	t.Run("ref not found in definitions", func(t *testing.T) {
		defs := &[]api.UpstreamDefinition{upstreamDef("other", "6s")}
		errs := validator.Validate(providerWithUpstream(defs, api.LLMProviderConfigData_Upstream{Ref: stringPtr("missing")}))
		assertHasFieldError(t, errs, "spec.upstream.ref")
	})

	t.Run("both url and ref rejected", func(t *testing.T) {
		defs := &[]api.UpstreamDefinition{upstreamDef("openai-backend", "6s")}
		errs := validator.Validate(providerWithUpstream(defs, api.LLMProviderConfigData_Upstream{
			Url: stringPtr("https://api.openai.com"),
			Ref: stringPtr("openai-backend"),
		}))
		assertHasFieldError(t, errs, "spec.upstream")
	})

	t.Run("malformed connect timeout rejected (must match CRD pattern)", func(t *testing.T) {
		defs := &[]api.UpstreamDefinition{upstreamDef("openai-backend", "1h30m")}
		errs := validator.Validate(providerWithUpstream(defs, api.LLMProviderConfigData_Upstream{Ref: stringPtr("openai-backend")}))
		assertHasFieldError(t, errs, "spec.upstreamDefinitions[0].timeout.connect")
	})

	t.Run("valid fractional connect timeout accepted", func(t *testing.T) {
		defs := &[]api.UpstreamDefinition{upstreamDef("openai-backend", "500ms")}
		errs := validator.Validate(providerWithUpstream(defs, api.LLMProviderConfigData_Upstream{Ref: stringPtr("openai-backend")}))
		assert.Empty(t, errs)
	})

	t.Run("valid basePath accepted", func(t *testing.T) {
		def := upstreamDef("openai-backend", "6s")
		def.BasePath = stringPtr("/api/v2")
		errs := validator.Validate(providerWithUpstream(&[]api.UpstreamDefinition{def}, api.LLMProviderConfigData_Upstream{Ref: stringPtr("openai-backend")}))
		assert.Empty(t, errs)
	})

	t.Run("basePath without leading slash rejected", func(t *testing.T) {
		def := upstreamDef("openai-backend", "6s")
		def.BasePath = stringPtr("api/v2")
		errs := validator.Validate(providerWithUpstream(&[]api.UpstreamDefinition{def}, api.LLMProviderConfigData_Upstream{Ref: stringPtr("openai-backend")}))
		assertHasFieldError(t, errs, "spec.upstreamDefinitions[0].basePath")
	})

	t.Run("basePath with trailing slash rejected", func(t *testing.T) {
		def := upstreamDef("openai-backend", "6s")
		def.BasePath = stringPtr("/api/v2/")
		errs := validator.Validate(providerWithUpstream(&[]api.UpstreamDefinition{def}, api.LLMProviderConfigData_Upstream{Ref: stringPtr("openai-backend")}))
		assertHasFieldError(t, errs, "spec.upstreamDefinitions[0].basePath")
	})

	t.Run("connect timeout that overflows time.Duration rejected", func(t *testing.T) {
		defs := &[]api.UpstreamDefinition{upstreamDef("openai-backend", "99999999999999999999s")}
		errs := validator.Validate(providerWithUpstream(defs, api.LLMProviderConfigData_Upstream{Ref: stringPtr("openai-backend")}))
		assertHasFieldError(t, errs, "spec.upstreamDefinitions[0].timeout.connect")
	})

	t.Run("definition name with invalid characters rejected (CRD pattern)", func(t *testing.T) {
		defs := &[]api.UpstreamDefinition{upstreamDef("bad name!", "6s")}
		errs := validator.Validate(providerWithUpstream(defs, api.LLMProviderConfigData_Upstream{Ref: stringPtr("bad name!")}))
		assertHasFieldError(t, errs, "spec.upstreamDefinitions[0].name")
	})

	t.Run("definition name over 100 chars rejected", func(t *testing.T) {
		long := strings.Repeat("a", 101)
		defs := &[]api.UpstreamDefinition{upstreamDef(long, "6s")}
		errs := validator.Validate(providerWithUpstream(defs, api.LLMProviderConfigData_Upstream{Ref: stringPtr(long)}))
		assertHasFieldError(t, errs, "spec.upstreamDefinitions[0].name")
	})

	t.Run("url with surrounding whitespace is accepted", func(t *testing.T) {
		errs := validator.Validate(providerWithUpstream(nil, api.LLMProviderConfigData_Upstream{Url: stringPtr("  https://api.openai.com  ")}))
		assert.Empty(t, errs)
	})
}

func TestValidateModelFailoverPolicy_RejectsCoexistenceWithResilienceRetry(t *testing.T) {
	// model-failover declares retry-source ownership, so a route with it plus
	// resilience.retry has retrySourceCount=1 and a non-nil retry — the same
	// conflict ValidateAtMostOneRetrySourcePerRoute rejects generically now
	// (see TestValidateAtMostOneRetrySourcePerRoute_RejectsDeclarationPlusResilienceRetry).
	retry := &api.Retry{StatusCodes: []int{401}}
	if err := ValidateAtMostOneRetrySourcePerRoute(1, retry); err == nil {
		t.Error("expected an error when both model-failover and resilience.retry are configured on the same route")
	}
	if err := ValidateAtMostOneRetrySourcePerRoute(1, nil); err != nil {
		t.Errorf("expected no error when resilience.retry is absent, got: %v", err)
	}
}

// retrySourceTestResolver builds the generic resolver over a policy-definition registry in
// which "model-failover" declares x-wso2-retry-source — i.e. exactly what Task 11 ships in
// that policy's own policy-definition.yaml. Every case below therefore exercises the generic
// discovery path (metadata lookup, not a name match) while asserting the same verdicts the
// name-matching implementation used to reach.
func retrySourceTestResolver() RetrySourceResolver {
	defs := map[string]models.PolicyDefinition{
		"model-failover|v1.0.0": {
			Name:        "model-failover",
			Version:     "v1.0.0",
			RetrySource: &models.RetrySourceMetadata{GroupKeyField: "model"},
		},
		"some-other-policy|v1.0.0": {Name: "some-other-policy", Version: "v1.0.0"},
	}
	return NewRetrySourceResolver(defs, BuildLatestVersionIndex(defs))
}

// Regression test for a confirmed-live gap: registering an LlmProxy whose model-failover
// config used a basePath-carrying upstreamDefinition (an additionalProviders alias) as an
// aggregate-cluster member returned HTTP 201 — the only enforcement was the async xDS
// transform, which runs off the request thread and never surfaces its error to the caller.
// The route was then silently persisted and 500'd on every real invocation.
// ValidateModelFailoverForOperations is the fix: it must be called synchronously, before
// persisting, from every deploy path (LlmProvider, LlmProxy, plain RestAPI).
func TestValidateRetrySourcesForOperations_RejectsBasePathAggregateMember(t *testing.T) {
	basePath := "/some-alias-ctx"
	spec := &api.APIConfigData{
		UpstreamDefinitions: &[]api.UpstreamDefinition{
			{Name: "openai-alias", BasePath: &basePath},
			{Name: "anthropic-alias", BasePath: &basePath},
		},
		Operations: []api.Operation{
			{
				Policies: &[]api.Policy{
					{
						Name: "model-failover",
						Params: &map[string]interface{}{
							"targets": []interface{}{
								map[string]interface{}{
									"model":              "gpt-4o",
									"upstreamDefinition": "openai-alias",
									"fallbacks": []interface{}{
										map[string]interface{}{"model": "claude-3-haiku", "upstreamDefinition": "anthropic-alias"},
									},
								},
							},
							"statusCodes": []interface{}{float64(500)},
						},
					},
				},
			},
		},
	}
	err := ValidateRetrySourcesForOperations(spec, retrySourceTestResolver())
	if err == nil {
		t.Fatal("expected an error for a basePath-carrying upstreamDefinition used as an aggregate-cluster member, got nil")
	}
}

func TestValidateRetrySourcesForOperations_RejectsUndeclaredUpstreamReference(t *testing.T) {
	spec := &api.APIConfigData{
		UpstreamDefinitions: &[]api.UpstreamDefinition{{Name: "primary"}},
		Operations: []api.Operation{
			{
				Policies: &[]api.Policy{
					{
						Name: "model-failover",
						Params: &map[string]interface{}{
							"targets": []interface{}{
								map[string]interface{}{"model": "gpt-4o", "upstreamDefinition": "does-not-exist"},
							},
							"statusCodes": []interface{}{float64(500)},
						},
					},
				},
			},
		},
	}
	err := ValidateRetrySourcesForOperations(spec, retrySourceTestResolver())
	if err == nil {
		t.Fatal("expected an error for a target referencing an undeclared upstreamDefinition, got nil")
	}
}

func TestValidateRetrySourcesForOperations_ValidZeroFallbackConfigPasses(t *testing.T) {
	spec := &api.APIConfigData{
		UpstreamDefinitions: &[]api.UpstreamDefinition{{Name: "primary"}},
		Operations: []api.Operation{
			{
				Policies: &[]api.Policy{
					{
						Name: "model-failover",
						Params: &map[string]interface{}{
							"targets": []interface{}{
								map[string]interface{}{"model": "gpt-4o", "upstreamDefinition": "primary"},
							},
							"statusCodes": []interface{}{float64(500)},
						},
					},
				},
			},
		},
	}
	if err := ValidateRetrySourcesForOperations(spec, retrySourceTestResolver()); err != nil {
		t.Errorf("expected no error for a valid zero-fallback config, got: %v", err)
	}
}

func TestValidateRetrySourcesForOperations_NoModelFailoverPolicyIsNoOp(t *testing.T) {
	spec := &api.APIConfigData{
		Operations: []api.Operation{{Policies: &[]api.Policy{{Name: "some-other-policy"}}}},
	}
	if err := ValidateRetrySourcesForOperations(spec, retrySourceTestResolver()); err != nil {
		t.Errorf("expected no error when no operation has a model-failover policy, got: %v", err)
	}
	if err := ValidateRetrySourcesForOperations(nil, retrySourceTestResolver()); err != nil {
		t.Errorf("expected no error for a nil spec, got: %v", err)
	}
}

// The minimal, common-case config: no upstreamDefinitions declared at all, and every
// target/fallback omits upstreamDefinition - everything defaults to the API's own main
// upstream, exactly the backend used with no model-failover configured at all.
func TestValidateRetrySourcesForOperations_DefaultsToMainWhenUpstreamDefinitionOmitted(t *testing.T) {
	mainURL := "http://backend.example.com:9711"
	spec := &api.APIConfigData{
		Upstream: struct {
			Main    api.Upstream  `json:"main" yaml:"main"`
			Sandbox *api.Upstream `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
		}{Main: api.Upstream{Url: &mainURL}},
		Operations: []api.Operation{
			{
				Policies: &[]api.Policy{
					{
						Name: "model-failover",
						Params: &map[string]interface{}{
							"targets": []interface{}{
								map[string]interface{}{
									"model": "gpt-4o",
									"fallbacks": []interface{}{
										map[string]interface{}{"model": "gpt-4o-mini"},
									},
								},
							},
							"statusCodes": []interface{}{float64(500)},
						},
					},
				},
			},
		},
	}
	if err := ValidateRetrySourcesForOperations(spec, retrySourceTestResolver()); err != nil {
		t.Errorf("expected no error for the minimal config (no upstreamDefinitions, no explicit references), got: %v", err)
	}
}

// LlmProxy's own main upstream is ALWAYS loopback-routed with the provider's context baked
// into its URL path (see llm_transformer.go's transformProxy) - structurally identical to an
// additionalProviders alias's BasePath. A group that has fallbacks and defaults to that main
// upstream must be rejected the exact same way an explicit BasePath-carrying upstreamDefinition
// reference would be.
func TestValidateRetrySourcesForOperations_RejectsMainWithBasePathAsAggregateMember(t *testing.T) {
	loopbackURL := "http://127.0.0.1:8080/mf-proxy-primary-ctx"
	spec := &api.APIConfigData{
		Upstream: struct {
			Main    api.Upstream  `json:"main" yaml:"main"`
			Sandbox *api.Upstream `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
		}{Main: api.Upstream{Url: &loopbackURL}},
		UpstreamDefinitions: &[]api.UpstreamDefinition{{Name: "anthropic-alias", BasePath: strPtr("/mf-proxy-anthropic-ctx")}},
		Operations: []api.Operation{
			{
				Policies: &[]api.Policy{
					{
						Name: "model-failover",
						Params: &map[string]interface{}{
							"targets": []interface{}{
								map[string]interface{}{
									"model": "gpt-4o", // defaults to main - which has a non-trivial basePath here
									"fallbacks": []interface{}{
										map[string]interface{}{"model": "claude-3-haiku", "upstreamDefinition": "anthropic-alias"},
									},
								},
							},
							"statusCodes": []interface{}{float64(500)},
						},
					},
				},
			},
		},
	}
	err := ValidateRetrySourcesForOperations(spec, retrySourceTestResolver())
	if err == nil {
		t.Fatal("expected an error - main upstream has a non-trivial basePath and this target has fallbacks, so it would be used as an aggregate-cluster member")
	}
}

// A zero-fallback group defaulting to a basePath-carrying main upstream is safe (never routed
// through an aggregate cluster) - this is exactly what makes it the ONLY way for LlmProxy to
// use model-failover with its own primary provider, with no additionalProviders self-aliasing
// needed at all.
func TestValidateRetrySourcesForOperations_ZeroFallbackDefaultsToMainWithBasePathPasses(t *testing.T) {
	loopbackURL := "http://127.0.0.1:8080/mf-proxy-primary-ctx"
	spec := &api.APIConfigData{
		Upstream: struct {
			Main    api.Upstream  `json:"main" yaml:"main"`
			Sandbox *api.Upstream `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`
		}{Main: api.Upstream{Url: &loopbackURL}},
		Operations: []api.Operation{
			{
				Policies: &[]api.Policy{
					{
						Name: "model-failover",
						Params: &map[string]interface{}{
							"targets": []interface{}{
								map[string]interface{}{"model": "gpt-4o"}, // defaults to main, no fallbacks
							},
							"statusCodes": []interface{}{float64(500)},
						},
					},
				},
			},
		},
	}
	if err := ValidateRetrySourcesForOperations(spec, retrySourceTestResolver()); err != nil {
		t.Errorf("expected no error - a zero-fallback group is never an aggregate-cluster member regardless of main's basePath, got: %v", err)
	}
}

func strPtr(s string) *string { return &s }

func TestMainUpstreamBasePath(t *testing.T) {
	t.Run("direct URL with a path", func(t *testing.T) {
		u := "http://127.0.0.1:8080/mf-proxy-primary-ctx"
		if got := mainUpstreamBasePath(api.Upstream{Url: &u}, nil); got != "/mf-proxy-primary-ctx" {
			t.Errorf("expected '/mf-proxy-primary-ctx', got %q", got)
		}
	})
	t.Run("direct URL with no path", func(t *testing.T) {
		u := "http://backend.example.com:9711"
		if got := mainUpstreamBasePath(api.Upstream{Url: &u}, nil); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
	t.Run("ref resolves to the referenced definition's basePath", func(t *testing.T) {
		ref := "shared"
		defs := []api.UpstreamDefinition{{Name: "shared", BasePath: strPtr("/some-path")}}
		if got := mainUpstreamBasePath(api.Upstream{Ref: &ref}, &defs); got != "/some-path" {
			t.Errorf("expected '/some-path', got %q", got)
		}
	})
	t.Run("ref with no matching definition returns empty (caught elsewhere)", func(t *testing.T) {
		ref := "missing"
		defs := []api.UpstreamDefinition{{Name: "shared", BasePath: strPtr("/some-path")}}
		if got := mainUpstreamBasePath(api.Upstream{Ref: &ref}, &defs); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
}

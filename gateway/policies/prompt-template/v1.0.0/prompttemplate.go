/*
 * Copyright (c) 2025, WSO2 LLC. (http://www.wso2.com).
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

package prompttemplate

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	policy "github.com/wso2/api-platform/sdk/gateway/policy/v1alpha"
)

var (
	promptTemplateRegex = regexp.MustCompile(`template://[a-zA-Z0-9_-]+\?[^\s"']*`)
	textCleanRegex      = regexp.MustCompile(`^"|"$`)
)

// PromptTemplatePolicy implements prompt template replacement
type PromptTemplatePolicy struct {
	templates map[string]string
}

// NewPolicy creates a new PromptTemplatePolicy instance
func NewPolicy() policy.Policy {
	return &PromptTemplatePolicy{
		templates: make(map[string]string),
	}
}

// Mode returns the processing mode for this policy
func (p *PromptTemplatePolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeSkip,
	}
}

// Validate validates the policy configuration
func (p *PromptTemplatePolicy) Validate(params map[string]interface{}) error {
	// Validate promptTemplateConfig parameter
	configRaw, ok := params["promptTemplateConfig"]
	if !ok {
		return fmt.Errorf("'promptTemplateConfig' parameter is required")
	}
	configStr, ok := configRaw.(string)
	if !ok {
		return fmt.Errorf("'promptTemplateConfig' must be a string")
	}
	if configStr == "" {
		return fmt.Errorf("'promptTemplateConfig' cannot be empty")
	}

	// Validate that config is valid JSON array
	var templates []map[string]interface{}
	if err := json.Unmarshal([]byte(configStr), &templates); err != nil {
		return fmt.Errorf("'promptTemplateConfig' must be a valid JSON array: %w", err)
	}

	// Validate each template has name and prompt
	for i, template := range templates {
		name, ok := template["name"].(string)
		if !ok || name == "" {
			return fmt.Errorf("template[%d] must have a non-empty 'name' field", i)
		}
		prompt, ok := template["prompt"].(string)
		if !ok || prompt == "" {
			return fmt.Errorf("template[%d] must have a non-empty 'prompt' field", i)
		}
	}

	return nil
}

// OnRequest performs prompt template replacement on request
func (p *PromptTemplatePolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
	if ctx.Body == nil || !ctx.Body.Present || len(ctx.Body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	// Load templates from config
	configStr := params["promptTemplateConfig"].(string)
	var templates []map[string]interface{}
	if err := json.Unmarshal([]byte(configStr), &templates); err != nil {
		return policy.UpstreamRequestModifications{} // Skip on error
	}

	// Build template map
	templateMap := make(map[string]string)
	for _, template := range templates {
		name := template["name"].(string)
		prompt := template["prompt"].(string)
		templateMap[name] = prompt
	}

	// Get JSON content as string
	jsonContent := string(ctx.Body.Content)

	// Find and replace template:// references
	updatedContent := promptTemplateRegex.ReplaceAllStringFunc(jsonContent, func(matched string) string {
		return replaceTemplate(matched, templateMap)
	})

	// If content changed, update body
	if updatedContent != jsonContent {
		return policy.UpstreamRequestModifications{
			Body: []byte(updatedContent),
		}
	}

	return policy.UpstreamRequestModifications{}
}

// OnResponse is not used for this policy
func (p *PromptTemplatePolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
	return policy.UpstreamResponseModifications{}
}

// replaceTemplate replaces a template:// reference with the resolved template
func replaceTemplate(templateRef string, templates map[string]string) string {
	// Parse template://<name>?<params>
	// Example: template://translate?from=english&to=spanish&text=Hello
	uri, err := url.Parse(templateRef)
	if err != nil {
		return templateRef // Return original on parse error
	}

	templateName := uri.Host
	if templateName == "" {
		return templateRef // Return original if no template name
	}

	// Get template
	template, exists := templates[templateName]
	if !exists {
		return templateRef // Return original if template not found
	}

	// Parse query parameters
	params := make(map[string]string)
	for key, values := range uri.Query() {
		if len(values) > 0 {
			params[key] = values[0] // Use first value
		}
	}

	// Replace placeholders in template (format: [[parameter-name]])
	resolvedPrompt := template
	for key, value := range params {
		placeholder := "[[" + key + "]]"
		resolvedPrompt = strings.ReplaceAll(resolvedPrompt, placeholder, value)
	}

	// Escape and return as JSON string
	jsonBytes, err := json.Marshal(resolvedPrompt)
	if err != nil {
		return templateRef // Return original on error
	}

	// Remove surrounding quotes added by json.Marshal
	jsonStr := string(jsonBytes)
	jsonStr = textCleanRegex.ReplaceAllString(jsonStr, "")

	return jsonStr
}

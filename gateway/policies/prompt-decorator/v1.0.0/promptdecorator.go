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

package promptdecorator

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	policy "github.com/wso2/api-platform/sdk/gateway/policy/v1alpha"
)

// DecorationType represents the type of decoration
type DecorationType string

const (
	DecorationTypeString DecorationType = "STRING"
	DecorationTypeArray  DecorationType = "ARRAY"
)

// PromptDecoratorPolicy implements prompt decoration
type PromptDecoratorPolicy struct{}

// NewPolicy creates a new PromptDecoratorPolicy instance
func NewPolicy() policy.Policy {
	return &PromptDecoratorPolicy{}
}

// Mode returns the processing mode for this policy
func (p *PromptDecoratorPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeSkip,
	}
}

// Validate validates the policy configuration
func (p *PromptDecoratorPolicy) Validate(params map[string]interface{}) error {
	// Validate promptDecoratorConfig parameter
	configRaw, ok := params["promptDecoratorConfig"]
	if !ok {
		return fmt.Errorf("'promptDecoratorConfig' parameter is required")
	}
	configStr, ok := configRaw.(string)
	if !ok {
		return fmt.Errorf("'promptDecoratorConfig' must be a string")
	}
	if configStr == "" {
		return fmt.Errorf("'promptDecoratorConfig' cannot be empty")
	}

	// Validate that config is valid JSON
	var configJSON map[string]interface{}
	if err := json.Unmarshal([]byte(configStr), &configJSON); err != nil {
		return fmt.Errorf("'promptDecoratorConfig' must be valid JSON: %w", err)
	}

	// Validate decoration field exists
	decoration, ok := configJSON["decoration"]
	if !ok {
		return fmt.Errorf("'decoration' field is required in promptDecoratorConfig")
	}

	// Validate decoration is either string or array
	switch decoration.(type) {
	case string:
		// Valid
	case []interface{}:
		// Valid
	default:
		return fmt.Errorf("'decoration' must be either a string or an array")
	}

	// Validate jsonPath parameter
	jsonPathRaw, ok := params["jsonPath"]
	if !ok {
		return fmt.Errorf("'jsonPath' parameter is required")
	}
	_, ok = jsonPathRaw.(string)
	if !ok {
		return fmt.Errorf("'jsonPath' must be a string")
	}

	// Validate append parameter (optional)
	if appendRaw, ok := params["append"]; ok {
		_, ok := appendRaw.(bool)
		if !ok {
			return fmt.Errorf("'append' must be a boolean")
		}
	}

	return nil
}

// OnRequest performs prompt decoration on request
func (p *PromptDecoratorPolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
	if ctx.Body == nil || !ctx.Body.Present || len(ctx.Body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	// Get configuration
	configStr := params["promptDecoratorConfig"].(string)
	jsonPath := params["jsonPath"].(string)
	shouldAppend := false
	if appendRaw, ok := params["append"]; ok {
		shouldAppend = appendRaw.(bool)
	}

	// Parse config
	var configJSON map[string]interface{}
	if err := json.Unmarshal([]byte(configStr), &configJSON); err != nil {
		return policy.UpstreamRequestModifications{} // Skip on error
	}

	decoration := configJSON["decoration"]
	var decorationType DecorationType
	var decorationValue interface{}

	switch v := decoration.(type) {
	case string:
		decorationType = DecorationTypeString
		decorationValue = v
	case []interface{}:
		decorationType = DecorationTypeArray
		decorationValue = v
	default:
		return policy.UpstreamRequestModifications{} // Skip invalid decoration type
	}

	// Parse JSON payload
	var jsonData map[string]interface{}
	if err := json.Unmarshal(ctx.Body.Content, &jsonData); err != nil {
		return policy.UpstreamRequestModifications{} // Skip on parse error
	}

	// Apply decoration
	modifiedJSON, err := p.applyDecoration(jsonData, jsonPath, decorationType, decorationValue, shouldAppend)
	if err != nil {
		return policy.UpstreamRequestModifications{} // Skip on error
	}

	// Marshal back to JSON
	modifiedBody, err := json.Marshal(modifiedJSON)
	if err != nil {
		return policy.UpstreamRequestModifications{} // Skip on error
	}

	return policy.UpstreamRequestModifications{
		Body: modifiedBody,
	}
}

// OnResponse is not used for this policy
func (p *PromptDecoratorPolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
	return policy.UpstreamResponseModifications{}
}

// applyDecoration applies decoration to the JSON payload at the specified JSONPath
func (p *PromptDecoratorPolicy) applyDecoration(jsonData map[string]interface{}, jsonPath string, decorationType DecorationType, decorationValue interface{}, shouldAppend bool) (map[string]interface{}, error) {
	// Extract value at JSONPath
	currentValue, err := getValueAtJSONPath(jsonData, jsonPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get value at JSONPath: %w", err)
	}

	var updatedValue interface{}

	switch decorationType {
	case DecorationTypeString:
		// Decoration is a string
		decorationStr := decorationValue.(string)
		existingStr, ok := currentValue.(string)
		if !ok {
			return nil, fmt.Errorf("value at JSONPath must be a string for string decoration")
		}

		if shouldAppend {
			updatedValue = existingStr + " " + decorationStr
		} else {
			updatedValue = decorationStr + " " + existingStr
		}

	case DecorationTypeArray:
		// Decoration is an array
		decorationArray := decorationValue.([]interface{})
		existingArray, ok := currentValue.([]interface{})
		if !ok {
			// If current value is not an array, create a new array
			existingArray = []interface{}{currentValue}
		}

		updatedArray := make([]interface{}, 0)
		if shouldAppend {
			// Append: existing array first, then decoration array
			updatedArray = append(updatedArray, existingArray...)
			updatedArray = append(updatedArray, decorationArray...)
		} else {
			// Prepend: decoration array first, then existing array
			updatedArray = append(updatedArray, decorationArray...)
			updatedArray = append(updatedArray, existingArray...)
		}
		updatedValue = updatedArray

	default:
		return nil, fmt.Errorf("unknown decoration type: %v", decorationType)
	}

	// Set updated value back at JSONPath
	if err := setValueAtJSONPath(jsonData, jsonPath, updatedValue); err != nil {
		return nil, fmt.Errorf("failed to set value at JSONPath: %w", err)
	}

	return jsonData, nil
}

// getValueAtJSONPath gets a value from JSON using JSONPath
func getValueAtJSONPath(jsonData map[string]interface{}, jsonPath string) (interface{}, error) {
	if jsonPath == "" || jsonPath == "$" {
		return jsonData, nil
	}

	// Parse JSONPath: handle both dot notation and array indices like messages[0].content
	path := strings.TrimPrefix(jsonPath, "$.")
	if path == "" {
		return jsonData, nil
	}

	current := interface{}(jsonData)
	parts := parseJSONPath(path)

	for _, part := range parts {
		if part.isArray {
			// Handle array access
			arr, ok := current.([]interface{})
			if !ok {
				return nil, fmt.Errorf("expected array at index %d, got %T", part.index, current)
			}
			if part.index < 0 || part.index >= len(arr) {
				return nil, fmt.Errorf("array index out of bounds: %d (length: %d)", part.index, len(arr))
			}
			current = arr[part.index]
		} else {
			// Handle map access
			m, ok := current.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("expected map at key %s, got %T", part.key, current)
			}
			if val, exists := m[part.key]; exists {
				current = val
			} else {
				return nil, fmt.Errorf("key not found: %s", part.key)
			}
		}
	}

	return current, nil
}

// jsonPathPart represents a part of a JSONPath
type jsonPathPart struct {
	key     string
	isArray bool
	index   int
}

// parseJSONPath parses a JSONPath string into parts
// Handles: "messages[0].content" -> [{key: "messages", isArray: true, index: 0}, {key: "content", isArray: false}]
func parseJSONPath(path string) []jsonPathPart {
	var parts []jsonPathPart
	var current strings.Builder
	var i int

	for i < len(path) {
		if path[i] == '[' {
			// Save current key
			if current.Len() > 0 {
				parts = append(parts, jsonPathPart{key: current.String(), isArray: false})
				current.Reset()
			}
			// Parse index
			i++ // skip '['
			var indexStr strings.Builder
			for i < len(path) && path[i] != ']' {
				indexStr.WriteByte(path[i])
				i++
			}
			if i < len(path) && path[i] == ']' {
				i++ // skip ']'
				idx, err := strconv.Atoi(indexStr.String())
				if err != nil {
					// Invalid index, treat as key
					current.WriteString("[" + indexStr.String() + "]")
					continue
				}
				parts = append(parts, jsonPathPart{key: "", isArray: true, index: idx})
			}
		} else if path[i] == '.' {
			// Save current key
			if current.Len() > 0 {
				parts = append(parts, jsonPathPart{key: current.String(), isArray: false})
				current.Reset()
			}
			i++ // skip '.'
		} else {
			current.WriteByte(path[i])
			i++
		}
	}

	// Add remaining key
	if current.Len() > 0 {
		parts = append(parts, jsonPathPart{key: current.String(), isArray: false})
	}

	return parts
}

// setValueAtJSONPath sets a value in JSON using JSONPath
func setValueAtJSONPath(jsonData map[string]interface{}, jsonPath string, value interface{}) error {
	if jsonPath == "" || jsonPath == "$" {
		return fmt.Errorf("cannot set root value")
	}

	// Parse JSONPath
	path := strings.TrimPrefix(jsonPath, "$.")
	if path == "" {
		return fmt.Errorf("cannot set root value")
	}

	parts := parseJSONPath(path)
	if len(parts) == 0 {
		return fmt.Errorf("invalid JSONPath: %s", jsonPath)
	}

	current := interface{}(jsonData)

	// Navigate to parent (all parts except the last)
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		if part.isArray {
			arr, ok := current.([]interface{})
			if !ok {
				return fmt.Errorf("expected array at index %d, got %T", part.index, current)
			}
			if part.index < 0 || part.index >= len(arr) {
				return fmt.Errorf("array index out of bounds: %d (length: %d)", part.index, len(arr))
			}
			current = arr[part.index]
		} else {
			m, ok := current.(map[string]interface{})
			if !ok {
				return fmt.Errorf("expected map at key %s, got %T", part.key, current)
			}
			if val, exists := m[part.key]; exists {
				current = val
			} else {
				// Create new map or array based on next part
				if i+1 < len(parts) && parts[i+1].isArray {
					m[part.key] = make([]interface{}, 0)
				} else {
					m[part.key] = make(map[string]interface{})
				}
				current = m[part.key]
			}
		}
	}

	// Set value at final part
	finalPart := parts[len(parts)-1]
	if finalPart.isArray {
		arr, ok := current.([]interface{})
		if !ok {
			return fmt.Errorf("expected array at final index %d, got %T", finalPart.index, current)
		}
		if finalPart.index < 0 || finalPart.index >= len(arr) {
			return fmt.Errorf("array index out of bounds: %d (length: %d)", finalPart.index, len(arr))
		}
		arr[finalPart.index] = value
		return nil
	} else {
		m, ok := current.(map[string]interface{})
		if !ok {
			return fmt.Errorf("expected map at final key %s, got %T", finalPart.key, current)
		}
		m[finalPart.key] = value
		return nil
	}
}

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

package jsonschemaguardrail

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	policy "github.com/wso2/api-platform/sdk/gateway/policy/v1alpha"
	"github.com/xeipuuv/gojsonschema"
)

var (
	jsonContentRegex = regexp.MustCompile(`\{.*?\}`)
	textCleanRegex   = regexp.MustCompile(`^"|"$`)
)

// JSONSchemaGuardrailPolicy implements JSON schema validation
type JSONSchemaGuardrailPolicy struct {
	schemaLoader *gojsonschema.SchemaLoader
	schema       *gojsonschema.Schema
}

// NewPolicy creates a new JSONSchemaGuardrailPolicy instance
func NewPolicy() policy.Policy {
	return &JSONSchemaGuardrailPolicy{}
}

// Mode returns the processing mode for this policy
func (p *JSONSchemaGuardrailPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeBuffer,
	}
}

// Validate validates the policy configuration
func (p *JSONSchemaGuardrailPolicy) Validate(params map[string]interface{}) error {
	// Check if request/response sections exist
	requestParams, hasRequest := params["request"]
	responseParams, hasResponse := params["response"]

	// If neither request nor response sections exist, validate params directly (backward compatibility)
	if !hasRequest && !hasResponse {
		return p.validateParams(params)
	}

	// Validate request section if present
	if hasRequest {
		requestConfig, ok := requestParams.(map[string]interface{})
		if !ok {
			return fmt.Errorf("'request' must be an object")
		}
		if err := p.validateParams(requestConfig); err != nil {
			return fmt.Errorf("request section: %w", err)
		}
	}

	// Validate response section if present
	if hasResponse {
		responseConfig, ok := responseParams.(map[string]interface{})
		if !ok {
			return fmt.Errorf("'response' must be an object")
		}
		if err := p.validateParams(responseConfig); err != nil {
			return fmt.Errorf("response section: %w", err)
		}
	}

	return nil
}

// validateParams validates the actual policy parameters
func (p *JSONSchemaGuardrailPolicy) validateParams(params map[string]interface{}) error {
	// Validate schema parameter
	schemaRaw, ok := params["schema"]
	if !ok {
		return fmt.Errorf("'schema' parameter is required")
	}
	schemaStr, ok := schemaRaw.(string)
	if !ok {
		return fmt.Errorf("'schema' must be a string")
	}
	if schemaStr == "" {
		return fmt.Errorf("'schema' cannot be empty")
	}

	// Validate that schema is valid JSON
	var schemaJSON map[string]interface{}
	if err := json.Unmarshal([]byte(schemaStr), &schemaJSON); err != nil {
		return fmt.Errorf("'schema' must be valid JSON: %w", err)
	}

	// Validate jsonPath parameter (optional)
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		_, ok := jsonPathRaw.(string)
		if !ok {
			return fmt.Errorf("'jsonPath' must be a string")
		}
	}

	// Validate invert parameter (optional)
	if invertRaw, ok := params["invert"]; ok {
		_, ok := invertRaw.(bool)
		if !ok {
			return fmt.Errorf("'invert' must be a boolean")
		}
	}

	// Validate showAssessment parameter (optional)
	if showAssessmentRaw, ok := params["showAssessment"]; ok {
		_, ok := showAssessmentRaw.(bool)
		if !ok {
			return fmt.Errorf("'showAssessment' must be a boolean")
		}
	}

	return nil
}

// OnRequest performs JSON schema validation on request
func (p *JSONSchemaGuardrailPolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
	// Check if request configuration exists
	requestParams, ok := params["request"]
	if !ok {
		// No request configuration, pass through
		return policy.UpstreamRequestModifications{}
	}

	// Extract request params (could be a map or the params themselves if no request/response separation)
	requestConfig, ok := requestParams.(map[string]interface{})
	if !ok {
		// If request is not a map, use params directly (backward compatibility)
		requestConfig = params
	}

	return p.validateSchema(ctx.Body, requestConfig, false)
}

// OnResponse performs JSON schema validation on response
func (p *JSONSchemaGuardrailPolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
	// Check if response configuration exists
	responseParams, ok := params["response"]
	if !ok {
		// No response configuration, pass through
		return policy.UpstreamResponseModifications{}
	}

	// Extract response params (could be a map or the params themselves if no request/response separation)
	responseConfig, ok := responseParams.(map[string]interface{})
	if !ok {
		// If response is not a map, use params directly (backward compatibility)
		responseConfig = params
	}

	return p.validateSchemaResponse(ctx.ResponseBody, responseConfig, true)
}

// validateSchema validates JSON schema for request
func (p *JSONSchemaGuardrailPolicy) validateSchema(body *policy.Body, params map[string]interface{}, isResponse bool) policy.RequestAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	// Safely get schema parameter
	schemaRaw, ok := params["schema"]
	if !ok {
		return p.buildErrorResponse("'schema' parameter is required", "", "", isResponse, false)
	}
	schemaStr, ok := schemaRaw.(string)
	if !ok || schemaStr == "" {
		return p.buildErrorResponse("'schema' must be a non-empty string", "", "", isResponse, false)
	}

	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}
	inverted := false
	if invertRaw, ok := params["invert"]; ok {
		inverted = invertRaw.(bool)
	}
	showAssessment := false
	if showAssessmentRaw, ok := params["showAssessment"]; ok {
		showAssessment = showAssessmentRaw.(bool)
	}

	extractedValue := string(body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(body.Content, jsonPath)
		if err != nil {
			return p.buildErrorResponse("Error extracting value from JSON using JSONPath: "+err.Error(), schemaStr, jsonPath, isResponse, showAssessment)
		}
	}

	// Validate JSON against schema
	matchedAndValid, err := p.validateJSONAgainstSchema(extractedValue, schemaStr)
	if err != nil {
		return p.buildErrorResponse("Error validating JSON against schema: "+err.Error(), schemaStr, jsonPath, isResponse, showAssessment)
	}

	// Apply inversion logic
	finalResult := inverted != matchedAndValid

	if !finalResult {
		// Build assessment details
		assessmentMessage := "Violation of enforced JSON schema detected."
		if showAssessment {
			assessmentMessage = fmt.Sprintf("The inspected payload content: %s does not satisfy the JSON schema: %s", extractedValue, schemaStr)
		}
		return p.buildErrorResponse(assessmentMessage, schemaStr, jsonPath, isResponse, showAssessment)
	}

	return policy.UpstreamRequestModifications{}
}

// validateSchemaResponse validates JSON schema for response
func (p *JSONSchemaGuardrailPolicy) validateSchemaResponse(body *policy.Body, params map[string]interface{}, isResponse bool) policy.ResponseAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamResponseModifications{}
	}

	// Safely get schema parameter
	schemaRaw, ok := params["schema"]
	if !ok {
		return p.buildErrorResponseResponse("'schema' parameter is required", "", "", isResponse, false)
	}
	schemaStr, ok := schemaRaw.(string)
	if !ok || schemaStr == "" {
		return p.buildErrorResponseResponse("'schema' must be a non-empty string", "", "", isResponse, false)
	}

	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}
	inverted := false
	if invertRaw, ok := params["invert"]; ok {
		inverted = invertRaw.(bool)
	}
	showAssessment := false
	if showAssessmentRaw, ok := params["showAssessment"]; ok {
		showAssessment = showAssessmentRaw.(bool)
	}

	extractedValue := string(body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(body.Content, jsonPath)
		if err != nil {
			return p.buildErrorResponseResponse("Error extracting value from JSON using JSONPath: "+err.Error(), schemaStr, jsonPath, isResponse, showAssessment)
		}
	}

	// Validate JSON against schema
	matchedAndValid, err := p.validateJSONAgainstSchema(extractedValue, schemaStr)
	if err != nil {
		return p.buildErrorResponseResponse("Error validating JSON against schema: "+err.Error(), schemaStr, jsonPath, isResponse, showAssessment)
	}

	// Apply inversion logic
	finalResult := inverted != matchedAndValid

	if !finalResult {
		// Build assessment details
		assessmentMessage := "Violation of enforced JSON schema detected."
		if showAssessment {
			assessmentMessage = fmt.Sprintf("The inspected payload content: %s does not satisfy the JSON schema: %s", extractedValue, schemaStr)
		}
		return p.buildErrorResponseResponse(assessmentMessage, schemaStr, jsonPath, isResponse, showAssessment)
	}

	return policy.UpstreamResponseModifications{}
}

// validateJSONAgainstSchema validates JSON content against a JSON schema
func (p *JSONSchemaGuardrailPolicy) validateJSONAgainstSchema(input, schemaStr string) (bool, error) {
	// Try to find JSON objects in the input using regex
	matches := jsonContentRegex.FindAllString(input, -1)
	if len(matches) == 0 {
		return false, nil
	}

	// Load schema
	schemaLoader := gojsonschema.NewStringLoader(schemaStr)

	for _, match := range matches {
		// Clean up the match (remove quotes if present)
		cleanedMatch := textCleanRegex.ReplaceAllString(match, "")
		cleanedMatch = strings.TrimSpace(cleanedMatch)

		// Try to parse as JSON
		var jsonData interface{}
		if err := json.Unmarshal([]byte(cleanedMatch), &jsonData); err != nil {
			continue // Skip invalid JSON
		}

		// Create document loader
		documentLoader := gojsonschema.NewGoLoader(jsonData)

		// Validate
		result, err := gojsonschema.Validate(schemaLoader, documentLoader)
		if err != nil {
			continue // Skip validation errors, try next match
		}

		if result.Valid() {
			return true, nil // Found valid match
		}
	}

	return false, nil
}

// buildErrorResponse builds an error response for request
func (p *JSONSchemaGuardrailPolicy) buildErrorResponse(message, schemaStr, jsonPath string, isResponse bool, showAssessment bool) policy.RequestAction {
	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "JSON_SCHEMA_GUARDRAIL",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "JSONSchemaGuardrail",
			"direction":            "REQUEST",
			"actionReason":         message,
		},
	}

	if showAssessment {
		responseBody["message"].(map[string]interface{})["assessments"] = message
	}

	bodyBytes, _ := json.Marshal(responseBody)
	return policy.ImmediateResponse{
		StatusCode: 446,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		Body: bodyBytes,
	}
}

// buildErrorResponseResponse builds an error response for response
func (p *JSONSchemaGuardrailPolicy) buildErrorResponseResponse(message, schemaStr, jsonPath string, isResponse bool, showAssessment bool) policy.ResponseAction {
	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "JSON_SCHEMA_GUARDRAIL",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "JSONSchemaGuardrail",
			"direction":            "RESPONSE",
			"actionReason":         message,
		},
	}

	if showAssessment {
		responseBody["message"].(map[string]interface{})["assessments"] = message
	}

	bodyBytes, _ := json.Marshal(responseBody)
	return policy.UpstreamResponseModifications{
		SetHeaders: map[string]string{
			"Content-Type": "application/json",
		},
		Body:       bodyBytes,
		StatusCode: intPtr(446),
	}
}

func intPtr(i int) *int {
	return &i
}

// extractValueFromJSONPath extracts a value from JSON using JSONPath
func extractValueFromJSONPath(payload []byte, jsonPath string) (string, error) {
	if jsonPath == "" {
		return string(payload), nil
	}

	var jsonData map[string]interface{}
	if err := json.Unmarshal(payload, &jsonData); err != nil {
		return "", err
	}

	keys := strings.Split(strings.TrimPrefix(jsonPath, "$."), ".")
	current := interface{}(jsonData)

	for _, key := range keys {
		if m, ok := current.(map[string]interface{}); ok {
			if val, exists := m[key]; exists {
				current = val
			} else {
				return "", fmt.Errorf("key not found: %s", key)
			}
		} else {
			return "", fmt.Errorf("invalid structure at key: %s", key)
		}
	}

	switch v := current.(type) {
	case string:
		return v, nil
	case float64:
		return fmt.Sprintf("%.0f", v), nil
	case int:
		return fmt.Sprintf("%d", v), nil
	default:
		jsonBytes, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v), nil
		}
		return string(jsonBytes), nil
	}
}

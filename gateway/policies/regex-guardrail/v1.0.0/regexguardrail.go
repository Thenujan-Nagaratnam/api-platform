package regexguardrail

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	policy "github.com/policy-engine/sdk/policy/v1alpha"
)

// RegexGuardrailPolicy implements regex pattern validation
type RegexGuardrailPolicy struct{}

// NewPolicy creates a new RegexGuardrailPolicy instance
func NewPolicy() policy.Policy {
	return &RegexGuardrailPolicy{}
}

// Mode returns the processing mode for this policy
func (p *RegexGuardrailPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeBuffer,
	}
}

// Validate validates the policy configuration
func (p *RegexGuardrailPolicy) Validate(params map[string]interface{}) error {
	regexRaw, ok := params["regex"]
	if !ok {
		return fmt.Errorf("'regex' parameter is required")
	}
	regexStr, ok := regexRaw.(string)
	if !ok {
		return fmt.Errorf("'regex' must be a string")
	}
	if regexStr == "" {
		return fmt.Errorf("'regex' cannot be empty")
	}

	// Validate regex pattern is compilable
	_, err := regexp.Compile(regexStr)
	if err != nil {
		return fmt.Errorf("'regex' is not a valid regular expression: %w", err)
	}

	if jsonPathRaw, ok := params["jsonPath"]; ok {
		_, ok := jsonPathRaw.(string)
		if !ok {
			return fmt.Errorf("'jsonPath' must be a string")
		}
	}

	if invertRaw, ok := params["invert"]; ok {
		_, ok := invertRaw.(bool)
		if !ok {
			return fmt.Errorf("'invert' must be a boolean")
		}
	}

	if showAssessmentRaw, ok := params["showAssessment"]; ok {
		_, ok := showAssessmentRaw.(bool)
		if !ok {
			return fmt.Errorf("'showAssessment' must be a boolean")
		}
	}

	return nil
}

// OnRequest performs regex validation on request
func (p *RegexGuardrailPolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
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

	return p.validateRegex(ctx.Body, requestConfig, false)
}

// OnResponse performs regex validation on response
func (p *RegexGuardrailPolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
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

	return p.validateRegexResponse(ctx.ResponseBody, responseConfig, true)
}

// validateRegex validates regex pattern for request
func (p *RegexGuardrailPolicy) validateRegex(body *policy.Body, params map[string]interface{}, isResponse bool) policy.RequestAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	// Safely get regex parameter
	regexRaw, ok := params["regex"]
	if !ok {
		return p.buildErrorResponse("'regex' parameter is required", "", "", isResponse)
	}
	regexStr, ok := regexRaw.(string)
	if !ok || regexStr == "" {
		return p.buildErrorResponse("'regex' must be a non-empty string", "", "", isResponse)
	}

	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}
	inverted := false
	if invertRaw, ok := params["invert"]; ok {
		inverted = invertRaw.(bool)
	}

	extractedValue := string(body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(body.Content, jsonPath)
		if err != nil {
			return p.buildErrorResponse("Error extracting value from JSON using JSONPath: "+err.Error(), regexStr, jsonPath, isResponse)
		}
	}

	matched, err := regexp.MatchString(regexStr, extractedValue)
	if err != nil {
		return p.buildErrorResponse("Error matching regex: "+err.Error(), regexStr, jsonPath, isResponse)
	}

	// Logic based on policy definition:
	// - invert=false (default): Content MUST match pattern (validation mode - require pattern)
	//   - Pattern matches → PASS (content is valid)
	//   - Pattern doesn't match → BLOCK (content is invalid)
	// - invert=true: Content MUST NOT match pattern (guardrail mode - block pattern)
	//   - Pattern matches → BLOCK (bad content detected)
	//   - Pattern doesn't match → PASS (content is safe)
	if !matched && !inverted {
		// Pattern didn't match and not inverted: block (validation mode - require pattern)
		return p.buildErrorResponse("Regex pattern did not match", regexStr, jsonPath, isResponse)
	} else if matched && inverted {
		// Pattern matched and inverted: block (guardrail mode - block bad content)
		return p.buildErrorResponse("Regex pattern matched (inverted mode)", regexStr, jsonPath, isResponse)
	}

	// Pattern matched with invert=false: allow (validation passed)
	// Pattern didn't match with invert=true: allow (guardrail passed - no bad content)
	return policy.UpstreamRequestModifications{}
}

// validateRegexResponse validates regex pattern for response
func (p *RegexGuardrailPolicy) validateRegexResponse(body *policy.Body, params map[string]interface{}, isResponse bool) policy.ResponseAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamResponseModifications{}
	}

	// Safely get regex parameter
	regexRaw, ok := params["regex"]
	if !ok {
		return p.buildErrorResponseResponse("'regex' parameter is required", "", "", isResponse)
	}
	regexStr, ok := regexRaw.(string)
	if !ok || regexStr == "" {
		return p.buildErrorResponseResponse("'regex' must be a non-empty string", "", "", isResponse)
	}

	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}
	inverted := false
	if invertRaw, ok := params["invert"]; ok {
		inverted = invertRaw.(bool)
	}

	extractedValue := string(body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(body.Content, jsonPath)
		if err != nil {
			return p.buildErrorResponseResponse("Error extracting value from JSON using JSONPath: "+err.Error(), regexStr, jsonPath, isResponse)
		}
	}

	matched, err := regexp.MatchString(regexStr, extractedValue)
	if err != nil {
		return p.buildErrorResponseResponse("Error matching regex: "+err.Error(), regexStr, jsonPath, isResponse)
	}

	// Logic based on policy definition:
	// - invert=false (default): Content MUST match pattern (validation mode - require pattern)
	//   - Pattern matches → PASS (content is valid)
	//   - Pattern doesn't match → BLOCK (content is invalid)
	// - invert=true: Content MUST NOT match pattern (guardrail mode - block pattern)
	//   - Pattern matches → BLOCK (bad content detected)
	//   - Pattern doesn't match → PASS (content is safe)
	if !matched && !inverted {
		// Pattern didn't match and not inverted: block (validation mode - require pattern)
		return p.buildErrorResponseResponse("Regex pattern did not match", regexStr, jsonPath, isResponse)
	} else if matched && inverted {
		// Pattern matched and inverted: block (guardrail mode - block bad content)
		return p.buildErrorResponseResponse("Regex pattern matched (inverted mode)", regexStr, jsonPath, isResponse)
	}

	// Pattern matched with invert=false: allow (validation passed)
	// Pattern didn't match with invert=true: allow (guardrail passed - no bad content)
	return policy.UpstreamResponseModifications{}
}

// buildErrorResponse builds an error response for request
func (p *RegexGuardrailPolicy) buildErrorResponse(message, regexStr, jsonPath string, isResponse bool) policy.RequestAction {
	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "REGEX_GUARDRAIL",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "RegexGuardrail",
			"direction":            "REQUEST",
			"actionReason":         "Violation of regular expression detected.",
		},
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
func (p *RegexGuardrailPolicy) buildErrorResponseResponse(message, regexStr, jsonPath string, isResponse bool) policy.ResponseAction {
	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "REGEX_GUARDRAIL",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "RegexGuardrail",
			"direction":            "RESPONSE",
			"actionReason":         "Violation of regular expression detected.",
		},
	}

	bodyBytes, _ := json.Marshal(responseBody)
	return policy.UpstreamResponseModifications{
		Body:       bodyBytes,
		StatusCode: intPtr(446),
	}
}

func intPtr(i int) *int {
	return &i
}

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
		return fmt.Sprintf("%v", v), nil
	}
}

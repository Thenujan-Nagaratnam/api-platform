package contentlengthguardrail

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	policy "github.com/policy-engine/sdk/policy/v1alpha"
)

var (
	textCleanRegex = regexp.MustCompile(`^"|"$`)
)

// ContentLengthGuardrailPolicy implements content length validation
type ContentLengthGuardrailPolicy struct{}

// NewPolicy creates a new ContentLengthGuardrailPolicy instance
func NewPolicy() policy.Policy {
	return &ContentLengthGuardrailPolicy{}
}

// Mode returns the processing mode for this policy
func (p *ContentLengthGuardrailPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeBuffer,
	}
}

// Validate validates the policy configuration
func (p *ContentLengthGuardrailPolicy) Validate(params map[string]interface{}) error {
	// Validate min parameter
	minRaw, ok := params["min"]
	if !ok {
		return fmt.Errorf("'min' parameter is required")
	}
	min, ok := minRaw.(float64)
	if !ok {
		// Try string conversion
		if minStr, ok := minRaw.(string); ok {
			var err error
			min, err = strconv.ParseFloat(minStr, 64)
			if err != nil {
				return fmt.Errorf("'min' must be a number")
			}
		} else {
			return fmt.Errorf("'min' must be a number")
		}
	}
	if min < 0 {
		return fmt.Errorf("'min' cannot be negative")
	}

	// Validate max parameter
	maxRaw, ok := params["max"]
	if !ok {
		return fmt.Errorf("'max' parameter is required")
	}
	max, ok := maxRaw.(float64)
	if !ok {
		// Try string conversion
		if maxStr, ok := maxRaw.(string); ok {
			var err error
			max, err = strconv.ParseFloat(maxStr, 64)
			if err != nil {
				return fmt.Errorf("'max' must be a number")
			}
		} else {
			return fmt.Errorf("'max' must be a number")
		}
	}
	if max <= 0 {
		return fmt.Errorf("'max' must be greater than 0")
	}

	if min > max {
		return fmt.Errorf("'min' cannot be greater than 'max'")
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

// OnRequest performs content length validation on request
func (p *ContentLengthGuardrailPolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
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

	return p.validateContentLength(ctx.Body, requestConfig, false)
}

// OnResponse performs content length validation on response
func (p *ContentLengthGuardrailPolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
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

	return p.validateContentLengthResponse(ctx.ResponseBody, responseConfig, true)
}

// validateContentLength validates content length for request
func (p *ContentLengthGuardrailPolicy) validateContentLength(body *policy.Body, params map[string]interface{}, isResponse bool) policy.RequestAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		// No body to validate, allow to proceed
		return policy.UpstreamRequestModifications{}
	}

	// Safely get min parameter
	minRaw, ok := params["min"]
	if !ok {
		return p.buildErrorResponse("'min' parameter is required", "", isResponse)
	}
	min, ok := minRaw.(float64)
	if !ok {
		return p.buildErrorResponse("'min' must be a number", "", isResponse)
	}
	minInt := int(min)

	// Safely get max parameter
	maxRaw, ok := params["max"]
	if !ok {
		return p.buildErrorResponse("'max' parameter is required", "", isResponse)
	}
	max, ok := maxRaw.(float64)
	if !ok {
		return p.buildErrorResponse("'max' must be a number", "", isResponse)
	}
	maxInt := int(max)

	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}
	inverted := false
	if invertRaw, ok := params["invert"]; ok {
		inverted = invertRaw.(bool)
	}

	// Extract content using JSONPath if specified
	extractedValue := string(body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(body.Content, jsonPath)
		if err != nil {
			return p.buildErrorResponse("Error extracting value from JSON using JSONPath: "+err.Error(), jsonPath, isResponse)
		}
	}

	// Clean and trim
	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	// Count bytes
	byteCount := len([]byte(extractedValue))

	isWithinRange := byteCount >= minInt && byteCount <= maxInt

	if inverted {
		// When inverted, fail if content length is within the range
		if isWithinRange {
			return p.buildErrorResponse(fmt.Sprintf("Content length validation failed (inverted): %d bytes found, should NOT be between %d and %d bytes", byteCount, minInt, maxInt), jsonPath, isResponse)
		}
		return policy.UpstreamRequestModifications{}
	}

	// When not inverted, fail if content length is outside the range
	if !isWithinRange {
		return p.buildErrorResponse(fmt.Sprintf("Content length validation failed: %d bytes found, expected between %d and %d bytes", byteCount, minInt, maxInt), jsonPath, isResponse)
	}

	return policy.UpstreamRequestModifications{}
}

// validateContentLengthResponse validates content length for response
func (p *ContentLengthGuardrailPolicy) validateContentLengthResponse(body *policy.Body, params map[string]interface{}, isResponse bool) policy.ResponseAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		// No body to validate, allow to proceed
		return policy.UpstreamResponseModifications{}
	}

	// Safely get min parameter
	minRaw, ok := params["min"]
	if !ok {
		return p.buildErrorResponseResponse("'min' parameter is required", "", isResponse)
	}
	min, ok := minRaw.(float64)
	if !ok {
		return p.buildErrorResponseResponse("'min' must be a number", "", isResponse)
	}
	minInt := int(min)

	// Safely get max parameter
	maxRaw, ok := params["max"]
	if !ok {
		return p.buildErrorResponseResponse("'max' parameter is required", "", isResponse)
	}
	max, ok := maxRaw.(float64)
	if !ok {
		return p.buildErrorResponseResponse("'max' must be a number", "", isResponse)
	}
	maxInt := int(max)

	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}
	inverted := false
	if invertRaw, ok := params["invert"]; ok {
		inverted = invertRaw.(bool)
	}

	// Extract content using JSONPath if specified
	extractedValue := string(body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(body.Content, jsonPath)
		if err != nil {
			return p.buildErrorResponseResponse("Error extracting value from JSON using JSONPath: "+err.Error(), jsonPath, isResponse)
		}
	}

	// Clean and trim
	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	// Count bytes
	byteCount := len([]byte(extractedValue))

	isWithinRange := byteCount >= minInt && byteCount <= maxInt

	if inverted {
		// When inverted, fail if content length is within the range
		if isWithinRange {
			return p.buildErrorResponseResponse(fmt.Sprintf("Content length validation failed (inverted): %d bytes found, should NOT be between %d and %d bytes", byteCount, minInt, maxInt), jsonPath, isResponse)
		}
		return policy.UpstreamResponseModifications{}
	}

	// When not inverted, fail if content length is outside the range
	if !isWithinRange {
		return p.buildErrorResponseResponse(fmt.Sprintf("Content length validation failed: %d bytes found, expected between %d and %d bytes", byteCount, minInt, maxInt), jsonPath, isResponse)
	}

	return policy.UpstreamResponseModifications{}
}

// buildErrorResponse builds an error response for request
func (p *ContentLengthGuardrailPolicy) buildErrorResponse(message, jsonPath string, isResponse bool) policy.RequestAction {
	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "CONTENT_LENGTH_GUARDRAIL",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "ContentLengthGuardrail",
			"direction":            "REQUEST",
			"actionReason":         "Violation of applied content length constraints detected.",
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
func (p *ContentLengthGuardrailPolicy) buildErrorResponseResponse(message, jsonPath string, isResponse bool) policy.ResponseAction {
	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "CONTENT_LENGTH_GUARDRAIL",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "ContentLengthGuardrail",
			"direction":            "RESPONSE",
			"actionReason":         "Violation of applied content length constraints detected.",
		},
	}

	bodyBytes, _ := json.Marshal(responseBody)
	return policy.UpstreamResponseModifications{
		Body:       bodyBytes,
		StatusCode: intPtr(446),
	}
}

// Helper functions
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

	// Simple JSONPath implementation (supports dot notation)
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

	// Convert to string
	switch v := current.(type) {
	case string:
		return v, nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(v), nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}

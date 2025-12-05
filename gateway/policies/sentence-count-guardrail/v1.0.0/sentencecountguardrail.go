package sentencecountguardrail

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	policy "github.com/policy-engine/sdk/policy/v1alpha"
)

var (
	textCleanRegex     = regexp.MustCompile(`^"|"$`)
	sentenceSplitRegex = regexp.MustCompile(`[.!?]`)
)

// SentenceCountGuardrailPolicy implements sentence count validation
type SentenceCountGuardrailPolicy struct{}

// NewPolicy creates a new SentenceCountGuardrailPolicy instance
func NewPolicy() policy.Policy {
	return &SentenceCountGuardrailPolicy{}
}

// Mode returns the processing mode for this policy
func (p *SentenceCountGuardrailPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeBuffer,
	}
}

// Validate validates the policy configuration
func (p *SentenceCountGuardrailPolicy) Validate(params map[string]interface{}) error {
	minRaw, ok := params["min"]
	if !ok {
		return fmt.Errorf("'min' parameter is required")
	}
	min, ok := minRaw.(float64)
	if !ok {
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

	maxRaw, ok := params["max"]
	if !ok {
		return fmt.Errorf("'max' parameter is required")
	}
	max, ok := maxRaw.(float64)
	if !ok {
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

// OnRequest performs sentence count validation on request
func (p *SentenceCountGuardrailPolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
	return p.validateSentenceCount(ctx.Body, params, false)
}

// OnResponse performs sentence count validation on response
func (p *SentenceCountGuardrailPolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
	return p.validateSentenceCountResponse(ctx.ResponseBody, params, true)
}

// validateSentenceCount validates sentence count for request
func (p *SentenceCountGuardrailPolicy) validateSentenceCount(body *policy.Body, params map[string]interface{}, isResponse bool) policy.RequestAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	min := int(params["min"].(float64))
	max := int(params["max"].(float64))
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
			return p.buildErrorResponse("Error extracting value from JSON using JSONPath: "+err.Error(), jsonPath, isResponse)
		}
	}

	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	sentences := sentenceSplitRegex.Split(extractedValue, -1)
	sentenceCount := 0
	for _, s := range sentences {
		if s != "" {
			sentenceCount++
		}
	}

	isWithinRange := sentenceCount >= min && sentenceCount <= max

	if inverted {
		if isWithinRange {
			return p.buildErrorResponse(fmt.Sprintf("Sentence count validation failed (inverted): %d sentences found, should NOT be between %d and %d sentences", sentenceCount, min, max), jsonPath, isResponse)
		}
		return policy.UpstreamRequestModifications{}
	}

	if !isWithinRange {
		return p.buildErrorResponse(fmt.Sprintf("Sentence count validation failed: %d sentences found, expected between %d and %d sentences", sentenceCount, min, max), jsonPath, isResponse)
	}

	return policy.UpstreamRequestModifications{}
}

// validateSentenceCountResponse validates sentence count for response
func (p *SentenceCountGuardrailPolicy) validateSentenceCountResponse(body *policy.Body, params map[string]interface{}, isResponse bool) policy.ResponseAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamResponseModifications{}
	}

	min := int(params["min"].(float64))
	max := int(params["max"].(float64))
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
			return p.buildErrorResponseResponse("Error extracting value from JSON using JSONPath: "+err.Error(), jsonPath, isResponse)
		}
	}

	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	sentences := sentenceSplitRegex.Split(extractedValue, -1)
	sentenceCount := 0
	for _, s := range sentences {
		if s != "" {
			sentenceCount++
		}
	}

	isWithinRange := sentenceCount >= min && sentenceCount <= max

	if inverted {
		if isWithinRange {
			return p.buildErrorResponseResponse(fmt.Sprintf("Sentence count validation failed (inverted): %d sentences found, should NOT be between %d and %d sentences", sentenceCount, min, max), jsonPath, isResponse)
		}
		return policy.UpstreamResponseModifications{}
	}

	if !isWithinRange {
		return p.buildErrorResponseResponse(fmt.Sprintf("Sentence count validation failed: %d sentences found, expected between %d and %d sentences", sentenceCount, min, max), jsonPath, isResponse)
	}

	return policy.UpstreamResponseModifications{}
}

// buildErrorResponse builds an error response for request
func (p *SentenceCountGuardrailPolicy) buildErrorResponse(message, jsonPath string, isResponse bool) policy.RequestAction {
	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "SENTENCE_COUNT_GUARDRAIL",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "SentenceCountGuardrail",
			"direction":            "REQUEST",
			"actionReason":         "Violation of applied sentence count constraints detected.",
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
func (p *SentenceCountGuardrailPolicy) buildErrorResponseResponse(message, jsonPath string, isResponse bool) policy.ResponseAction {
	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "SENTENCE_COUNT_GUARDRAIL",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "SentenceCountGuardrail",
			"direction":            "RESPONSE",
			"actionReason":         "Violation of applied sentence count constraints detected.",
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
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(v), nil
	default:
		return fmt.Sprintf("%v", v), nil
	}
}


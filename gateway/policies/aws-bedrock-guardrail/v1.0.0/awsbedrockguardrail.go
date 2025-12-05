package awsbedrockguardrail

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	policy "github.com/policy-engine/sdk/policy/v1alpha"
)

var (
	textCleanRegex = regexp.MustCompile(`^"|"$`)
)

// AWSBedrockGuardrailPolicy implements AWS Bedrock Guardrail validation
// NOTE: This is a simplified implementation. Full implementation requires AWS SDK dependencies
type AWSBedrockGuardrailPolicy struct{}

// NewPolicy creates a new AWSBedrockGuardrailPolicy instance
func NewPolicy() policy.Policy {
	return &AWSBedrockGuardrailPolicy{}
}

// Mode returns the processing mode for this policy
func (p *AWSBedrockGuardrailPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeBuffer,
	}
}

// Validate validates the policy configuration
func (p *AWSBedrockGuardrailPolicy) Validate(params map[string]interface{}) error {
	// Validate required parameters
	if regionRaw, ok := params["region"]; ok {
		region, ok := regionRaw.(string)
		if !ok || region == "" {
			return fmt.Errorf("'region' must be a non-empty string")
		}
	} else {
		return fmt.Errorf("'region' parameter is required")
	}

	if guardrailIDRaw, ok := params["guardrailID"]; ok {
		guardrailID, ok := guardrailIDRaw.(string)
		if !ok || guardrailID == "" {
			return fmt.Errorf("'guardrailID' must be a non-empty string")
		}
	} else {
		return fmt.Errorf("'guardrailID' parameter is required")
	}

	if guardrailVersionRaw, ok := params["guardrailVersion"]; ok {
		guardrailVersion, ok := guardrailVersionRaw.(string)
		if !ok || guardrailVersion == "" {
			return fmt.Errorf("'guardrailVersion' must be a non-empty string")
		}
	} else {
		return fmt.Errorf("'guardrailVersion' parameter is required")
	}

	// Validate optional parameters
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		_, ok := jsonPathRaw.(string)
		if !ok {
			return fmt.Errorf("'jsonPath' must be a string")
		}
	}

	if redactPIIRaw, ok := params["redactPII"]; ok {
		_, ok := redactPIIRaw.(bool)
		if !ok {
			return fmt.Errorf("'redactPII' must be a boolean")
		}
	}

	if passthroughOnErrorRaw, ok := params["passthroughOnError"]; ok {
		_, ok := passthroughOnErrorRaw.(bool)
		if !ok {
			return fmt.Errorf("'passthroughOnError' must be a boolean")
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

// OnRequest performs AWS Bedrock Guardrail validation on request
func (p *AWSBedrockGuardrailPolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
	if ctx.Body == nil || !ctx.Body.Present || len(ctx.Body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}

	extractedValue := string(ctx.Body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(ctx.Body.Content, jsonPath)
		if err != nil {
			return p.buildErrorResponse("Error extracting value from JSON using JSONPath: "+err.Error(), false)
		}
	}

	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	// TODO: Implement actual AWS Bedrock Guardrail API call
	// This requires AWS SDK dependencies:
	// - github.com/aws/aws-sdk-go-v2/config
	// - github.com/aws/aws-sdk-go-v2/service/bedrockruntime
	// For now, this is a placeholder that always passes
	// In production, this should call AWS Bedrock Guardrail API

	return policy.UpstreamRequestModifications{}
}

// OnResponse performs AWS Bedrock Guardrail validation on response
func (p *AWSBedrockGuardrailPolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
	if ctx.ResponseBody == nil || !ctx.ResponseBody.Present || len(ctx.ResponseBody.Content) == 0 {
		return policy.UpstreamResponseModifications{}
	}

	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}

	extractedValue := string(ctx.ResponseBody.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(ctx.ResponseBody.Content, jsonPath)
		if err != nil {
			return p.buildErrorResponseResponse("Error extracting value from JSON using JSONPath: "+err.Error(), true)
		}
	}

	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	// TODO: Implement actual AWS Bedrock Guardrail API call
	// See OnRequest for implementation details

	return policy.UpstreamResponseModifications{}
}

// buildErrorResponse builds an error response for request
func (p *AWSBedrockGuardrailPolicy) buildErrorResponse(message string, isResponse bool) policy.RequestAction {
	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "AWS_BEDROCK_GUARDRAIL",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "AWSBedrockGuardrail",
			"direction":            "REQUEST",
			"actionReason":         "Violation of AWS Bedrock Guardrails detected.",
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
func (p *AWSBedrockGuardrailPolicy) buildErrorResponseResponse(message string, isResponse bool) policy.ResponseAction {
	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "AWS_BEDROCK_GUARDRAIL",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "AWSBedrockGuardrail",
			"direction":            "RESPONSE",
			"actionReason":         "Violation of AWS Bedrock Guardrails detected.",
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


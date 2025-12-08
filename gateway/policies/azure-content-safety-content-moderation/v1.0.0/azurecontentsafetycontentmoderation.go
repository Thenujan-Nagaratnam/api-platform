package azurecontentsafetycontentmoderation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	policy "github.com/wso2/api-platform/sdk/gateway/policy/v1alpha"
)

var (
	textCleanRegex = regexp.MustCompile(`^"|"$`)
)

const (
	azureContentSafetyAPIVersion = "2024-09-01"
	azureContentSafetyEndpoint   = "/contentsafety/text:analyze"
)

// AzureContentSafetyContentModerationPolicy implements Azure Content Safety validation
type AzureContentSafetyContentModerationPolicy struct {
	httpClient *http.Client
}

// NewPolicy creates a new AzureContentSafetyContentModerationPolicy instance
func NewPolicy() policy.Policy {
	return &AzureContentSafetyContentModerationPolicy{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Mode returns the processing mode for this policy
func (p *AzureContentSafetyContentModerationPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeBuffer,
	}
}

// Validate validates the policy configuration
func (p *AzureContentSafetyContentModerationPolicy) Validate(params map[string]interface{}) error {
	// Validate required parameters
	if endpointRaw, ok := params["azureContentSafetyEndpoint"]; ok {
		endpoint, ok := endpointRaw.(string)
		if !ok || endpoint == "" {
			return fmt.Errorf("'azureContentSafetyEndpoint' must be a non-empty string")
		}
	} else {
		return fmt.Errorf("'azureContentSafetyEndpoint' parameter is required")
	}

	if keyRaw, ok := params["azureContentSafetyKey"]; ok {
		_, ok := keyRaw.(string)
		if !ok {
			return fmt.Errorf("'azureContentSafetyKey' must be a string")
		}
	} else {
		return fmt.Errorf("'azureContentSafetyKey' parameter is required")
	}

	// Validate category thresholds (0-7 or -1 to disable)
	categories := []string{"hateCategory", "sexualCategory", "selfHarmCategory", "violenceCategory"}
	for _, cat := range categories {
		if catRaw, ok := params[cat]; ok {
			catVal, ok := catRaw.(float64)
			if !ok {
				return fmt.Errorf("'%s' must be a number", cat)
			}
			if catVal != -1 && (catVal < 0 || catVal > 7) {
				return fmt.Errorf("'%s' must be between 0-7 or -1 to disable", cat)
			}
		}
	}

	// Validate optional parameters
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		_, ok := jsonPathRaw.(string)
		if !ok {
			return fmt.Errorf("'jsonPath' must be a string")
		}
	}

	if passthroughRaw, ok := params["passthroughOnError"]; ok {
		_, ok := passthroughRaw.(bool)
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

// OnRequest performs Azure Content Safety validation on request
func (p *AzureContentSafetyContentModerationPolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
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

	return p.validateContent(ctx.Body, requestConfig, false)
}

// OnResponse performs Azure Content Safety validation on response
func (p *AzureContentSafetyContentModerationPolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
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

	return p.validateContentResponse(ctx.ResponseBody, responseConfig, true)
}

// validateContent validates content using Azure Content Safety API
func (p *AzureContentSafetyContentModerationPolicy) validateContent(body *policy.Body, params map[string]interface{}, isResponse bool) policy.RequestAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	endpoint := params["azureContentSafetyEndpoint"].(string)
	apiKey := params["azureContentSafetyKey"].(string)
	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}
	passthroughOnError := false
	if passthroughRaw, ok := params["passthroughOnError"]; ok {
		passthroughOnError = passthroughRaw.(bool)
	}

	extractedValue := string(body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(body.Content, jsonPath)
		if err != nil {
			return p.buildErrorResponse("Error extracting value from JSON using JSONPath: "+err.Error(), jsonPath, isResponse, passthroughOnError)
		}
	}

	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	// Build category map
	categoryMap := make(map[string]int)
	categories := []string{}
	for _, cat := range []struct {
		name string
		key  string
	}{
		{"Hate", "hateCategory"},
		{"Sexual", "sexualCategory"},
		{"SelfHarm", "selfHarmCategory"},
		{"Violence", "violenceCategory"},
	} {
		threshold := -1
		if catRaw, ok := params[cat.key]; ok {
			threshold = int(catRaw.(float64))
		}
		if threshold >= 0 && threshold <= 7 {
			categoryMap[cat.name] = threshold
			categories = append(categories, cat.name)
		}
	}

	if len(categories) == 0 {
		// No categories configured, allow through
		return policy.UpstreamRequestModifications{}
	}

	// Call Azure Content Safety API
	apiURL := strings.TrimSuffix(endpoint, "/") + azureContentSafetyEndpoint + "?api-version=" + azureContentSafetyAPIVersion
	requestBody := map[string]interface{}{
		"text":               extractedValue,
		"categories":         categories,
		"haltOnBlocklistHit": true,
		"outputType":         "EightSeverityLevels",
	}

	bodyBytes, _ := json.Marshal(requestBody)
	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		if passthroughOnError {
			return policy.UpstreamRequestModifications{}
		}
		return p.buildErrorResponse("Failed to create Azure API request: "+err.Error(), jsonPath, isResponse, passthroughOnError)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Ocp-Apim-Subscription-Key", apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		if passthroughOnError {
			return policy.UpstreamRequestModifications{}
		}
		return p.buildErrorResponse("Failed to call Azure Content Safety API: "+err.Error(), jsonPath, isResponse, passthroughOnError)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		if passthroughOnError {
			return policy.UpstreamRequestModifications{}
		}
		return p.buildErrorResponse(fmt.Sprintf("Azure API returned status %d: %s", resp.StatusCode, string(bodyBytes)), jsonPath, isResponse, passthroughOnError)
	}

	var responseBody map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&responseBody); err != nil {
		if passthroughOnError {
			return policy.UpstreamRequestModifications{}
		}
		return p.buildErrorResponse("Failed to decode Azure API response: "+err.Error(), jsonPath, isResponse, passthroughOnError)
	}

	categoriesAnalysis, ok := responseBody["categoriesAnalysis"].([]interface{})
	if !ok {
		if passthroughOnError {
			return policy.UpstreamRequestModifications{}
		}
		return p.buildErrorResponse("Invalid response format from Azure API", jsonPath, isResponse, passthroughOnError)
	}

	// Check for violations
	for _, item := range categoriesAnalysis {
		analysis, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		category, _ := analysis["category"].(string)
		severityFloat, _ := analysis["severity"].(float64)
		severity := int(severityFloat)
		threshold := categoryMap[category]
		if threshold >= 0 && severity >= threshold {
			return p.buildErrorResponse(fmt.Sprintf("Content safety violation detected: %s category severity %d exceeds threshold %d", category, severity, threshold), jsonPath, isResponse, passthroughOnError)
		}
	}

	return policy.UpstreamRequestModifications{}
}

// validateContentResponse validates response content
func (p *AzureContentSafetyContentModerationPolicy) validateContentResponse(body *policy.Body, params map[string]interface{}, isResponse bool) policy.ResponseAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamResponseModifications{}
	}

	endpoint := params["azureContentSafetyEndpoint"].(string)
	apiKey := params["azureContentSafetyKey"].(string)
	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}
	passthroughOnError := false
	if passthroughRaw, ok := params["passthroughOnError"]; ok {
		passthroughOnError = passthroughRaw.(bool)
	}

	extractedValue := string(body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(body.Content, jsonPath)
		if err != nil {
			return p.buildErrorResponseResponse("Error extracting value from JSON using JSONPath: "+err.Error(), jsonPath, isResponse, passthroughOnError)
		}
	}

	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	// Build category map
	categoryMap := make(map[string]int)
	categories := []string{}
	for _, cat := range []struct {
		name string
		key  string
	}{
		{"Hate", "hateCategory"},
		{"Sexual", "sexualCategory"},
		{"SelfHarm", "selfHarmCategory"},
		{"Violence", "violenceCategory"},
	} {
		threshold := -1
		if catRaw, ok := params[cat.key]; ok {
			threshold = int(catRaw.(float64))
		}
		if threshold >= 0 && threshold <= 7 {
			categoryMap[cat.name] = threshold
			categories = append(categories, cat.name)
		}
	}

	if len(categories) == 0 {
		return policy.UpstreamResponseModifications{}
	}

	// Call Azure Content Safety API
	apiURL := strings.TrimSuffix(endpoint, "/") + azureContentSafetyEndpoint + "?api-version=" + azureContentSafetyAPIVersion
	requestBody := map[string]interface{}{
		"text":               extractedValue,
		"categories":         categories,
		"haltOnBlocklistHit": true,
		"outputType":         "EightSeverityLevels",
	}

	bodyBytes, _ := json.Marshal(requestBody)
	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		if passthroughOnError {
			return policy.UpstreamResponseModifications{}
		}
		return p.buildErrorResponseResponse("Failed to create Azure API request: "+err.Error(), jsonPath, isResponse, passthroughOnError)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Ocp-Apim-Subscription-Key", apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		if passthroughOnError {
			return policy.UpstreamResponseModifications{}
		}
		return p.buildErrorResponseResponse("Failed to call Azure Content Safety API: "+err.Error(), jsonPath, isResponse, passthroughOnError)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		if passthroughOnError {
			return policy.UpstreamResponseModifications{}
		}
		return p.buildErrorResponseResponse(fmt.Sprintf("Azure API returned status %d: %s", resp.StatusCode, string(bodyBytes)), jsonPath, isResponse, passthroughOnError)
	}

	var responseBody map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&responseBody); err != nil {
		if passthroughOnError {
			return policy.UpstreamResponseModifications{}
		}
		return p.buildErrorResponseResponse("Failed to decode Azure API response: "+err.Error(), jsonPath, isResponse, passthroughOnError)
	}

	categoriesAnalysis, ok := responseBody["categoriesAnalysis"].([]interface{})
	if !ok {
		if passthroughOnError {
			return policy.UpstreamResponseModifications{}
		}
		return p.buildErrorResponseResponse("Invalid response format from Azure API", jsonPath, isResponse, passthroughOnError)
	}

	// Check for violations
	for _, item := range categoriesAnalysis {
		analysis, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		category, _ := analysis["category"].(string)
		severityFloat, _ := analysis["severity"].(float64)
		severity := int(severityFloat)
		threshold := categoryMap[category]
		if threshold >= 0 && severity >= threshold {
			return p.buildErrorResponseResponse(fmt.Sprintf("Content safety violation detected: %s category severity %d exceeds threshold %d", category, severity, threshold), jsonPath, isResponse, passthroughOnError)
		}
	}

	return policy.UpstreamResponseModifications{}
}

// buildErrorResponse builds an error response for request
func (p *AzureContentSafetyContentModerationPolicy) buildErrorResponse(message, jsonPath string, isResponse bool, passthroughOnError bool) policy.RequestAction {
	if passthroughOnError {
		return policy.UpstreamRequestModifications{}
	}

	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "AZURE_CONTENT_SAFETY_CONTENT_MODERATION",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "AzureContentSafetyContentModeration",
			"direction":            "REQUEST",
			"actionReason":         "Violation of applied Azure content safety constraints detected.",
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
func (p *AzureContentSafetyContentModerationPolicy) buildErrorResponseResponse(message, jsonPath string, isResponse bool, passthroughOnError bool) policy.ResponseAction {
	if passthroughOnError {
		return policy.UpstreamResponseModifications{}
	}

	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "AZURE_CONTENT_SAFETY_CONTENT_MODERATION",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "AzureContentSafetyContentModeration",
			"direction":            "RESPONSE",
			"actionReason":         "Violation of applied Azure content safety constraints detected.",
		},
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

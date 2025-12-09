package piimaskingguardrailsai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	policy "github.com/wso2/api-platform/sdk/gateway/policy/v1alpha"
)

const (
	APIMInternalErrorCode     = 500
	APIMInternalExceptionCode = 900967
	TextCleanRegex            = "^\"|\"$"
	MetadataKeyPIIEntities    = "piimaskingguardrailsai:pii_entities"
	piiServiceURL             = "https://db720294-98fd-40f4-85a1-cc6a3b65bc9a-prod.e1-us-east-azure.choreoapis.dev/godzilla/guardrails-pii/v1/validate"
	piiServiceAPIKeyHeader    = "api-key"
	piiServiceAPIKey          = "chk_eyJrZXkiOiJyNG51dW9uOGFmNngwbm9nMG04dmxrb2UzOTB1NnVmeXE0ZGljZGF1amV0cWlpYjN5cm45In0=gzX8sA"
	requestTimeout            = 30 * time.Second
	maxRetries                = 3
	retryDelay                = 1 * time.Second
)

var textCleanRegexCompiled = regexp.MustCompile(TextCleanRegex)

// PIIServiceRequest represents the request payload for the PII masking service
type PIIServiceRequest struct {
	Text        string   `json:"text"`
	Redact      bool     `json:"redact"`
	PiiEntities []string `json:"piiEntities"`
}

// PIIAssessment represents individual PII assessment from the service
type PIIAssessment struct {
	PiiEntity string `json:"piiEntity"`
	PiiValue  string `json:"piiValue"`
}

// PIIServiceResponse represents the response from the PII masking service
type PIIServiceResponse struct {
	AnonymizedText string          `json:"anonymizedText"`
	Assessment     []PIIAssessment `json:"assessment"`
}

// PIIMaskingGuardrailsAIPolicy implements REST-based PII masking
type PIIMaskingGuardrailsAIPolicy struct{}

// NewPolicy creates a new PIIMaskingGuardrailsAIPolicy instance
func NewPolicy() policy.Policy {
	return &PIIMaskingGuardrailsAIPolicy{}
}

// Mode returns the processing mode for this policy
func (p *PIIMaskingGuardrailsAIPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeProcess,
		ResponseBodyMode:   policy.BodyModeBuffer,
	}
}

// Validate validates the policy configuration (empty as requested)
func (p *PIIMaskingGuardrailsAIPolicy) Validate(params map[string]interface{}) error {
	// Validation logic moved to OnRequest/OnResponse
	return nil
}

// OnRequest masks PII in request body
func (p *PIIMaskingGuardrailsAIPolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
	// Extract request-specific parameters
	var requestParams map[string]interface{}
	if reqParams, ok := params["request"].(map[string]interface{}); ok {
		requestParams = reqParams
	} else {
		return policy.UpstreamRequestModifications{}
	}

	// Validate parameters
	if err := p.validateParams(requestParams); err != nil {
		return p.buildErrorResponse(fmt.Sprintf("parameter validation failed: %v", err)).(policy.RequestAction)
	}

	jsonPath, _ := requestParams["jsonPath"].(string)
	redactPII, _ := requestParams["redactPII"].(bool)

	// Parse PII entities
	piiEntities := p.parsePIIEntities(requestParams)
	if len(piiEntities) == 0 {
		// No PII entities configured, pass through
		return policy.UpstreamRequestModifications{}
	}

	payload := ctx.Body.Content
	if payload == nil {
		return policy.UpstreamRequestModifications{}
	}

	// Extract value using JSONPath
	extractedValue, err := extractStringValueFromJSONPath(payload, jsonPath)
	if err != nil {
		return p.buildErrorResponse(fmt.Sprintf("error extracting value from JSONPath: %v", err)).(policy.RequestAction)
	}

	// Clean and trim
	extractedValue = textCleanRegexCompiled.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	var modifiedContent string
	if redactPII {
		// Redaction mode: call service with redact=true
		modifiedContent, err = p.callPIIService(extractedValue, true, piiEntities, ctx.Metadata)
		if err != nil {
			return p.buildErrorResponse(fmt.Sprintf("error calling PII service: %v", err)).(policy.RequestAction)
		}
	} else {
		// Masking mode: call service and store mappings
		modifiedContent, err = p.callPIIService(extractedValue, false, piiEntities, ctx.Metadata)
		if err != nil {
			return p.buildErrorResponse(fmt.Sprintf("error calling PII service: %v", err)).(policy.RequestAction)
		}
	}

	// If content was modified, update the payload
	if modifiedContent != "" && modifiedContent != extractedValue {
		modifiedPayload := p.updatePayloadWithMaskedContent(payload, extractedValue, modifiedContent, jsonPath)
		return policy.UpstreamRequestModifications{
			Body: modifiedPayload,
		}
	}

	return policy.UpstreamRequestModifications{}
}

// OnResponse restores PII in response body (if redactPII is false)
func (p *PIIMaskingGuardrailsAIPolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
	// Extract response-specific parameters
	var responseParams map[string]interface{}
	if respParams, ok := params["response"].(map[string]interface{}); ok {
		responseParams = respParams
	} else {
		return policy.UpstreamResponseModifications{}
	}

	// Validate parameters (only redactPII is used in response)
	if redactPIIRaw, ok := responseParams["redactPII"]; ok {
		_, ok := redactPIIRaw.(bool)
		if !ok {
			return p.buildErrorResponse(fmt.Sprintf("parameter validation failed: 'redactPII' must be a boolean")).(policy.ResponseAction)
		}
	}

	redactPII, _ := responseParams["redactPII"].(bool)

	// If redactPII is true, no restoration needed
	if redactPII {
		return policy.UpstreamResponseModifications{}
	}

	// Check if PII entities were masked in request
	maskedPII, exists := ctx.Metadata[MetadataKeyPIIEntities]
	if !exists {
		return policy.UpstreamResponseModifications{}
	}

	maskedPIIMap, ok := maskedPII.(map[string]string)
	if !ok {
		return policy.UpstreamResponseModifications{}
	}

	payload := ctx.ResponseBody.Content
	if payload == nil {
		return policy.UpstreamResponseModifications{}
	}

	// Restore PII in response
	restoredContent := p.restorePIIInResponse(string(payload), maskedPIIMap)
	if restoredContent != string(payload) {
		return policy.UpstreamResponseModifications{
			Body: []byte(restoredContent),
		}
	}

	return policy.UpstreamResponseModifications{}
}

// parsePIIEntities parses PII entities from parameters
func (p *PIIMaskingGuardrailsAIPolicy) parsePIIEntities(params map[string]interface{}) []string {
	piiEntitiesRaw, ok := params["piiEntities"]
	if !ok {
		return []string{}
	}

	var piiEntities []string
	switch v := piiEntitiesRaw.(type) {
	case string:
		// Comma-separated string
		if v != "" {
			piiEntities = strings.Split(v, ",")
			// Trim whitespace from each entity
			for i, entity := range piiEntities {
				piiEntities[i] = strings.TrimSpace(entity)
			}
		}
	case []interface{}:
		// Array of strings
		for _, item := range v {
			if str, ok := item.(string); ok {
				piiEntities = append(piiEntities, str)
			}
		}
	case []string:
		piiEntities = v
	}

	return piiEntities
}

// validateParams validates the actual policy parameters
func (p *PIIMaskingGuardrailsAIPolicy) validateParams(params map[string]interface{}) error {
	// Validate piiEntities parameter (required)
	piiEntitiesRaw, ok := params["piiEntities"]
	if !ok {
		return fmt.Errorf("'piiEntities' parameter is required")
	}

	var piiEntities []string
	switch v := piiEntitiesRaw.(type) {
	case string:
		// Comma-separated string
		if v == "" {
			return fmt.Errorf("'piiEntities' cannot be empty")
		}
		piiEntities = strings.Split(v, ",")
		for i, entity := range piiEntities {
			piiEntities[i] = strings.TrimSpace(entity)
			if piiEntities[i] == "" {
				return fmt.Errorf("'piiEntities' contains empty entity")
			}
		}
	case []interface{}:
		// Array of strings
		if len(v) == 0 {
			return fmt.Errorf("'piiEntities' cannot be empty")
		}
		for i, item := range v {
			if str, ok := item.(string); ok {
				if str == "" {
					return fmt.Errorf("'piiEntities[%d]' cannot be empty", i)
				}
				piiEntities = append(piiEntities, str)
			} else {
				return fmt.Errorf("'piiEntities[%d]' must be a string", i)
			}
		}
	case []string:
		if len(v) == 0 {
			return fmt.Errorf("'piiEntities' cannot be empty")
		}
		for i, str := range v {
			if str == "" {
				return fmt.Errorf("'piiEntities[%d]' cannot be empty", i)
			}
		}
		piiEntities = v
	default:
		return fmt.Errorf("'piiEntities' must be a string or an array of strings")
	}

	if len(piiEntities) == 0 {
		return fmt.Errorf("'piiEntities' cannot be empty")
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

	return nil
}

// callPIIService makes HTTP request to PII service for masking
func (p *PIIMaskingGuardrailsAIPolicy) callPIIService(content string, redact bool, piiEntities []string, metadata map[string]interface{}) (string, error) {
	// Prepare request payload - always send redact: false to get original PII values
	requestPayload := PIIServiceRequest{
		Text:        content,
		Redact:      false, // Always false to get original PII values for local processing
		PiiEntities: piiEntities,
	}

	jsonData, err := json.Marshal(requestPayload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal PII service request: %w", err)
	}

	// Prepare headers
	headers := map[string]string{
		"Content-Type":         "application/json",
		piiServiceAPIKeyHeader: piiServiceAPIKey,
	}

	// Make HTTP request with retry
	var resp *http.Response
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(retryDelay)
		}

		resp, lastErr = p.makeHTTPRequest("POST", piiServiceURL, headers, jsonData)
		if lastErr == nil && resp.StatusCode == http.StatusOK {
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
	}

	if lastErr != nil {
		return "", fmt.Errorf("failed to call PII masking service after %d attempts: %w", maxRetries, lastErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("unexpected status code %d from PII service: %s", resp.StatusCode, string(bodyBytes))
	}

	// Parse response
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	var piiResponse PIIServiceResponse
	if err := json.Unmarshal(responseBody, &piiResponse); err != nil {
		return "", fmt.Errorf("failed to unmarshal PII service response: %w", err)
	}

	// Process the response based on redact flag
	processedContent := p.processPIIResponse(content, piiResponse, redact, metadata)

	return processedContent, nil
}

// makeHTTPRequest makes an HTTP request
func (p *PIIMaskingGuardrailsAIPolicy) makeHTTPRequest(method, url string, headers map[string]string, body []byte) (*http.Response, error) {
	client := &http.Client{
		Timeout: requestTimeout,
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// processPIIResponse processes the PII service response based on redact/mask mode
func (p *PIIMaskingGuardrailsAIPolicy) processPIIResponse(originalContent string, piiResponse PIIServiceResponse, redact bool, metadata map[string]interface{}) string {
	if len(piiResponse.Assessment) == 0 {
		return originalContent
	}

	processedContent := originalContent
	counter := 0

	if redact {
		// Redaction mode: replace all PII values with *****
		for _, assessment := range piiResponse.Assessment {
			if strings.Contains(processedContent, assessment.PiiValue) {
				processedContent = strings.ReplaceAll(processedContent, assessment.PiiValue, "*****")
			}
		}
	} else {
		// Masking mode: replace PII values with piiEntity + hexID and store mappings
		maskedPIIEntities := make(map[string]string)

		for _, assessment := range piiResponse.Assessment {
			if strings.Contains(processedContent, assessment.PiiValue) {
				// Generate unique placeholder like [PERSON_0001]
				placeholder := fmt.Sprintf("[%s_%04x]", strings.ToUpper(assessment.PiiEntity), counter)
				processedContent = strings.ReplaceAll(processedContent, assessment.PiiValue, placeholder)

				// Store original value for response restoration
				maskedPIIEntities[assessment.PiiValue] = placeholder
				counter++
			}
		}

		// Store PII mappings for response restoration
		if len(maskedPIIEntities) > 0 {
			metadata[MetadataKeyPIIEntities] = maskedPIIEntities
		}
	}

	return processedContent
}

// restorePIIInResponse handles PII restoration in responses when redactPII is disabled
func (p *PIIMaskingGuardrailsAIPolicy) restorePIIInResponse(originalContent string, maskedPIIEntities map[string]string) string {
	if maskedPIIEntities == nil || len(maskedPIIEntities) == 0 {
		return originalContent
	}

	transformedContent := originalContent

	// The map structure is originalValue -> placeholder, so we need to iterate and replace placeholder with original
	for original, placeholder := range maskedPIIEntities {
		if strings.Contains(transformedContent, placeholder) {
			transformedContent = strings.ReplaceAll(transformedContent, placeholder, original)
		}
	}

	return transformedContent
}

// updatePayloadWithMaskedContent updates the original payload by replacing the extracted content
func (p *PIIMaskingGuardrailsAIPolicy) updatePayloadWithMaskedContent(originalPayload []byte, extractedValue, modifiedContent string, jsonPath string) []byte {
	if jsonPath == "" {
		// If no JSONPath, the entire payload was processed, return the modified content
		return []byte(modifiedContent)
	}

	// If JSONPath is specified, update only the specific field in the JSON structure
	var jsonData map[string]interface{}
	if err := json.Unmarshal(originalPayload, &jsonData); err != nil {
		// Fallback to returning the modified content as-is
		return []byte(modifiedContent)
	}

	// Set the new value at the JSONPath location
	err := setValueAtJSONPath(jsonData, jsonPath, modifiedContent)
	if err != nil {
		// Fallback to returning the original payload
		return originalPayload
	}

	// Marshal back to JSON to get the full modified payload
	updatedPayload, err := json.Marshal(jsonData)
	if err != nil {
		// Fallback to returning the original payload
		return originalPayload
	}

	return updatedPayload
}

// setValueAtJSONPath sets a value at the specified JSONPath in the given JSON object
func setValueAtJSONPath(jsonData map[string]interface{}, jsonPath, value string) error {
	// Remove the leading "$." if present
	path := strings.TrimPrefix(jsonPath, "$.")
	if path == "" {
		return fmt.Errorf("invalid empty path")
	}

	// Split the path into components
	pathComponents := strings.Split(path, ".")

	// Navigate to the parent object/array
	current := interface{}(jsonData)
	arrayIndexRegex := regexp.MustCompile(`^([a-zA-Z0-9_]+)\[(-?\d+)\]$`)

	for i := 0; i < len(pathComponents)-1; i++ {
		key := pathComponents[i]

		// Check if this key contains array indexing
		if matches := arrayIndexRegex.FindStringSubmatch(key); len(matches) == 3 {
			arrayName := matches[1]
			idxStr := matches[2]
			idx := 0
			fmt.Sscanf(idxStr, "%d", &idx)

			if node, ok := current.(map[string]interface{}); ok {
				if arrVal, exists := node[arrayName]; exists {
					if arr, ok := arrVal.([]interface{}); ok {
						if idx < 0 {
							idx = len(arr) + idx
						}
						if idx < 0 || idx >= len(arr) {
							return fmt.Errorf("array index out of range: %s", idxStr)
						}
						current = arr[idx]
					} else {
						return fmt.Errorf("not an array: %s", arrayName)
					}
				} else {
					return fmt.Errorf("key not found: %s", arrayName)
				}
			} else {
				return fmt.Errorf("invalid structure for key: %s", arrayName)
			}
		} else {
			// Regular object key
			if node, ok := current.(map[string]interface{}); ok {
				if val, exists := node[key]; exists {
					current = val
				} else {
					return fmt.Errorf("key not found: %s", key)
				}
			} else {
				return fmt.Errorf("invalid structure for key: %s", key)
			}
		}
	}

	// Handle the final key (could be array index or object key)
	finalKey := pathComponents[len(pathComponents)-1]

	// Check if the final key contains array indexing
	if matches := arrayIndexRegex.FindStringSubmatch(finalKey); len(matches) == 3 {
		arrayName := matches[1]
		idxStr := matches[2]
		idx := 0
		fmt.Sscanf(idxStr, "%d", &idx)

		if node, ok := current.(map[string]interface{}); ok {
			if arrVal, exists := node[arrayName]; exists {
				if arr, ok := arrVal.([]interface{}); ok {
					if idx < 0 {
						idx = len(arr) + idx
					}
					if idx < 0 || idx >= len(arr) {
						return fmt.Errorf("array index out of range: %s", idxStr)
					}
					arr[idx] = value
				} else {
					return fmt.Errorf("not an array: %s", arrayName)
				}
			} else {
				return fmt.Errorf("key not found: %s", arrayName)
			}
		} else {
			return fmt.Errorf("invalid structure for key: %s", arrayName)
		}
	} else {
		// Regular object key
		if node, ok := current.(map[string]interface{}); ok {
			node[finalKey] = value
		} else {
			return fmt.Errorf("invalid structure for final key: %s", finalKey)
		}
	}

	return nil
}

// buildErrorResponse builds an error response for both request and response phases
func (p *PIIMaskingGuardrailsAIPolicy) buildErrorResponse(reason string) interface{} {
	responseBody := map[string]interface{}{
		"code":    APIMInternalExceptionCode,
		"message": "Error occurred during PIIMaskingGuardrailsAI mediation: " + reason,
	}

	bodyBytes, _ := json.Marshal(responseBody)

	// For PII masking, errors typically occur in request phase, but return as ImmediateResponse
	return policy.ImmediateResponse{
		StatusCode: APIMInternalErrorCode,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		Body: bodyBytes,
	}
}

// extractStringValueFromJSONPath extracts a value from JSON using JSONPath
func extractStringValueFromJSONPath(payload []byte, jsonPath string) (string, error) {
	if jsonPath == "" {
		return string(payload), nil
	}

	var jsonData map[string]interface{}
	if err := json.Unmarshal(payload, &jsonData); err != nil {
		return "", fmt.Errorf("error unmarshaling JSON: %w", err)
	}

	value, err := extractValueFromJSONPath(jsonData, jsonPath)
	if err != nil {
		return "", err
	}

	// Convert to string
	switch v := value.(type) {
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

// extractValueFromJSONPath extracts a value from a nested JSON structure based on a JSON path
func extractValueFromJSONPath(data map[string]interface{}, jsonPath string) (interface{}, error) {
	keys := strings.Split(jsonPath, ".")
	if len(keys) > 0 && keys[0] == "$" {
		keys = keys[1:]
	}

	return extractRecursive(data, keys)
}

func extractRecursive(current interface{}, keys []string) (interface{}, error) {
	if len(keys) == 0 {
		return current, nil
	}

	key := keys[0]
	remaining := keys[1:]

	// Handle array indexing
	arrayIndexRegex := regexp.MustCompile(`^([a-zA-Z0-9_]+)\[(-?\d+)\]$`)
	if matches := arrayIndexRegex.FindStringSubmatch(key); len(matches) == 3 {
		arrayName := matches[1]
		idxStr := matches[2]
		idx := 0
		fmt.Sscanf(idxStr, "%d", &idx)

		if node, ok := current.(map[string]interface{}); ok {
			if arrVal, exists := node[arrayName]; exists {
				if arr, ok := arrVal.([]interface{}); ok {
					if idx < 0 {
						idx = len(arr) + idx
					}
					if idx < 0 || idx >= len(arr) {
						return nil, fmt.Errorf("array index out of range: %d", idx)
					}
					return extractRecursive(arr[idx], remaining)
				}
				return nil, fmt.Errorf("not an array: %s", arrayName)
			}
			return nil, fmt.Errorf("key not found: %s", arrayName)
		}
		return nil, fmt.Errorf("invalid structure for key: %s", arrayName)
	}

	// Handle wildcard
	if key == "*" {
		var results []interface{}
		switch node := current.(type) {
		case map[string]interface{}:
			for _, v := range node {
				res, err := extractRecursive(v, remaining)
				if err == nil {
					results = append(results, res)
				}
			}
		case []interface{}:
			for _, v := range node {
				res, err := extractRecursive(v, remaining)
				if err == nil {
					results = append(results, res)
				}
			}
		default:
			return nil, fmt.Errorf("wildcard used on non-iterable node")
		}
		return results, nil
	}

	// Regular object key
	if node, ok := current.(map[string]interface{}); ok {
		if val, exists := node[key]; exists {
			return extractRecursive(val, remaining)
		}
		return nil, fmt.Errorf("key not found: %s", key)
	}

	return nil, fmt.Errorf("invalid structure for key: %s", key)
}

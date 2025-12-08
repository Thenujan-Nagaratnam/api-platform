package piimaskingguardrailsai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	policy "github.com/policy-engine/sdk/policy/v1alpha"
)

var (
	textCleanRegex = regexp.MustCompile(`^"|"$`)
)

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

// PIIMaskingGuardrailsAIPolicy implements PII masking using AI service
type PIIMaskingGuardrailsAIPolicy struct {
	httpClient *http.Client
}

// NewPolicy creates a new PIIMaskingGuardrailsAIPolicy instance
func NewPolicy() policy.Policy {
	return &PIIMaskingGuardrailsAIPolicy{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Mode returns the processing mode for this policy
func (p *PIIMaskingGuardrailsAIPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeBuffer,
	}
}

// Validate validates the policy configuration
func (p *PIIMaskingGuardrailsAIPolicy) Validate(params map[string]interface{}) error {
	// Validate required parameters
	if serviceURLRaw, ok := params["piiServiceURL"]; ok {
		_, ok := serviceURLRaw.(string)
		if !ok {
			return fmt.Errorf("'piiServiceURL' must be a string")
		}
	} else {
		return fmt.Errorf("'piiServiceURL' parameter is required")
	}

	if apiKeyRaw, ok := params["piiServiceAPIKey"]; ok {
		_, ok := apiKeyRaw.(string)
		if !ok {
			return fmt.Errorf("'piiServiceAPIKey' must be a string")
		}
	} else {
		return fmt.Errorf("'piiServiceAPIKey' parameter is required")
	}

	if piiEntitiesRaw, ok := params["piiEntities"]; ok {
		piiEntities, ok := piiEntitiesRaw.([]interface{})
		if !ok {
			return fmt.Errorf("'piiEntities' must be an array")
		}
		if len(piiEntities) == 0 {
			return fmt.Errorf("'piiEntities' cannot be empty")
		}
	} else {
		return fmt.Errorf("'piiEntities' parameter is required")
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

// OnRequest performs PII masking on request
func (p *PIIMaskingGuardrailsAIPolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
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

	return p.processBody(ctx.Body, requestConfig, false, ctx)
}

// OnResponse performs PII restoration/masking on response
func (p *PIIMaskingGuardrailsAIPolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
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

	return p.processBodyResponse(ctx.ResponseBody, responseConfig, true, ctx)
}

// processBody processes request body for PII masking
func (p *PIIMaskingGuardrailsAIPolicy) processBody(body *policy.Body, params map[string]interface{}, isResponse bool, ctx *policy.RequestContext) policy.RequestAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	serviceURL := params["piiServiceURL"].(string)
	apiKey := params["piiServiceAPIKey"].(string)
	piiEntitiesRaw := params["piiEntities"].([]interface{})
	piiEntities := make([]string, len(piiEntitiesRaw))
	for i, e := range piiEntitiesRaw {
		piiEntities[i] = e.(string)
	}

	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}
	redactPII := false
	if redactPIIRaw, ok := params["redactPII"]; ok {
		redactPII = redactPIIRaw.(bool)
	}

	extractedValue := string(body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(body.Content, jsonPath)
		if err != nil {
			extractedValue = string(body.Content)
		}
	}

	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	if extractedValue == "" {
		return policy.UpstreamRequestModifications{}
	}

	// Call PII service
	modifiedContent := p.callPIIService(extractedValue, piiEntities, redactPII, serviceURL, apiKey, ctx)

	if modifiedContent == extractedValue {
		return policy.UpstreamRequestModifications{}
	}

	modifiedPayload := p.updatePayloadWithMaskedContent(body.Content, extractedValue, modifiedContent, jsonPath)
	return policy.UpstreamRequestModifications{
		Body: modifiedPayload,
	}
}

// processBodyResponse processes response body
func (p *PIIMaskingGuardrailsAIPolicy) processBodyResponse(body *policy.Body, params map[string]interface{}, isResponse bool, ctx *policy.ResponseContext) policy.ResponseAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamResponseModifications{}
	}

	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}
	redactPII := false
	if redactPIIRaw, ok := params["redactPII"]; ok {
		redactPII = redactPIIRaw.(bool)
	}

	extractedValue := string(body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(body.Content, jsonPath)
		if err != nil {
			extractedValue = string(body.Content)
		}
	}

	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	if extractedValue == "" {
		return policy.UpstreamResponseModifications{}
	}

	// If not redacting and we have stored PII mappings, restore them
	if !redactPII {
		if piiMappingsRaw, ok := ctx.SharedContext.Metadata["piimaskingguardrailsai.pii.entities"]; ok {
			if piiMappings, ok := piiMappingsRaw.(map[string]string); ok {
				modifiedContent := p.restorePIIInResponse(extractedValue, piiMappings)
				if modifiedContent != extractedValue {
					modifiedPayload := p.updatePayloadWithMaskedContent(body.Content, extractedValue, modifiedContent, jsonPath)
					return policy.UpstreamResponseModifications{
						Body: modifiedPayload,
					}
				}
			}
		}
	}

	return policy.UpstreamResponseModifications{}
}

// callPIIService makes HTTP request to PII service
func (p *PIIMaskingGuardrailsAIPolicy) callPIIService(content string, piiEntities []string, redactPII bool, serviceURL, apiKey string, ctx *policy.RequestContext) string {
	requestPayload := PIIServiceRequest{
		Text:        content,
		Redact:      false, // Always false to get original PII values
		PiiEntities: piiEntities,
	}

	jsonData, err := json.Marshal(requestPayload)
	if err != nil {
		return content
	}

	req, err := http.NewRequest("POST", serviceURL, bytes.NewReader(jsonData))
	if err != nil {
		return content
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return content
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return content
	}

	var piiResponse PIIServiceResponse
	if err := json.NewDecoder(resp.Body).Decode(&piiResponse); err != nil {
		return content
	}

	return p.processPIIResponse(content, piiResponse, redactPII, ctx)
}

// processPIIResponse processes the PII service response
func (p *PIIMaskingGuardrailsAIPolicy) processPIIResponse(originalContent string, piiResponse PIIServiceResponse, redactPII bool, ctx *policy.RequestContext) string {
	if len(piiResponse.Assessment) == 0 {
		return originalContent
	}

	processedContent := originalContent
	counter := 0

	if redactPII {
		// Redaction mode: replace with *****
		for _, assessment := range piiResponse.Assessment {
			processedContent = strings.ReplaceAll(processedContent, assessment.PiiValue, "*****")
		}
	} else {
		// Masking mode: replace with placeholders
		maskedPIIEntities := make(map[string]string)
		for _, assessment := range piiResponse.Assessment {
			if strings.Contains(processedContent, assessment.PiiValue) {
				placeholder := fmt.Sprintf("[%s_%04x]", strings.ToUpper(assessment.PiiEntity), counter)
				processedContent = strings.ReplaceAll(processedContent, assessment.PiiValue, placeholder)
				maskedPIIEntities[assessment.PiiValue] = placeholder
				counter++
			}
		}

		// Store mappings in metadata
		if len(maskedPIIEntities) > 0 && ctx != nil {
			ctx.SharedContext.Metadata["piimaskingguardrailsai.pii.entities"] = maskedPIIEntities
		}
	}

	return processedContent
}

// restorePIIInResponse restores PII in response content
func (p *PIIMaskingGuardrailsAIPolicy) restorePIIInResponse(content string, piiMappings map[string]string) string {
	if len(piiMappings) == 0 {
		return content
	}

	restoredContent := content
	for original, placeholder := range piiMappings {
		restoredContent = strings.ReplaceAll(restoredContent, placeholder, original)
	}

	return restoredContent
}

// updatePayloadWithMaskedContent updates the payload with masked content
func (p *PIIMaskingGuardrailsAIPolicy) updatePayloadWithMaskedContent(originalPayload []byte, extractedValue, modifiedContent string, jsonPath string) []byte {
	if jsonPath == "" {
		return []byte(modifiedContent)
	}

	var jsonData map[string]interface{}
	if err := json.Unmarshal(originalPayload, &jsonData); err != nil {
		return []byte(modifiedContent)
	}

	err := setValueAtJSONPath(jsonData, jsonPath, modifiedContent)
	if err != nil {
		return originalPayload
	}

	updatedPayload, err := json.Marshal(jsonData)
	if err != nil {
		return originalPayload
	}

	return updatedPayload
}

func setValueAtJSONPath(jsonData map[string]interface{}, jsonPath string, value string) error {
	keys := strings.Split(strings.TrimPrefix(jsonPath, "$."), ".")
	current := interface{}(jsonData)

	for i := 0; i < len(keys)-1; i++ {
		if m, ok := current.(map[string]interface{}); ok {
			if val, exists := m[keys[i]]; exists {
				current = val
			} else {
				return fmt.Errorf("key not found: %s", keys[i])
			}
		} else {
			return fmt.Errorf("invalid structure at key: %s", keys[i])
		}
	}

	if m, ok := current.(map[string]interface{}); ok {
		m[keys[len(keys)-1]] = value
		return nil
	}

	return fmt.Errorf("cannot set value at path: %s", jsonPath)
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

package piimaskingregex

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	policy "github.com/policy-engine/sdk/policy/v1alpha"
)

var (
	textCleanRegex = regexp.MustCompile(`^"|"$`)
)

// PIIMaskingRegexPolicy implements PII masking using regex patterns
type PIIMaskingRegexPolicy struct {
	piiEntities map[string]*regexp.Regexp
	patternMu   sync.RWMutex
}

// NewPolicy creates a new PIIMaskingRegexPolicy instance
func NewPolicy() policy.Policy {
	return &PIIMaskingRegexPolicy{
		piiEntities: make(map[string]*regexp.Regexp),
	}
}

// Mode returns the processing mode for this policy
func (p *PIIMaskingRegexPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeBuffer,
	}
}

// Validate validates the policy configuration
func (p *PIIMaskingRegexPolicy) Validate(params map[string]interface{}) error {
	// Validate piiEntities parameter
	piiEntitiesRaw, ok := params["piiEntities"]
	if !ok {
		return fmt.Errorf("'piiEntities' parameter is required")
	}

	piiEntitiesArray, ok := piiEntitiesRaw.([]interface{})
	if !ok {
		return fmt.Errorf("'piiEntities' must be an array")
	}

	if len(piiEntitiesArray) == 0 {
		return fmt.Errorf("'piiEntities' cannot be empty")
	}

	// Compile regex patterns
	p.patternMu.Lock()
	defer p.patternMu.Unlock()
	p.piiEntities = make(map[string]*regexp.Regexp)

	for i, item := range piiEntitiesArray {
		entityConfig, ok := item.(map[string]interface{})
		if !ok {
			return fmt.Errorf("'piiEntities[%d]' must be an object", i)
		}

		entityNameRaw, ok := entityConfig["piiEntity"]
		if !ok {
			return fmt.Errorf("'piiEntities[%d].piiEntity' is required", i)
		}
		entityName, ok := entityNameRaw.(string)
		if !ok || entityName == "" {
			return fmt.Errorf("'piiEntities[%d].piiEntity' must be a non-empty string", i)
		}

		regexRaw, ok := entityConfig["piiRegex"]
		if !ok {
			return fmt.Errorf("'piiEntities[%d].piiRegex' is required", i)
		}
		regexStr, ok := regexRaw.(string)
		if !ok || regexStr == "" {
			return fmt.Errorf("'piiEntities[%d].piiRegex' must be a non-empty string", i)
		}

		compiledRegex, err := regexp.Compile(regexStr)
		if err != nil {
			return fmt.Errorf("'piiEntities[%d].piiRegex' is invalid: %v", i, err)
		}

		p.piiEntities[entityName] = compiledRegex
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
func (p *PIIMaskingRegexPolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
	return p.processBody(ctx.Body, params, false, ctx)
}

// OnResponse performs PII restoration/masking on response
func (p *PIIMaskingRegexPolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
	return p.processBodyResponse(ctx.ResponseBody, params, true, ctx)
}

// processBody processes request body for PII masking
func (p *PIIMaskingRegexPolicy) processBody(body *policy.Body, params map[string]interface{}, isResponse bool, ctx *policy.RequestContext) policy.RequestAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
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
			// On error, continue with original content
			extractedValue = string(body.Content)
		}
	}

	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	if extractedValue == "" {
		return policy.UpstreamRequestModifications{}
	}

	// Process PII
	modifiedContent := p.maskPIIFromContent(extractedValue, redactPII, ctx)

	if modifiedContent == extractedValue {
		// No PII found, no modification needed
		return policy.UpstreamRequestModifications{}
	}

	// Update payload with masked content
	modifiedPayload := p.updatePayloadWithMaskedContent(body.Content, extractedValue, modifiedContent, jsonPath)

	return policy.UpstreamRequestModifications{
		Body: modifiedPayload,
	}
}

// processBodyResponse processes response body
func (p *PIIMaskingRegexPolicy) processBodyResponse(body *policy.Body, params map[string]interface{}, isResponse bool, ctx *policy.ResponseContext) policy.ResponseAction {
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
		if piiMappingsRaw, ok := ctx.SharedContext.Metadata["piimaskingregex.pii.entities"]; ok {
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

	// Otherwise, mask PII in response (no context needed for response-only masking)
	modifiedContent := p.maskPIIFromContentResponse(extractedValue, redactPII)
	if modifiedContent == extractedValue {
		return policy.UpstreamResponseModifications{}
	}

	modifiedPayload := p.updatePayloadWithMaskedContent(body.Content, extractedValue, modifiedContent, jsonPath)
	return policy.UpstreamResponseModifications{
		Body: modifiedPayload,
	}
}

// maskPIIFromContent masks PII from content using regex patterns
func (p *PIIMaskingRegexPolicy) maskPIIFromContent(content string, redactPII bool, ctx *policy.RequestContext) string {
	if content == "" {
		return content
	}

	p.patternMu.RLock()
	patterns := p.piiEntities
	p.patternMu.RUnlock()

	if len(patterns) == 0 {
		return content
	}

	maskedContent := content
	maskedPIIEntities := make(map[string]string)
	counter := 0

	if redactPII {
		// Redaction mode: replace with *****
		for _, pattern := range patterns {
			maskedContent = pattern.ReplaceAllString(maskedContent, "*****")
		}
	} else {
		// Masking mode: replace with placeholders
		allMatches := make(map[string]string) // original -> placeholder

		for entityName, pattern := range patterns {
			matches := pattern.FindAllString(maskedContent, -1)
			for _, match := range matches {
				if _, exists := allMatches[match]; !exists && !strings.Contains(match, "[") && !strings.Contains(match, "]") {
					placeholder := fmt.Sprintf("[%s_%04x]", strings.ToUpper(entityName), counter)
					allMatches[match] = placeholder
					maskedPIIEntities[match] = placeholder
					counter++
				}
			}
		}

		// Replace all matches
		for original, placeholder := range allMatches {
			maskedContent = strings.ReplaceAll(maskedContent, original, placeholder)
		}

		// Store mappings in metadata for response restoration
		if len(maskedPIIEntities) > 0 && ctx != nil {
			ctx.SharedContext.Metadata["piimaskingregex.pii.entities"] = maskedPIIEntities
		}
	}

	return maskedContent
}

// restorePIIInResponse restores PII in response content
func (p *PIIMaskingRegexPolicy) restorePIIInResponse(content string, piiMappings map[string]string) string {
	if len(piiMappings) == 0 {
		return content
	}

	restoredContent := content
	for original, placeholder := range piiMappings {
		restoredContent = strings.ReplaceAll(restoredContent, placeholder, original)
	}

	return restoredContent
}

// maskPIIFromContentResponse masks PII in response content (without storing metadata)
func (p *PIIMaskingRegexPolicy) maskPIIFromContentResponse(content string, redactPII bool) string {
	if content == "" {
		return content
	}

	p.patternMu.RLock()
	patterns := p.piiEntities
	p.patternMu.RUnlock()

	if len(patterns) == 0 {
		return content
	}

	maskedContent := content

	if redactPII {
		// Redaction mode: replace with *****
		for _, pattern := range patterns {
			maskedContent = pattern.ReplaceAllString(maskedContent, "*****")
		}
	} else {
		// Masking mode: replace with placeholders (but don't store mappings)
		counter := 0
		allMatches := make(map[string]string)

		for entityName, pattern := range patterns {
			matches := pattern.FindAllString(maskedContent, -1)
			for _, match := range matches {
				if _, exists := allMatches[match]; !exists && !strings.Contains(match, "[") && !strings.Contains(match, "]") {
					placeholder := fmt.Sprintf("[%s_%04x]", strings.ToUpper(entityName), counter)
					allMatches[match] = placeholder
					counter++
				}
			}
		}

		for original, placeholder := range allMatches {
			maskedContent = strings.ReplaceAll(maskedContent, original, placeholder)
		}
	}

	return maskedContent
}

// updatePayloadWithMaskedContent updates the payload with masked content
func (p *PIIMaskingRegexPolicy) updatePayloadWithMaskedContent(originalPayload []byte, extractedValue, modifiedContent string, jsonPath string) []byte {
	if jsonPath == "" {
		return []byte(modifiedContent)
	}

	// Update JSON at specific path
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

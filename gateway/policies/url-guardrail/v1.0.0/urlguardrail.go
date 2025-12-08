package urlguardrail

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	policy "github.com/wso2/api-platform/sdk/gateway/policy/v1alpha"
)

var (
	textCleanRegex = regexp.MustCompile(`^"|"$`)
	urlRegex       = regexp.MustCompile(`https?://[^\s,"'{}\[\]\\` + "`" + `*]+`)
)

// URLGuardrailPolicy implements URL validation
type URLGuardrailPolicy struct{}

// NewPolicy creates a new URLGuardrailPolicy instance
func NewPolicy() policy.Policy {
	return &URLGuardrailPolicy{}
}

// Mode returns the processing mode for this policy
func (p *URLGuardrailPolicy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeSkip,
		RequestBodyMode:    policy.BodyModeBuffer,
		ResponseHeaderMode: policy.HeaderModeSkip,
		ResponseBodyMode:   policy.BodyModeBuffer,
	}
}

// Validate validates the policy configuration
func (p *URLGuardrailPolicy) Validate(params map[string]interface{}) error {
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		_, ok := jsonPathRaw.(string)
		if !ok {
			return fmt.Errorf("'jsonPath' must be a string")
		}
	}

	if onlyDNSRaw, ok := params["onlyDNS"]; ok {
		_, ok := onlyDNSRaw.(bool)
		if !ok {
			return fmt.Errorf("'onlyDNS' must be a boolean")
		}
	}

	if timeoutRaw, ok := params["timeout"]; ok {
		switch v := timeoutRaw.(type) {
		case float64:
			if v < 0 {
				return fmt.Errorf("'timeout' cannot be negative")
			}
		case string:
			timeout, err := strconv.Atoi(v)
			if err != nil || timeout < 0 {
				return fmt.Errorf("'timeout' must be a non-negative number")
			}
		default:
			return fmt.Errorf("'timeout' must be a number")
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

// OnRequest performs URL validation on request
func (p *URLGuardrailPolicy) OnRequest(ctx *policy.RequestContext, params map[string]interface{}) policy.RequestAction {
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

	return p.validateURLs(ctx.Body, requestConfig, false)
}

// OnResponse performs URL validation on response
func (p *URLGuardrailPolicy) OnResponse(ctx *policy.ResponseContext, params map[string]interface{}) policy.ResponseAction {
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

	return p.validateURLsResponse(ctx.ResponseBody, responseConfig, true)
}

// validateURLs validates URLs for request
func (p *URLGuardrailPolicy) validateURLs(body *policy.Body, params map[string]interface{}, isResponse bool) policy.RequestAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamRequestModifications{}
	}

	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}
	onlyDNS := false
	if onlyDNSRaw, ok := params["onlyDNS"]; ok {
		onlyDNS = onlyDNSRaw.(bool)
	}
	timeout := 3000
	if timeoutRaw, ok := params["timeout"]; ok {
		switch v := timeoutRaw.(type) {
		case float64:
			timeout = int(v)
		case string:
			if t, err := strconv.Atoi(v); err == nil {
				timeout = t
			}
		}
	}

	extractedValue := string(body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(body.Content, jsonPath)
		if err != nil {
			return p.buildErrorResponse("Error extracting value from JSON using JSONPath: "+err.Error(), nil, isResponse)
		}
	}

	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	urls := urlRegex.FindAllString(extractedValue, -1)
	invalidURLs := make([]string, 0)

	for _, urlStr := range urls {
		var valid bool
		if onlyDNS {
			valid = p.checkDNS(urlStr, timeout)
		} else {
			valid = p.checkURL(urlStr, timeout)
		}
		if !valid {
			invalidURLs = append(invalidURLs, urlStr)
		}
	}

	if len(invalidURLs) > 0 {
		return p.buildErrorResponse("One or more URLs failed validation", invalidURLs, isResponse)
	}

	return policy.UpstreamRequestModifications{}
}

// validateURLsResponse validates URLs for response
func (p *URLGuardrailPolicy) validateURLsResponse(body *policy.Body, params map[string]interface{}, isResponse bool) policy.ResponseAction {
	if body == nil || !body.Present || len(body.Content) == 0 {
		return policy.UpstreamResponseModifications{}
	}

	jsonPath := ""
	if jsonPathRaw, ok := params["jsonPath"]; ok {
		jsonPath = jsonPathRaw.(string)
	}
	onlyDNS := false
	if onlyDNSRaw, ok := params["onlyDNS"]; ok {
		onlyDNS = onlyDNSRaw.(bool)
	}
	timeout := 3000
	if timeoutRaw, ok := params["timeout"]; ok {
		switch v := timeoutRaw.(type) {
		case float64:
			timeout = int(v)
		case string:
			if t, err := strconv.Atoi(v); err == nil {
				timeout = t
			}
		}
	}

	extractedValue := string(body.Content)
	if jsonPath != "" {
		var err error
		extractedValue, err = extractValueFromJSONPath(body.Content, jsonPath)
		if err != nil {
			return p.buildErrorResponseResponse("Error extracting value from JSON using JSONPath: "+err.Error(), nil, isResponse)
		}
	}

	extractedValue = textCleanRegex.ReplaceAllString(extractedValue, "")
	extractedValue = strings.TrimSpace(extractedValue)

	urls := urlRegex.FindAllString(extractedValue, -1)
	invalidURLs := make([]string, 0)

	for _, urlStr := range urls {
		var valid bool
		if onlyDNS {
			valid = p.checkDNS(urlStr, timeout)
		} else {
			valid = p.checkURL(urlStr, timeout)
		}
		if !valid {
			invalidURLs = append(invalidURLs, urlStr)
		}
	}

	if len(invalidURLs) > 0 {
		return p.buildErrorResponseResponse("One or more URLs failed validation", invalidURLs, isResponse)
	}

	return policy.UpstreamResponseModifications{}
}

// checkDNS checks if the URL is resolved via DNS
func (p *URLGuardrailPolicy) checkDNS(target string, timeout int) bool {
	parsedURL, err := url.Parse(target)
	if err != nil {
		return false
	}

	host := parsedURL.Hostname()
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{
				Timeout: time.Duration(timeout) * time.Millisecond,
			}
			return d.DialContext(ctx, network, address)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Millisecond)
	defer cancel()

	ips, err := resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return false
	}

	return len(ips) > 0
}

// checkURL checks if the URL is reachable via HTTP HEAD request
func (p *URLGuardrailPolicy) checkURL(target string, timeout int) bool {
	client := &http.Client{
		Timeout: time.Duration(timeout) * time.Millisecond,
	}

	req, err := http.NewRequest("HEAD", target, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "URLValidator/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

// buildErrorResponse builds an error response for request
func (p *URLGuardrailPolicy) buildErrorResponse(message string, invalidURLs []string, isResponse bool) policy.RequestAction {
	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "URL_GUARDRAIL",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "URLGuardrail",
			"direction":            "REQUEST",
			"actionReason":         "Violation of url validity detected.",
		},
	}

	if invalidURLs != nil && len(invalidURLs) > 0 {
		if msg, ok := responseBody["message"].(map[string]interface{}); ok {
			msg["assessments"] = map[string]interface{}{
				"message":     message,
				"invalidUrls": invalidURLs,
			}
		}
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
func (p *URLGuardrailPolicy) buildErrorResponseResponse(message string, invalidURLs []string, isResponse bool) policy.ResponseAction {
	responseBody := map[string]interface{}{
		"code": 900514,
		"type": "URL_GUARDRAIL",
		"message": map[string]interface{}{
			"action":               "GUARDRAIL_INTERVENED",
			"interveningGuardrail": "URLGuardrail",
			"direction":            "RESPONSE",
			"actionReason":         "Violation of url validity detected.",
		},
	}

	if invalidURLs != nil && len(invalidURLs) > 0 {
		if msg, ok := responseBody["message"].(map[string]interface{}); ok {
			msg["assessments"] = map[string]interface{}{
				"message":     message,
				"invalidUrls": invalidURLs,
			}
		}
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

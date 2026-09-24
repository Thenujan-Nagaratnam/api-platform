/*
 *  Copyright (c) 2026, WSO2 LLC. (http://www.wso2.com) All Rights Reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 */

package policyv1alpha2

import (
	"encoding/json"
	"net/http"
)

// Gateway conventions for common OpenAI-compatible HTTP error "type" values.
// This is not an exhaustive OpenAI vocabulary: clients branch on the HTTP status
// first and may also inspect endpoint-specific type and code values.
const (
	OpenAIErrorTypeInvalidRequest = "invalid_request_error"
	OpenAIErrorTypeAuthentication = "authentication_error"
	OpenAIErrorTypePermission     = "permission_error"
	OpenAIErrorTypeNotFound       = "not_found_error"
	OpenAIErrorTypeRateLimit      = "rate_limit_error"
	OpenAIErrorTypeServer         = "server_error"
)

// IsLLM reports whether the API kind fronts an LLM (LlmProvider or LlmProxy).
// Clients of these APIs expect OpenAI-compatible error bodies, so policies shared
// with other API kinds use this to pick the error format.
func (k APIKind) IsLLM() bool {
	return k == APIKindLlmProvider || k == APIKindLlmProxy
}

// IsLLMAPI reports whether the request being processed belongs to an LLM API.
// Safe to call on a nil receiver (e.g. a context built without a SharedContext),
// in which case it reports false.
func (c *SharedContext) IsLLMAPI() bool {
	return c != nil && c.APIKind.IsLLM()
}

// OpenAIError is the "error" object of the conventional OpenAI-compatible
// non-streaming HTTP error envelope:
//
//	{"error": {"message": "...", "type": "...", "param": null, "code": null}}
//
// Param and Code are rendered as JSON null when empty, matching OpenAI.
// Guardrail is a gateway extension carrying a guardrail's intervention details;
// OpenAI clients ignore unknown fields, so it is safe to include.
type OpenAIError struct {
	Message   string
	Type      string // empty: derived from the HTTP status via OpenAIErrorTypeForStatus
	Param     string
	Code      string
	Guardrail map[string]any
}

type openAIErrorJSON struct {
	Message   string         `json:"message"`
	Type      string         `json:"type"`
	Param     *string        `json:"param"`
	Code      *string        `json:"code"`
	Guardrail map[string]any `json:"guardrail,omitempty"`
}

// OpenAIErrorTypeForStatus maps an HTTP status to the OpenAI error type a client
// would expect alongside it.
func OpenAIErrorTypeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return OpenAIErrorTypeAuthentication
	case status == http.StatusForbidden:
		return OpenAIErrorTypePermission
	case status == http.StatusNotFound:
		return OpenAIErrorTypeNotFound
	case status == http.StatusTooManyRequests:
		return OpenAIErrorTypeRateLimit
	case status >= 500:
		return OpenAIErrorTypeServer
	default:
		return OpenAIErrorTypeInvalidRequest
	}
}

// GuardrailStatusCode is the status of a guardrail intervention on an LLM API.
// OpenAI reports content-policy refusals as 400 invalid_request_error.
const GuardrailStatusCode = http.StatusBadRequest

// NewOpenAIErrorBody renders e in the conventional OpenAI-compatible
// non-streaming HTTP error envelope.
func NewOpenAIErrorBody(status int, e OpenAIError) []byte {
	errType := e.Type
	if errType == "" {
		errType = OpenAIErrorTypeForStatus(status)
	}
	out := openAIErrorJSON{
		Message:   e.Message,
		Type:      errType,
		Guardrail: e.Guardrail,
	}
	if e.Param != "" {
		out.Param = &e.Param
	}
	if e.Code != "" {
		out.Code = &e.Code
	}
	body, err := json.Marshal(map[string]openAIErrorJSON{"error": out})
	if err != nil {
		// Only reachable when Guardrail holds a value json cannot encode; drop it
		// rather than lose the error envelope.
		out.Guardrail = nil
		body, _ = json.Marshal(map[string]openAIErrorJSON{"error": out})
	}
	return body
}

// GuardrailInterventionCode is the OpenAI error "code" for a request or response
// blocked by a guardrail.
const GuardrailInterventionCode = "guardrail_intervened"

// NewGuardrailOpenAIError converts a guardrail assessment object into an
// OpenAIError. Guardrails share one assessment shape:
//
//	{"action": ..., "interveningGuardrail": ..., "direction": ..., "actionReason": ..., "assessments": ...}
//
// actionReason becomes the error message; interveningGuardrail, direction and
// assessments (present only when the guardrail is configured to show them) are
// carried under the "guardrail" extension field as name, direction and assessments.
func NewGuardrailOpenAIError(assessment map[string]any) OpenAIError {
	message, _ := assessment["actionReason"].(string)
	details := map[string]any{}
	for from, to := range map[string]string{
		"interveningGuardrail": "name",
		"direction":            "direction",
		"assessments":          "assessments",
	} {
		if v, ok := assessment[from]; ok {
			details[to] = v
		}
	}
	return OpenAIError{
		Message:   message,
		Type:      OpenAIErrorTypeInvalidRequest,
		Code:      GuardrailInterventionCode,
		Guardrail: details,
	}
}

// NewOpenAIErrorResponse builds an ImmediateResponse carrying the conventional
// OpenAI-compatible non-streaming HTTP error envelope with a JSON content type.
// The caller's status is preserved because SDK classification and retry behavior
// are driven primarily by the HTTP status.
func NewOpenAIErrorResponse(status int, e OpenAIError) ImmediateResponse {
	return ImmediateResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       NewOpenAIErrorBody(status, e),
	}
}

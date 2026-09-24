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
	"testing"
)

func TestAPIKindIsLLM(t *testing.T) {
	for kind, want := range map[APIKind]bool{
		APIKindLlmProvider: true,
		APIKindLlmProxy:    true,
		APIKindRestApi:     false,
		APIKindMCP:         false,
		APIKindWebSubApi:   false,
		"":                 false,
	} {
		if got := kind.IsLLM(); got != want {
			t.Errorf("APIKind(%q).IsLLM() = %v, want %v", kind, got, want)
		}
	}
}

func TestOpenAIErrorTypeForStatus(t *testing.T) {
	for status, want := range map[int]string{
		400: OpenAIErrorTypeInvalidRequest,
		413: OpenAIErrorTypeInvalidRequest,
		422: OpenAIErrorTypeInvalidRequest,
		401: OpenAIErrorTypeAuthentication,
		403: OpenAIErrorTypePermission,
		404: OpenAIErrorTypeNotFound,
		429: OpenAIErrorTypeRateLimit,
		500: OpenAIErrorTypeServer,
		502: OpenAIErrorTypeServer,
		503: OpenAIErrorTypeServer,
	} {
		if got := OpenAIErrorTypeForStatus(status); got != want {
			t.Errorf("OpenAIErrorTypeForStatus(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestNewOpenAIErrorBody_NullsAndDerivedType(t *testing.T) {
	body := NewOpenAIErrorBody(401, OpenAIError{Message: "Invalid or expired credentials."})
	want := `{"error":{"message":"Invalid or expired credentials.","type":"authentication_error","param":null,"code":null}}`
	if string(body) != want {
		t.Fatalf("body = %s\nwant %s", body, want)
	}
}

func TestNewOpenAIErrorBody_AllFields(t *testing.T) {
	body := NewOpenAIErrorBody(422, OpenAIError{
		Message:   "blocked",
		Type:      "custom_type",
		Param:     "messages",
		Code:      "guardrail_intervened",
		Guardrail: map[string]any{"name": "regex-guardrail", "direction": "REQUEST"},
	})
	var got struct {
		Error struct {
			Message   string         `json:"message"`
			Type      string         `json:"type"`
			Param     *string        `json:"param"`
			Code      *string        `json:"code"`
			Guardrail map[string]any `json:"guardrail"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Error.Message != "blocked" || got.Error.Type != "custom_type" {
		t.Errorf("message/type = %q/%q", got.Error.Message, got.Error.Type)
	}
	if got.Error.Param == nil || *got.Error.Param != "messages" {
		t.Errorf("param = %v", got.Error.Param)
	}
	if got.Error.Code == nil || *got.Error.Code != "guardrail_intervened" {
		t.Errorf("code = %v", got.Error.Code)
	}
	if got.Error.Guardrail["name"] != "regex-guardrail" {
		t.Errorf("guardrail = %v", got.Error.Guardrail)
	}
}

func TestNewOpenAIErrorBody_UnencodableGuardrailKeepsEnvelope(t *testing.T) {
	body := NewOpenAIErrorBody(500, OpenAIError{Message: "x", Guardrail: map[string]any{"bad": make(chan int)}})
	want := `{"error":{"message":"x","type":"server_error","param":null,"code":null}}`
	if string(body) != want {
		t.Fatalf("body = %s\nwant %s", body, want)
	}
}

func TestNewOpenAIErrorResponse(t *testing.T) {
	resp := NewOpenAIErrorResponse(429, OpenAIError{Message: "slow down", Code: "rate_limit_exceeded"})
	if resp.StatusCode != 429 || resp.Headers["Content-Type"] != "application/json" {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestSharedContextIsLLMAPI_NilSafe(t *testing.T) {
	var nilCtx *SharedContext
	if nilCtx.IsLLMAPI() {
		t.Error("nil SharedContext should not be an LLM API")
	}
	rc := &RequestContext{} // embedded *SharedContext is nil
	if rc.IsLLMAPI() {
		t.Error("RequestContext without SharedContext should not be an LLM API")
	}
	rc.SharedContext = &SharedContext{APIKind: APIKindLlmProxy}
	if !rc.IsLLMAPI() {
		t.Error("LlmProxy request should be an LLM API")
	}
}

func TestNewGuardrailOpenAIError(t *testing.T) {
	e := NewGuardrailOpenAIError(map[string]any{
		"action":               "GUARDRAIL_INTERVENED",
		"interveningGuardrail": "regex-guardrail",
		"direction":            "REQUEST",
		"actionReason":         "Violation of regular expression detected.",
		"assessments":          "matched forbidden pattern",
	})
	body := NewOpenAIErrorBody(422, e)
	want := `{"error":{"message":"Violation of regular expression detected.","type":"invalid_request_error","param":null,"code":"guardrail_intervened","guardrail":{"assessments":"matched forbidden pattern","direction":"REQUEST","name":"regex-guardrail"}}}`
	if string(body) != want {
		t.Fatalf("body = %s\nwant %s", body, want)
	}

	// assessments omitted when the guardrail does not show them
	e = NewGuardrailOpenAIError(map[string]any{"interveningGuardrail": "x", "direction": "RESPONSE", "actionReason": "r"})
	if _, ok := e.Guardrail["assessments"]; ok {
		t.Error("assessments should be absent")
	}
}

func TestNewOpenAIErrorResponsePreservesStatus(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 408, 409, 413, 415, 422, 429, 500, 502, 503, 504} {
		if resp := NewOpenAIErrorResponse(status, OpenAIError{Message: "x"}); resp.StatusCode != status {
			t.Errorf("NewOpenAIErrorResponse(%d) status = %d", status, resp.StatusCode)
		}
	}
}

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

package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"

	"github.com/wso2/api-platform/gateway/gateway-runtime/policy-engine/internal/resolver"
)

type openAIErrorEnvelope struct {
	Error struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    *string `json:"code"`
	} `json:"error"`
}

func decodeOpenAIError(t *testing.T, body []byte) openAIErrorEnvelope {
	t.Helper()
	var env openAIErrorEnvelope
	require.NoError(t, json.Unmarshal(body, &env), "body: %s", body)
	return env
}

func TestEngineErrorBody_NonLLMShapeUnchanged(t *testing.T) {
	assert.Equal(t, `{"error":"Internal Server Error","error_id":"abc-123"}`,
		string(engineErrorBody(false, 500, "Internal Server Error", "abc-123")))
	assert.Equal(t, `{"error":"Internal Server Error"}`,
		string(engineErrorBody(false, 500, "Internal Server Error", "")))
}

func TestEngineErrorBody_LLMIsOpenAICompatible(t *testing.T) {
	env := decodeOpenAIError(t, engineErrorBody(true, 413, "Payload Too Large", "abc-123"))
	assert.Equal(t, "Payload Too Large (error_id: abc-123)", env.Error.Message)
	assert.Equal(t, "invalid_request_error", env.Error.Type)
	assert.Nil(t, env.Error.Param)
	assert.Nil(t, env.Error.Code)

	env = decodeOpenAIError(t, engineErrorBody(true, 500, "Internal Server Error", ""))
	assert.Equal(t, "Internal Server Error", env.Error.Message)
	assert.Equal(t, "server_error", env.Error.Type)
}

func TestIsLLMKind(t *testing.T) {
	assert.True(t, isLLMKind("LlmProvider"))
	assert.True(t, isLLMKind("LlmProxy"))
	assert.False(t, isLLMKind("RestApi"))
	assert.False(t, isLLMKind(""))
}

func TestGenericResolutionFailure_LLMRoute(t *testing.T) {
	out := genericResolutionFailure(resolver.FailureUnknownOperation, "abc-123", true)
	assert.Equal(t, 404, out.StatusCode)
	assert.Equal(t, "abc-123", out.Headers["x-error-id"])
	env := decodeOpenAIError(t, out.Body)
	assert.Equal(t, "not_found_error", env.Error.Type)
	assert.Contains(t, env.Error.Message, "abc-123")
}

func TestHandlePolicyError_BodyFollowsAPIKind(t *testing.T) {
	for kind, wantOpenAI := range map[policy.APIKind]bool{
		policy.APIKindLlmProvider: true,
		policy.APIKindLlmProxy:    true,
		policy.APIKindRestApi:     false,
	} {
		ec := &PolicyExecutionContext{sharedCtx: &policy.SharedContext{APIKind: kind}}
		resp := ec.handlePolicyError(context.Background(), errors.New("boom"), "request_headers")
		imm := resp.GetImmediateResponse()
		require.NotNil(t, imm, kind)
		assert.EqualValues(t, 500, imm.GetStatus().GetCode(), kind)

		var body map[string]any
		require.NoError(t, json.Unmarshal(imm.GetBody(), &body), kind)
		_, isOpenAI := body["error"].(map[string]any)
		assert.Equal(t, wantOpenAI, isOpenAI, "%s body: %s", kind, imm.GetBody())
		assert.NotContains(t, string(imm.GetBody()), "boom", "internal cause must never reach the client")
	}
}

func TestGenericResolutionFailure_DeploymentFaultsUseLLMEnvelope(t *testing.T) {
	for _, kind := range []resolver.FailureKind{resolver.FailureUnknownResolver, resolver.FailureChainMissing} {
		out := genericResolutionFailure(kind, "abc-123", true)
		env := decodeOpenAIError(t, out.Body)
		assert.Equal(t, "server_error", env.Error.Type, kind)
		assert.Contains(t, env.Error.Message, "abc-123", kind)
	}
}

func TestGenericResolutionFailure_LLMStatusIsPreserved(t *testing.T) {
	assert.Equal(t, 413, genericResolutionFailure(resolver.FailurePayloadTooLarge, "id", true).StatusCode)
	assert.Equal(t, 415, genericResolutionFailure(resolver.FailureUnsupportedEncoding, "id", true).StatusCode)
	assert.Equal(t, 413, genericResolutionFailure(resolver.FailurePayloadTooLarge, "id", false).StatusCode)
	assert.Equal(t, 415, genericResolutionFailure(resolver.FailureUnsupportedEncoding, "id", false).StatusCode)
}

func TestHandlePayloadTooLarge_StatusFollowsAPIKind(t *testing.T) {
	for kind, want := range map[policy.APIKind]int32{policy.APIKindLlmProxy: 413, policy.APIKindRestApi: 413} {
		ec := &PolicyExecutionContext{sharedCtx: &policy.SharedContext{APIKind: kind}}
		imm := ec.handlePayloadTooLarge(context.Background(), errors.New("too big"), "request_body").GetImmediateResponse()
		require.NotNil(t, imm, kind)
		assert.EqualValues(t, want, imm.GetStatus().GetCode(), kind)
		assert.EqualValues(t, want, ec.generated.outcome.StatusCode, "span outcome must match the status sent (%s)", kind)
	}
}

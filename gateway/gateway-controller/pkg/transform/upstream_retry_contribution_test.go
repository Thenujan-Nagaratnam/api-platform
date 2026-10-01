/*
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package transform

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

func TestResolveUpstreamRetryContribution_NilDef_ReturnsNil(t *testing.T) {
	got := resolveUpstreamRetryContribution(nil, "some-policy", nil, nil)
	assert.Nil(t, got)
}

func TestResolveUpstreamRetryContribution_FixedMode(t *testing.T) {
	def := &models.Retry{
		StatusCodes: []int{401},
		MaxAttempts: 2,
	}

	got := resolveUpstreamRetryContribution(def, "oauth2-generator", map[string]interface{}{}, nil)

	require.NotNil(t, got)
	assert.Equal(t, "oauth2-generator", got.PolicyName)
	assert.Equal(t, []int{401}, got.StatusCodes)
	assert.Equal(t, 2, got.MaxAttempts)
}

func TestResolveUpstreamRetryContribution_FromParam_InstanceValuePresent(t *testing.T) {
	def := &models.Retry{
		StatusCodesFromParam: "rateLimitStatusCode",
		MaxAttempts:          2,
	}
	instanceParams := map[string]interface{}{"rateLimitStatusCode": []interface{}{float64(503)}}

	got := resolveUpstreamRetryContribution(def, "p", instanceParams, nil)

	require.NotNil(t, got)
	assert.Equal(t, []int{503}, got.StatusCodes)
}

func TestResolveUpstreamRetryContribution_FromParam_InstanceAbsent_UsesSchemaDefault(t *testing.T) {
	def := &models.Retry{
		StatusCodesFromParam: "rateLimitStatusCode",
		MaxAttempts:          2,
	}
	paramSchema := map[string]interface{}{
		"properties": map[string]interface{}{
			"rateLimitStatusCode": map[string]interface{}{
				"type":    "array",
				"default": []interface{}{float64(429)},
			},
		},
	}

	got := resolveUpstreamRetryContribution(def, "p", map[string]interface{}{}, paramSchema)

	require.NotNil(t, got)
	assert.Equal(t, []int{429}, got.StatusCodes)
}

func TestResolveUpstreamRetryContribution_FromParam_NeitherPresent_FallsBackToFieldDefault(t *testing.T) {
	def := &models.Retry{
		StatusCodesFromParam: "rateLimitStatusCode",
		MaxAttempts:          2,
	}

	got := resolveUpstreamRetryContribution(def, "p", map[string]interface{}{}, nil)

	require.NotNil(t, got)
	assert.Empty(t, got.StatusCodes)
}

func TestResolveUpstreamRetryContribution_EnabledByParam_ResolvesFalse_ReturnsNil(t *testing.T) {
	def := &models.Retry{
		EnabledByParam: "autoRetry",
		StatusCodes:    []int{429},
		MaxAttempts:    2,
	}
	instanceParams := map[string]interface{}{"autoRetry": false}

	got := resolveUpstreamRetryContribution(def, "p", instanceParams, nil)

	assert.Nil(t, got, "a disabled contributor must produce no contribution at all, not a partial one")
}

func TestResolveUpstreamRetryContribution_EnabledByParam_ResolvesTrue_ProducesContribution(t *testing.T) {
	def := &models.Retry{
		EnabledByParam: "autoRetry",
		StatusCodes:    []int{429},
		MaxAttempts:    2,
	}
	instanceParams := map[string]interface{}{"autoRetry": true}

	got := resolveUpstreamRetryContribution(def, "p", instanceParams, nil)

	require.NotNil(t, got)
	assert.Equal(t, []int{429}, got.StatusCodes)
}

func TestResolveUpstreamRetryContribution_MaxAttemptsFromLongestList(t *testing.T) {
	def := &models.Retry{
		StatusCodes: []int{503},
		MaxAttemptsFromLongestList: &models.LongestListAttempts{
			ParamPath: "fallbackTargets",
			Plus:      1,
		},
	}

	threeElement := resolveUpstreamRetryContribution(def, "p", map[string]interface{}{
		"fallbackTargets": []interface{}{"a", "b", "c"},
	}, nil)
	require.NotNil(t, threeElement)
	assert.Equal(t, 4, threeElement.MaxAttempts)

	oneElement := resolveUpstreamRetryContribution(def, "p", map[string]interface{}{
		"fallbackTargets": []interface{}{"a"},
	}, nil)
	require.NotNil(t, oneElement)
	assert.Equal(t, 2, oneElement.MaxAttempts)
}

func TestResolveUpstreamRetryContribution_MaxAttemptsFromLongestList_ParamAbsent_FallsBackToFieldDefault(t *testing.T) {
	def := &models.Retry{
		StatusCodes: []int{503},
		MaxAttemptsFromLongestList: &models.LongestListAttempts{
			ParamPath: "fallbackTargets",
			Plus:      1,
		},
	}

	got := resolveUpstreamRetryContribution(def, "p", map[string]interface{}{}, nil)

	require.NotNil(t, got)
	assert.Equal(t, 1, got.MaxAttempts)
}

func TestResolveUpstreamRetryContribution_SameDefinitionTwoAttachments_ResolvesIndependently(t *testing.T) {
	def := &models.Retry{
		StatusCodesFromParam: "rateLimitStatusCode",
		MaxAttempts:          2,
	}

	routeA := resolveUpstreamRetryContribution(def, "p", map[string]interface{}{
		"rateLimitStatusCode": []interface{}{float64(429)},
	}, nil)
	routeB := resolveUpstreamRetryContribution(def, "p", map[string]interface{}{
		"rateLimitStatusCode": []interface{}{float64(503)},
	}, nil)

	require.NotNil(t, routeA)
	require.NotNil(t, routeB)
	assert.Equal(t, []int{429}, routeA.StatusCodes)
	assert.Equal(t, []int{503}, routeB.StatusCodes)
}

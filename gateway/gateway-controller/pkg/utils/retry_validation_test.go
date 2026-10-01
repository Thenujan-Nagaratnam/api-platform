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

package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// basePolicy returns a minimal, otherwise-valid policy definition with a
// parameters schema declaring one param of each type these tests reference.
func basePolicy(retry *models.Retry) *models.PolicyDefinition {
	params := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"rateLimitStatusCode": map[string]interface{}{"type": "array"},
			"autoRetry":           map[string]interface{}{"type": "boolean"},
			"maxTries":            map[string]interface{}{"type": "integer"},
			"timeoutMs":           map[string]interface{}{"type": "integer"},
			"fallbackTargets":     map[string]interface{}{"type": "array"},
			"wrongType":           map[string]interface{}{"type": "string"},
		},
	}
	return &models.PolicyDefinition{
		Name:       "test-policy",
		Version:    "v1.0.0",
		Parameters: &params,
		Retry:      retry,
	}
}

func TestValidateRetry_NilRetry_Valid(t *testing.T) {
	err := validateRetry(basePolicy(nil))
	assert.NoError(t, err)
}

func TestValidateRetry_Rule1_BothStatusCodeModesSet_Rejected(t *testing.T) {
	err := validateRetry(basePolicy(&models.Retry{
		StatusCodes:          []int{401},
		StatusCodesFromParam: "rateLimitStatusCode",
		MaxAttempts:          2,
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "statusCodes")
}

func TestValidateRetry_Rule1_MoreThanOneMaxAttemptsModeSet_Rejected(t *testing.T) {
	err := validateRetry(basePolicy(&models.Retry{
		StatusCodes:          []int{401},
		MaxAttempts:          2,
		MaxAttemptsFromParam: "maxTries",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxAttempts")
}

func TestValidateRetry_Rule2_FromParamReferencesUndeclaredParam_Rejected(t *testing.T) {
	err := validateRetry(basePolicy(&models.Retry{
		StatusCodesFromParam: "doesNotExist",
		MaxAttempts:          2,
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "doesNotExist")
}

func TestValidateRetry_Rule3_FromParamTypeMismatch_Rejected(t *testing.T) {
	err := validateRetry(basePolicy(&models.Retry{
		StatusCodesFromParam: "wrongType", // declared as "string", statusCodes needs "array"
		MaxAttempts:          2,
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wrongType")
}

func TestValidateRetry_Rule4_NeitherStatusCodeModeSet_Rejected(t *testing.T) {
	err := validateRetry(basePolicy(&models.Retry{
		MaxAttempts: 2,
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "statusCodes")
}

func TestValidateRetry_Rule4_NoMaxAttemptsModeSet_Rejected(t *testing.T) {
	err := validateRetry(basePolicy(&models.Retry{
		StatusCodes: []int{401},
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxAttempts")
}

func TestValidateRetry_Rule5_FixedMaxAttemptsNegative_Rejected(t *testing.T) {
	err := validateRetry(basePolicy(&models.Retry{
		StatusCodes: []int{401},
		MaxAttempts: -1,
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxAttempts")
}

func TestValidateRetry_Rule5_FixedPerAttemptTimeoutNegative_Rejected(t *testing.T) {
	err := validateRetry(basePolicy(&models.Retry{
		StatusCodes:         []int{401},
		MaxAttempts:         2,
		PerAttemptTimeoutMs: -1,
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "perAttemptTimeoutMs")
}

func TestValidateRetry_Rule5_LongestListPlusNegative_Rejected(t *testing.T) {
	err := validateRetry(basePolicy(&models.Retry{
		StatusCodes: []int{401},
		MaxAttemptsFromLongestList: &models.LongestListAttempts{
			ParamPath: "fallbackTargets",
			Plus:      -1,
		},
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plus")
}

func TestValidateRetry_ValidFixedMode_Accepted(t *testing.T) {
	err := validateRetry(basePolicy(&models.Retry{
		StatusCodes: []int{401},
		MaxAttempts: 2,
	}))
	assert.NoError(t, err)
}

func TestValidateRetry_ValidDerivedMode_Accepted(t *testing.T) {
	err := validateRetry(basePolicy(&models.Retry{
		EnabledByParam:               "autoRetry",
		StatusCodesFromParam:         "rateLimitStatusCode",
		MaxAttemptsFromParam:         "maxTries",
		PerAttemptTimeoutMsFromParam: "timeoutMs",
	}))
	assert.NoError(t, err)
}

func TestValidateRetry_ValidMaxAttemptsFromLongestList_Accepted(t *testing.T) {
	err := validateRetry(basePolicy(&models.Retry{
		StatusCodes: []int{503},
		MaxAttemptsFromLongestList: &models.LongestListAttempts{
			ParamPath: "fallbackTargets",
			Plus:      1,
		},
	}))
	assert.NoError(t, err)
}

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

package config

import (
	"fmt"
	"sort"
)

// modelFailoverPolicyName is kept in lockstep with the constant of the same
// name in pkg/transform and pkg/utils (this package cannot import either).
const modelFailoverPolicyName = "model-failover"

// LLM operation policy params are only checked for name and version, never
// against the policy's schema, because the controller merges template and
// routing values into them later. model-failover's authored keys are checked
// here instead, so a typo such as perTryTimeoutMs is rejected rather than
// deployed as a silent no-op.
var (
	modelFailoverParamKeys = map[string]bool{
		"targets":               true,
		"statusCodes":           true,
		"suspendDuration":       true,
		"suspendAfterFailures":  true,
		"maxSuspendMultiplier":  true,
		"recoverAfterSuccesses": true,
		"perTryTimeout":         true,
	}
	modelFailoverTargetKeys = map[string]bool{
		"model":     true,
		"provider":  true,
		"fallbacks": true,
	}
	modelFailoverFallbackKeys = map[string]bool{
		"model":    true,
		"provider": true,
	}
)

// validateModelFailoverParamKeys rejects any authored model-failover key the
// policy does not define, at the top level, on a target or on a fallback.
func validateModelFailoverParamKeys(params map[string]interface{}, fieldPath string) []ValidationError {
	var errors []ValidationError
	errors = append(errors, unknownKeyErrors(params, modelFailoverParamKeys, fieldPath)...)

	targets, _ := params["targets"].([]interface{})
	for i, rawTarget := range targets {
		target, ok := rawTarget.(map[string]interface{})
		if !ok {
			continue
		}
		targetPath := fmt.Sprintf("%s.targets[%d]", fieldPath, i)
		errors = append(errors, unknownKeyErrors(target, modelFailoverTargetKeys, targetPath)...)

		fallbacks, _ := target["fallbacks"].([]interface{})
		for j, rawFallback := range fallbacks {
			fallback, ok := rawFallback.(map[string]interface{})
			if !ok {
				continue
			}
			errors = append(errors, unknownKeyErrors(fallback, modelFailoverFallbackKeys,
				fmt.Sprintf("%s.fallbacks[%d]", targetPath, j))...)
		}
	}
	return errors
}

func unknownKeyErrors(obj map[string]interface{}, allowed map[string]bool, fieldPath string) []ValidationError {
	var unknown []string
	for key := range obj {
		if !allowed[key] {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	errors := make([]ValidationError, 0, len(unknown))
	for _, key := range unknown {
		errors = append(errors, ValidationError{
			Field:   fieldPath + "." + key,
			Message: fmt.Sprintf("unknown %s parameter %q", modelFailoverPolicyName, key),
		})
	}
	return errors
}

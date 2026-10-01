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

import "github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"

// resolveUpstreamRetryContribution resolves one attached policy instance's
// Retry declaration into a concrete, per-attachment UpstreamRetryContribution.
// Returns nil when def is nil (the policy declares nothing), or when
// EnabledByParam resolves false for this attachment (a disabled contributor
// is indistinguishable from "no Retry block at all" — no partial result is
// produced). See specs/001-upstream-retry-contribution/data-model.md
// "Resolving a `...FromParam` field, and `enabledByParam`" for the
// three-step resolution order this implements for every field: (1) the
// instance's own configured param, (2) that param's own JSON-schema
// default, (3) this field's own fallback — never a deploy-time error.
func resolveUpstreamRetryContribution(def *models.Retry, policyName string, instanceParams map[string]interface{}, paramSchema map[string]interface{}) *models.UpstreamRetryContribution {
	if def == nil {
		return nil
	}

	if !resolveEnabledByParam(def.EnabledByParam, instanceParams, paramSchema) {
		return nil
	}

	return &models.UpstreamRetryContribution{
		PolicyName:                   policyName,
		StatusCodes:                  resolveStatusCodes(def, instanceParams, paramSchema),
		MaxAttempts:                  resolveMaxAttempts(def, instanceParams, paramSchema),
		PerAttemptTimeoutMs:          resolveIntField(def.PerAttemptTimeoutMs, def.PerAttemptTimeoutMsFromParam, 0, instanceParams, paramSchema),
		AlsoRetryOnConnectionFailure: resolveBoolField(def.AlsoRetryOnConnectionFailure, def.AlsoRetryOnConnectionFailureFromParam, false, instanceParams, paramSchema),
	}
}

// resolveEnabledByParam implements the one field whose own "fixed" form is
// implicit: an empty paramName means always on (step 3's field-level
// default), never a separate literal to set alongside it.
func resolveEnabledByParam(paramName string, instanceParams, paramSchema map[string]interface{}) bool {
	if paramName == "" {
		return true
	}
	return resolveBoolField(true, paramName, true, instanceParams, paramSchema)
}

func resolveStatusCodes(def *models.Retry, instanceParams, paramSchema map[string]interface{}) []int {
	if def.StatusCodesFromParam == "" {
		return def.StatusCodes
	}
	if v, ok := instanceParams[def.StatusCodesFromParam]; ok {
		if codes, ok := toIntSlice(v); ok {
			return codes
		}
	}
	if raw, ok := schemaPropertyDefault(paramSchema, def.StatusCodesFromParam); ok {
		if codes, ok := toIntSlice(raw); ok {
			return codes
		}
	}
	return nil // field's own default: no status codes resolved for this instance
}

// resolveMaxAttempts is the only field with three modes. A fixed value or a
// plain FromParam reference follow the same instance->schema-default->field-
// default order as every other field; FromLongestList additionally needs the
// referenced param to actually be list-shaped, falling through to the same
// field-level default otherwise.
//
// The field-level default (used only when every other source is
// unavailable) is 1 — "no extra attempts from this field" — not an error,
// matching this function's graceful-degradation contract: an unresolvable
// MaxAttempts reference never disqualifies the rest of this contribution or
// fails the deploy, it just adds no attempt budget of its own.
func resolveMaxAttempts(def *models.Retry, instanceParams, paramSchema map[string]interface{}) int {
	const fieldDefault = 1

	switch {
	case def.MaxAttemptsFromLongestList != nil:
		if v, ok := instanceParams[def.MaxAttemptsFromLongestList.ParamPath]; ok {
			if list, ok := v.([]interface{}); ok {
				return len(list) + def.MaxAttemptsFromLongestList.Plus
			}
		}
		return fieldDefault
	case def.MaxAttemptsFromParam != "":
		return resolveIntField(0, def.MaxAttemptsFromParam, fieldDefault, instanceParams, paramSchema)
	default:
		if def.MaxAttempts > 0 {
			return def.MaxAttempts
		}
		return fieldDefault
	}
}

// resolveIntField resolves a fixed-or-FromParam int field in the standard
// three-step order. fromParam == "" means "use the fixed value" (step-3
// equivalent handled by the caller already knowing which mode is set).
func resolveIntField(fixed int, fromParam string, fieldDefault int, instanceParams, paramSchema map[string]interface{}) int {
	if fromParam == "" {
		return fixed
	}
	if v, ok := instanceParams[fromParam]; ok {
		if n, ok := toInt(v); ok {
			return n
		}
	}
	if raw, ok := schemaPropertyDefault(paramSchema, fromParam); ok {
		if n, ok := toInt(raw); ok {
			return n
		}
	}
	return fieldDefault
}

// resolveBoolField resolves a fixed-or-FromParam bool field in the standard
// three-step order.
func resolveBoolField(fixed bool, fromParam string, fieldDefault bool, instanceParams, paramSchema map[string]interface{}) bool {
	if fromParam == "" {
		return fixed
	}
	if v, ok := instanceParams[fromParam]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	if raw, ok := schemaPropertyDefault(paramSchema, fromParam); ok {
		if b, ok := raw.(bool); ok {
			return b
		}
	}
	return fieldDefault
}

// schemaPropertyDefault reads properties.<name>.default out of a policy's
// JSON-schema-shaped Parameters map, mirroring the same schema-walking shape
// pkg/config/policy_validator.go's coerceParamsBySchema and
// pkg/resolver/policy_resolver.go's walkSchema already use elsewhere in this
// codebase for the same Parameters structure.
func schemaPropertyDefault(paramSchema map[string]interface{}, name string) (interface{}, bool) {
	if paramSchema == nil {
		return nil, false
	}
	properties, ok := paramSchema["properties"].(map[string]interface{})
	if !ok {
		return nil, false
	}
	propSchema, ok := properties[name].(map[string]interface{})
	if !ok {
		return nil, false
	}
	def, ok := propSchema["default"]
	return def, ok
}

func toIntSlice(v interface{}) ([]int, bool) {
	arr, ok := v.([]interface{})
	if !ok {
		return nil, false
	}
	result := make([]int, 0, len(arr))
	for _, item := range arr {
		n, ok := toInt(item)
		if !ok {
			return nil, false
		}
		result = append(result, n)
	}
	return result, true
}

// derefParams safely dereferences a PolicyDefinition's optional Parameters
// pointer, returning nil (not panicking) when it's unset.
func derefParams(params *map[string]interface{}) map[string]interface{} {
	if params == nil {
		return nil
	}
	return *params
}

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

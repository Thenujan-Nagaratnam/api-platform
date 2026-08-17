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
	"time"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// resolveConditionField reads one RetryConditions field's raw YAML value —
// either a literal (returned as-is) or a {fromParam: "<name>"} pointer
// (resolved against the as-deployed params map, falling back to the
// policy's own JSON-schema-declared default when the param key is entirely
// absent).
func resolveConditionField(raw interface{}, params map[string]interface{}, schema *map[string]interface{}) (interface{}, error) {
	ptr, isPointer := raw.(map[string]interface{})
	if !isPointer {
		return raw, nil
	}
	paramName, ok := ptr["fromParam"].(string)
	if !ok || paramName == "" {
		return nil, fmt.Errorf("retry-conditions field: {fromParam: ...} must name a non-empty param field")
	}
	if val, present := params[paramName]; present {
		return val, nil
	}
	return schemaFieldDefault(schema, paramName), nil
}

// schemaFieldDefault reads properties.<field>.default from a policy's
// JSON-schema parameters, returning nil if schema is nil or the path is
// absent/malformed at any level.
func schemaFieldDefault(schema *map[string]interface{}, field string) interface{} {
	if schema == nil {
		return nil
	}
	properties, ok := (*schema)["properties"].(map[string]interface{})
	if !ok {
		return nil
	}
	propSchema, ok := properties[field].(map[string]interface{})
	if !ok {
		return nil
	}
	return propSchema["default"]
}

// ParseRetryConditions parses one contributor's x-wso2-retry-conditions raw
// YAML block (or an equivalently-shaped block from resilience.retry — see
// Task 7) into a policy.RetryConditions, resolving every field through
// resolveConditionField. raw may be nil (a policy/operator contributing
// nothing), which yields the zero-value RetryConditions, not an error.
func ParseRetryConditions(raw map[string]interface{}, params map[string]interface{}, schema *map[string]interface{}) (*policy.RetryConditions, error) {
	rc := &policy.RetryConditions{}
	if raw == nil {
		return rc, nil
	}

	if v, ok := raw["on"]; ok {
		resolved, err := resolveConditionField(v, params, schema)
		if err != nil {
			return nil, fmt.Errorf("on: %w", err)
		}
		rc.On = toStringSlice(resolved)
	}
	if v, ok := raw["statusCodes"]; ok {
		resolved, err := resolveConditionField(v, params, schema)
		if err != nil {
			return nil, fmt.Errorf("statusCodes: %w", err)
		}
		codes, err := toIntSlice(resolved)
		if err != nil {
			return nil, fmt.Errorf("statusCodes: %w", err)
		}
		rc.StatusCodes = codes
	}
	if v, ok := raw["minAttempts"]; ok {
		resolved, err := resolveConditionField(v, params, schema)
		if err != nil {
			return nil, fmt.Errorf("minAttempts: %w", err)
		}
		n, err := toInt(resolved)
		if err != nil {
			return nil, fmt.Errorf("minAttempts: %w", err)
		}
		rc.MinAttempts = &n
	}
	if v, ok := raw["numRetries"]; ok {
		resolved, err := resolveConditionField(v, params, schema)
		if err != nil {
			return nil, fmt.Errorf("numRetries: %w", err)
		}
		n, err := toInt(resolved)
		if err != nil {
			return nil, fmt.Errorf("numRetries: %w", err)
		}
		rc.NumRetries = &n
	}
	if v, ok := raw["perTryTimeout"]; ok {
		resolved, err := resolveConditionField(v, params, schema)
		if err != nil {
			return nil, fmt.Errorf("perTryTimeout: %w", err)
		}
		d, err := toDuration(resolved)
		if err != nil {
			return nil, fmt.Errorf("perTryTimeout: %w", err)
		}
		rc.PerTryTimeout = &d
	}
	if v, ok := raw["avoidPreviousHosts"]; ok {
		resolved, err := resolveConditionField(v, params, schema)
		if err != nil {
			return nil, fmt.Errorf("avoidPreviousHosts: %w", err)
		}
		if b, ok := resolved.(bool); ok {
			rc.AvoidPreviousHosts = b
		}
	}
	if v, ok := raw["backOff"]; ok {
		boRaw, ok := v.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("backOff: expected an object")
		}
		base, err := toDuration(boRaw["baseInterval"])
		if err != nil {
			return nil, fmt.Errorf("backOff.baseInterval: %w", err)
		}
		bo := &policy.RetryBackOff{BaseInterval: base}
		if maxRaw, ok := boRaw["maxInterval"]; ok {
			maxD, err := toDuration(maxRaw)
			if err != nil {
				return nil, fmt.Errorf("backOff.maxInterval: %w", err)
			}
			bo.MaxInterval = &maxD
		}
		rc.BackOff = bo
	}

	return rc, nil
}

func toStringSlice(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toIntSlice(v interface{}) ([]int, error) {
	arr, ok := v.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected an array, got %T", v)
	}
	out := make([]int, 0, len(arr))
	for _, e := range arr {
		n, err := toInt(e)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func toInt(v interface{}) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	default:
		return 0, fmt.Errorf("expected an integer, got %T", v)
	}
}

func toDuration(v interface{}) (time.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("expected a duration string, got %T", v)
	}
	return time.ParseDuration(s)
}

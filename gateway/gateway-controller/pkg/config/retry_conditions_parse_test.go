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
	"testing"
	"time"
)

func TestResolveConditionField_Literal(t *testing.T) {
	got, err := resolveConditionField(2, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 2 {
		t.Errorf("expected literal passthrough, got %v", got)
	}
}

func TestResolveConditionField_FromParamPresent(t *testing.T) {
	raw := map[string]interface{}{"fromParam": "statusCodes"}
	params := map[string]interface{}{"statusCodes": []interface{}{401}}
	got, err := resolveConditionField(raw, params, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	codes, ok := got.([]interface{})
	if !ok || len(codes) != 1 || codes[0] != 401 {
		t.Errorf("expected params value to be read through, got %v", got)
	}
}

func TestResolveConditionField_FromParamAbsent_FallsBackToSchemaDefault(t *testing.T) {
	raw := map[string]interface{}{"fromParam": "statusCodes"}
	schema := &map[string]interface{}{
		"properties": map[string]interface{}{
			"statusCodes": map[string]interface{}{"default": []interface{}{401}},
		},
	}
	got, err := resolveConditionField(raw, map[string]interface{}{}, schema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	codes, ok := got.([]interface{})
	if !ok || len(codes) != 1 || codes[0] != 401 {
		t.Errorf("expected schema default fallback, got %v", got)
	}
}

func TestParseRetryConditions_FullShape(t *testing.T) {
	raw := map[string]interface{}{
		"statusCodes":   map[string]interface{}{"fromParam": "statusCodes"},
		"minAttempts":   2,
		"perTryTimeout": "5s",
	}
	params := map[string]interface{}{"statusCodes": []interface{}{401}}

	rc, err := ParseRetryConditions(raw, params, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.StatusCodes) != 1 || rc.StatusCodes[0] != 401 {
		t.Errorf("unexpected StatusCodes: %v", rc.StatusCodes)
	}
	if rc.MinAttempts == nil || *rc.MinAttempts != 2 {
		t.Errorf("unexpected MinAttempts: %v", rc.MinAttempts)
	}
	if rc.PerTryTimeout == nil || *rc.PerTryTimeout != 5*time.Second {
		t.Errorf("unexpected PerTryTimeout: %v", rc.PerTryTimeout)
	}
}

func TestParseRetryConditions_EmptyRawYieldsZeroValue(t *testing.T) {
	rc, err := ParseRetryConditions(nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rc.StatusCodes) != 0 || rc.MinAttempts != nil {
		t.Errorf("expected zero-value RetryConditions for nil raw block, got %+v", rc)
	}
}

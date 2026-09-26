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

package failover

import (
	"testing"
	"time"
)

func twoTargets() []interface{} {
	return []interface{}{
		map[string]interface{}{"provider": "a", "model": "m"},
		map[string]interface{}{"provider": "b", "model": "m"},
	}
}

func TestParseSettingsDefaultsAndDerivedValues(t *testing.T) {
	s, err := ParseSettings(map[string]interface{}{"targets": twoTargets()})
	if err != nil {
		t.Fatal(err)
	}
	if s.PerAttemptTimeout != 30*time.Second || !s.RetryOnTimeout {
		t.Fatalf("defaults: %+v", s)
	}
	if s.NumRetries() != 1 || s.FrontTimeout() != 62*time.Second {
		t.Fatalf("derived: retries=%d timeout=%s", s.NumRetries(), s.FrontTimeout())
	}
	if s.RetryOn() != "retriable-headers,connect-failure,reset" {
		t.Fatalf("retry_on: %s", s.RetryOn())
	}
}

func TestRetryOnWithoutTimeoutFailover(t *testing.T) {
	s, err := ParseSettings(map[string]interface{}{"targets": twoTargets(), "failoverOn": map[string]interface{}{"timeout": false}})
	if err != nil {
		t.Fatal(err)
	}
	if s.RetryOn() != "retriable-headers,connect-failure" {
		t.Fatalf("reset must be dropped when timeouts do not fail over: %s", s.RetryOn())
	}
}

func TestValidateParams(t *testing.T) {
	ok := []map[string]interface{}{
		{"targets": twoTargets()},
		{"targets": twoTargets(), "failoverOn": map[string]interface{}{"statusCodes": []interface{}{float64(429), float64(599)}}},
		{"targets": twoTargets(), "suspendDuration": "60m", "probeConcurrency": float64(10), "recoverAfterSuccessfulProbes": float64(20)},
	}
	for i, p := range ok {
		if err := ValidateParams(p); err != nil {
			t.Errorf("case %d: unexpected error %v", i, err)
		}
	}
	bad := []map[string]interface{}{
		{},
		{"targets": twoTargets(), "_chainId": "x"},
		{"targets": twoTargets(), "failoverOn": map[string]interface{}{"statusCodes": []interface{}{float64(499)}}},
		{"targets": twoTargets(), "failoverOn": map[string]interface{}{"statusCodes": []interface{}{float64(503), float64(503)}}},
		{"targets": twoTargets(), "failoverOn": map[string]interface{}{"reset": "yes"}},
		{"targets": twoTargets(), "suspendDuration": "2h"},
		{"targets": twoTargets(), "probeConcurrency": float64(1.5)},
		{"targets": twoTargets(), "perAttemptTimeout": "301s"},
	}
	for i, p := range bad {
		if err := ValidateParams(p); err == nil {
			t.Errorf("case %d: expected an error for %v", i, p)
		}
	}
}

func TestHopSecretIsStableAndRandom(t *testing.T) {
	a, b := HopSecret(), HopSecret()
	if a != b || len(a) != 64 {
		t.Fatalf("hop secret must be a stable 256-bit hex value, got %q / %q", a, b)
	}
}

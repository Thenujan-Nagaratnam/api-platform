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

import "testing"

func TestRetryTriggerDeclaration_Shape(t *testing.T) {
	d := RetryTriggerDeclaration{
		RetriableStatusCodes: []int{401},
		MinAttempts:          2,
	}
	if len(d.RetriableStatusCodes) != 1 || d.RetriableStatusCodes[0] != 401 {
		t.Errorf("unexpected RetriableStatusCodes: %v", d.RetriableStatusCodes)
	}
	if d.MinAttempts != 2 {
		t.Errorf("MinAttempts = %d, want 2", d.MinAttempts)
	}
}

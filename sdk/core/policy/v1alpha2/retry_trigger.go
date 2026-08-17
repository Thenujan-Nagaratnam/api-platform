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

// RetryTriggerDeclaration contributes retry conditions without claiming
// ownership of retry-target selection. Any number of policies on a route
// may declare one — they compose by union at gateway-controller's
// discovery step (Task 6), never conflict, unlike RetrySourceDeclaration.
// A plain data type — gateway-controller's own generic parser (Task 5)
// produces this by reading a policy's params according to its
// policy-definition.yaml's x-wso2-retry-trigger metadata; no policy Go
// code is ever called to produce one.
type RetryTriggerDeclaration struct {
	// RetriableStatusCodes is unioned with every other declared
	// RetryTriggerDeclaration's (and, if present, the route's
	// RetrySourceDeclaration's) own status codes into one
	// RouteAction.RetryPolicy.
	RetriableStatusCodes []int

	// MinAttempts is the minimum total attempts this policy needs to get
	// value from retrying (e.g. 2: one to observe the failure, one to
	// retry with a corrected request). The route's final NumRetries is at
	// least max(every declared MinAttempts) - 1.
	MinAttempts int
}

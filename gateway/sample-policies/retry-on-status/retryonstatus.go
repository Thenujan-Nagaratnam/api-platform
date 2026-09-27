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

// Package retryonstatus retries a request when the backend answers with a
// chosen status. It uses only sdk/core/attempts; the gateway knows it only
// through its policy definition.
package retryonstatus

import (
	"context"
	"fmt"
	"strconv"

	"github.com/wso2/api-platform/sdk/core/attempts"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// Name is the policy name, which is also how the gateway accounts for its
// retries.
const Name = "retry-on-status"

const headerAttempt = "x-retry-attempt"

// Policy is one instance.
type Policy struct {
	status int
}

// GetPolicy is the factory the gateway builder registers.
func GetPolicy(_ policy.PolicyMetadata, params map[string]interface{}) (policy.Policy, error) {
	status := 503
	if v, ok := params["status"]; ok {
		n, ok := toInt(v)
		if !ok || n < 400 || n > 599 {
			return nil, fmt.Errorf("status must be an integer between 400 and 599")
		}
		status = n
	}
	return &Policy{status: status}, nil
}

func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), n == float64(int(n))
	}
	return 0, false
}

// Mode: headers only.
func (p *Policy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{
		RequestHeaderMode:  policy.HeaderModeProcess,
		RequestBodyMode:    policy.BodyModeSkip,
		ResponseHeaderMode: policy.HeaderModeProcess,
		ResponseBodyMode:   policy.BodyModeSkip,
	}
}

// OnRequestHeaders tells the backend which attempt this is.
func (p *Policy) OnRequestHeaders(_ context.Context, reqCtx *policy.RequestHeaderContext, _ map[string]interface{}) policy.RequestHeaderAction {
	a := attempts.Current(reqCtx.SharedContext)
	if a == nil {
		return policy.UpstreamRequestHeaderModifications{}
	}
	return policy.UpstreamRequestHeaderModifications{
		HeadersToSet: map[string]string{headerAttempt: strconv.Itoa(a.Number)},
	}
}

// OnResponseHeaders asks for another attempt on the configured status.
func (p *Policy) OnResponseHeaders(_ context.Context, respCtx *policy.ResponseHeaderContext, _ map[string]interface{}) policy.ResponseHeaderAction {
	a := attempts.Current(respCtx.SharedContext)
	if a == nil {
		return policy.DownstreamResponseHeaderModifications{}
	}
	if respCtx.ResponseStatus == p.status {
		if act, ok := attempts.Retry(respCtx.SharedContext, Name, "status_"+strconv.Itoa(p.status), nil).(policy.DownstreamResponseHeaderModifications); ok && len(act.HeadersToSet) > 0 {
			return act
		}
	}
	return policy.DownstreamResponseHeaderModifications{
		HeadersToSet: map[string]string{headerAttempt: strconv.Itoa(a.Number)},
	}
}

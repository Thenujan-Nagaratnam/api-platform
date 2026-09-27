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

// Package attemptspolicy is the gateway's system policy for the per-attempt
// retry hop. See policy-definition.yaml.
package attemptspolicy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wso2/api-platform/sdk/core/attempts"
	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// Controller-written params.
const (
	ParamRole       = "_role"         // "front" or "attempt"
	ParamScope      = "_attemptScope" // the operation's scope token
	ParamTTL        = "_attemptTTL"   // how long a request's scope may live (front)
	ParamAllowances = "_allowances"   // retrying policy name → declared max attempts (front)
	ParamHopSecret  = "_hopSecret"    // per-boot secret for transport-failure labels (attempt)

	roleFront   = "front"
	roleAttempt = "attempt"

	metaScopeID = "wso2.attempts.scope"
)

var (
	busyBody     = []byte(`{"error":{"message":"The gateway is too busy to take this request. Please retry later.","type":"server_error","code":"gateway_busy"}}`)
	rejectedBody = []byte(`{"error":{"message":"The request could not be routed.","type":"server_error","code":"internal_error"}}`)
	configBody   = []byte(`{"error":{"message":"The request could not be routed.","type":"server_error","code":"internal_error"}}`)
)

// Policy is one instance, in either role.
type Policy struct {
	role       string
	operation  string
	ttl        time.Duration
	allowances map[string]int
	hopSecret  string
	configErr  error
	reg        *attempts.Registry
}

// GetPolicy is the factory the gateway builder registers.
func GetPolicy(_ policy.PolicyMetadata, params map[string]any) (policy.Policy, error) {
	attempts.Default.StartSweeper(time.Second)
	return newPolicy(params, attempts.Default), nil
}

// GetPolicyV2 is the v1alpha2 factory name.
func GetPolicyV2(md policy.PolicyMetadata, params map[string]any) (policy.Policy, error) {
	return GetPolicy(md, params)
}

func newPolicy(params map[string]any, reg *attempts.Registry) *Policy {
	p := &Policy{reg: reg}
	p.role, _ = params[ParamRole].(string)
	p.operation, _ = params[ParamScope].(string)
	switch {
	case p.role != roleFront && p.role != roleAttempt:
		p.configErr = fmt.Errorf("%s must be %q or %q", ParamRole, roleFront, roleAttempt)
	case p.operation == "":
		p.configErr = fmt.Errorf("%s is required", ParamScope)
	}
	if p.role == roleFront && p.configErr == nil {
		ttl, err := time.ParseDuration(fmt.Sprint(params[ParamTTL]))
		if err != nil || ttl <= 0 {
			p.configErr = fmt.Errorf("%s must be a positive duration", ParamTTL)
		}
		p.ttl = ttl
		p.allowances = map[string]int{}
		raw, _ := params[ParamAllowances].(map[string]any)
		for name, v := range raw {
			n, ok := toInt(v)
			if !ok || n < 1 {
				p.configErr = fmt.Errorf("%s[%s] must be a positive integer", ParamAllowances, name)
				break
			}
			p.allowances[name] = n
		}
	}
	if p.role == roleAttempt && p.configErr == nil {
		p.hopSecret, _ = params[ParamHopSecret].(string)
		if p.hopSecret == "" {
			p.configErr = fmt.Errorf("%s is required", ParamHopSecret)
		}
	}
	return p
}

func toInt(v any) (int, bool) {
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

func jsonResponse(status int, body []byte) policy.ImmediateResponse {
	return policy.ImmediateResponse{StatusCode: status, Headers: map[string]string{"content-type": "application/json"}, Body: body}
}

// OnRequestHeaders routes to the role's handler.
func (p *Policy) OnRequestHeaders(ctx context.Context, reqCtx *policy.RequestHeaderContext, _ map[string]any) policy.RequestHeaderAction {
	if p.configErr != nil {
		slog.ErrorContext(ctx, "attempts: invalid system policy params", "error", p.configErr)
		return jsonResponse(http.StatusInternalServerError, configBody)
	}
	if p.role == roleFront {
		return p.frontRequest(ctx, reqCtx)
	}
	return p.attemptRequest(ctx, reqCtx)
}

// OnResponseHeaders routes to the role's handler.
func (p *Policy) OnResponseHeaders(ctx context.Context, respCtx *policy.ResponseHeaderContext, _ map[string]any) policy.ResponseHeaderAction {
	if p.configErr != nil {
		return policy.DownstreamResponseHeaderModifications{}
	}
	if p.role == roleFront {
		return p.frontResponse(respCtx)
	}
	return p.attemptResponse(respCtx)
}

func (p *Policy) frontRequest(ctx context.Context, reqCtx *policy.RequestHeaderContext) policy.RequestHeaderAction {
	id, err := p.reg.Begin(p.operation, p.allowances, p.ttl)
	if err != nil {
		slog.WarnContext(ctx, "attempts: cannot start a request scope", "error", err)
		return jsonResponse(http.StatusServiceUnavailable, busyBody)
	}
	reqCtx.SharedContext.Metadata[metaScopeID] = id
	return policy.UpstreamRequestHeaderModifications{
		HeadersToSet:    map[string]string{attempts.HeaderScope: id},
		HeadersToRemove: clientInternalHeaders(reqCtx.Headers),
	}
}

// clientInternalHeaders lists attempt headers the client sent, other than the
// scope header, which is overwritten.
func clientInternalHeaders(h *policy.Headers) []string {
	var out []string
	if h == nil {
		return out
	}
	h.Iterate(func(name string, _ []string) {
		if attempts.IsInternalHeader(name) && !strings.EqualFold(name, attempts.HeaderScope) {
			out = append(out, name)
		}
	})
	return out
}

func (p *Policy) frontResponse(respCtx *policy.ResponseHeaderContext) policy.ResponseHeaderAction {
	strip := policy.DownstreamResponseHeaderModifications{
		HeadersToRemove: []string{attempts.HeaderRetry, attempts.HeaderUpstreamFailure, attempts.HeaderScope},
	}
	id, _ := respCtx.SharedContext.Metadata[metaScopeID].(string)
	if id == "" {
		return strip
	}
	out, _ := p.reg.Close(id)
	// Still tagged: a policy wanted another attempt but none was left.
	if respCtx.ResponseHeaders != nil && respCtx.ResponseHeaders.Has(attempts.HeaderRetry) && out.GiveUp != nil {
		return *out.GiveUp
	}
	return strip
}

func (p *Policy) attemptRequest(ctx context.Context, reqCtx *policy.RequestHeaderContext) policy.RequestHeaderAction {
	id := ""
	if v := reqCtx.Headers.Get(attempts.HeaderScope); len(v) > 0 {
		id = v[0]
	}
	a, err := p.reg.Arrive(id, p.operation)
	if err != nil {
		// Never retried: the response carries no tag.
		slog.WarnContext(ctx, "attempts: attempt rejected", "error", err)
		return jsonResponse(http.StatusInternalServerError, rejectedBody)
	}
	attempts.Bind(reqCtx.SharedContext, a)
	reqCtx.SharedContext.Metadata[metaScopeID] = id
	return policy.UpstreamRequestHeaderModifications{
		HeadersToSet:    map[string]string{attempts.HeaderHop: p.hopSecret},
		HeadersToRemove: []string{attempts.HeaderScope},
	}
}

func (p *Policy) attemptResponse(respCtx *policy.ResponseHeaderContext) policy.ResponseHeaderAction {
	if id, _ := respCtx.SharedContext.Metadata[metaScopeID].(string); id != "" {
		p.reg.Responded(id)
	}
	return policy.DownstreamResponseHeaderModifications{HeadersToRemove: []string{attempts.HeaderUpstreamFailure}}
}

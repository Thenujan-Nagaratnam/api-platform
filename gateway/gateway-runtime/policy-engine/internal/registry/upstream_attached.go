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

package registry

import (
	"context"
	"log/slog"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// WrapUpstreamAttached adapts a policy attached via upstreamPolicies so the
// existing upstream-attempt executor runs it. The policy keeps its single,
// ordinary RequestPolicy/ResponsePolicy implementation; the wrapper exposes it
// only through the upstream-attempt interfaces and reports every downstream
// mode as skip, so the downstream chain never sees it. The attachment point in
// configuration — not the policy's code — selects the phase.
//
// Known lossiness, accepted by design: the wrapped policy receives a
// RequestContext/ResponseContext built from the attempt's existing fields.
// Authority, Scheme, Vhost, UpstreamInfo, Downstream and RequestHeaders have no
// attempt-level equivalent and are zero-valued; RequestPath/RequestMethod are
// the attempt's resolved outbound request line; ResponseHeaders holds only the
// mutations accumulated in this attempt's chain, not a snapshot of the real
// upstream response headers. A policy attached this way must not depend on them.
func WrapUpstreamAttached(impl policy.Policy, name, route string) policy.Policy {
	w := &upstreamAttached{inner: impl}
	_, w.req = impl.(policy.RequestPolicy)
	_, w.resp = impl.(policy.ResponsePolicy)
	m := impl.Mode()
	w.req = w.req && m.RequestBodyMode != policy.BodyModeSkip
	w.resp = w.resp && m.ResponseBodyMode != policy.BodyModeSkip
	if !w.req && !w.resp {
		slog.Warn("[chain-build] policy attached via upstreamPolicies has no request/response body phase and will never run",
			"policy", name, "route", route)
	}
	return w
}

type upstreamAttached struct {
	inner policy.Policy
	req   bool
	resp  bool
}

func (u *upstreamAttached) Mode() policy.ProcessingMode {
	var m policy.ProcessingMode
	if u.req {
		m.UpstreamRequestMode = policy.BodyModeBuffer
	}
	if u.resp {
		m.UpstreamResponseMode = policy.BodyModeBuffer
	}
	return m
}

func (u *upstreamAttached) OnUpstreamRequestBody(ctx context.Context, upCtx *policy.UpstreamAttemptContext, params map[string]interface{}) policy.RequestAction {
	rp, ok := u.inner.(policy.RequestPolicy)
	if !u.req || !ok {
		return nil
	}
	return rp.OnRequestBody(ctx, &policy.RequestContext{
		SharedContext: upCtx.SharedContext,
		Headers:       upCtx.Headers,
		Body:          upCtx.Body,
		Path:          upCtx.Path,
		Method:        upCtx.Method,
		Upstream:      upCtx.UpstreamRequestContext,
	}, params)
}

func (u *upstreamAttached) OnUpstreamResponseBody(ctx context.Context, upCtx *policy.UpstreamAttemptContext, params map[string]interface{}) policy.ResponseAction {
	rp, ok := u.inner.(policy.ResponsePolicy)
	if !u.resp || !ok {
		return nil
	}
	return rp.OnResponseBody(ctx, &policy.ResponseContext{
		SharedContext: upCtx.SharedContext,
		RequestBody: &policy.Body{
			Content:     upCtx.OriginalRequestRaw,
			Present:     len(upCtx.OriginalRequestRaw) > 0,
			EndOfStream: true,
		},
		RequestPath:     upCtx.Path,
		RequestMethod:   upCtx.Method,
		ResponseHeaders: upCtx.Headers,
		ResponseBody:    upCtx.Body,
		ResponseStatus:  upCtx.ResponseStatusCode,
		Upstream: &policy.UpstreamResponseContext{
			Name:     upCtx.Name,
			URL:      upCtx.URL,
			BasePath: upCtx.BasePath,
			Response: &policy.UpstreamResponse{Headers: upCtx.Headers, StatusCode: upCtx.ResponseStatusCode},
		},
	}, params)
}

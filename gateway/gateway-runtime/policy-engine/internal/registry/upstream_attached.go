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
// ordinary RequestPolicy/ResponsePolicy/RequestHeaderPolicy/ResponseHeaderPolicy
// implementation — no new SDK interfaces and no new ProcessingMode fields exist
// for this; the wrapper only reuses UpstreamRequestPolicy/UpstreamResponsePolicy
// (pre-existing) and reports every downstream mode as skip, so the downstream
// chain never sees it. The attachment point in configuration — not the
// policy's code — selects the phase.
//
// Header-only policies (no RequestPolicy/ResponsePolicy) run through this same
// upstream-attempt BODY phase: there is no separate upstream header phase in
// Envoy or the kernel. OnUpstreamRequestBody/OnUpstreamResponseBody delegate to
// the inner policy's OnRequestHeaders/OnResponseHeaders instead and convert the
// returned header action into the equivalent body-phase action type — the two
// share the same header-mutation fields, and the kernel already delivers a
// body-phase action's header mutations to Envoy via the body response
// (see upstream_extproc.go's processRequestBody/processResponseBody), exactly
// like it would for a policy that mutates headers from its own body phase.
// A policy implementing both RequestPolicy and RequestHeaderPolicy runs only
// its body phase upstream (the header phase is unused upstream in that case).
//
// Known lossiness, accepted by design: the wrapped policy receives a
// RequestContext/ResponseContext/RequestHeaderContext/ResponseHeaderContext
// built from the attempt's existing fields. Authority, Scheme, Vhost,
// UpstreamInfo, Downstream and RequestHeaders have no attempt-level equivalent
// and are zero-valued; RequestPath/RequestMethod are the attempt's resolved
// outbound request line; ResponseHeaders holds only the mutations accumulated
// in this attempt's chain, not a snapshot of the real upstream response
// headers. A policy attached this way must not depend on them.
func WrapUpstreamAttached(impl policy.Policy, name, route string) policy.Policy {
	w := &upstreamAttached{inner: impl}
	m := impl.Mode()
	_, hasReqBody := impl.(policy.RequestPolicy)
	_, hasRespBody := impl.(policy.ResponsePolicy)
	_, hasReqHdr := impl.(policy.RequestHeaderPolicy)
	_, hasRespHdr := impl.(policy.ResponseHeaderPolicy)
	w.reqBody = hasReqBody && m.RequestBodyMode != policy.BodyModeSkip
	w.respBody = hasRespBody && m.ResponseBodyMode != policy.BodyModeSkip
	w.reqHdr = !w.reqBody && hasReqHdr && m.RequestHeaderMode == policy.HeaderModeProcess
	w.respHdr = !w.respBody && hasRespHdr && m.ResponseHeaderMode == policy.HeaderModeProcess
	if !w.reqBody && !w.respBody && !w.reqHdr && !w.respHdr {
		slog.Warn("[chain-build] policy attached via upstreamPolicies participates in no request/response header or body phase and will never run",
			"policy", name, "route", route)
	}
	return w
}

type upstreamAttached struct {
	inner    policy.Policy
	reqBody  bool
	respBody bool
	reqHdr   bool
	respHdr  bool
}

func (u *upstreamAttached) Mode() policy.ProcessingMode {
	var m policy.ProcessingMode
	if u.reqBody || u.reqHdr {
		m.UpstreamRequestMode = policy.BodyModeBuffer
	}
	if u.respBody || u.respHdr {
		m.UpstreamResponseMode = policy.BodyModeBuffer
	}
	return m
}

// requestHeaderModsToAction converts a RequestHeaderAction into the
// equivalent RequestAction so a header-only policy's mutations flow through
// the same upstream body-phase application path (applyUpstreamRequestModifications)
// as every other upstream-attached policy's action.
func requestHeaderModsToAction(a policy.RequestHeaderAction) policy.RequestAction {
	switch v := a.(type) {
	case policy.ImmediateResponse:
		return v
	case policy.UpstreamRequestHeaderModifications:
		return policy.UpstreamRequestModifications{
			HeadersToSet:            v.HeadersToSet,
			HeadersToAppend:         v.HeadersToAppend,
			HeadersToRemove:         v.HeadersToRemove,
			UpstreamName:            v.UpstreamName,
			UpstreamSlot:            v.UpstreamSlot,
			Path:                    v.Path,
			Host:                    v.Host,
			Method:                  v.Method,
			QueryParametersToAdd:    v.QueryParametersToAdd,
			QueryParametersToRemove: v.QueryParametersToRemove,
			AnalyticsMetadata:       v.AnalyticsMetadata,
			DynamicMetadata:         v.DynamicMetadata,
			AnalyticsHeaderFilter:   v.AnalyticsHeaderFilter,
		}
	default:
		return nil
	}
}

// responseHeaderModsToAction is the response-phase analog of requestHeaderModsToAction.
func responseHeaderModsToAction(a policy.ResponseHeaderAction) policy.ResponseAction {
	switch v := a.(type) {
	case policy.ImmediateResponse:
		return v
	case policy.DownstreamResponseHeaderModifications:
		return policy.DownstreamResponseModifications{
			HeadersToSet:          v.HeadersToSet,
			HeadersToAppend:       v.HeadersToAppend,
			HeadersToRemove:       v.HeadersToRemove,
			AnalyticsMetadata:     v.AnalyticsMetadata,
			DynamicMetadata:       v.DynamicMetadata,
			AnalyticsHeaderFilter: v.AnalyticsHeaderFilter,
		}
	default:
		return nil
	}
}

// selectedProviderMetadataKey is the SharedContext.Metadata key llm-header-router
// writes downstream and provider-scoped policies (e.g. the OpenAI->Anthropic
// transformer) read to gate themselves. Seeding it with the attempt's resolved
// provider lets those policies run unmodified per attempt: they no-op for the
// primary and run for the fallback they target.
const selectedProviderMetadataKey = "selected_provider"

func seedSelectedProvider(upCtx *policy.UpstreamAttemptContext) {
	if upCtx.ResolvedProvider == "" || upCtx.SharedContext == nil {
		return
	}
	if upCtx.SharedContext.Metadata == nil {
		upCtx.SharedContext.Metadata = map[string]interface{}{}
	}
	upCtx.SharedContext.Metadata[selectedProviderMetadataKey] = upCtx.ResolvedProvider
}

func (u *upstreamAttached) OnUpstreamRequestBody(ctx context.Context, upCtx *policy.UpstreamAttemptContext, params map[string]interface{}) policy.RequestAction {
	seedSelectedProvider(upCtx)
	if rp, ok := u.inner.(policy.RequestPolicy); ok && u.reqBody {
		return rp.OnRequestBody(ctx, &policy.RequestContext{
			SharedContext: upCtx.SharedContext,
			Headers:       upCtx.Headers,
			Body:          upCtx.Body,
			Path:          upCtx.Path,
			Method:        upCtx.Method,
			Upstream:      upCtx.UpstreamRequestContext,
		}, params)
	}
	if hp, ok := u.inner.(policy.RequestHeaderPolicy); ok && u.reqHdr {
		return requestHeaderModsToAction(hp.OnRequestHeaders(ctx, &policy.RequestHeaderContext{
			SharedContext: upCtx.SharedContext,
			Headers:       upCtx.Headers,
			Path:          upCtx.Path,
			Method:        upCtx.Method,
			Upstream:      upCtx.UpstreamRequestContext,
		}, params))
	}
	return nil
}

func (u *upstreamAttached) OnUpstreamResponseBody(ctx context.Context, upCtx *policy.UpstreamAttemptContext, params map[string]interface{}) policy.ResponseAction {
	seedSelectedProvider(upCtx)
	if rp, ok := u.inner.(policy.ResponsePolicy); ok && u.respBody {
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
	if hp, ok := u.inner.(policy.ResponseHeaderPolicy); ok && u.respHdr {
		return responseHeaderModsToAction(hp.OnResponseHeaders(ctx, &policy.ResponseHeaderContext{
			SharedContext:   upCtx.SharedContext,
			RequestPath:     upCtx.Path,
			RequestMethod:   upCtx.Method,
			ResponseHeaders: upCtx.Headers,
			ResponseStatus:  upCtx.ResponseStatusCode,
			Upstream: &policy.UpstreamResponseContext{
				Name:     upCtx.Name,
				URL:      upCtx.URL,
				BasePath: upCtx.BasePath,
				Response: &policy.UpstreamResponse{Headers: upCtx.Headers, StatusCode: upCtx.ResponseStatusCode},
			},
		}, params))
	}
	return nil
}

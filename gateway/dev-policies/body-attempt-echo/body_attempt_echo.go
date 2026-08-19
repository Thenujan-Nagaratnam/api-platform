/*
 *  Copyright (c) 2026, WSO2 LLC. (http://www.wso2.org) All Rights Reserved.
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

// Package bodyattemptecho is a dev-only policy (see dev-policies/README.md)
// that exists solely to exercise, end to end, gateway-controller's
// x-wso2-upstream-attempt: {body: true} opt-in for a plain, same-endpoint
// resilience.retry route - the capability added on top of model-failover's
// pre-existing, unconditional retry-source body buffering. It is never wired
// into a production build.yaml.
package bodyattemptecho

import (
	"context"
	"encoding/json"
	"log/slog"

	policy "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// Policy implements only what this dev policy needs: UpstreamAttemptPolicy.
// It deliberately implements no downstream-phase interface (RequestHeaderPolicy,
// RequestPolicy, etc.) - Mode() returns the all-SKIP zero value, so the kernel
// never buffers or inspects the client's original request on this policy's
// account. The route's x-wso2-retry-conditions (declared in
// policy-definition.yaml) drives Envoy's native RetryPolicy; the
// x-wso2-upstream-attempt: {body: true} declaration is what makes
// gateway-controller buffer the outgoing body for THIS policy's cluster even
// though it is not a retry-source/aggregate-cluster route - see
// collectClustersNeedingUpstreamAttemptBodyFilter in gateway-controller.
type Policy struct{}

// GetPolicy is the v1alpha2 factory entry point. This policy takes no
// configuration of its own - retryStatusCodes lives in policy-definition.yaml's
// x-wso2-retry-conditions block, resolved generically by gateway-controller,
// never read here.
func GetPolicy(_ policy.PolicyMetadata, _ map[string]interface{}) (policy.Policy, error) {
	return &Policy{}, nil
}

// Mode declares no downstream-phase participation - every field is the
// zero-value SKIP mode.
func (p *Policy) Mode() policy.ProcessingMode {
	return policy.ProcessingMode{}
}

// OnUpstreamAttemptRequest fires once per Envoy dial attempt for this
// policy's cluster (see UpstreamAttemptContext's own doc comment: it is NOT
// scoped to one client request). It rewrites the outgoing JSON body's
// "attempt" field to the current AttemptCount, so a mock backend recording
// what it actually received can prove the mutation reached it on retry -
// not just that the gateway eventually returned 2xx.
//
// actx.Body is populated here ONLY because this policy's policy-definition.yaml
// declares x-wso2-upstream-attempt: {body: true} - this route carries a plain
// resilience.retry (via x-wso2-retry-conditions), not a retry-source/
// aggregate-cluster RetryPolicy, so without that declaration actx.Body would
// be nil (see the companion doc's decision matrix). Fails open (no body
// mutation, whatever the original body was is sent unmodified) on any
// decode/encode error - a bug in this dev-only policy must never turn into a
// broken retry attempt.
func (p *Policy) OnUpstreamAttemptRequest(ctx context.Context, actx *policy.UpstreamAttemptContext) policy.UpstreamAttemptAction {
	if actx.Body == nil || !actx.Body.Present {
		return policy.UpstreamAttemptRequestModifications{}
	}

	decoded := map[string]interface{}{}
	if len(actx.Body.Content) > 0 {
		if err := json.Unmarshal(actx.Body.Content, &decoded); err != nil {
			slog.WarnContext(ctx, "BodyAttemptEcho: failed to decode upstream-attempt body, failing open",
				"attemptCount", actx.AttemptCount, "err", err)
			return policy.UpstreamAttemptRequestModifications{}
		}
	}

	decoded["attempt"] = actx.AttemptCount

	mutated, err := json.Marshal(decoded)
	if err != nil {
		slog.WarnContext(ctx, "BodyAttemptEcho: failed to re-encode upstream-attempt body, failing open",
			"attemptCount", actx.AttemptCount, "err", err)
		return policy.UpstreamAttemptRequestModifications{}
	}

	return policy.UpstreamAttemptRequestModifications{Body: mutated}
}

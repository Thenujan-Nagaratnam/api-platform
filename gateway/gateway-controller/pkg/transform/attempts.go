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

package transform

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/attempts"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/config"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/failover"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/utils"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/xds"
)

// scopeTTLMargin keeps a request's attempt scope alive a little past the
// route's own deadline, so the front route can still close it.
const scopeTTLMargin = 5 * time.Second

// applyAttemptSplits splits every operation on which an attached policy can
// retry into a front route and a per-attempt route. Which policies can retry,
// how many attempts they may cause and where each policy runs all come from
// the policies' definitions (package attempts); nothing here names a policy.
//
// An operation with no retrying policy is left exactly as it was built.
func (t *RestAPITransformer) applyAttemptSplits(rdc *models.RuntimeDeployConfig, cfg *models.StoredConfig, apiData api.APIConfigData, mcpResolved bool, vhosts []string) error {
	for _, op := range apiData.Operations {
		method, opPath := op.EffectiveMethod(), op.EffectivePath()
		if mcpResolved && isMCPMultiplexedRoute(method, opPath) {
			// The MCP route's chain is chosen per request by a resolver; the
			// retry hop does not apply to it.
			continue
		}
		headerMatches := routeHeaderMatches(op)
		discriminator := xds.HeaderMatchDiscriminator(headerMatches)
		for _, vhost := range vhosts {
			frontKey := xds.GenerateRouteNameWithDiscriminator(method, apiData.Context, apiData.Version, opPath, vhost, discriminator)
			front := rdc.Routes[frontKey]
			chain := rdc.PolicyChains[frontKey]
			if front == nil || chain == nil || front.Failover != nil {
				continue
			}
			plan, err := attempts.PlanChain(chain.Policies, t.definitionOf, attempts.DefaultLimits, utils.IsSystemPolicyName)
			if err != nil {
				return fmt.Errorf("operation %s %s: %w", method, opPath, err)
			}
			if !plan.Split {
				continue
			}
			token := attemptScopeToken(cfg.UUID, frontKey)

			attemptHeaders := append(append([]models.RouteHeaderMatch{}, headerMatches...),
				models.RouteHeaderMatch{Name: failover.HeaderChain, Value: token, Type: "Exact"})
			attemptKey := xds.GenerateRouteNameWithDiscriminator(method, apiData.Context, apiData.Version, opPath, vhost, xds.HeaderMatchDiscriminator(attemptHeaders))

			attempt := *front
			attempt.MatchHeaders = attemptHeaders
			attempt.Failover = &models.RouteFailover{Role: string(failover.RoleDispatch), ChainID: token, SameUpstream: true}
			rdc.Routes[attemptKey] = &attempt

			front.Failover = &models.RouteFailover{
				Role:          string(failover.RoleFront),
				ChainID:       token,
				NumRetries:    plan.NumRetries,
				PerTryTimeout: plan.PerTryTimeout,
				RouteTimeout:  plan.RouteTimeout,
				RetryOn:       plan.RetryOn,
			}

			rdc.PolicyChains[frontKey] = &models.PolicyChain{Policies: splitChain(chain.Policies, plan.FrontIndexes, plan.BothIndexes, attempts.RunningAsClientRequest,
				models.Policy{Name: attempts.SystemPolicyName, Version: attempts.SystemPolicyVersion, Params: map[string]interface{}{
					attempts.ParamRole:       attempts.RoleFront,
					attempts.ParamScope:      token,
					attempts.ParamTTL:        (plan.RouteTimeout + scopeTTLMargin).String(),
					attempts.ParamAllowances: allowancesParam(plan.Allowances),
				}})}
			rdc.PolicyChains[attemptKey] = &models.PolicyChain{Policies: splitChain(chain.Policies, plan.AttemptIndexes, plan.BothIndexes, attempts.RunningAsAttempt,
				models.Policy{Name: attempts.SystemPolicyName, Version: attempts.SystemPolicyVersion, Params: map[string]interface{}{
					attempts.ParamRole:      attempts.RoleAttempt,
					attempts.ParamScope:     token,
					attempts.ParamHopSecret: failover.HopSecret(),
				}})}
		}
	}
	return nil
}

// definitionOf finds a chain entry's definition. Chains carry the version as
// attached (often a major such as v1), so it is resolved like everywhere else.
func (t *RestAPITransformer) definitionOf(name, version string) (models.PolicyDefinition, bool) {
	full, err := config.ResolvePolicyVersion(t.policyDefinitions, t.latestVersions, name, version)
	if err != nil {
		return models.PolicyDefinition{}, false
	}
	d, ok := t.policyDefinitions[name+"|"+full]
	return d, ok
}

// splitChain builds one route's chain: the gateway's system policy first,
// then the chosen policies in their original order. A policy that runs on
// both routes is told which part it is.
func splitChain(all []models.Policy, indexes []int, both map[int]bool, runningAs string, system models.Policy) []models.Policy {
	out := make([]models.Policy, 0, len(indexes)+1)
	out = append(out, system)
	for _, i := range indexes {
		p := all[i]
		if both[i] {
			params := make(map[string]interface{}, len(p.Params)+1)
			for k, v := range p.Params {
				params[k] = v
			}
			params[attempts.ParamRunningAs] = runningAs
			p.Params = params
		}
		out = append(out, p)
	}
	return out
}

func allowancesParam(a map[string]int) map[string]interface{} {
	out := make(map[string]interface{}, len(a))
	for k, v := range a {
		out[k] = v
	}
	return out
}

// attemptScopeToken identifies one front route's per-attempt route. It is
// stable across redeploys and unique across APIs.
func attemptScopeToken(apiUUID, routeKey string) string {
	sum := sha256.Sum256([]byte(apiUUID + "\x00" + routeKey))
	return hex.EncodeToString(sum[:12])
}

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
	"fmt"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/failover"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/utils"
	policyv1alpha "github.com/wso2/api-platform/sdk/core/policy/v1alpha2"
)

// applyFailoverRoutes marks the routes the LlmProxy transformer's
// model-failover expansion produced and finishes their chains:
//
//   - a front route gets its Envoy retry settings, taken from the authored
//     params of its model-failover instance;
//   - a dispatch route loses its API-level and system policies, which already
//     ran once on the front route and must not run again per attempt.
func applyFailoverRoutes(rdc *models.RuntimeDeployConfig) error {
	for routeKey, route := range rdc.Routes {
		chainKey := rdc.EffectiveCanonicalChainKey(routeKey, route)
		chain := rdc.PolicyChains[chainKey]
		if chain == nil {
			continue
		}
		inst, role, ok := failoverInstance(chain)
		if !ok {
			continue
		}
		chainID, _ := inst.Params[failover.ParamChainID].(string)
		switch role {
		case failover.RoleFront:
			settings, err := failover.ParseSettings(inst.Params)
			if err != nil {
				return fmt.Errorf("route %s: %s: %w", routeKey, failover.PolicyName, err)
			}
			route.Failover = &models.RouteFailover{
				Role:          string(failover.RoleFront),
				ChainID:       chainID,
				NumRetries:    settings.NumRetries(),
				PerTryTimeout: settings.PerAttemptTimeout,
				RouteTimeout:  settings.FrontTimeout(),
				RetryOn:       settings.RetryOn(),
			}
		case failover.RoleDispatch:
			route.Failover = &models.RouteFailover{Role: string(failover.RoleDispatch), ChainID: chainID}
			kept := chain.Policies[:0:0]
			for _, p := range chain.Policies {
				if attachedTo, _ := p.Params["attachedTo"].(string); attachedTo == string(policyv1alpha.LevelAPI) {
					continue
				}
				if utils.IsSystemPolicyName(p.Name) {
					continue
				}
				kept = append(kept, p)
			}
			chain.Policies = kept
		}
	}
	return nil
}

func failoverInstance(chain *models.PolicyChain) (models.Policy, failover.Role, bool) {
	for _, p := range chain.Policies {
		if p.Name != failover.PolicyName {
			continue
		}
		if role, ok := failover.RoleOf(p.Params); ok {
			return p, role, true
		}
	}
	return models.Policy{}, "", false
}

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

package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/failover"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// failoverAttachment is what the dispatch chain needs to know about one
// provider attachment a failover target can reference.
type failoverAttachment struct {
	attachment  models.LLMProxyAttachment
	context     string
	valuePrefix string
}

// failoverBuild collects everything transformProxy needs from the failover
// expansion of one proxy.
type failoverBuild struct {
	// front marks the indices of operations that became front routes; the
	// provider-scoped transformer, credential and marker policies are not
	// attached to them because they run on the dispatch route instead.
	front map[int]bool
	// dispatchOps are the extra operations, one per front operation, matched
	// on the chain header and carrying the dispatch chain.
	dispatchOps []api.Operation
	// upstreamDefs are the per-target loopback upstream definitions.
	upstreamDefs []api.UpstreamDefinition
}

// findFailoverPolicy returns the index of the model-failover instance in a
// policy list, or -1.
func findFailoverPolicy(policies *[]api.Policy) int {
	if policies == nil {
		return -1
	}
	for i, p := range *policies {
		if p.Name == failover.PolicyName {
			return i
		}
	}
	return -1
}

// takeGlobalFailoverPolicy removes a model-failover instance from the proxy's
// global policies and returns it. A global attachment applies the chain to
// every operation, so it is expanded per operation instead of being left at
// API level, where it would also run on the dispatch routes.
func takeGlobalFailoverPolicy(global []api.Policy) ([]api.Policy, *api.Policy, error) {
	var found *api.Policy
	out := make([]api.Policy, 0, len(global))
	for i := range global {
		if global[i].Name == failover.PolicyName {
			if found != nil {
				return nil, nil, fmt.Errorf("globalPolicies: %s may be attached at most once", failover.PolicyName)
			}
			p := global[i]
			found = &p
			continue
		}
		out = append(out, global[i])
	}
	return out, found, nil
}

// chainToken identifies one front operation's chain. It is the value of the
// chain header the dispatch route matches on, so it must be stable across
// redeploys and unique per proxy operation.
func chainToken(proxyName, method, path string) string {
	sum := sha256.Sum256([]byte(proxyName + "\x00" + method + "\x00" + path))
	return hex.EncodeToString(sum[:12])
}

// buildFailover expands every operation that carries model-failover (attached
// to the operation, or globally) into a front operation plus a dispatch
// operation. See package failover for the two-route model.
func (t *LLMProviderTransformer) buildFailover(
	proxyName string,
	ops []api.Operation,
	globalFailover *api.Policy,
	attachments map[string]failoverAttachment,
	listenerPort int,
) (*failoverBuild, error) {
	build := &failoverBuild{front: map[int]bool{}}
	markerPolicy, err := t.proxyInternalLoopbackMarkerPolicy()
	if err != nil {
		return nil, err
	}

	for i := range ops {
		op := &ops[i]
		idx := findFailoverPolicy(op.Policies)
		var authored api.Policy
		switch {
		case idx >= 0:
			authored = (*op.Policies)[idx]
		case globalFailover != nil:
			authored = *globalFailover
		default:
			continue
		}
		field := fmt.Sprintf("%s %s: %s", op.EffectiveMethod(), op.EffectivePath(), failover.PolicyName)

		params := map[string]interface{}{}
		if authored.Params != nil {
			params = *authored.Params
		}
		if key, bad := failover.HasInternalParams(params); bad {
			return nil, fmt.Errorf("%s: parameter %q is reserved for gateway-internal use", field, key)
		}
		settings, err := failover.ParseSettings(params)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}

		token := chainToken(proxyName, op.EffectiveMethod(), op.EffectivePath())
		targetIDs := make([]interface{}, len(settings.Targets))
		native := make([]interface{}, len(settings.Targets))
		var transformers, auths []api.Policy
		for k, target := range settings.Targets {
			att, ok := attachments[target.Provider]
			if !ok {
				return nil, fmt.Errorf("%s: targets[%d].provider %q is not a provider attached to this proxy", field, k, target.Provider)
			}
			id := fmt.Sprintf("%s-t%d", token[:8], k)
			upstreamName := failover.TargetUpstreamName(id)
			targetIDs[k] = id
			native[k] = att.attachment.Transformer == nil
			build.upstreamDefs = append(build.upstreamDefs, loopbackUpstreamDefinition(upstreamName, att.context, listenerPort))

			if att.attachment.Transformer != nil {
				tr := *att.attachment.Transformer
				trParams := map[string]interface{}{}
				if tr.Params != nil {
					for pk, pv := range *tr.Params {
						trParams[pk] = pv
					}
				}
				trParams["model"] = target.Model
				tr.Params = &trParams
				pol, err := t.proxyTransformerPolicy(&tr, upstreamName, fmt.Sprintf("%s: targets[%d] transformer", field, k), false)
				if err != nil {
					return nil, err
				}
				transformers = append(transformers, *pol)
			}
			if att.attachment.Auth != nil {
				pol, err := t.proxyUpstreamAuthPolicy(att.attachment.Auth, att.valuePrefix, fmt.Sprintf("%s: targets[%d] auth", field, k))
				if err != nil {
					return nil, err
				}
				if pol != nil {
					condition := selectedProviderExecutionCondition(upstreamName, false)
					pol.ExecutionCondition = &condition
					auths = append(auths, *pol)
				}
			}
		}

		front := copyPolicy(authored)
		(*front.Params)[failover.ParamRole] = string(failover.RoleFront)
		(*front.Params)[failover.ParamChainID] = token
		(*front.Params)[failover.ParamTargetIDs] = targetIDs
		if idx >= 0 {
			(*op.Policies)[idx] = front
		} else {
			withFront := append([]api.Policy{front}, derefPolicies(op.Policies)...)
			op.Policies = &withFront
		}
		build.front[i] = true

		dispatch := copyPolicy(authored)
		(*dispatch.Params)[failover.ParamRole] = string(failover.RoleDispatch)
		(*dispatch.Params)[failover.ParamChainID] = token
		(*dispatch.Params)[failover.ParamTargetIDs] = targetIDs
		(*dispatch.Params)[failover.ParamTargetNative] = native
		(*dispatch.Params)[failover.ParamHopSecret] = failover.HopSecret()
		// The dispatch route carries only what must run per attempt: target
		// selection, then the selected target's transformer and credential,
		// in the same order the proxy route uses today.
		dispatchPolicies := []api.Policy{dispatch}
		dispatchPolicies = append(dispatchPolicies, transformers...)
		dispatchPolicies = append(dispatchPolicies, auths...)
		dispatchPolicies = append(dispatchPolicies, *markerPolicy)

		headers := []api.OperationHeaderMatch{{Name: failover.HeaderChain, Value: token}}
		build.dispatchOps = append(build.dispatchOps, api.Operation{
			Match: &api.OperationMatch{
				Method:  api.OperationMethod(op.EffectiveMethod()),
				Path:    api.OperationPathMatch{Value: op.EffectivePath()},
				Headers: &headers,
			},
			Policies:   &dispatchPolicies,
			Resilience: op.Resilience,
		})
	}
	return build, nil
}

// copyPolicy returns p with its own params map, so the front and dispatch
// instances never alias the authored params or each other.
func copyPolicy(p api.Policy) api.Policy {
	params := map[string]interface{}{}
	if p.Params != nil {
		for k, v := range *p.Params {
			params[k] = v
		}
	}
	p.Params = &params
	return p
}

func derefPolicies(p *[]api.Policy) []api.Policy {
	if p == nil {
		return nil
	}
	out := make([]api.Policy, len(*p))
	copy(out, *p)
	return out
}

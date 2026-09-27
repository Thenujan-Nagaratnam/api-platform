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
// redeploys and unique across every proxy and provider operation: kind is
// part of it because an LlmProxy and an LlmProvider may share a name.
func chainToken(kind, name, method, path string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + name + "\x00" + method + "\x00" + path))
	return hex.EncodeToString(sum[:12])
}

// buildFailover expands every operation that carries model-failover (attached
// to the operation, or globally) into a front operation plus a dispatch
// operation. See package failover for the two-route model.
func (t *LLMProviderTransformer) buildFailover(
	proxyName string,
	primaryProvider string,
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
		if err := failover.CheckAuthored(params); err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		settings, err := failover.ParseSettingsFor(params, primaryProvider, false)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}

		token := chainToken("LlmProxy", proxyName, op.EffectiveMethod(), op.EffectivePath())
		// Every chain target, then the pass-through target, in the order the
		// policy flattens them.
		all := append(append([]failover.Target{}, settings.Targets...), failover.PassThroughTarget(primaryProvider))
		targetIDs := make([]interface{}, len(all))
		native := make([]interface{}, len(all))
		var transformers, auths []api.Policy
		for k, target := range all {
			att, ok := attachments[target.Provider]
			if !ok {
				return nil, fmt.Errorf("%s: chains: provider %q is not a provider attached to this proxy", field, target.Provider)
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
				if target.Model != "" { // the pass-through target keeps the request's model
					trParams["model"] = target.Model
				}
				tr.Params = &trParams
				pol, err := t.proxyTransformerPolicy(&tr, upstreamName, fmt.Sprintf("%s: %s transformer", field, target.Provider), false)
				if err != nil {
					return nil, err
				}
				transformers = append(transformers, *pol)
			}
			if att.attachment.Auth != nil {
				pol, err := t.proxyUpstreamAuthPolicy(att.attachment.Auth, att.valuePrefix, fmt.Sprintf("%s: %s auth", field, target.Provider))
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
		(*front.Params)["chains"] = settings.ResolvedChains()
		(*front.Params)[failover.ParamRole] = string(failover.RoleFront)
		(*front.Params)[failover.ParamChainID] = token
		(*front.Params)[failover.ParamTargetIDs] = targetIDs
		placeFront(op, idx, front)
		build.front[i] = true

		dispatch := copyPolicy(authored)
		(*dispatch.Params)["chains"] = settings.ResolvedChains()
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

// placeFront puts the front instance where the authored model-failover was
// (idx >= 0), or, for a global attachment, right after the operation's last
// model-selecting policy, so a model picked by round-robin is the model the
// front role reads.
func placeFront(op *api.Operation, idx int, front api.Policy) {
	if idx >= 0 {
		(*op.Policies)[idx] = front
		return
	}
	pols := derefPolicies(op.Policies)
	at := 0
	for k, p := range pols {
		if failover.IsModelSelector(p.Name) {
			at = k + 1
		}
	}
	out := make([]api.Policy, 0, len(pols)+1)
	out = append(out, pols[:at]...)
	out = append(out, front)
	out = append(out, pols[at:]...)
	op.Policies = &out
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

// buildProviderFailover expands every forwarding operation of an LlmProvider
// that carries model-failover into a front and a dispatch operation. Every
// target is a model of this provider: the dispatch route keeps the provider's
// own upstream, the dispatch role only rewrites the model per attempt, and
// the provider's upstream credential moves onto the dispatch operation so it
// is applied to every attempt.
func (t *LLMProviderTransformer) buildProviderFailover(
	providerName string,
	ops []api.Operation,
	globalFailover *api.Policy,
	upstreamAuth *api.Policy,
	tmpl *models.StoredLLMProviderTemplate,
	denyKeys map[pathMethodKey]bool,
) (*failoverBuild, error) {
	build := &failoverBuild{front: map[int]bool{}}
	for i := range ops {
		op := &ops[i]
		if denyKeys[pathMethodKey{path: op.EffectivePath(), method: op.EffectiveMethod()}] {
			continue
		}
		idx := findFailoverPolicy(op.Policies)
		params := map[string]interface{}{}
		switch {
		case idx >= 0:
			if p := (*op.Policies)[idx].Params; p != nil {
				params = *p
			}
		case globalFailover != nil:
			// A global attachment carries no template params; add this
			// operation's, as an operation-level attachment gets them.
			if globalFailover.Params != nil {
				for k, v := range *globalFailover.Params {
					params[k] = v
				}
			}
			templateParams, err := buildTemplateParams(tmpl, op.EffectivePath())
			if err != nil {
				return nil, fmt.Errorf("failed to build template params: %w", err)
			}
			for k, v := range templateParams {
				if _, set := params[k]; !set {
					params[k] = v
				}
			}
		default:
			continue
		}
		authored := api.Policy{Name: failover.PolicyName, Params: &params}
		if idx >= 0 {
			authored = (*op.Policies)[idx]
			authored.Params = &params
		} else {
			authored.Version = globalFailover.Version
			authored.ExecutionCondition = globalFailover.ExecutionCondition
		}
		field := fmt.Sprintf("%s %s: %s", op.EffectiveMethod(), op.EffectivePath(), failover.PolicyName)

		if err := failover.CheckAuthored(params); err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		if err := failover.ValidateRequestModel(params["requestModel"], op.EffectivePath()); err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		settings, err := failover.ParseSettingsFor(params, providerName, true)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}

		token := chainToken("LlmProvider", providerName, op.EffectiveMethod(), op.EffectivePath())
		// Every chain target plus the pass-through target, all on this provider.
		n := len(settings.Targets) + 1
		targetIDs := make([]interface{}, n)
		native := make([]interface{}, n)
		for k := range targetIDs {
			targetIDs[k] = fmt.Sprintf("%s-t%d", token[:8], k)
			native[k] = true
		}
		chains := settings.ResolvedChains()

		front := copyPolicy(authored)
		(*front.Params)["chains"] = chains
		(*front.Params)[failover.ParamRole] = string(failover.RoleFront)
		(*front.Params)[failover.ParamChainID] = token
		(*front.Params)[failover.ParamTargetIDs] = targetIDs
		// The front role reads the requested model from the template's
		// location, which it honours only in provider mode.
		(*front.Params)[failover.ParamRouteToTarget] = false
		placeFront(op, idx, front)
		build.front[i] = true

		dispatch := copyPolicy(authored)
		(*dispatch.Params)["chains"] = chains
		(*dispatch.Params)[failover.ParamRole] = string(failover.RoleDispatch)
		(*dispatch.Params)[failover.ParamChainID] = token
		(*dispatch.Params)[failover.ParamTargetIDs] = targetIDs
		(*dispatch.Params)[failover.ParamTargetNative] = native
		(*dispatch.Params)[failover.ParamHopSecret] = failover.HopSecret()
		(*dispatch.Params)[failover.ParamRouteToTarget] = false
		dispatchPolicies := []api.Policy{dispatch}
		if upstreamAuth != nil {
			dispatchPolicies = append(dispatchPolicies, *upstreamAuth)
		}

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

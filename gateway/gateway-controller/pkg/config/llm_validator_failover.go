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

package config

import (
	"fmt"
	"strings"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/failover"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

type failoverAttachmentRef struct {
	field  string
	params map[string]interface{}
}

// policyPlacement is where one attachment of a policy applies: every
// operation (global), or one path and its methods, and its position among the
// operation policies (API-level policies run first).
type policyPlacement struct {
	field   string
	name    string
	params  map[string]interface{}
	global  bool
	path    string
	methods map[string]bool
	order   int
}

// overlaps reports whether two placements can land on the same operation.
func (a policyPlacement) overlaps(b policyPlacement) bool {
	if a.global || b.global {
		return true
	}
	if !pathsOverlap(a.path, b.path) {
		return false
	}
	for m := range a.methods {
		if b.methods[m] || b.methods["*"] || m == "*" {
			return true
		}
	}
	return false
}

func pathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	wild := func(p, q string) bool {
		return strings.HasSuffix(p, "/*") && strings.HasPrefix(q, strings.TrimSuffix(p, "*"))
	}
	return wild(a, b) || wild(b, a)
}

// failoverAttachments collects every model-failover attachment and every
// provider- or model-selecting policy across the three policy lists an LLM
// resource can carry, with the field path and placement of each.
func failoverAttachments(global *[]api.Policy, opPolicies *[]api.OperationPolicy, legacy *[]api.LLMPolicy) (refs []failoverAttachmentRef, failovers, selectors []policyPlacement, globalCount int) {
	isSelector := map[string]bool{}
	for _, n := range failover.ProviderSelectingPolicies {
		isSelector[n] = true
	}
	add := func(pl policyPlacement) {
		if pl.name == failover.PolicyName {
			refs = append(refs, failoverAttachmentRef{field: pl.field + ".params", params: pl.params})
			failovers = append(failovers, pl)
		}
		if isSelector[pl.name] {
			selectors = append(selectors, pl)
		}
	}
	if global != nil {
		for i, p := range *global {
			params := map[string]interface{}{}
			if p.Params != nil {
				params = *p.Params
			}
			if p.Name == failover.PolicyName {
				globalCount++
			}
			add(policyPlacement{field: fmt.Sprintf("spec.globalPolicies[%d]", i), name: p.Name, params: params, global: true, order: -1})
		}
	}
	order := 0
	if opPolicies != nil {
		for i, p := range *opPolicies {
			for j, path := range p.Paths {
				methods := map[string]bool{}
				for _, m := range path.Methods {
					methods[strings.ToUpper(string(m))] = true
				}
				add(policyPlacement{field: fmt.Sprintf("spec.operationPolicies[%d].paths[%d]", i, j), name: p.Name, params: path.Params, path: path.Path, methods: methods, order: order})
			}
			order++
		}
	}
	if legacy != nil {
		for i, p := range *legacy {
			for j, path := range p.Paths {
				methods := map[string]bool{}
				for _, m := range path.Methods {
					methods[strings.ToUpper(string(m))] = true
				}
				add(policyPlacement{field: fmt.Sprintf("spec.policies[%d].paths[%d]", i, j), name: p.Name, params: path.Params, path: path.Path, methods: methods, order: order})
			}
			order++
		}
	}
	return refs, failovers, selectors, globalCount
}

// namesProvider reports whether any entry of a round-robin style models list
// routes to a provider.
func namesProvider(params map[string]interface{}) bool {
	models, _ := params["models"].([]interface{})
	for _, m := range models {
		if e, ok := m.(map[string]interface{}); ok {
			if p, _ := e["provider"].(string); p != "" {
				return true
			}
		}
	}
	return false
}

// commonFailoverErrors reports the rules every attachment point shares: at
// most one global attachment, and on any operation model-failover shares, no
// other selecting policy except a model-only round-robin that runs first and
// so picks the primary whose chain runs.
func commonFailoverErrors(failovers, selectors []policyPlacement, globalCount int) []ValidationError {
	var errs []ValidationError
	if globalCount > 1 {
		errs = append(errs, ValidationError{Field: "spec.globalPolicies",
			Message: fmt.Sprintf("%s may be attached at most once in globalPolicies", failover.PolicyName)})
	}
	for _, sel := range selectors {
		for _, fo := range failovers {
			if !sel.overlaps(fo) {
				continue
			}
			switch {
			case !failover.IsModelSelector(sel.name) || namesProvider(sel.params):
				errs = append(errs, ValidationError{Field: sel.field,
					Message: fmt.Sprintf("this policy selects providers and cannot share an operation with %s", failover.PolicyName)})
			case !fo.global && !sel.global && sel.order > fo.order:
				errs = append(errs, ValidationError{Field: sel.field,
					Message: fmt.Sprintf("%s must come before %s on the same operation, so the model it picks selects the chain", sel.name, failover.PolicyName)})
			default:
				continue
			}
			break
		}
	}
	return errs
}

// validateModelFailover checks every model-failover attachment on a proxy at
// registration: its params, that each fallback names a provider attached to
// this proxy, and the selecting-policy rules above.
func validateModelFailover(spec *api.LLMProxyConfigData, attachments []models.LLMProxyAttachment) []ValidationError {
	refs, failovers, selectors, globalCount := failoverAttachments(spec.GlobalPolicies, spec.OperationPolicies, spec.Policies)
	if len(refs) == 0 {
		return nil
	}
	errs := commonFailoverErrors(failovers, selectors, globalCount)
	names := map[string]bool{}
	primary := ""
	for _, a := range attachments {
		names[a.EffectiveName()] = true
		names[a.Id] = true
		if a.IsPrimary {
			primary = a.EffectiveName()
		}
	}
	for _, ref := range refs {
		if err := failover.ValidateParams(ref.params, primary, false); err != nil {
			errs = append(errs, ValidationError{Field: ref.field, Message: err.Error()})
			continue
		}
		chains, _ := ref.params["chains"].([]interface{})
		for i, c := range chains {
			m, _ := c.(map[string]interface{})
			fallbacks, _ := m["fallbacks"].([]interface{})
			for j, f := range fallbacks {
				fm, _ := f.(map[string]interface{})
				if p, _ := fm["provider"].(string); p != "" && !names[p] {
					errs = append(errs, ValidationError{
						Field:   fmt.Sprintf("%s.chains[%d].fallbacks[%d].provider", ref.field, i, j),
						Message: fmt.Sprintf("provider %q is not attached to this proxy (use its id or alias from provider/additionalProviders)", p),
					})
				}
			}
		}
	}
	return errs
}

// validateProviderModelFailover checks model-failover attachments on an
// LlmProvider, where fallbacks are this provider's models. The template's
// model location is checked when the provider is transformed, since the
// validator has no template access; that error is also returned as 400.
func validateProviderModelFailover(spec *api.LLMProviderConfigData, providerName string) []ValidationError {
	refs, failovers, selectors, globalCount := failoverAttachments(spec.GlobalPolicies, spec.OperationPolicies, spec.Policies)
	if len(refs) == 0 {
		return nil
	}
	errs := commonFailoverErrors(failovers, selectors, globalCount)
	for _, ref := range refs {
		if err := failover.ValidateParams(ref.params, providerName, true); err != nil {
			errs = append(errs, ValidationError{Field: ref.field, Message: err.Error()})
		}
	}
	return errs
}

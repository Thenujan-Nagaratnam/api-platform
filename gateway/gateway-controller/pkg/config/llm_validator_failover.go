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

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/failover"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

type failoverAttachmentRef struct {
	field  string
	params map[string]interface{}
}

// validateModelFailover checks every model-failover attachment on a proxy at
// registration: its params, that each target names a provider attached to
// this proxy, and that no other policy on the proxy selects providers.
func validateModelFailover(spec *api.LLMProxyConfigData, attachments []models.LLMProxyAttachment) []ValidationError {
	var refs []failoverAttachmentRef
	var selectors []string
	isSelector := map[string]bool{}
	for _, n := range failover.ProviderSelectingPolicies {
		isSelector[n] = true
	}

	globalCount := 0
	if spec.GlobalPolicies != nil {
		for i, p := range *spec.GlobalPolicies {
			field := fmt.Sprintf("spec.globalPolicies[%d]", i)
			if p.Name == failover.PolicyName {
				globalCount++
				params := map[string]interface{}{}
				if p.Params != nil {
					params = *p.Params
				}
				refs = append(refs, failoverAttachmentRef{field: field + ".params", params: params})
			}
			if isSelector[p.Name] {
				selectors = append(selectors, field)
			}
		}
	}
	if spec.OperationPolicies != nil {
		for i, p := range *spec.OperationPolicies {
			for j, path := range p.Paths {
				field := fmt.Sprintf("spec.operationPolicies[%d].paths[%d]", i, j)
				if p.Name == failover.PolicyName {
					refs = append(refs, failoverAttachmentRef{field: field + ".params", params: path.Params})
				}
				if isSelector[p.Name] {
					selectors = append(selectors, field)
				}
			}
		}
	}
	if spec.Policies != nil {
		for i, p := range *spec.Policies {
			for j, path := range p.Paths {
				field := fmt.Sprintf("spec.policies[%d].paths[%d]", i, j)
				if p.Name == failover.PolicyName {
					refs = append(refs, failoverAttachmentRef{field: field + ".params", params: path.Params})
				}
				if isSelector[p.Name] {
					selectors = append(selectors, field)
				}
			}
		}
	}
	if len(refs) == 0 {
		return nil
	}

	var errs []ValidationError
	if globalCount > 1 {
		errs = append(errs, ValidationError{Field: "spec.globalPolicies",
			Message: fmt.Sprintf("%s may be attached at most once in globalPolicies", failover.PolicyName)})
	}
	for _, field := range selectors {
		errs = append(errs, ValidationError{Field: field,
			Message: fmt.Sprintf("this policy selects providers and cannot be combined with %s on the same proxy", failover.PolicyName)})
	}

	names := map[string]bool{}
	for _, a := range attachments {
		names[a.EffectiveName()] = true
		names[a.Id] = true
	}
	for _, ref := range refs {
		if err := failover.ValidateParams(ref.params); err != nil {
			errs = append(errs, ValidationError{Field: ref.field, Message: err.Error()})
			continue
		}
		settings, _ := failover.ParseSettings(ref.params)
		for k, target := range settings.Targets {
			if !names[target.Provider] {
				errs = append(errs, ValidationError{
					Field:   fmt.Sprintf("%s.targets[%d].provider", ref.field, k),
					Message: fmt.Sprintf("provider %q is not attached to this proxy (use its id or alias from provider/additionalProviders)", target.Provider),
				})
			}
		}
	}
	return errs
}

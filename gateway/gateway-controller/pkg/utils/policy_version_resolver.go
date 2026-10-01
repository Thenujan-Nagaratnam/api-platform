/*
 * Copyright (c) 2025, WSO2 LLC. (https://www.wso2.com).
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
	"errors"
	"fmt"

	versionutil "github.com/wso2/api-platform/common/version"
	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// PolicyDefinitionMissingUserMessage is returned to clients when a required policy definition is missing.
const PolicyDefinitionMissingUserMessage = "Required policy definition is missing in the gateway controller configuration. Please contact the administrator."

// PolicyDefinitionMissingError indicates a required policy definition was not loaded.
type PolicyDefinitionMissingError struct {
	PolicyName string
}

func (e *PolicyDefinitionMissingError) Error() string {
	return fmt.Sprintf("policy definition for '%s' not loaded", e.PolicyName)
}

// IsPolicyDefinitionMissingError checks whether err indicates a missing policy definition.
func IsPolicyDefinitionMissingError(err error) bool {
	var missingErr *PolicyDefinitionMissingError
	return errors.As(err, &missingErr)
}

// PolicyVersionResolver resolves a policy name to a version string, and can
// look up a specific (name, version) pair's full definition.
type PolicyVersionResolver interface {
	Resolve(name string) (string, error)

	// Definition returns the loaded PolicyDefinition for an exact
	// (name, version) pair, and whether it was found. Used by the
	// specs/001-upstream-retry-contribution transform-phase resolution step
	// to read an attached policy instance's own Retry declaration.
	Definition(name, version string) (*models.PolicyDefinition, bool)
}

// StaticPolicyVersionResolver resolves policy versions from a fixed map.
type StaticPolicyVersionResolver struct {
	versions map[string]string
	// definitions is keyed "name|version" (matching PolicyLoader's own key
	// shape, see pkg/utils/policy_loader.go) and is nil for a resolver built
	// via NewStaticPolicyVersionResolver — only NewLoadedPolicyVersionResolver
	// has full PolicyDefinition values to populate it with.
	definitions map[string]models.PolicyDefinition
}

// NewStaticPolicyVersionResolver creates a resolver from an explicit policy->version map.
func NewStaticPolicyVersionResolver(versions map[string]string) *StaticPolicyVersionResolver {
	clone := make(map[string]string, len(versions))
	for name, version := range versions {
		clone[name] = version
	}
	return &StaticPolicyVersionResolver{versions: clone}
}

// NewLoadedPolicyVersionResolver builds a resolver from loaded policy definitions.
// The highest semantic version per policy name is selected, then converted to
// a major-only version (e.g., v0) for compatibility with policy version
// validation rules. The full definitions remain available via Definition,
// keyed by their own exact (unresolved) name/version, not the major-only
// version Resolve returns.
func NewLoadedPolicyVersionResolver(policyDefinitions map[string]models.PolicyDefinition) *StaticPolicyVersionResolver {
	versions := make(map[string]string)
	for _, def := range policyDefinitions {
		existing, ok := versions[def.Name]
		if !ok || versionutil.CompareSemver(def.Version, existing) > 0 {
			versions[def.Name] = def.Version
		}
	}
	for name, version := range versions {
		versions[name] = versionutil.MajorVersion(version)
	}
	resolver := NewStaticPolicyVersionResolver(versions)
	resolver.definitions = make(map[string]models.PolicyDefinition, len(policyDefinitions))
	for key, def := range policyDefinitions {
		resolver.definitions[key] = def
	}
	return resolver
}

// Resolve returns the version for a policy name or an error if missing.
func (r *StaticPolicyVersionResolver) Resolve(name string) (string, error) {
	if r == nil || r.versions == nil {
		return "", &PolicyDefinitionMissingError{PolicyName: name}
	}
	version, ok := r.versions[name]
	if !ok {
		return "", &PolicyDefinitionMissingError{PolicyName: name}
	}
	return version, nil
}

// Definition returns the loaded PolicyDefinition for an exact (name, version)
// pair, and whether it was found.
func (r *StaticPolicyVersionResolver) Definition(name, version string) (*models.PolicyDefinition, bool) {
	if r == nil || r.definitions == nil {
		return nil, false
	}
	def, ok := r.definitions[name+"|"+version]
	if !ok {
		return nil, false
	}
	return &def, true
}

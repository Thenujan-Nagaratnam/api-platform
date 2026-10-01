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
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
	"gopkg.in/yaml.v3"
)

// PolicyLoader loads policy definitions from files
type PolicyLoader struct {
	logger *slog.Logger
}

// NewPolicyLoader creates a new policy loader
func NewPolicyLoader(logger *slog.Logger) *PolicyLoader {
	return &PolicyLoader{
		logger: logger,
	}
}

// LoadPoliciesFromDirectory loads all policy definition files from a directory
// Supports both JSON and YAML files
func (pl *PolicyLoader) LoadPoliciesFromDirectory(dirPath string) (map[string]models.PolicyDefinition, error) {
	policies := make(map[string]models.PolicyDefinition)

	// Check if directory exists
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		pl.logger.Warn("Policy directory does not exist", slog.String("path", dirPath))
		return policies, nil
	}

	// Walk through the directory
	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Only process JSON and YAML files
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".json" && ext != ".yaml" && ext != ".yml" {
			pl.logger.Debug("Skipping non-policy file", slog.String("file", path))
			return nil
		}

		// Load the policy definition
		policy, err := pl.loadPolicyFile(path)
		if err != nil {
			pl.logger.Error("Failed to load policy file",
				slog.String("file", path),
				slog.Any("error", err))
			return err
		}

		// Validate required fields
		if err := pl.validatePolicy(policy); err != nil {
			pl.logger.Error("Invalid policy definition",
				slog.String("file", path),
				slog.Any("error", err))
			return err
		}

		// Check for duplicates
		key := policy.Name + "|" + policy.Version
		if _, exists := policies[key]; exists {
			return fmt.Errorf("duplicate policy definition: %s (version %s)", policy.Name, policy.Version)
		}

		policies[key] = *policy
		pl.logger.Info("Loaded policy definition",
			slog.String("name", policy.Name),
			slog.String("version", policy.Version),
		)
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to load policies from directory: %w", err)
	}

	pl.logger.Info("Successfully loaded policy definitions",
		slog.Int("count", len(policies)),
	)

	return policies, nil
}

// loadPolicyFile loads a single policy definition file
func (pl *PolicyLoader) loadPolicyFile(filePath string) (*models.PolicyDefinition, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	ext := strings.ToLower(filepath.Ext(filePath))

	var policyDef models.PolicyDefinition

	if ext == ".json" {
		if err := json.Unmarshal(data, &policyDef); err != nil {
			return nil, fmt.Errorf("failed to parse JSON: %w", err)
		}
	} else {
		// For YAML, unmarshal to a generic map first, then convert to JSON and unmarshal again
		// This works around the issue where yaml.v3 doesn't use json tags as fallback
		var yamlData map[string]interface{}
		if err := yaml.Unmarshal(data, &yamlData); err != nil {
			return nil, fmt.Errorf("failed to parse YAML: %w", err)
		}

		// Convert to JSON and unmarshal to get proper field mapping via json tags
		jsonData, err := json.Marshal(yamlData)
		if err != nil {
			return nil, fmt.Errorf("failed to convert YAML to JSON: %w", err)
		}

		if err := json.Unmarshal(jsonData, &policyDef); err != nil {
			return nil, fmt.Errorf("failed to unmarshal converted JSON: %w", err)
		}

		pl.logger.Debug("Parsed policy from YAML",
			slog.String("file", filePath),
			slog.String("name", policyDef.Name),
			slog.String("version", policyDef.Version),
			slog.Any("parameters", policyDef.Parameters))
	}

	// Log serialized JSON to see what will be stored
	jsonBytes, _ := json.Marshal(policyDef)
	pl.logger.Debug("Serialized policy to JSON",
		slog.String("file", filePath),
		slog.String("json", string(jsonBytes)))

	return &policyDef, nil
}

// buildManifestEntry is a single entry in build-manifest.yaml
type buildManifestEntry struct {
	Name     string `yaml:"name"`
	Version  string `yaml:"version,omitempty"`
	FilePath string `yaml:"filePath,omitempty"`
	Gomodule string `yaml:"gomodule,omitempty"`
}

// buildManifestFile represents the structure of build-manifest.yaml
type buildManifestFile struct {
	Version  string               `yaml:"version"`
	Policies []buildManifestEntry `yaml:"policies"`
}

// GetCustomPolicyNames parses build-manifest.yaml and returns a set of policy names
// that are locally developed (have a filePath entry rather than a gomodule).
// Returns an empty set without error if the file does not exist.
func (pl *PolicyLoader) GetCustomPolicyNames(buildManifestPath string) (map[string]bool, error) {
	customPolicies := make(map[string]bool)

	data, err := os.ReadFile(buildManifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			pl.logger.Debug("build-manifest.yaml not found, skipping custom policy detection",
				slog.String("path", buildManifestPath))
			return customPolicies, nil
		}
		return nil, fmt.Errorf("failed to read build-manifest.yaml: %w", err)
	}

	var manifest buildManifestFile
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse build-manifest.yaml: %w", err)
	}

	for _, entry := range manifest.Policies {
		if entry.FilePath != "" {
			customPolicies[entry.Name+"|"+entry.Version] = true
			pl.logger.Debug("Detected locally developed custom policy via filePath",
				slog.String("name", entry.Name),
				slog.String("version", entry.Version),
				slog.String("filePath", entry.FilePath))
		} else if entry.Gomodule != "" && !strings.HasPrefix(entry.Gomodule, "github.com/wso2") {
			customPolicies[entry.Name+"|"+entry.Version] = true
			pl.logger.Debug("Detected third-party custom policy via gomodule",
				slog.String("name", entry.Name),
				slog.String("version", entry.Version),
				slog.String("gomodule", entry.Gomodule))
		}
	}

	pl.logger.Info("Parsed build-manifest.yaml for custom policy detection",
		slog.String("path", buildManifestPath),
		slog.Int("localCount", len(customPolicies)),
		slog.Int("totalCount", len(manifest.Policies)))

	return customPolicies, nil
}

// validatePolicy validates a policy definition
func (pl *PolicyLoader) validatePolicy(policy *models.PolicyDefinition) error {
	if strings.TrimSpace(policy.Name) == "" {
		return fmt.Errorf("policy name is required")
	}

	if strings.TrimSpace(policy.Version) == "" {
		return fmt.Errorf("policy version is required")
	}

	// Validate version format (should match pattern ^v\d+\.\d+\.\d+$)
	versionPattern := regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	if !versionPattern.MatchString(policy.Version) {
		return fmt.Errorf("policy version must match semantic version format (e.g., v1.0.0, v2.1.3)")
	}

	if err := validateRetry(policy); err != nil {
		return err
	}

	return nil
}

// validateRetry checks a policy's optional Retry declaration against the
// rules in specs/001-upstream-retry-contribution/data-model.md's
// "Validation Rules". Every check here is independent of any specific
// deployment — a defect in the shipped policy definition, caught once at
// load time rather than resolving to something wrong on every later deploy.
func validateRetry(policy *models.PolicyDefinition) error {
	r := policy.Retry
	if r == nil {
		return nil
	}

	paramSchema := func(name string) (map[string]interface{}, bool) {
		if policy.Parameters == nil {
			return nil, false
		}
		properties, ok := (*policy.Parameters)["properties"].(map[string]interface{})
		if !ok {
			return nil, false
		}
		propSchema, ok := properties[name].(map[string]interface{})
		return propSchema, ok
	}

	// checkParamRef implements Validation Rules 2 and 3: a non-empty
	// paramName must exist in this same policy's own Parameters schema,
	// and (when the schema states a type at all) that type must match what
	// the referencing field expects.
	checkParamRef := func(fieldName, paramName, expectedType string) error {
		if paramName == "" {
			return nil
		}
		propSchema, ok := paramSchema(paramName)
		if !ok {
			return fmt.Errorf("policy %s@%s: retry.%s references param %q, which is not declared in this policy's parameters", policy.Name, policy.Version, fieldName, paramName)
		}
		if actualType, _ := propSchema["type"].(string); actualType != "" && actualType != expectedType {
			return fmt.Errorf("policy %s@%s: retry.%s references param %q of type %q, expected %q", policy.Name, policy.Version, fieldName, paramName, actualType, expectedType)
		}
		return nil
	}

	// Rule 1: mutual exclusivity between each field's fixed and FromParam forms.
	if len(r.StatusCodes) > 0 && r.StatusCodesFromParam != "" {
		return fmt.Errorf("policy %s@%s: retry.statusCodes and retry.statusCodesFromParam cannot both be set", policy.Name, policy.Version)
	}
	maxAttemptsModes := 0
	if r.MaxAttempts != 0 {
		maxAttemptsModes++
	}
	if r.MaxAttemptsFromParam != "" {
		maxAttemptsModes++
	}
	if r.MaxAttemptsFromLongestList != nil {
		maxAttemptsModes++
	}
	if maxAttemptsModes > 1 {
		return fmt.Errorf("policy %s@%s: retry.maxAttempts, retry.maxAttemptsFromParam, and retry.maxAttemptsFromLongestList are mutually exclusive — exactly one must be set", policy.Name, policy.Version)
	}
	if r.AlsoRetryOnConnectionFailure && r.AlsoRetryOnConnectionFailureFromParam != "" {
		return fmt.Errorf("policy %s@%s: retry.alsoRetryOnConnectionFailure and retry.alsoRetryOnConnectionFailureFromParam cannot both be set", policy.Name, policy.Version)
	}

	// Rule 4: statusCodes and maxAttempts are never optional once retry is present.
	if len(r.StatusCodes) == 0 && r.StatusCodesFromParam == "" {
		return fmt.Errorf("policy %s@%s: retry must set either statusCodes or statusCodesFromParam", policy.Name, policy.Version)
	}
	if maxAttemptsModes == 0 {
		return fmt.Errorf("policy %s@%s: retry must set exactly one of maxAttempts, maxAttemptsFromParam, or maxAttemptsFromLongestList", policy.Name, policy.Version)
	}

	if err := checkParamRef("statusCodesFromParam", r.StatusCodesFromParam, "array"); err != nil {
		return err
	}
	if err := checkParamRef("enabledByParam", r.EnabledByParam, "boolean"); err != nil {
		return err
	}
	if err := checkParamRef("maxAttemptsFromParam", r.MaxAttemptsFromParam, "integer"); err != nil {
		return err
	}
	if err := checkParamRef("perAttemptTimeoutMsFromParam", r.PerAttemptTimeoutMsFromParam, "integer"); err != nil {
		return err
	}
	if err := checkParamRef("alsoRetryOnConnectionFailureFromParam", r.AlsoRetryOnConnectionFailureFromParam, "boolean"); err != nil {
		return err
	}
	if r.MaxAttemptsFromLongestList != nil {
		if err := checkParamRef("maxAttemptsFromLongestList.paramPath", r.MaxAttemptsFromLongestList.ParamPath, "array"); err != nil {
			return err
		}
		if r.MaxAttemptsFromLongestList.Plus < 0 {
			return fmt.Errorf("policy %s@%s: retry.maxAttemptsFromLongestList.plus must be >= 0", policy.Name, policy.Version)
		}
	}

	// Rule 5: fixed-value bounds. A fixed value of exactly 0 is
	// indistinguishable from "not set" (Go's int zero value) and is already
	// handled by the mode-count checks above; a negative value is
	// unambiguous and rejected here.
	if r.MaxAttempts < 0 {
		return fmt.Errorf("policy %s@%s: retry.maxAttempts must be >= 1", policy.Name, policy.Version)
	}
	if r.PerAttemptTimeoutMsFromParam == "" && r.PerAttemptTimeoutMs < 0 {
		return fmt.Errorf("policy %s@%s: retry.perAttemptTimeoutMs must be >= 0", policy.Name, policy.Version)
	}

	return nil
}

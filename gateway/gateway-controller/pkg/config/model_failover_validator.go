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

package config

import (
	"fmt"
	"time"

	api "github.com/wso2/api-platform/gateway/gateway-controller/pkg/api/management"
)

// ModelFailoverTarget is one entry in model-failover's ordered fallback chain.
// Models[0] is the primary; index i corresponds to Envoy attempt i+1 — see
// the "AttemptCount alone is sufficient" note in the design spec.
type ModelFailoverTarget struct {
	Name               string // injected into the request body's "model" field for this target
	UpstreamDefinition string // must match an api.UpstreamDefinition.Name declared on the same API
}

// ModelFailoverParams is the parsed, validated shape of the model-failover
// policy's policyParams. RequestTimeout/SuspendDuration are nil when unset
// (SuspendDuration nil means "no suspend tracking at all" — see the design
// spec's cache.strategy note).
type ModelFailoverParams struct {
	Models          []ModelFailoverTarget
	StatusCodes     []int
	RequestTimeout  *time.Duration
	SuspendDuration *time.Duration
	CacheStrategy   string // "memory" (default) or "redis"; only consulted when SuspendDuration != nil
}

// ParseModelFailoverParams parses and validates model-failover's policyParams
// map (as stored in models.Policy.Params). numRetries is deliberately not a
// field here — the translator derives it as len(Models)-1 (Task 6), so it
// can never drift out of sync with the fallback list.
func ParseModelFailoverParams(params map[string]interface{}) (*ModelFailoverParams, error) {
	rawModels, ok := params["models"].([]interface{})
	if !ok || len(rawModels) < 2 {
		return nil, fmt.Errorf("model-failover requires at least 2 entries in 'models' (a primary plus at least one fallback), got %d", len(rawModels))
	}

	models := make([]ModelFailoverTarget, 0, len(rawModels))
	for i, raw := range rawModels {
		m, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("model-failover: models[%d] is not an object", i)
		}
		name, _ := m["name"].(string)
		def, _ := m["upstreamDefinition"].(string)
		if name == "" {
			return nil, fmt.Errorf("model-failover: models[%d].name is required", i)
		}
		if def == "" {
			return nil, fmt.Errorf("model-failover: models[%d].upstreamDefinition is required", i)
		}
		models = append(models, ModelFailoverTarget{Name: name, UpstreamDefinition: def})
	}

	rawStatusCodes, ok := params["statusCodes"].([]interface{})
	if !ok || len(rawStatusCodes) == 0 {
		return nil, fmt.Errorf("model-failover requires a non-empty 'statusCodes' list")
	}
	statusCodes := make([]int, 0, len(rawStatusCodes))
	for i, raw := range rawStatusCodes {
		code, ok := raw.(int)
		if !ok {
			if f, ok := raw.(float64); ok { // YAML/JSON numeric decode may hand back float64
				code = int(f)
			} else {
				return nil, fmt.Errorf("model-failover: statusCodes[%d] must be an integer, got %T", i, raw)
			}
		}
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("model-failover: statusCodes[%d] value %d is not a valid HTTP status code", i, code)
		}
		statusCodes = append(statusCodes, code)
	}

	mf := &ModelFailoverParams{Models: models, StatusCodes: statusCodes, CacheStrategy: "memory"}

	if raw, ok := params["requestTimeout"].(string); ok && raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("model-failover: invalid requestTimeout %q: %w", raw, err)
		}
		mf.RequestTimeout = &d
	}
	if raw, ok := params["suspendDuration"].(string); ok && raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("model-failover: invalid suspendDuration %q: %w", raw, err)
		}
		mf.SuspendDuration = &d
	}
	if cache, ok := params["cache"].(map[string]interface{}); ok {
		if strategy, ok := cache["strategy"].(string); ok && strategy != "" {
			if strategy != "memory" && strategy != "redis" {
				return nil, fmt.Errorf("model-failover: cache.strategy must be 'memory' or 'redis', got %q", strategy)
			}
			mf.CacheStrategy = strategy
		}
	}

	return mf, nil
}

// ValidateModelFailoverPolicy rejects a route that configures both
// model-failover and resilience.retry — see Global Constraints in this
// feature's implementation plan for why they're mutually exclusive in v1:
// both policies drive RouteAction.RetryPolicy, and letting both configure it
// independently would mean whichever one translates last silently wins.
func ValidateModelFailoverPolicy(mf *ModelFailoverParams, retry *api.Retry) error {
	if mf == nil {
		return nil
	}
	if retry != nil {
		return fmt.Errorf("model-failover cannot be combined with resilience.retry on the same route/operation — they both drive RouteAction.RetryPolicy")
	}
	return nil
}

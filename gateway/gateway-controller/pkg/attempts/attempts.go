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

// Package attempts decides, from policy definitions alone, whether an
// operation needs the per-attempt retry hop, and how to build it. It knows
// no policy by name: every decision comes from a definition's
// `retryBehavior` block (models.RetryBehavior).
package attempts

import (
	"fmt"
	"strings"
	"time"

	"github.com/wso2/api-platform/gateway/gateway-controller/pkg/models"
)

// SystemPolicyName is the gateway system policy that runs first on both
// routes of a split operation (see gateway/system-policies/attempts).
const (
	SystemPolicyName    = "wso2_apip_sys_attempts"
	SystemPolicyVersion = "v1"
)

// Params the controller writes into policy instances on a split operation.
const (
	ParamRole       = "_role"
	ParamScope      = "_attemptScope"
	ParamTTL        = "_attemptTTL"
	ParamAllowances = "_allowances"
	ParamHopSecret  = "_hopSecret"
	// ParamRunningAs tells a policy declared runs: onBoth which part it is.
	ParamRunningAs = "_runningAs"

	RoleFront   = "front"
	RoleAttempt = "attempt"

	RunningAsClientRequest = "clientRequest"
	RunningAsAttempt       = "attempt"
)

// Limits are the gateway-wide bounds.
type Limits struct {
	// MaxRetries caps the retries of one client request across every
	// retrying policy on the operation.
	MaxRetries int
	// DefaultPerAttemptTimeout applies when no policy declares one.
	DefaultPerAttemptTimeout time.Duration
}

// DefaultLimits are used when the gateway config sets none.
var DefaultLimits = Limits{MaxRetries: 10, DefaultPerAttemptTimeout: 30 * time.Second}

const routeTimeoutMargin = 2 * time.Second

// Plan is how to build one operation.
type Plan struct {
	// Split is false when no attached policy can retry (or none has it
	// turned on); the operation is then built exactly as without this package.
	Split bool
	// FrontIndexes and AttemptIndexes are positions in the chain, in chain
	// order. A policy declared onBoth appears in both.
	FrontIndexes   []int
	AttemptIndexes []int
	// BothIndexes are the positions of onBoth policies.
	BothIndexes map[int]bool
	// Allowances maps each enabled retrying policy to its max attempts.
	Allowances    map[string]int
	NumRetries    int
	PerTryTimeout time.Duration
	RouteTimeout  time.Duration
	RetryOn       string
}

// Definitions looks up a policy's definition by name and resolved version.
type Definitions func(name, version string) (models.PolicyDefinition, bool)

// FromMap adapts the controller's "name|version" definition map.
func FromMap(defs map[string]models.PolicyDefinition) Definitions {
	return func(name, version string) (models.PolicyDefinition, bool) {
		d, ok := defs[name+"|"+version]
		return d, ok
	}
}

// PlanChain decides how to build an operation whose policy chain is chain.
// isGatewayPolicy reports policies the gateway itself injected (system
// policies); they always stay on the front route.
func PlanChain(chain []models.Policy, defs Definitions, limits Limits, isGatewayPolicy func(string) bool) (Plan, error) {
	p := Plan{BothIndexes: map[int]bool{}, Allowances: map[string]int{}}
	var timeouts []time.Duration
	seesConnection := false
	sumRetries := 0

	behaviors := make([]*models.RetryBehavior, len(chain))
	for i, pol := range chain {
		if def, ok := defs(pol.Name, pol.Version); ok {
			behaviors[i] = def.RetryBehavior
		}
		c := canRetryOf(behaviors[i])
		if c == nil {
			continue
		}
		enabled, err := enabledBy(c, pol.Params)
		if err != nil {
			return Plan{}, fmt.Errorf("%s: %w", pol.Name, err)
		}
		if !enabled {
			continue
		}
		max, err := maxAttempts(c, pol.Params)
		if err != nil {
			return Plan{}, fmt.Errorf("%s: %w", pol.Name, err)
		}
		if prev, dup := p.Allowances[pol.Name]; dup && prev >= max {
			continue
		}
		p.Allowances[pol.Name] = max
		sumRetries += max - 1
		if d, ok, err := perAttemptTimeout(c, pol.Params); err != nil {
			return Plan{}, fmt.Errorf("%s: %w", pol.Name, err)
		} else if ok {
			timeouts = append(timeouts, d)
		}
		seesConnection = seesConnection || c.SeesConnectionFailures
	}
	if len(p.Allowances) == 0 {
		return Plan{}, nil
	}
	if err := checkSameOperation(chain, behaviors); err != nil {
		return Plan{}, err
	}
	if sumRetries > limits.MaxRetries {
		return Plan{}, fmt.Errorf("the policies on this operation can ask for %d retries of one request; the gateway allows at most %d", sumRetries, limits.MaxRetries)
	}

	p.Split = true
	p.NumRetries = sumRetries
	p.PerTryTimeout = limits.DefaultPerAttemptTimeout
	for i, d := range timeouts {
		if i == 0 || d < p.PerTryTimeout {
			p.PerTryTimeout = d
		}
	}
	p.RouteTimeout = time.Duration(p.NumRetries+1)*p.PerTryTimeout + routeTimeoutMargin
	p.RetryOn = "retriable-headers"
	if seesConnection {
		p.RetryOn = "retriable-headers,connect-failure,reset"
	}

	for i, pol := range chain {
		if isGatewayPolicy != nil && isGatewayPolicy(pol.Name) {
			p.FrontIndexes = append(p.FrontIndexes, i)
			continue
		}
		switch behaviors[i].EffectiveRuns() {
		case models.RunsOnEveryAttempt:
			p.AttemptIndexes = append(p.AttemptIndexes, i)
		case models.RunsOnBoth:
			p.FrontIndexes = append(p.FrontIndexes, i)
			p.AttemptIndexes = append(p.AttemptIndexes, i)
			p.BothIndexes[i] = true
		default:
			p.FrontIndexes = append(p.FrontIndexes, i)
		}
	}
	return p, nil
}

func canRetryOf(b *models.RetryBehavior) *models.CanRetry {
	if b == nil {
		return nil
	}
	return b.CanRetry
}

func enabledBy(c *models.CanRetry, params map[string]interface{}) (bool, error) {
	if c.EnabledByParam == "" {
		return true, nil
	}
	v, ok := params[c.EnabledByParam]
	if !ok || v == nil {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", c.EnabledByParam)
	}
	return b, nil
}

func maxAttempts(c *models.CanRetry, params map[string]interface{}) (int, error) {
	switch {
	case c.MaxAttempts != nil:
		return *c.MaxAttempts, nil
	case c.MaxAttemptsFromParam != "":
		n, ok := asInt(params[c.MaxAttemptsFromParam])
		if !ok || n < 1 {
			return 0, fmt.Errorf("%s must be a whole number of at least 1 (it sets how many attempts the policy may cause)", c.MaxAttemptsFromParam)
		}
		return n, nil
	case c.MaxAttemptsFromLongestList != nil:
		l := c.MaxAttemptsFromLongestList
		longest, found := longestList(params, l.Path)
		if !found {
			return 0, fmt.Errorf("no list found at %s", l.Path)
		}
		n := longest + l.Plus
		if n < 1 {
			return 0, fmt.Errorf("the longest list at %s plus %d is below 1", l.Path, l.Plus)
		}
		return n, nil
	}
	return 0, fmt.Errorf("retryBehavior.canRetry has no maxAttempts source")
}

func perAttemptTimeout(c *models.CanRetry, params map[string]interface{}) (time.Duration, bool, error) {
	raw := c.PerAttemptTimeout
	if c.PerAttemptTimeoutFromParam != "" {
		v, ok := params[c.PerAttemptTimeoutFromParam]
		if !ok || v == nil {
			return 0, false, nil
		}
		s, ok := v.(string)
		if !ok {
			return 0, false, fmt.Errorf("%s must be a duration such as 30s", c.PerAttemptTimeoutFromParam)
		}
		raw = s
	}
	if raw == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, false, fmt.Errorf("per-attempt timeout %q must be a positive duration", raw)
	}
	return d, true, nil
}

// checkSameOperation applies every sameOperation rule declared by a policy on
// the chain to the other policies on it.
func checkSameOperation(chain []models.Policy, behaviors []*models.RetryBehavior) error {
	for i, b := range behaviors {
		if b == nil {
			continue
		}
		for _, rule := range b.SameOperation {
			for j, other := range chain {
				if j == i || other.Name != rule.Policy {
					continue
				}
				switch rule.Allowed {
				case models.AllowedNever:
					return fmt.Errorf("%s cannot share an operation with %s", other.Name, chain[i].Name)
				case models.AllowedOnlyIf:
					if rule.OnlyIf.RunsBeforeThisPolicy && j > i {
						return fmt.Errorf("%s must come before %s on the same operation", other.Name, chain[i].Name)
					}
					if rule.OnlyIf.ParamNotSet != "" && pathIsSet(other.Params, rule.OnlyIf.ParamNotSet) {
						return fmt.Errorf("%s may share an operation with %s only if %s is not set", other.Name, chain[i].Name, rule.OnlyIf.ParamNotSet)
					}
				}
			}
		}
	}
	return nil
}

// walk visits every value at path in params. A segment ending in [] fans out
// over that list's elements.
func walk(v interface{}, segments []string, visit func(interface{})) {
	if len(segments) == 0 {
		visit(v)
		return
	}
	seg := segments[0]
	fan := strings.HasSuffix(seg, "[]")
	key := strings.TrimSuffix(seg, "[]")
	m, ok := v.(map[string]interface{})
	if !ok {
		return
	}
	next, ok := m[key]
	if !ok || next == nil {
		return
	}
	if !fan {
		walk(next, segments[1:], visit)
		return
	}
	list, ok := next.([]interface{})
	if !ok {
		return
	}
	for _, e := range list {
		walk(e, segments[1:], visit)
	}
}

func splitPath(path string) []string {
	return strings.Split(strings.TrimSpace(path), ".")
}

func longestList(params map[string]interface{}, path string) (int, bool) {
	longest, found := 0, false
	walk(params, splitPath(path), func(v interface{}) {
		if list, ok := v.([]interface{}); ok {
			found = true
			if len(list) > longest {
				longest = len(list)
			}
		}
	})
	return longest, found
}

func pathIsSet(params map[string]interface{}, path string) bool {
	set := false
	walk(params, splitPath(path), func(v interface{}) {
		if s, ok := v.(string); !ok || s != "" {
			set = true
		}
	})
	return set
}

func asInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), n == float64(int(n))
	}
	return 0, false
}

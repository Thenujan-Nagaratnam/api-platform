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

// Package failover holds what gateway-controller's LlmProxy transformer, RDC
// post-pass, xDS translator and validator must agree on for the model-failover
// policy: the internal header and resource names, the per-boot hop secret, and
// the subset of the policy's parameters the controller itself consumes.
//
// A route carrying model-failover becomes two routes. The front route stays on
// the client-facing listener and gets an Envoy retry policy; every retry
// attempt goes to a dispatch route on an internal listener, where the
// per-target transformer and credential policies run.
package failover

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// PolicyName is the model-failover policy.
const PolicyName = "model-failover"

// Envoy resource names.
const (
	InternalListenerName    = "failover_dispatch"
	DispatchClusterName     = "failover_dispatch"
	DispatchRouteConfigName = "failover_dispatch_routes"
)

// Internal headers. None of them reach a client or a provider.
const (
	HeaderPrefix          = "x-wso2-failover-"
	HeaderChain           = "x-wso2-failover-chain"
	HeaderHop             = "x-wso2-failover-hop"
	HeaderRetry           = "x-wso2-failover-retry"
	HeaderExhausted       = "x-wso2-failover-exhausted"
	HeaderUpstreamFailure = "x-wso2-upstream-failure"
)

// Controller-internal policy params, merged into each instance's params.
const (
	ParamRole         = "_role"
	ParamChainID      = "_chainId"
	ParamTargetIDs    = "_targetIds"
	ParamTargetNative = "_targetNative"
	ParamHopSecret    = "_hopSecret"
	internalPrefix    = "_"
)

// Role is the part a model-failover instance or route plays.
type Role string

const (
	RoleFront    Role = "front"
	RoleDispatch Role = "dispatch"
)

// Retry settings shared by every front route.
const (
	RetryBackOffBase       = time.Millisecond
	RetryBackOffMax        = 10 * time.Millisecond
	DispatchMaxRetries     = 1024
	frontTimeoutMargin     = 2 * time.Second
	defaultPerAttempt      = 30 * time.Second
	minPerAttempt          = time.Second
	maxPerAttempt          = 300 * time.Second
	maxTargets             = 10
	targetUpstreamPrefix   = "failover-"
	UpstreamFailureFlags   = "UF,URX,UH,UO,UT,UC,DC,LR"
	RouteMetadataKey       = "failover"
	RouteMetadataNamespace = "wso2.route"
	// HopMetadataNamespace/HopMetadataKey hold the hop secret once the
	// client-facing listener's hop filter has taken it off the request.
	HopMetadataNamespace = "wso2.failover"
	HopMetadataKey       = "hop"
	HopFilterName        = "wso2.failover.hop"
)

var (
	hopSecretOnce sync.Once
	hopSecret     string
	durationRe    = regexp.MustCompile(`^[0-9]+(ms|s|m)$`)
)

// HopSecret returns a random value generated once per controller process. The
// dispatch hop sends it on its loopback request, and the provider hop's
// local-reply mapper only annotates transport failures on requests that carry
// it, so a client calling a provider route directly never sees the flags.
func HopSecret() string {
	hopSecretOnce.Do(func() {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic(fmt.Sprintf("failover: cannot generate hop secret: %v", err))
		}
		hopSecret = hex.EncodeToString(b)
	})
	return hopSecret
}

// TargetUpstreamName is the per-target loopback upstream definition.
func TargetUpstreamName(targetID string) string {
	return targetUpstreamPrefix + targetID
}

// Target is one authored chain entry.
type Target struct {
	Provider string
	Model    string
}

// Settings is what the controller reads from an authored model-failover
// instance. The policy re-parses and fully validates its own params at
// runtime; ValidateParams below is the registration-time equivalent.
type Settings struct {
	Targets           []Target
	PerAttemptTimeout time.Duration
	// RetryOnTimeout is failoverOn.timeout: whether a per-attempt timeout
	// moves the request on, which the front route expresses as retry_on reset.
	RetryOnTimeout bool
}

// NumRetries is the Envoy num_retries for the chain.
func (s Settings) NumRetries() int {
	return len(s.Targets) - 1
}

// FrontTimeout bounds the whole chain: every attempt at its limit plus a
// margin for back-off and the hops themselves. It is the outer bound derived
// from the inner per-attempt one, never a generic default.
func (s Settings) FrontTimeout() time.Duration {
	return time.Duration(len(s.Targets))*s.PerAttemptTimeout + frontTimeoutMargin
}

// RetryOn is the front route's retry_on value.
func (s Settings) RetryOn() string {
	if s.RetryOnTimeout {
		return "retriable-headers,connect-failure,reset"
	}
	return "retriable-headers,connect-failure"
}

// ParseSettings extracts Settings from authored params, applying the policy
// definition's defaults for omitted keys.
func ParseSettings(params map[string]interface{}) (Settings, error) {
	s := Settings{PerAttemptTimeout: defaultPerAttempt, RetryOnTimeout: true}
	raw, ok := params["targets"].([]interface{})
	if !ok || len(raw) == 0 {
		return s, fmt.Errorf("targets is required and must be a non-empty array")
	}
	if len(raw) > maxTargets {
		return s, fmt.Errorf("targets must contain at most %d entries", maxTargets)
	}
	seen := map[string]int{}
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			return s, fmt.Errorf("targets[%d] must be an object", i)
		}
		provider, _ := m["provider"].(string)
		model, _ := m["model"].(string)
		if strings.TrimSpace(provider) == "" {
			return s, fmt.Errorf("targets[%d].provider is required", i)
		}
		if strings.TrimSpace(model) == "" {
			return s, fmt.Errorf("targets[%d].model is required", i)
		}
		key := provider + "\x00" + model
		if prev, dup := seen[key]; dup {
			return s, fmt.Errorf("targets[%d] duplicates targets[%d] (provider %q, model %q)", i, prev, provider, model)
		}
		seen[key] = i
		s.Targets = append(s.Targets, Target{Provider: provider, Model: model})
	}
	if v, present := params["perAttemptTimeout"]; present {
		str, ok := v.(string)
		if !ok || !durationRe.MatchString(str) {
			return s, fmt.Errorf("perAttemptTimeout must be a duration like 30s, 500ms or 2m")
		}
		d, err := time.ParseDuration(str)
		if err != nil || d < minPerAttempt || d > maxPerAttempt {
			return s, fmt.Errorf("perAttemptTimeout must be between %s and %s", minPerAttempt, maxPerAttempt)
		}
		s.PerAttemptTimeout = d
	}
	if fo, present := params["failoverOn"]; present {
		m, ok := fo.(map[string]interface{})
		if !ok {
			return s, fmt.Errorf("failoverOn must be an object")
		}
		if v, present := m["timeout"]; present {
			b, ok := v.(bool)
			if !ok {
				return s, fmt.Errorf("failoverOn.timeout must be a boolean")
			}
			s.RetryOnTimeout = b
		}
	}
	return s, nil
}

// HasInternalParams reports whether authored params set a controller-internal key.
func HasInternalParams(params map[string]interface{}) (string, bool) {
	for k := range params {
		if strings.HasPrefix(k, internalPrefix) {
			return k, true
		}
	}
	return "", false
}

// RoleOf returns the role recorded in an instance's params, if any.
func RoleOf(params map[string]interface{}) (Role, bool) {
	r, _ := params[ParamRole].(string)
	switch Role(r) {
	case RoleFront, RoleDispatch:
		return Role(r), true
	}
	return "", false
}

// ValidateParams checks authored params against every bound in the policy
// definition, so a bad configuration is rejected at registration rather than
// when the policy engine first builds the chain.
func ValidateParams(params map[string]interface{}) error {
	if key, bad := HasInternalParams(params); bad {
		return fmt.Errorf("parameter %q is reserved for gateway-internal use", key)
	}
	if _, err := ParseSettings(params); err != nil {
		return err
	}
	if fo, present := params["failoverOn"]; present {
		m, _ := fo.(map[string]interface{})
		if codesRaw, ok := m["statusCodes"]; ok {
			codes, ok := codesRaw.([]interface{})
			if !ok {
				return fmt.Errorf("failoverOn.statusCodes must be an array")
			}
			seen := map[int]bool{}
			for i, c := range codes {
				code, ok := asInt(c)
				if !ok || (code != 429 && (code < 500 || code > 599)) {
					return fmt.Errorf("failoverOn.statusCodes[%d] must be 429 or 500-599", i)
				}
				if seen[code] {
					return fmt.Errorf("failoverOn.statusCodes[%d]: duplicate status code %d", i, code)
				}
				seen[code] = true
			}
		}
		for _, key := range []string{"connectFailure", "reset"} {
			if v, present := m[key]; present {
				if _, ok := v.(bool); !ok {
					return fmt.Errorf("failoverOn.%s must be a boolean", key)
				}
			}
		}
	}
	for _, b := range []struct {
		key    string
		lo, hi int
	}{
		{"suspendAfterConsecutiveFailures", 1, 100},
		{"probeConcurrency", 1, 10},
		{"recoverAfterSuccessfulProbes", 1, 20},
	} {
		if v, present := params[b.key]; present {
			n, ok := asInt(v)
			if !ok || n < b.lo || n > b.hi {
				return fmt.Errorf("%s must be an integer between %d and %d", b.key, b.lo, b.hi)
			}
		}
	}
	if v, present := params["suspendDuration"]; present {
		str, ok := v.(string)
		d, err := time.ParseDuration(str)
		if !ok || !durationRe.MatchString(str) || err != nil || d < time.Second || d > time.Hour {
			return fmt.Errorf("suspendDuration must be a duration between 1s and 1h")
		}
	}
	return nil
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

// ProviderSelectingPolicies set selected_provider themselves and so cannot
// share a route with model-failover, whose dispatch hop owns that key.
var ProviderSelectingPolicies = []string{
	"llm-header-router",
	"model-round-robin",
	"model-weighted-round-robin",
	"intelligent-model-routing",
	"cost-based-model-routing",
	"semantic-model-routing",
	"time-based-model-routing",
}

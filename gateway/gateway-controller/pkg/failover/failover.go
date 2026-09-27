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
	// ParamRouteToTarget is false in provider mode: every attempt goes to the
	// provider's own upstream, so the dispatch role selects no upstream.
	ParamRouteToTarget = "_routeToTarget"
	internalPrefix     = "_"
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
	maxChains              = 20
	maxFallbacks           = 9
	maxTargets             = 50
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

// Target is one provider and model a chain can try.
type Target struct {
	Provider string
	Model    string
}

// Chain is one primary model and its fallbacks, as indices into
// Settings.Targets: primary first.
type Chain struct {
	Primary string
	Targets []int
}

// Settings is what the controller reads from a model-failover instance. The
// policy re-parses and fully validates its own params at runtime;
// ValidateParams below is the registration-time equivalent.
type Settings struct {
	// Targets are the distinct targets of every chain in flattening order
	// (chains in order, primary then fallbacks, repeats skipped): the order
	// the policy uses, so per-target ids and upstreams line up with it. The
	// policy's pass-through target, appended after these, is not included.
	Targets           []Target
	Chains            []Chain
	PerAttemptTimeout time.Duration
	// RetryOnTimeout is failoverOn.timeout: whether a per-attempt timeout
	// moves the request on, which the front route expresses as retry_on reset.
	RetryOnTimeout bool
}

// LongestChain is the most attempts one request can make.
func (s Settings) LongestChain() int {
	n := 1
	for _, c := range s.Chains {
		if len(c.Targets) > n {
			n = len(c.Targets)
		}
	}
	return n
}

// NumRetries is the Envoy num_retries for the route: enough for the longest
// chain. Shorter chains stop early when their plan runs out.
func (s Settings) NumRetries() int {
	return s.LongestChain() - 1
}

// FrontTimeout bounds the whole chain: every attempt at its limit plus a
// margin for back-off and the hops themselves. It is the outer bound derived
// from the inner per-attempt one, never a generic default.
func (s Settings) FrontTimeout() time.Duration {
	return time.Duration(s.LongestChain())*s.PerAttemptTimeout + frontTimeoutMargin
}

// RetryOn is the front route's retry_on value.
func (s Settings) RetryOn() string {
	if s.RetryOnTimeout {
		return "retriable-headers,connect-failure,reset"
	}
	return "retriable-headers,connect-failure"
}

// ParseSettings extracts Settings without resolving providers, which is
// enough for the retry budget. Omitted keys take the policy definition's
// defaults.
func ParseSettings(params map[string]interface{}) (Settings, error) {
	return ParseSettingsFor(params, "", false)
}

// ParseSettingsFor extracts Settings and fills in providers. primaryProvider
// is where a chain's primary runs: an LlmProxy's primary provider, or the
// LlmProvider itself; a fallback without a provider uses it too. When
// sameProvider is set (an LlmProvider), a fallback may name no other provider,
// because every attempt goes to the provider's own upstream.
func ParseSettingsFor(params map[string]interface{}, primaryProvider string, sameProvider bool) (Settings, error) {
	s := Settings{PerAttemptTimeout: defaultPerAttempt, RetryOnTimeout: true}
	if _, old := params["targets"]; old {
		return s, fmt.Errorf("targets was replaced by chains; list a primary model and its fallbacks")
	}
	raw, ok := params["chains"].([]interface{})
	if !ok || len(raw) == 0 {
		return s, fmt.Errorf("chains is required and must be a non-empty array")
	}
	if len(raw) > maxChains {
		return s, fmt.Errorf("chains must contain at most %d entries", maxChains)
	}
	index := map[Target]int{}
	primaries := map[string]int{}
	for i, item := range raw {
		path := fmt.Sprintf("chains[%d]", i)
		m, ok := item.(map[string]interface{})
		if !ok {
			return s, fmt.Errorf("%s must be an object", path)
		}
		pm, ok := m["primary"].(map[string]interface{})
		if !ok {
			return s, fmt.Errorf("%s.primary is required", path)
		}
		primaryModel, _ := pm["model"].(string)
		if strings.TrimSpace(primaryModel) == "" {
			return s, fmt.Errorf("%s.primary.model is required", path)
		}
		if prev, dup := primaries[primaryModel]; dup {
			return s, fmt.Errorf("%s.primary.model: %q already has a chain (chains[%d])", path, primaryModel, prev)
		}
		primaries[primaryModel] = i
		members := []Target{{Provider: primaryProvider, Model: primaryModel}}
		if p, _ := pm["provider"].(string); p != "" && primaryProvider == "" {
			members[0].Provider = p // controller-rewritten params
		}

		fl, ok := m["fallbacks"].([]interface{})
		if !ok || len(fl) == 0 || len(fl) > maxFallbacks {
			return s, fmt.Errorf("%s.fallbacks must contain between 1 and %d entries", path, maxFallbacks)
		}
		for j, f := range fl {
			fm, ok := f.(map[string]interface{})
			if !ok {
				return s, fmt.Errorf("%s.fallbacks[%d] must be an object", path, j)
			}
			provider, _ := fm["provider"].(string)
			model, _ := fm["model"].(string)
			if strings.TrimSpace(model) == "" {
				return s, fmt.Errorf("%s.fallbacks[%d].model is required", path, j)
			}
			switch {
			case sameProvider && provider != "" && provider != primaryProvider:
				return s, fmt.Errorf("%s.fallbacks[%d].provider: on an LlmProvider, fallbacks name models of this provider only; attach %q to an LlmProxy to fail over to it", path, j, provider)
			case provider == "":
				provider = members[0].Provider
			}
			members = append(members, Target{Provider: provider, Model: model})
		}

		chain := Chain{Primary: primaryModel}
		inChain := map[Target]bool{}
		for k, t := range members {
			if inChain[t] {
				return s, fmt.Errorf("%s lists provider %q, model %q more than once (entry %d)", path, t.Provider, t.Model, k)
			}
			inChain[t] = true
			idx, seen := index[t]
			if !seen {
				idx = len(s.Targets)
				index[t] = idx
				s.Targets = append(s.Targets, t)
			}
			chain.Targets = append(chain.Targets, idx)
		}
		s.Chains = append(s.Chains, chain)
	}
	if len(s.Targets) > maxTargets {
		return s, fmt.Errorf("chains name %d distinct provider and model pairs; at most %d are allowed", len(s.Targets), maxTargets)
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

// ResolvedChains renders chains with every provider filled in, for the
// params the policy receives.
func (s Settings) ResolvedChains() []interface{} {
	out := make([]interface{}, len(s.Chains))
	for i, c := range s.Chains {
		p := s.Targets[c.Targets[0]]
		fallbacks := make([]interface{}, 0, len(c.Targets)-1)
		for _, idx := range c.Targets[1:] {
			t := s.Targets[idx]
			fallbacks = append(fallbacks, map[string]interface{}{"provider": t.Provider, "model": t.Model})
		}
		out[i] = map[string]interface{}{
			"primary":   map[string]interface{}{"provider": p.Provider, "model": p.Model},
			"fallbacks": fallbacks,
		}
	}
	return out
}

// CheckAuthored rejects what only the controller may write.
func CheckAuthored(params map[string]interface{}) error {
	if key, bad := HasInternalParams(params); bad {
		return fmt.Errorf("parameter %q is reserved for gateway-internal use", key)
	}
	chains, _ := params["chains"].([]interface{})
	for i, c := range chains {
		m, _ := c.(map[string]interface{})
		pm, _ := m["primary"].(map[string]interface{})
		if _, set := pm["provider"]; set {
			return fmt.Errorf("chains[%d].primary.provider: the primary is the requested model on the provider the request is routed to; name providers on fallbacks", i)
		}
	}
	return nil
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
// when the policy engine first builds the chain. primaryProvider and
// sameProvider are as in ParseSettingsFor.
func ValidateParams(params map[string]interface{}, primaryProvider string, sameProvider bool) error {
	if err := CheckAuthored(params); err != nil {
		return err
	}
	if _, err := ParseSettingsFor(params, primaryProvider, sameProvider); err != nil {
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

// PassThroughTarget is the target that forwards a request whose model has no
// chain: the primary's provider with no model override. The policy expects it
// after every chain target.
func PassThroughTarget(primaryProvider string) Target {
	return Target{Provider: primaryProvider}
}

// ModelSelectingPolicies pick only a model when none of their entries names a
// provider. Placed before model-failover on an operation, they choose the
// primary whose chain runs.
var ModelSelectingPolicies = []string{"model-round-robin", "model-weighted-round-robin"}

// IsModelSelector reports whether name is one of ModelSelectingPolicies.
func IsModelSelector(name string) bool {
	for _, n := range ModelSelectingPolicies {
		if n == name {
			return true
		}
	}
	return false
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

// ValidateRequestModel checks a provider template's requestModel, which the
// dispatch role uses to write each attempt's model: a JSONPath into the body,
// a header, a query parameter, or a path regex whose first capture group is
// the model. A nil requestModel means the top-level "model" of the body.
// opPath is the operation's path: a path model must sit in a wildcard or
// parameter segment, or the rewritten request no longer matches its route.
func ValidateRequestModel(requestModel interface{}, opPath string) error {
	if requestModel == nil {
		return nil
	}
	m, ok := requestModel.(map[string]interface{})
	if !ok {
		return fmt.Errorf("the template's requestModel must be an object")
	}
	// The controller stores the template's typed enum, a JSON round trip a
	// plain string; fmt.Sprint reads both the same way.
	location := fmt.Sprint(m["location"])
	identifier := fmt.Sprint(m["identifier"])
	if m["identifier"] == nil || identifier == "" {
		return fmt.Errorf("the template's requestModel has no identifier")
	}
	switch location {
	case "payload", "header", "queryParam":
		return nil
	case "pathParam":
	default:
		return fmt.Errorf("the template's requestModel location %q is not supported", location)
	}
	re, err := regexp.Compile(identifier)
	if err != nil || re.NumSubexp() < 1 {
		return fmt.Errorf("the template's requestModel path pattern %q needs a capture group around the model", identifier)
	}
	if sub := re.FindStringSubmatch(opPath); len(sub) > 1 && !strings.ContainsAny(sub[1], "*{") {
		return fmt.Errorf("the path %s fixes the model to %q; use a wildcard path (for example /models/*) so every target's model matches the operation", opPath, sub[1])
	}
	return nil
}

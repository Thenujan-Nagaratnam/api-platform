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

package failover

import (
	"strings"
	"testing"
	"time"
)

// oneChain is primary model m on the primary provider, falling back to m on b.
func oneChain() []interface{} {
	return []interface{}{map[string]interface{}{
		"primary":   map[string]interface{}{"model": "m"},
		"fallbacks": []interface{}{map[string]interface{}{"provider": "b", "model": "m"}},
	}}
}

func chain(primary string, fallbacks ...map[string]interface{}) map[string]interface{} {
	fl := make([]interface{}, len(fallbacks))
	for i, f := range fallbacks {
		fl[i] = f
	}
	return map[string]interface{}{"primary": map[string]interface{}{"model": primary}, "fallbacks": fl}
}

func fb(provider, model string) map[string]interface{} {
	m := map[string]interface{}{"model": model}
	if provider != "" {
		m["provider"] = provider
	}
	return m
}

func TestParseSettingsDefaultsAndDerivedValues(t *testing.T) {
	s, err := ParseSettingsFor(map[string]interface{}{"chains": oneChain()}, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if s.PerAttemptTimeout != 30*time.Second || !s.RetryOnTimeout {
		t.Fatalf("defaults: %+v", s)
	}
	if s.NumRetries() != 1 || s.FrontTimeout() != 62*time.Second {
		t.Fatalf("derived: retries=%d timeout=%s", s.NumRetries(), s.FrontTimeout())
	}
	if s.RetryOn() != "retriable-headers,connect-failure,reset" {
		t.Fatalf("retry_on: %s", s.RetryOn())
	}
}

func TestRetryOnWithoutTimeoutFailover(t *testing.T) {
	s, err := ParseSettings(map[string]interface{}{"chains": oneChain(), "failoverOn": map[string]interface{}{"timeout": false}})
	if err != nil {
		t.Fatal(err)
	}
	if s.RetryOn() != "retriable-headers,connect-failure" {
		t.Fatalf("reset must be dropped when timeouts do not fail over: %s", s.RetryOn())
	}
}

func TestChainsFlattenLikeThePolicy(t *testing.T) {
	s, err := ParseSettingsFor(map[string]interface{}{"chains": []interface{}{
		chain("gpt-4o", fb("b", "gpt-4o"), fb("anthropic", "claude")),
		chain("gpt-4.1", fb("", "gpt-4.1-mini"), fb("anthropic", "claude")),
	}}, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []Target{{"a", "gpt-4o"}, {"b", "gpt-4o"}, {"anthropic", "claude"}, {"a", "gpt-4.1"}, {"a", "gpt-4.1-mini"}}
	if len(s.Targets) != len(want) {
		t.Fatalf("targets %+v", s.Targets)
	}
	for i := range want {
		if s.Targets[i] != want[i] {
			t.Fatalf("targets %+v, want %+v", s.Targets, want)
		}
	}
	if c := s.Chains[1]; c.Primary != "gpt-4.1" || len(c.Targets) != 3 || c.Targets[2] != 2 {
		t.Fatalf("gpt-4.1 chain %+v must reuse claude's index", c)
	}
	if s.NumRetries() != 2 {
		t.Fatalf("num_retries follows the longest chain, got %d", s.NumRetries())
	}
	resolved := s.ResolvedChains()[1].(map[string]interface{})
	if resolved["primary"].(map[string]interface{})["provider"] != "a" || resolved["fallbacks"].([]interface{})[0].(map[string]interface{})["provider"] != "a" {
		t.Fatalf("resolved chains must name every provider: %v", resolved)
	}
	// Re-parsing the rewritten params (no primary provider known) gives the same targets.
	again, err := ParseSettings(map[string]interface{}{"chains": s.ResolvedChains()})
	if err != nil || len(again.Targets) != len(want) || again.Targets[3] != want[3] {
		t.Fatalf("rewritten params must re-parse identically: %+v %v", again.Targets, err)
	}
}

func TestValidateParams(t *testing.T) {
	ok := []map[string]interface{}{
		{"chains": oneChain()},
		{"chains": oneChain(), "failoverOn": map[string]interface{}{"statusCodes": []interface{}{float64(429), float64(599)}}},
		{"chains": oneChain(), "suspendDuration": "60m", "probeConcurrency": float64(10), "recoverAfterSuccessfulProbes": float64(20)},
	}
	for i, p := range ok {
		if err := ValidateParams(p, "a", false); err != nil {
			t.Errorf("case %d: unexpected error %v", i, err)
		}
	}
	many := func(n int) []map[string]interface{} {
		out := make([]map[string]interface{}, n)
		for i := range out {
			out[i] = fb("", "f"+string(rune('a'+i)))
		}
		return out
	}
	bad := map[string]map[string]interface{}{
		"empty":               {},
		"old targets":         {"targets": []interface{}{map[string]interface{}{"provider": "a", "model": "m"}}},
		"internal key":        {"chains": oneChain(), "_chainId": "x"},
		"primary provider":    {"chains": []interface{}{map[string]interface{}{"primary": map[string]interface{}{"provider": "a", "model": "m"}, "fallbacks": []interface{}{fb("", "n")}}}},
		"duplicate primary":   {"chains": []interface{}{chain("m", fb("", "n")), chain("m", fb("", "o"))}},
		"no fallbacks":        {"chains": []interface{}{chain("m")}},
		"ten fallbacks":       {"chains": []interface{}{chain("m", many(10)...)}},
		"repeat in chain":     {"chains": []interface{}{chain("m", fb("", "n"), fb("a", "n"))}},
		"fallback is primary": {"chains": []interface{}{chain("m", fb("a", "m"))}},
		"status 499":          {"chains": oneChain(), "failoverOn": map[string]interface{}{"statusCodes": []interface{}{float64(499)}}},
		"dup status":          {"chains": oneChain(), "failoverOn": map[string]interface{}{"statusCodes": []interface{}{float64(503), float64(503)}}},
		"reset not bool":      {"chains": oneChain(), "failoverOn": map[string]interface{}{"reset": "yes"}},
		"suspend 2h":          {"chains": oneChain(), "suspendDuration": "2h"},
		"probes 1.5":          {"chains": oneChain(), "probeConcurrency": float64(1.5)},
		"timeout 301s":        {"chains": oneChain(), "perAttemptTimeout": "301s"},
	}
	for name, p := range bad {
		if err := ValidateParams(p, "a", false); err == nil {
			t.Errorf("%s: expected an error for %v", name, p)
		}
	}
	var chains []interface{}
	for i := 0; i < 21; i++ {
		chains = append(chains, chain("p"+string(rune('a'+i)), fb("", "f")))
	}
	if err := ValidateParams(map[string]interface{}{"chains": chains}, "a", false); err == nil {
		t.Error("21 chains must be rejected")
	}
}

func TestHopSecretIsStableAndRandom(t *testing.T) {
	a, b := HopSecret(), HopSecret()
	if a != b || len(a) != 64 {
		t.Fatalf("hop secret must be a stable 256-bit hex value, got %q / %q", a, b)
	}
}

func TestParseSettingsForProviderFillsOwnName(t *testing.T) {
	s, err := ParseSettingsFor(map[string]interface{}{"chains": []interface{}{
		chain("gpt-4o", fb("", "gpt-4o-mini"), fb("openai", "gpt-4.1")),
	}}, "openai", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range s.Targets {
		if tg.Provider != "openai" {
			t.Fatalf("provider must be filled with the provider's own name: %+v", s.Targets)
		}
	}
}

func TestParseSettingsForProviderRejectsOtherProvider(t *testing.T) {
	_, err := ParseSettingsFor(map[string]interface{}{"chains": []interface{}{chain("gpt-4o", fb("anthropic", "claude"))}}, "openai", true)
	if err == nil || !strings.Contains(err.Error(), "attach \"anthropic\" to an LlmProxy") {
		t.Fatalf("expected the cross-provider rejection, got %v", err)
	}
}

type locationEnum string

func TestValidateRequestModel(t *testing.T) {
	gemini := map[string]interface{}{"location": locationEnum("pathParam"), "identifier": `models/([a-zA-Z0-9.\-]+)`}
	ok := []struct {
		rm   interface{}
		path string
	}{
		{nil, "/chat/completions"},
		{map[string]interface{}{"location": "payload", "identifier": "$.model"}, "/chat/completions"},
		{map[string]interface{}{"location": locationEnum("payload"), "identifier": "$.input.model"}, "/chat"},
		{map[string]interface{}{"location": "header", "identifier": "x-model"}, "/chat"},
		{map[string]interface{}{"location": "queryParam", "identifier": "model"}, "/chat"},
		{gemini, "/models/*"},
		{gemini, "/models/{model}:generateContent"},
	}
	for i, c := range ok {
		if err := ValidateRequestModel(c.rm, c.path); err != nil {
			t.Errorf("ok case %d: %v", i, err)
		}
	}
	bad := []struct {
		rm   interface{}
		path string
		msg  string
	}{
		{"payload", "/chat", "must be an object"},
		{map[string]interface{}{"location": "cookie", "identifier": "m"}, "/chat", "not supported"},
		{map[string]interface{}{"location": "header"}, "/chat", "no identifier"},
		{map[string]interface{}{"location": "pathParam", "identifier": "models/[a-z]+"}, "/models/*", "capture group"},
		{gemini, "/models/gemini-2.5-pro:generateContent", "use a wildcard path"},
	}
	for i, c := range bad {
		err := ValidateRequestModel(c.rm, c.path)
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("bad case %d: got %v, want %q", i, err, c.msg)
		}
	}
}

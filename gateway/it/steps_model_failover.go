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

package it

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cucumber/godog"
	"github.com/wso2/api-platform/gateway/it/steps"
)

// mockLLMPorts are the host ports of the scriptable mock LLM backends in
// docker-compose.test.yaml (tests/mock-servers/mock-llm-provider).
var mockLLMPorts = map[string]int{
	"openai-a":  8091,
	"openai-b":  8092,
	"anthropic": 8093,
}

var mockLLMClient = &http.Client{Timeout: 5 * time.Second}

type mockLLMRequests struct {
	Count int `json:"count"`
	Last  *struct {
		Path    string            `json:"path"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	} `json:"last"`
}

func mockLLMURL(name, path string) (string, error) {
	port, ok := mockLLMPorts[name]
	if !ok {
		return "", fmt.Errorf("unknown mock LLM %q (known: openai-a, openai-b, anthropic)", name)
	}
	return fmt.Sprintf("http://localhost:%d%s", port, path), nil
}

func mockLLMDo(method, name, path, body string) (*http.Response, error) {
	u, err := mockLLMURL(name, path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	return mockLLMClient.Do(req)
}

func mockLLMState(name string) (*mockLLMRequests, error) {
	resp, err := mockLLMDo(http.MethodGet, name, "/__requests", "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out mockLLMRequests
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("mock LLM %s: bad /__requests response: %w", name, err)
	}
	return &out, nil
}

func mockLLMLast(name string) (*mockLLMRequests, error) {
	st, err := mockLLMState(name)
	if err != nil {
		return nil, err
	}
	if st.Last == nil {
		return nil, fmt.Errorf("mock LLM %s has received no request", name)
	}
	return st, nil
}

// failoverFixtureProviders are the LlmProviders a fixture deploys, keyed by
// the suffix appended to the fixture prefix. "dead" points at a closed port
// so a request to it fails to connect.
var failoverFixtureProviders = []struct {
	suffix   string
	upstream string
}{
	{"openai-a", "http://mock-llm-openai-a:8080"},
	{"openai-b", "http://mock-llm-openai-b:8080"},
	{"dead", "http://mock-llm-openai-a:9"},
	{"anthropic", "http://mock-llm-anthropic:8080"},
}

func failoverProviderYAML(prefix, suffix, upstream string) string {
	name := prefix + "-" + suffix
	template, header, value := "openai", "Authorization", "Bearer "+name+"-key"
	if suffix == "anthropic" {
		template, header, value = "anthropic", "x-api-key", name+"-key"
	}
	return fmt.Sprintf(`apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProvider
metadata:
  name: %[1]s
spec:
  displayName: %[1]s
  version: v1.0
  template: %[3]s
  context: /%[1]s
  upstream:
    url: %[2]s
    auth:
      type: api-key
      header: %[4]s
      value: %[5]s
  accessControl:
    mode: allow_all
`, name, upstream, template, header, value)
}

// failoverProxyYAML attaches the fixture's providers to one proxy and puts
// model-failover, with the given JSON params, on POST /chat/completions.
func failoverProxyYAML(prefix, paramsJSON string) string {
	return fmt.Sprintf(`apiVersion: gateway.api-platform.wso2.com/v1
kind: LlmProxy
metadata:
  name: %[1]s-proxy
spec:
  displayName: %[1]s-proxy
  version: v1.0
  context: /%[1]s-proxy
  provider:
    id: %[1]s-openai-a
  additionalProviders:
    - id: %[1]s-openai-b
    - id: %[1]s-dead
    - id: %[1]s-anthropic
      transformer:
        type: openai-to-anthropic-transformer
        version: v0
        params:
          model: claude-default
  operationPolicies:
    - name: model-failover
      version: v0
      paths:
        - path: /chat/completions
          methods: [POST]
          params: %[2]s
`, prefix, strings.TrimSpace(paramsJSON))
}

func basicAuth(state *TestState) (string, error) {
	user, ok := state.Config.Users["admin"]
	if !ok {
		return "", fmt.Errorf("admin user not configured")
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user.Username+":"+user.Password)), nil
}

func expectStatus(httpSteps *steps.HTTPSteps, want int, what string) error {
	resp := httpSteps.LastResponse()
	if resp == nil {
		return fmt.Errorf("%s: no response", what)
	}
	if resp.StatusCode != want {
		return fmt.Errorf("%s: status %d, want %d: %s", what, resp.StatusCode, want, httpSteps.LastBody())
	}
	return nil
}

// RegisterModelFailoverSteps registers the steps that deploy the
// model-failover fixture and script and inspect the mock LLM backends used by
// features/model-failover.feature.
func RegisterModelFailoverSteps(ctx *godog.ScenarioContext, state *TestState, httpSteps *steps.HTTPSteps) {
	ctx.Step(`^I deploy the model-failover fixture "([^"]*)" with params:$`, func(prefix string, params *godog.DocString) error {
		auth, err := basicAuth(state)
		if err != nil {
			return err
		}
		for _, p := range failoverFixtureProviders {
			httpSteps.SetHeader("Authorization", auth)
			httpSteps.SetHeader("Content-Type", "application/yaml")
			doc := &godog.DocString{Content: failoverProviderYAML(prefix, p.suffix, p.upstream)}
			if err := httpSteps.SendPOSTToService("gateway-controller", "/llm-providers", doc); err != nil {
				return err
			}
			if err := expectStatus(httpSteps, http.StatusCreated, "create provider "+prefix+"-"+p.suffix); err != nil {
				return err
			}
		}
		httpSteps.SetHeader("Authorization", auth)
		httpSteps.SetHeader("Content-Type", "application/yaml")
		if err := httpSteps.SendPOSTToService("gateway-controller", "/llm-proxies",
			&godog.DocString{Content: failoverProxyYAML(prefix, params.Content)}); err != nil {
			return err
		}
		if err := expectStatus(httpSteps, http.StatusCreated, "create proxy "+prefix+"-proxy"); err != nil {
			return err
		}
		httpSteps.ClearHeader("Authorization")
		httpSteps.ClearHeader("Content-Type")
		return nil
	})

	ctx.Step(`^I delete the model-failover fixture "([^"]*)"$`, func(prefix string) error {
		auth, err := basicAuth(state)
		if err != nil {
			return err
		}
		httpSteps.SetHeader("Authorization", auth)
		var firstErr error
		note := func(err error) {
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		note(httpSteps.SendDELETEToService("gateway-controller", "/llm-proxies/"+prefix+"-proxy"))
		for _, p := range failoverFixtureProviders {
			note(httpSteps.SendDELETEToService("gateway-controller", "/llm-providers/"+prefix+"-"+p.suffix))
		}
		httpSteps.ClearHeader("Authorization")
		return firstErr
	})

	ctx.Step(`^the request should have taken less than "(\d+)" seconds since "([^"]*)"$`, func(maxSeconds int, key string) error {
		v, ok := state.GetContextValue(key)
		start, isTime := v.(time.Time)
		if !ok || !isTime {
			return fmt.Errorf("no start time recorded as %q", key)
		}
		if elapsed := time.Since(start); elapsed >= time.Duration(maxSeconds)*time.Second {
			return fmt.Errorf("request took %s, want less than %ds", elapsed, maxSeconds)
		}
		return nil
	})

	// A streamed response that the upstream cuts off mid-way ends with a
	// transport error on the client side; this step records the status and
	// whatever arrived instead of failing on the read error.
	ctx.Step(`^I send a POST request that may end early to "([^"]*)" with body:$`, func(url string, body *godog.DocString) error {
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body.Content))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			return fmt.Errorf("request failed before a response arrived: %w", err)
		}
		defer resp.Body.Close()
		partial, _ := io.ReadAll(resp.Body)
		state.SetContextValue("early_status", resp.StatusCode)
		state.SetContextValue("early_body", string(partial))
		return nil
	})

	ctx.Step(`^the early-ended response status should be (\d+)$`, func(want int) error {
		v, _ := state.GetContextValue("early_status")
		if got, _ := v.(int); got != want {
			return fmt.Errorf("status %v, want %d", v, want)
		}
		return nil
	})

	ctx.Step(`^the early-ended response body should contain "([^"]*)"$`, func(want string) error {
		v, _ := state.GetContextValue("early_body")
		if b, _ := v.(string); !strings.Contains(b, want) {
			return fmt.Errorf("partial body %q does not contain %q", b, want)
		}
		return nil
	})

	ctx.Step(`^I reset the mock LLMs$`, func() error {
		for name := range mockLLMPorts {
			resp, err := mockLLMDo(http.MethodDelete, name, "/__requests", "")
			if err != nil {
				return fmt.Errorf("reset mock LLM %s: %w", name, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		return nil
	})

	ctx.Step(`^the mock LLM "([^"]*)" is set to "([^"]*)"$`, func(name, mode string) error {
		resp, err := mockLLMDo(http.MethodPut, name, "/__mode", fmt.Sprintf(`{"mode":%q}`, mode))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("set mode on mock LLM %s: %d %s", name, resp.StatusCode, b)
		}
		return nil
	})

	ctx.Step(`^the mock LLM "([^"]*)" should have received (\d+) requests?$`, func(name string, want int) error {
		st, err := mockLLMState(name)
		if err != nil {
			return err
		}
		if st.Count != want {
			return fmt.Errorf("mock LLM %s received %d requests, want %d", name, st.Count, want)
		}
		return nil
	})

	ctx.Step(`^the mock LLM "([^"]*)" last request header "([^"]*)" should be "([^"]*)"$`, func(name, header, want string) error {
		st, err := mockLLMLast(name)
		if err != nil {
			return err
		}
		if got := st.Last.Headers[strings.ToLower(header)]; got != want {
			return fmt.Errorf("mock LLM %s last request header %s = %q, want %q", name, header, got, want)
		}
		return nil
	})

	ctx.Step(`^the mock LLM "([^"]*)" last request should not have header "([^"]*)"$`, func(name, header string) error {
		st, err := mockLLMLast(name)
		if err != nil {
			return err
		}
		if v, ok := st.Last.Headers[strings.ToLower(header)]; ok {
			return fmt.Errorf("mock LLM %s last request unexpectedly carried %s: %q", name, header, v)
		}
		return nil
	})

	ctx.Step(`^the mock LLM "([^"]*)" last request path should contain "([^"]*)"$`, func(name, want string) error {
		st, err := mockLLMLast(name)
		if err != nil {
			return err
		}
		if !strings.Contains(st.Last.Path, want) {
			return fmt.Errorf("mock LLM %s last request path %q does not contain %q", name, st.Last.Path, want)
		}
		return nil
	})

	ctx.Step(`^the mock LLM "([^"]*)" last request body should contain "([^"]*)"$`, func(name, want string) error {
		st, err := mockLLMLast(name)
		if err != nil {
			return err
		}
		if !strings.Contains(st.Last.Body, want) {
			return fmt.Errorf("mock LLM %s last request body does not contain %q: %s", name, want, st.Last.Body)
		}
		return nil
	})
}

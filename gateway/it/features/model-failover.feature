# --------------------------------------------------------------------
# Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
#
# WSO2 LLC. licenses this file to you under the Apache License,
# Version 2.0 (the "License"); you may not use this file except
# in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.
# --------------------------------------------------------------------

# model-failover walks an ordered chain of LLM targets using Envoy's native
# retry policy (specs/001-model-failover-policy). Each fixture deploys three
# LlmProviders backed by scriptable mocks (tests/mock-servers/mock-llm-provider):
#   <prefix>-openai-a -> mock-llm-openai-a   (host port 8091)
#   <prefix>-openai-b -> mock-llm-openai-b   (host port 8092)
#   <prefix>-dead      -> a closed port        (connection refused)
#   <prefix>-anthropic -> mock-llm-anthropic  (host port 8093), template anthropic,
#                         attached with openai-to-anthropic-transformer
# and one LlmProxy <prefix>-proxy with model-failover on POST /chat/completions.
# OpenAI providers authenticate upstream with "Authorization: Bearer <name>-key",
# the Anthropic one with "x-api-key: <name>-key".
Feature: Model failover across an ordered chain of LLM targets
  As an API publisher
  I want requests to fall back to the next model target when one fails
  So that clients keep getting answers when a provider is rate-limited or down

  Background:
    Given the gateway services are running

  Scenario: A healthy primary serves the request and no fallback is contacted
    When I deploy the model-failover fixture "mf-ok" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-ok-openai-b", "model": "gpt-b"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-ok-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    When I send a POST request to "http://localhost:8080/mf-ok-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": [{"role": "user", "content": "hi"}]}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-a"
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-b" should have received 0 requests
    And the mock LLM "openai-a" last request body should contain "gpt-a"
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-plan"
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-hop"
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-chain"
    And I delete the model-failover fixture "mf-ok"

  Scenario: A 429 on the primary fails over to the next target with that target's own credentials
    When I deploy the model-failover fixture "mf-429" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-429-openai-b", "model": "gpt-b"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-429-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:429"
    When I send a POST request to "http://localhost:8080/mf-429-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": [{"role": "user", "content": "hi"}]}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-b"
    And the response header "x-wso2-attempt-retry" should not exist
    And the response header "x-wso2-upstream-failure" should not exist
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-b" should have received 1 request
    And the mock LLM "openai-b" last request header "Authorization" should be "Bearer mf-429-openai-b-key"
    And the mock LLM "openai-b" last request body should contain "gpt-b"
    And I delete the model-failover fixture "mf-429"

  Scenario: A non-eligible 400 is returned to the client without failover
    When I deploy the model-failover fixture "mf-400" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-400-openai-b", "model": "gpt-b"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-400-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:400"
    When I send a POST request to "http://localhost:8080/mf-400-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 400
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-b" should have received 0 requests
    And I delete the model-failover fixture "mf-400"

  Scenario: A 503 is passed through unchanged when only 429 is configured to fail over
    When I deploy the model-failover fixture "mf-503" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-503-openai-b", "model": "gpt-b"}]}], "failoverOn": {"statusCodes": [429]}}
      """
    And I wait for the endpoint "http://localhost:8080/mf-503-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-503-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 503
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-b" should have received 0 requests
    And I delete the model-failover fixture "mf-503"

  Scenario: A connection failure on the primary fails over to the next target
    When I deploy the model-failover fixture "mf-conn" with primary "dead" and params:
      """
      {"chains": [{"primary": {"model": "gpt-dead"}, "fallbacks": [{"provider": "mf-conn-openai-a", "model": "gpt-a"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-conn-proxy/chat/completions" to be ready with method "POST" and body '{"model":"gpt-dead","messages":[]}'
    And I reset the mock LLMs
    When I send a POST request to "http://localhost:8080/mf-conn-proxy/chat/completions" with body:
      """
      {"model": "gpt-dead", "messages": []}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-a"
    And the response header "x-wso2-upstream-failure" should not exist
    And the mock LLM "openai-a" should have received 1 request
    And I delete the model-failover fixture "mf-conn"

  Scenario: A connection reset on the primary fails over to the next target
    When I deploy the model-failover fixture "mf-reset" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-reset-openai-b", "model": "gpt-b"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-reset-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "reset"
    When I send a POST request to "http://localhost:8080/mf-reset-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-b"
    And the mock LLM "openai-b" should have received 1 request
    And I delete the model-failover fixture "mf-reset"

  Scenario: Client-supplied internal failover headers are ignored
    When I deploy the model-failover fixture "mf-spoof" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-spoof-openai-b", "model": "gpt-b"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-spoof-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And I set header "x-wso2-attempt-plan" to "00112233445566778899aabbccddeeff"
    And I set header "x-wso2-attempt-chain" to "forged"
    And I set header "x-wso2-attempt-hop" to "guess"
    When I send a POST request to "http://localhost:8080/mf-spoof-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-a"
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-hop"
    And I clear all headers
    And I delete the model-failover fixture "mf-spoof"

  Scenario: Invalid model-failover configuration is rejected at registration
    Given I authenticate using basic auth as "admin"
    When I deploy this LLM proxy configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProxy
      metadata:
        name: mf-invalid-proxy
      spec:
        displayName: mf-invalid-proxy
        version: v1.0
        context: /mf-invalid-proxy
        provider:
          id: mf-invalid-missing-provider
        operationPolicies:
          - name: model-failover
            version: v0
            paths:
              - path: /chat/completions
                methods: [POST]
                params:
                  chains:
                    - primary: {model: m}
                      fallbacks:
                        - provider: not-attached
                          model: n
                  perAttemptTimeout: 0s
      """
    Then the response status code should be 400

  # ==================== US3: cross-provider failover ====================

  Scenario: An Anthropic fallback receives an Anthropic request and the client gets OpenAI format
    When I deploy the model-failover fixture "mf-x" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-x-anthropic", "model": "claude-sonnet-4-5"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-x-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-x-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": [{"role": "user", "content": "hi"}]}
      """
    Then the response status code should be 200
    And the response body should contain "chat.completion"
    And the response body should contain "hello from mock-llm-anthropic"
    And the mock LLM "anthropic" should have received 1 request
    And the mock LLM "anthropic" last request path should contain "/v1/messages"
    And the mock LLM "anthropic" last request header "x-api-key" should be "mf-x-anthropic-key"
    And the mock LLM "anthropic" last request should not have header "Authorization"
    And the mock LLM "anthropic" last request body should contain "claude-sonnet-4-5"
    And I delete the model-failover fixture "mf-x"

  Scenario: A streamed Anthropic fallback reaches the client as an OpenAI stream
    When I deploy the model-failover fixture "mf-xs" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-xs-anthropic", "model": "claude-sonnet-4-5"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-xs-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:429"
    When I send a POST request to "http://localhost:8080/mf-xs-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "stream": true, "messages": [{"role": "user", "content": "hi"}]}
      """
    Then the response status code should be 200
    And the response body should contain "chat.completion.chunk"
    And the response body should contain "hello from mock-llm-anthropic"
    And the response body should contain "[DONE]"
    And the mock LLM "anthropic" should have received 1 request
    And I delete the model-failover fixture "mf-xs"

  # ==================== US4: suspension and recovery ====================

  Scenario: A primary that keeps failing is suspended and skipped
    When I deploy the model-failover fixture "mf-susp" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-susp-openai-b", "model": "gpt-b"}]}], "suspendAfterConsecutiveFailures": 2, "suspendDuration": "30s"}
      """
    And I wait for the endpoint "http://localhost:8080/mf-susp-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-susp-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 200
    When I send a POST request to "http://localhost:8080/mf-susp-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" should have received 2 requests
    # openai-a is healthy again, but suspended: the next request goes straight to openai-b.
    When I reset the mock LLMs
    And I send a POST request to "http://localhost:8080/mf-susp-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-b"
    And the mock LLM "openai-a" should have received 0 requests
    And the mock LLM "openai-b" should have received 1 request
    And I delete the model-failover fixture "mf-susp"

  Scenario: A suspended target recovers after a successful probe
    When I deploy the model-failover fixture "mf-rec" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-rec-openai-b", "model": "gpt-b"}]}], "suspendAfterConsecutiveFailures": 2, "suspendDuration": "5s", "recoverAfterSuccessfulProbes": 1}
      """
    And I wait for the endpoint "http://localhost:8080/mf-rec-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-rec-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    And I send a POST request to "http://localhost:8080/mf-rec-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    And I reset the mock LLMs
    And I wait for 6 seconds
    # The suspension is over: the next request probes openai-a, which now succeeds.
    When I send a POST request to "http://localhost:8080/mf-rec-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-a"
    When I send a POST request to "http://localhost:8080/mf-rec-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response body should contain "hello from mock-llm-openai-a"
    And the mock LLM "openai-a" should have received 2 requests
    And the mock LLM "openai-b" should have received 0 requests
    And I delete the model-failover fixture "mf-rec"

  Scenario: A failed probe suspends the target again
    When I deploy the model-failover fixture "mf-reprobe" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-reprobe-openai-b", "model": "gpt-b"}]}], "suspendAfterConsecutiveFailures": 2, "suspendDuration": "5s"}
      """
    And I wait for the endpoint "http://localhost:8080/mf-reprobe-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-reprobe-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    And I send a POST request to "http://localhost:8080/mf-reprobe-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    And I wait for 6 seconds
    # The probe reaches openai-a, which still fails, so openai-b serves and openai-a is suspended again.
    When I send a POST request to "http://localhost:8080/mf-reprobe-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" should have received 3 requests
    When I send a POST request to "http://localhost:8080/mf-reprobe-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" should have received 3 requests
    And I delete the model-failover fixture "mf-reprobe"

  Scenario: When every target is suspended the gateway answers without contacting any
    When I deploy the model-failover fixture "mf-allsusp" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-allsusp-openai-b", "model": "gpt-b"}]}], "suspendAfterConsecutiveFailures": 1, "suspendDuration": "60s"}
      """
    And I wait for the endpoint "http://localhost:8080/mf-allsusp-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    And the mock LLM "openai-b" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-allsusp-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 503
    When I reset the mock LLMs
    And I send a POST request to "http://localhost:8080/mf-allsusp-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 503
    And the response body should contain "all_targets_unavailable"
    And the mock LLM "openai-a" should have received 0 requests
    And the mock LLM "openai-b" should have received 0 requests
    And I delete the model-failover fixture "mf-allsusp"

  # ==================== US5: bounded, time-boxed attempts ====================

  Scenario: A hanging primary is abandoned after the per-attempt timeout
    When I deploy the model-failover fixture "mf-hang" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-hang-openai-b", "model": "gpt-b"}]}], "perAttemptTimeout": "2s"}
      """
    And I wait for the endpoint "http://localhost:8080/mf-hang-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "hang:10"
    And I record the current time as "request_start"
    When I send a POST request to "http://localhost:8080/mf-hang-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-b"
    And the request should have taken at least "2" seconds since "request_start"
    And the request should have taken less than "5" seconds since "request_start"
    And I delete the model-failover fixture "mf-hang"

  Scenario: No failover happens once the response has started streaming
    When I deploy the model-failover fixture "mf-midstream" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-midstream-openai-b", "model": "gpt-b"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-midstream-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "stream-abort:2"
    When I send a POST request that may end early to "http://localhost:8080/mf-midstream-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "stream": true, "messages": []}
      """
    Then the early-ended response status should be 200
    And the early-ended response body should contain "part0"
    And the mock LLM "openai-b" should have received 0 requests
    And I delete the model-failover fixture "mf-midstream"

  # ==================== US6: exhaustion ====================

  Scenario: When every target fails the client gets the fixed exhaustion error and each target is tried once
    When I deploy the model-failover fixture "mf-exh" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a"}, "fallbacks": [{"provider": "mf-exh-openai-b", "model": "gpt-b"}, {"provider": "mf-exh-anthropic", "model": "claude-sonnet-4-5"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-exh-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    And the mock LLM "openai-b" is set to "status:429"
    And the mock LLM "anthropic" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-exh-proxy/chat/completions" with body:
      """
      {"model": "gpt-a", "messages": []}
      """
    Then the response status code should be 503
    And the response body should be:
      """
      {"error":{"message":"All configured model targets are currently unavailable. Please retry later.","type":"model_failover_exhausted","param":null,"code":"all_targets_unavailable"}}
      """
    And the response header "x-wso2-attempt-retry" should not exist
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-b" should have received 1 request
    And the mock LLM "anthropic" should have received 1 request
    And I delete the model-failover fixture "mf-exh"

  # ==================== Provider mode (specs/002-provider-model-failover) ====================

  Scenario: A provider falls back to its next model on 429
    When I deploy the provider model-failover fixture "mfp-429" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a1"}, "fallbacks": [{"model": "gpt-a2"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mfp-429/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "seq:status:429,ok"
    When I send a POST request to "http://localhost:8080/mfp-429/chat/completions" with body:
      """
      {"model": "gpt-a1", "messages": [{"role": "user", "content": "hi"}]}
      """
    Then the response status code should be 200
    And the response header "x-wso2-attempt-retry" should not exist
    And the mock LLM "openai-a" should have received 2 requests
    And the mock LLM "openai-a" last request body should contain "gpt-a2"
    And the mock LLM "openai-a" last request header "Authorization" should be "Bearer mfp-429-key"
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-hop"
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-plan"
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-chain"
    And I send a DELETE request to the "gateway-controller" service at "/llm-providers/mfp-429"

  Scenario: A provider's healthy first model serves without a second attempt
    When I deploy the provider model-failover fixture "mfp-ok" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a1"}, "fallbacks": [{"provider": "mfp-ok", "model": "gpt-a2"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mfp-ok/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    When I send a POST request to "http://localhost:8080/mfp-ok/chat/completions" with body:
      """
      {"model": "gpt-a1", "messages": []}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-a" last request body should contain "gpt-a1"
    And I send a DELETE request to the "gateway-controller" service at "/llm-providers/mfp-ok"

  Scenario: A provider passes a non-eligible 400 through and falls back on a reset
    When I deploy the provider model-failover fixture "mfp-400" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a1"}, "fallbacks": [{"model": "gpt-a2"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mfp-400/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:400"
    When I send a POST request to "http://localhost:8080/mfp-400/chat/completions" with body:
      """
      {"model": "gpt-a1", "messages": []}
      """
    Then the response status code should be 400
    And the mock LLM "openai-a" should have received 1 request
    When I reset the mock LLMs
    And the mock LLM "openai-a" is set to "seq:reset,ok"
    And I send a POST request to "http://localhost:8080/mfp-400/chat/completions" with body:
      """
      {"model": "gpt-a1", "messages": []}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" last request body should contain "gpt-a2"
    And I send a DELETE request to the "gateway-controller" service at "/llm-providers/mfp-400"

  Scenario: A provider whose models all fail returns the fixed exhaustion error
    When I deploy the provider model-failover fixture "mfp-exh" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a1"}, "fallbacks": [{"model": "gpt-a2"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mfp-exh/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mfp-exh/chat/completions" with body:
      """
      {"model": "gpt-a1", "messages": []}
      """
    Then the response status code should be 503
    And the response body should contain "all_targets_unavailable"
    And the mock LLM "openai-a" should have received 2 requests
    And I send a DELETE request to the "gateway-controller" service at "/llm-providers/mfp-exh"

  Scenario: A provider suspends a failing model and skips it
    When I deploy the provider model-failover fixture "mfp-susp" with params:
      """
      {"chains": [{"primary": {"model": "gpt-a1"}, "fallbacks": [{"model": "gpt-a2"}]}], "suspendAfterConsecutiveFailures": 2, "suspendDuration": "30s"}
      """
    And I wait for the endpoint "http://localhost:8080/mfp-susp/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "seq:status:503,ok,status:503,ok,ok"
    When I send a POST request to "http://localhost:8080/mfp-susp/chat/completions" with body:
      """
      {"model": "gpt-a1", "messages": []}
      """
    And I send a POST request to "http://localhost:8080/mfp-susp/chat/completions" with body:
      """
      {"model": "gpt-a1", "messages": []}
      """
    And I send a POST request to "http://localhost:8080/mfp-susp/chat/completions" with body:
      """
      {"model": "gpt-a1", "messages": []}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" should have received 5 requests
    And the mock LLM "openai-a" last request body should contain "gpt-a2"
    And I send a DELETE request to the "gateway-controller" service at "/llm-providers/mfp-susp"

  Scenario: A provider with a path-model template falls back by rewriting the path
    Given I authenticate using basic auth as "admin"
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: mfp-gemini
      spec:
        displayName: mfp-gemini
        version: v1.0
        template: gemini
        context: /mfp-gemini
        upstream:
          url: http://mock-llm-openai-a:8080
        accessControl:
          mode: allow_all
        operationPolicies:
          - name: model-failover
            version: v0
            paths:
              - path: /models/*
                methods: [POST]
                params:
                  chains:
                    - primary: {model: gemini-2.5-pro}
                      fallbacks:
                        - model: gemini-2.5-flash
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/mfp-gemini/models/client:generateContent" to be ready with method "POST" and body '{"contents":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "seq:status:429,ok"
    When I send a POST request to "http://localhost:8080/mfp-gemini/models/gemini-2.5-pro:generateContent?alt=sse" with body:
      """
      {"contents": [{"role": "user", "parts": [{"text": "hi"}]}]}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" should have received 2 requests
    And the mock LLM "openai-a" last request path should contain "/models/gemini-2.5-flash:generateContent"
    And the mock LLM "openai-a" last request path should contain "alt=sse"
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-hop"
    And I send a DELETE request to the "gateway-controller" service at "/llm-providers/mfp-gemini"

  Scenario: A provider path that fixes the model is rejected
    Given I authenticate using basic auth as "admin"
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: mfp-gemini-fixed
      spec:
        displayName: mfp-gemini-fixed
        version: v1.0
        template: gemini
        context: /mfp-gemini-fixed
        upstream:
          url: http://mock-llm-openai-a:8080
        accessControl:
          mode: allow_all
        operationPolicies:
          - name: model-failover
            version: v0
            paths:
              - path: /models/gemini-2.5-pro:generateContent
                methods: [POST]
                params:
                  chains:
                    - primary: {model: gemini-2.5-pro}
                      fallbacks:
                        - model: gemini-2.5-flash
      """
    Then the response status code should be 400
    And the response body should contain "use a wildcard path"

  Scenario: A provider target naming another provider is rejected
    Given I authenticate using basic auth as "admin"
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: mfp-cross
      spec:
        displayName: mfp-cross
        version: v1.0
        template: openai
        context: /mfp-cross
        upstream:
          url: http://mock-llm-openai-a:8080
        accessControl:
          mode: allow_all
        operationPolicies:
          - name: model-failover
            version: v0
            paths:
              - path: /chat/completions
                methods: [POST]
                params:
                  chains:
                    - primary: {model: gpt-a1}
                      fallbacks:
                        - provider: some-other-provider
                          model: gpt-b1
      """
    Then the response status code should be 400
    And the response body should contain "LlmProxy"

  # ==================== Chains keyed by the requested model (specs/003-model-keyed-chains) ====================

  Scenario: Each requested model fails over along its own chain
    When I deploy the model-failover fixture "mfc-keyed" with params:
      """
      {"chains": [{"primary": {"model": "gpt-4o"}, "fallbacks": [{"provider": "mfc-keyed-openai-b", "model": "gpt-4o"}, {"provider": "mfc-keyed-anthropic", "model": "claude-sonnet-4-5"}]}, {"primary": {"model": "gpt-4.1"}, "fallbacks": [{"model": "gpt-4.1-mini"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mfc-keyed-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "seq:status:429,ok"
    When I send a POST request to "http://localhost:8080/mfc-keyed-proxy/chat/completions" with body:
      """
      {"model": "gpt-4.1", "messages": [{"role": "user", "content": "hi"}]}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" should have received 2 requests
    And the mock LLM "openai-a" last request body should contain "gpt-4.1-mini"
    And the mock LLM "openai-b" should have received 0 requests
    And the mock LLM "anthropic" should have received 0 requests
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:429"
    When I send a POST request to "http://localhost:8080/mfc-keyed-proxy/chat/completions" with body:
      """
      {"model": "gpt-4o", "messages": [{"role": "user", "content": "hi"}]}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-b"
    And the mock LLM "openai-b" last request body should contain "gpt-4o"
    And I delete the model-failover fixture "mfc-keyed"

  Scenario: A model with no chain passes through with one attempt and its reply unchanged
    When I deploy the model-failover fixture "mfc-pass" with params:
      """
      {"chains": [{"primary": {"model": "gpt-4o"}, "fallbacks": [{"provider": "mfc-pass-openai-b", "model": "gpt-4o"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mfc-pass-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mfc-pass-proxy/chat/completions" with body:
      """
      {"model": "gpt-4o-mini", "messages": []}
      """
    Then the response status code should be 503
    And the response body should not contain "all_targets_unavailable"
    And the response header "x-wso2-attempt-retry" should not exist
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-a" last request body should contain "gpt-4o-mini"
    And the mock LLM "openai-a" last request header "Authorization" should be "Bearer mfc-pass-openai-a-key"
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-hop"
    And the mock LLM "openai-b" should have received 0 requests
    And I delete the model-failover fixture "mfc-pass"

  Scenario: Round-robin picks the primary and failover walks that primary's chain
    When I deploy the model-failover fixture "mfc-rr" with round-robin over "gpt-4.1" and params:
      """
      {"chains": [{"primary": {"model": "gpt-4o"}, "fallbacks": [{"provider": "mfc-rr-openai-b", "model": "gpt-4o"}]}, {"primary": {"model": "gpt-4.1"}, "fallbacks": [{"model": "gpt-4.1-mini"}]}]}
      """
    And I wait for the endpoint "http://localhost:8080/mfc-rr-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "seq:status:429,ok"
    When I send a POST request to "http://localhost:8080/mfc-rr-proxy/chat/completions" with body:
      """
      {"model": "gpt-4o", "messages": []}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" should have received 2 requests
    And the mock LLM "openai-a" last request body should contain "gpt-4.1-mini"
    And the mock LLM "openai-b" should have received 0 requests
    And I delete the model-failover fixture "mfc-rr"

  Scenario: Round-robin placed after model-failover on the same operation is rejected
    Given I authenticate using basic auth as "admin"
    When I deploy this LLM proxy configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProxy
      metadata:
        name: mfc-rr-after-proxy
      spec:
        displayName: mfc-rr-after-proxy
        version: v1.0
        context: /mfc-rr-after-proxy
        provider:
          id: mfc-rr-after-missing
        operationPolicies:
          - name: model-failover
            version: v0
            paths:
              - path: /chat/completions
                methods: [POST]
                params:
                  chains:
                    - primary: {model: gpt-4o}
                      fallbacks:
                        - model: gpt-4o-mini
          - name: model-round-robin
            version: v1
            paths:
              - path: /chat/completions
                methods: [POST]
                params:
                  models:
                    - model: gpt-4o
      """
    Then the response status code should be 400
    And the response body should contain "must come before"

  Scenario: The removed targets parameter is rejected
    Given I authenticate using basic auth as "admin"
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: mfc-old
      spec:
        displayName: mfc-old
        version: v1.0
        template: openai
        context: /mfc-old
        upstream:
          url: http://mock-llm-openai-a:8080
        accessControl:
          mode: allow_all
        operationPolicies:
          - name: model-failover
            version: v0
            paths:
              - path: /chat/completions
                methods: [POST]
                params:
                  targets:
                    - model: gpt-a1
                    - model: gpt-a2
      """
    Then the response status code should be 400
    And the response body should contain "chains"

  Scenario: A Gemini provider passes a model with no chain through unchanged
    Given I authenticate using basic auth as "admin"
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: mfc-gemini-pass
      spec:
        displayName: mfc-gemini-pass
        version: v1.0
        template: gemini
        context: /mfc-gemini-pass
        upstream:
          url: http://mock-llm-openai-a:8080
        accessControl:
          mode: allow_all
        operationPolicies:
          - name: model-failover
            version: v0
            paths:
              - path: /models/*
                methods: [POST]
                params:
                  chains:
                    - primary: {model: gemini-2.5-pro}
                      fallbacks:
                        - model: gemini-2.5-flash
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/mfc-gemini-pass/models/other:generateContent" to be ready with method "POST" and body '{"contents":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:429"
    When I send a POST request to "http://localhost:8080/mfc-gemini-pass/models/gemini-1.5:generateContent" with body:
      """
      {"contents": [{"role": "user", "parts": [{"text": "hi"}]}]}
      """
    Then the response status code should be 429
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-a" last request path should contain "/models/gemini-1.5:generateContent"
    And I send a DELETE request to the "gateway-controller" service at "/llm-providers/mfc-gemini-pass"

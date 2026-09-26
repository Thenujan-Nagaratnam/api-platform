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
      {"targets": [{"provider": "mf-ok-openai-a", "model": "gpt-a"}, {"provider": "mf-ok-openai-b", "model": "gpt-b"}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-ok-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    When I send a POST request to "http://localhost:8080/mf-ok-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": [{"role": "user", "content": "hi"}]}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-a"
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-b" should have received 0 requests
    And the mock LLM "openai-a" last request body should contain "gpt-a"
    And the mock LLM "openai-a" last request should not have header "x-wso2-failover-plan"
    And the mock LLM "openai-a" last request should not have header "x-wso2-failover-hop"
    And the mock LLM "openai-a" last request should not have header "x-wso2-failover-chain"
    And I delete the model-failover fixture "mf-ok"

  Scenario: A 429 on the primary fails over to the next target with that target's own credentials
    When I deploy the model-failover fixture "mf-429" with params:
      """
      {"targets": [{"provider": "mf-429-openai-a", "model": "gpt-a"}, {"provider": "mf-429-openai-b", "model": "gpt-b"}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-429-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:429"
    When I send a POST request to "http://localhost:8080/mf-429-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": [{"role": "user", "content": "hi"}]}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-b"
    And the response header "x-wso2-failover-retry" should not exist
    And the response header "x-wso2-upstream-failure" should not exist
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-b" should have received 1 request
    And the mock LLM "openai-b" last request header "Authorization" should be "Bearer mf-429-openai-b-key"
    And the mock LLM "openai-b" last request body should contain "gpt-b"
    And I delete the model-failover fixture "mf-429"

  Scenario: A non-eligible 400 is returned to the client without failover
    When I deploy the model-failover fixture "mf-400" with params:
      """
      {"targets": [{"provider": "mf-400-openai-a", "model": "gpt-a"}, {"provider": "mf-400-openai-b", "model": "gpt-b"}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-400-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:400"
    When I send a POST request to "http://localhost:8080/mf-400-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 400
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-b" should have received 0 requests
    And I delete the model-failover fixture "mf-400"

  Scenario: A 503 is passed through unchanged when only 429 is configured to fail over
    When I deploy the model-failover fixture "mf-503" with params:
      """
      {"targets": [{"provider": "mf-503-openai-a", "model": "gpt-a"}, {"provider": "mf-503-openai-b", "model": "gpt-b"}], "failoverOn": {"statusCodes": [429]}}
      """
    And I wait for the endpoint "http://localhost:8080/mf-503-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-503-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 503
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-b" should have received 0 requests
    And I delete the model-failover fixture "mf-503"

  Scenario: A connection failure on the primary fails over to the next target
    When I deploy the model-failover fixture "mf-conn" with params:
      """
      {"targets": [{"provider": "mf-conn-dead", "model": "gpt-dead"}, {"provider": "mf-conn-openai-a", "model": "gpt-a"}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-conn-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    When I send a POST request to "http://localhost:8080/mf-conn-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-a"
    And the response header "x-wso2-upstream-failure" should not exist
    And the mock LLM "openai-a" should have received 1 request
    And I delete the model-failover fixture "mf-conn"

  Scenario: A connection reset on the primary fails over to the next target
    When I deploy the model-failover fixture "mf-reset" with params:
      """
      {"targets": [{"provider": "mf-reset-openai-a", "model": "gpt-a"}, {"provider": "mf-reset-openai-b", "model": "gpt-b"}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-reset-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "reset"
    When I send a POST request to "http://localhost:8080/mf-reset-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-b"
    And the mock LLM "openai-b" should have received 1 request
    And I delete the model-failover fixture "mf-reset"

  Scenario: Client-supplied internal failover headers are ignored
    When I deploy the model-failover fixture "mf-spoof" with params:
      """
      {"targets": [{"provider": "mf-spoof-openai-a", "model": "gpt-a"}, {"provider": "mf-spoof-openai-b", "model": "gpt-b"}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-spoof-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And I set header "x-wso2-failover-plan" to "00112233445566778899aabbccddeeff"
    And I set header "x-wso2-failover-chain" to "forged"
    And I set header "x-wso2-failover-hop" to "guess"
    When I send a POST request to "http://localhost:8080/mf-spoof-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-a"
    And the mock LLM "openai-a" last request should not have header "x-wso2-failover-hop"
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
                  targets:
                    - provider: not-attached
                      model: m
                  perAttemptTimeout: 0s
      """
    Then the response status code should be 400

  # ==================== US3: cross-provider failover ====================

  Scenario: An Anthropic fallback receives an Anthropic request and the client gets OpenAI format
    When I deploy the model-failover fixture "mf-x" with params:
      """
      {"targets": [{"provider": "mf-x-openai-a", "model": "gpt-a"}, {"provider": "mf-x-anthropic", "model": "claude-sonnet-4-5"}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-x-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-x-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": [{"role": "user", "content": "hi"}]}
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
      {"targets": [{"provider": "mf-xs-openai-a", "model": "gpt-a"}, {"provider": "mf-xs-anthropic", "model": "claude-sonnet-4-5"}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-xs-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:429"
    When I send a POST request to "http://localhost:8080/mf-xs-proxy/chat/completions" with body:
      """
      {"model": "client", "stream": true, "messages": [{"role": "user", "content": "hi"}]}
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
      {"targets": [{"provider": "mf-susp-openai-a", "model": "gpt-a"}, {"provider": "mf-susp-openai-b", "model": "gpt-b"}], "suspendAfterConsecutiveFailures": 2, "suspendDuration": "30s"}
      """
    And I wait for the endpoint "http://localhost:8080/mf-susp-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-susp-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 200
    When I send a POST request to "http://localhost:8080/mf-susp-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" should have received 2 requests
    # openai-a is healthy again, but suspended: the next request goes straight to openai-b.
    When I reset the mock LLMs
    And I send a POST request to "http://localhost:8080/mf-susp-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-b"
    And the mock LLM "openai-a" should have received 0 requests
    And the mock LLM "openai-b" should have received 1 request
    And I delete the model-failover fixture "mf-susp"

  Scenario: A suspended target recovers after a successful probe
    When I deploy the model-failover fixture "mf-rec" with params:
      """
      {"targets": [{"provider": "mf-rec-openai-a", "model": "gpt-a"}, {"provider": "mf-rec-openai-b", "model": "gpt-b"}], "suspendAfterConsecutiveFailures": 2, "suspendDuration": "5s", "recoverAfterSuccessfulProbes": 1}
      """
    And I wait for the endpoint "http://localhost:8080/mf-rec-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-rec-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    And I send a POST request to "http://localhost:8080/mf-rec-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    And I reset the mock LLMs
    And I wait for 6 seconds
    # The suspension is over: the next request probes openai-a, which now succeeds.
    When I send a POST request to "http://localhost:8080/mf-rec-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-a"
    When I send a POST request to "http://localhost:8080/mf-rec-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response body should contain "hello from mock-llm-openai-a"
    And the mock LLM "openai-a" should have received 2 requests
    And the mock LLM "openai-b" should have received 0 requests
    And I delete the model-failover fixture "mf-rec"

  Scenario: A failed probe suspends the target again
    When I deploy the model-failover fixture "mf-reprobe" with params:
      """
      {"targets": [{"provider": "mf-reprobe-openai-a", "model": "gpt-a"}, {"provider": "mf-reprobe-openai-b", "model": "gpt-b"}], "suspendAfterConsecutiveFailures": 2, "suspendDuration": "5s"}
      """
    And I wait for the endpoint "http://localhost:8080/mf-reprobe-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-reprobe-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    And I send a POST request to "http://localhost:8080/mf-reprobe-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    And I wait for 6 seconds
    # The probe reaches openai-a, which still fails, so openai-b serves and openai-a is suspended again.
    When I send a POST request to "http://localhost:8080/mf-reprobe-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" should have received 3 requests
    When I send a POST request to "http://localhost:8080/mf-reprobe-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" should have received 3 requests
    And I delete the model-failover fixture "mf-reprobe"

  Scenario: When every target is suspended the gateway answers without contacting any
    When I deploy the model-failover fixture "mf-allsusp" with params:
      """
      {"targets": [{"provider": "mf-allsusp-openai-a", "model": "gpt-a"}, {"provider": "mf-allsusp-openai-b", "model": "gpt-b"}], "suspendAfterConsecutiveFailures": 1, "suspendDuration": "60s"}
      """
    And I wait for the endpoint "http://localhost:8080/mf-allsusp-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    And the mock LLM "openai-b" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-allsusp-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 503
    When I reset the mock LLMs
    And I send a POST request to "http://localhost:8080/mf-allsusp-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
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
      {"targets": [{"provider": "mf-hang-openai-a", "model": "gpt-a"}, {"provider": "mf-hang-openai-b", "model": "gpt-b"}], "perAttemptTimeout": "2s"}
      """
    And I wait for the endpoint "http://localhost:8080/mf-hang-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "hang:10"
    And I record the current time as "request_start"
    When I send a POST request to "http://localhost:8080/mf-hang-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 200
    And the response body should contain "hello from mock-llm-openai-b"
    And the request should have taken at least "2" seconds since "request_start"
    And the request should have taken less than "5" seconds since "request_start"
    And I delete the model-failover fixture "mf-hang"

  Scenario: No failover happens once the response has started streaming
    When I deploy the model-failover fixture "mf-midstream" with params:
      """
      {"targets": [{"provider": "mf-midstream-openai-a", "model": "gpt-a"}, {"provider": "mf-midstream-openai-b", "model": "gpt-b"}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-midstream-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "stream-abort:2"
    When I send a POST request that may end early to "http://localhost:8080/mf-midstream-proxy/chat/completions" with body:
      """
      {"model": "client", "stream": true, "messages": []}
      """
    Then the early-ended response status should be 200
    And the early-ended response body should contain "part0"
    And the mock LLM "openai-b" should have received 0 requests
    And I delete the model-failover fixture "mf-midstream"

  # ==================== US6: exhaustion ====================

  Scenario: When every target fails the client gets the fixed exhaustion error and each target is tried once
    When I deploy the model-failover fixture "mf-exh" with params:
      """
      {"targets": [{"provider": "mf-exh-openai-a", "model": "gpt-a"}, {"provider": "mf-exh-openai-b", "model": "gpt-b"}, {"provider": "mf-exh-anthropic", "model": "claude-sonnet-4-5"}]}
      """
    And I wait for the endpoint "http://localhost:8080/mf-exh-proxy/chat/completions" to be ready with method "POST" and body '{"model":"client","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:503"
    And the mock LLM "openai-b" is set to "status:429"
    And the mock LLM "anthropic" is set to "status:503"
    When I send a POST request to "http://localhost:8080/mf-exh-proxy/chat/completions" with body:
      """
      {"model": "client", "messages": []}
      """
    Then the response status code should be 503
    And the response body should be:
      """
      {"error":{"message":"All configured model targets are currently unavailable. Please retry later.","type":"model_failover_exhausted","param":null,"code":"all_targets_unavailable"}}
      """
    And the response header "x-wso2-failover-retry" should not exist
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-b" should have received 1 request
    And the mock LLM "anthropic" should have received 1 request
    And I delete the model-failover fixture "mf-exh"

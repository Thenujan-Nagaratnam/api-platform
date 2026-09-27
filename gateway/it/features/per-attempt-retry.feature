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

# The per-attempt retry hop (specs/004-per-attempt-retry-hop). The retrying
# policy here, retry-on-status, is known to the gateway only through its
# policy definition's retryBehavior block.

@per-attempt-retry
Feature: A policy retries through the gateway's per-attempt hop without gateway changes

  Background:
    Given the gateway services are running
    And I authenticate using basic auth as "admin"

  Scenario: A retrying policy gets a second attempt and the client sees its answer
    When I deploy an API with the following configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: par-retry
      spec:
        displayName: par-retry
        version: v1.0
        context: /par-retry/$version
        upstream:
          main:
            url: http://mock-llm-openai-a:8080
        operations:
          - method: POST
            path: /chat/completions
            policies:
              - name: retry-on-status
                version: v1
                params:
                  enabled: true
                  status: 418
                  attempts: 2
          - method: GET
            path: /health
      """
    Then the response should be successful
    And I wait for the endpoint "http://localhost:8080/par-retry/v1.0/chat/completions" to be ready with method "POST" and body '{"model":"m","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "seq:status:418,ok"
    When I send a POST request to "http://localhost:8080/par-retry/v1.0/chat/completions" with body:
      """
      {"model": "m", "messages": []}
      """
    Then the response status code should be 200
    And the response header "x-retry-attempt" should be "2"
    And the response header "x-wso2-attempt-retry" should not exist
    And the mock LLM "openai-a" should have received 2 requests
    And the mock LLM "openai-a" last request header "x-retry-attempt" should be "2"
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-scope"
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-hop"
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-chain"
    Given I authenticate using basic auth as "admin"
    When I delete the API "par-retry"
    Then the response should be successful

  Scenario: A retrying policy with retrying switched off leaves the operation unsplit
    When I deploy an API with the following configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: par-off
      spec:
        displayName: par-off
        version: v1.0
        context: /par-off/$version
        upstream:
          main:
            url: http://mock-llm-openai-a:8080
        operations:
          - method: POST
            path: /chat/completions
            policies:
              - name: retry-on-status
                version: v1
                params:
                  enabled: false
                  status: 418
                  attempts: 2
      """
    Then the response should be successful
    And I wait for the endpoint "http://localhost:8080/par-off/v1.0/chat/completions" to be ready with method "POST" and body '{"model":"m","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:418"
    When I send a POST request to "http://localhost:8080/par-off/v1.0/chat/completions" with body:
      """
      {"model": "m", "messages": []}
      """
    Then the response status code should be 418
    And the mock LLM "openai-a" should have received 1 request
    Given I authenticate using basic auth as "admin"
    When I delete the API "par-off"
    Then the response should be successful

  Scenario: A policy never gets more attempts than it declared
    When I deploy an API with the following configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: par-cap
      spec:
        displayName: par-cap
        version: v1.0
        context: /par-cap/$version
        upstream:
          main:
            url: http://mock-llm-openai-a:8080
        operations:
          - method: POST
            path: /chat/completions
            policies:
              - name: retry-on-status
                version: v1
                params:
                  enabled: true
                  status: 418
                  attempts: 2
      """
    Then the response should be successful
    And I wait for the endpoint "http://localhost:8080/par-cap/v1.0/chat/completions" to be ready with method "POST" and body '{"model":"m","messages":[]}'
    And I reset the mock LLMs
    And the mock LLM "openai-a" is set to "status:418"
    When I send a POST request to "http://localhost:8080/par-cap/v1.0/chat/completions" with body:
      """
      {"model": "m", "messages": []}
      """
    Then the response status code should be 418
    And the response header "x-retry-attempt" should be "2"
    And the mock LLM "openai-a" should have received 2 requests
    Given I authenticate using basic auth as "admin"
    When I delete the API "par-cap"
    Then the response should be successful

  Scenario: Attempt headers a client sends are removed and change nothing
    When I deploy an API with the following configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: par-forge
      spec:
        displayName: par-forge
        version: v1.0
        context: /par-forge/$version
        upstream:
          main:
            url: http://mock-llm-openai-a:8080
        operations:
          - method: POST
            path: /chat/completions
            policies:
              - name: retry-on-status
                version: v1
                params:
                  enabled: true
                  status: 418
                  attempts: 2
      """
    Then the response should be successful
    And I wait for the endpoint "http://localhost:8080/par-forge/v1.0/chat/completions" to be ready with method "POST" and body '{"model":"m","messages":[]}'
    And I reset the mock LLMs
    When I send a POST request to "http://localhost:8080/par-forge/v1.0/chat/completions" with header "x-wso2-attempt-retry" value "forged" with body:
      """
      {"model": "m", "messages": []}
      """
    Then the response status code should be 200
    And the mock LLM "openai-a" should have received 1 request
    And the mock LLM "openai-a" last request should not have header "x-wso2-attempt-retry"
    Given I authenticate using basic auth as "admin"
    When I delete the API "par-forge"
    Then the response should be successful

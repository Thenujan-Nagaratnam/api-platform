# --------------------------------------------------------------------
# Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
#
# WSO2 LLC. licenses this file to you under the Apache License,
# Version 2.0 (the "License"); you may not use this file except
# in compliance with the License. You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied. See the License for the
# specific language governing permissions and limitations
# under the License.
# --------------------------------------------------------------------

# openai-error-format is an ordinary fault policy: an LLM API opts in by listing it in
# globalFaultPolicies (or operationFaultPolicies) and nothing else on the gateway changes.
#
# Gateway-produced errors are synthesised with the shipped `respond` policy on individual operations,
# so every status, body shape and header combination is exercised without a custom backend. The
# LLM providers' upstream is a respond-based RestApi on this same gateway (oef-mock-backend), which is
# also how backend errors of any shape are produced for the passthrough scenarios.
#
# Router-failure scenarios rely on the suite's [policy_engine.fault_policies]
# handle_upstream_faults = true (test-config.toml): only then do router and upstream failures reach ANY
# fault policy. With it off they keep the gateway's existing reply - the policy's documented limitation.
#
# Not covered end to end, and why:
#   - ordering against another body-writing fault policy: no other shipped policy implements OnFault
#     yet, so this is covered by the policy's unit tests;
#   - a failure after a streamed response has started: no shipped policy can raise one, and the policy
#     never touches a committed response (unit-tested).
#
# The suite's "should have field" step treats a JSON null as absent, so nulls are asserted by pattern.

@llm @fault-policies @openai-error-format
Feature: OpenAI error format fault policy for LLM APIs
  As an API author publishing an LLM API to OpenAI SDK clients
  I want to attach a fault policy that returns gateway errors in the OpenAI error envelope
  So that those clients can parse every error the gateway sends, without changing other APIs

  Background:
    Given the gateway services are running


  # ── US1: scope is by API kind, so every built-in template gets the envelope ──────────────

  Scenario Outline: A <template> provider with the policy returns guardrail rejections as OpenAI errors
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-mock-tpl-<template>-v1.0
      spec:
        displayName: oef-mock-tpl-<template>-v1.0
        version: v1.0
        context: /oef-mock-tpl-<template>/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /get
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /basic
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /apikey
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /ratelimit
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: POST
          path: /post
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /chat/completions
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /embeddings
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"object":"list","data":[]}'
        - method: GET
          path: /openai-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"The model `nope` does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
        - method: GET
          path: /legacy-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"backend legacy detail"}'
        - method: GET
          path: /plain-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 503
              headers:
              - name: Content-Type
                value: text/plain
              body: backend is down
        - method: GET
          path: /empty-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 500
        - method: GET
          path: /rate-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '7'
              body: '{"error":{"message":"backend rate limit","type":"rate_limit_error"}}'
      """
    Then the response should be successful
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-tpl-<template>
      spec:
        displayName: oef-tpl-<template>
        version: v1.0
        template: <template>
        context: /oef-tpl-<template>
        upstream:
          url: http://gateway-runtime:8080/oef-mock-tpl-<template>/v1.0
        accessControl:
          mode: allow_all
        globalPolicies:
        - name: word-count-guardrail
          version: v1
          params:
            request:
              min: 1
              max: 5
              jsonPath: ''
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-mock-tpl-<template>/v1.0/get" to be ready
    And I wait for the endpoint "http://localhost:8080/oef-tpl-<template>/post" to be ready with method "POST" and body 'hello'

    When I send a POST request to "http://localhost:8080/oef-tpl-<template>/post" with body:
      """
      this request body has far more than five words in it
      """
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Violation of applied word count constraints detected"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail.interveningGuardrail" should be "word-count-guardrail"
    And the JSON response field "error.guardrail.action" should be "GUARDRAIL_INTERVENED"
    And the JSON response field "type" should not exist
    And the JSON response field "message" should not exist

    When I send a POST request to "http://localhost:8080/oef-tpl-<template>/post" with body:
      """
      hello there
      """
    Then the response status code should be 200
    And the response body should not contain "invalid_request_error"

    When I delete the LLM provider "oef-tpl-<template>"
    Then the response status code should be 200
    When I delete the API "oef-mock-tpl-<template>-v1.0"
    Then the response should be successful

    Examples:
      | template        |
      | openai          |
      | azure-openai    |
      | azureai-foundry |
      | anthropic       |
      | gemini          |
      | mistralai       |
      | awsbedrock      |

  # respond returns each status with an empty body; type is derived from the status and the message is the status text. Statuses below 400 are not failures, so the fault chain never runs.
  Scenario: US1: every error status gets OpenAI's error type, and non-errors are untouched
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-mock-status-v1.0
      spec:
        displayName: oef-mock-status-v1.0
        version: v1.0
        context: /oef-mock-status/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /get
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /basic
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /apikey
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /ratelimit
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: POST
          path: /post
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /chat/completions
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /embeddings
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"object":"list","data":[]}'
        - method: GET
          path: /openai-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"The model `nope` does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
        - method: GET
          path: /legacy-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"backend legacy detail"}'
        - method: GET
          path: /plain-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 503
              headers:
              - name: Content-Type
                value: text/plain
              body: backend is down
        - method: GET
          path: /empty-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 500
        - method: GET
          path: /rate-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '7'
              body: '{"error":{"message":"backend rate limit","type":"rate_limit_error"}}'
      """
    Then the response should be successful
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-status
      spec:
        displayName: oef-status
        version: v1.0
        template: openai
        context: /oef-status
        upstream:
          url: http://gateway-runtime:8080/oef-mock-status/v1.0
        accessControl:
          mode: allow_all
        operationPolicies:
        - name: respond
          version: v1
          paths:
          - path: /s400
            methods:
            - GET
            params:
              statusCode: 400
          - path: /s401
            methods:
            - GET
            params:
              statusCode: 401
          - path: /s403
            methods:
            - GET
            params:
              statusCode: 403
          - path: /s404
            methods:
            - GET
            params:
              statusCode: 404
          - path: /s405
            methods:
            - GET
            params:
              statusCode: 405
          - path: /s409
            methods:
            - GET
            params:
              statusCode: 409
          - path: /s413
            methods:
            - GET
            params:
              statusCode: 413
          - path: /s418
            methods:
            - GET
            params:
              statusCode: 418
          - path: /s422
            methods:
            - GET
            params:
              statusCode: 422
          - path: /s429
            methods:
            - GET
            params:
              statusCode: 429
          - path: /s451
            methods:
            - GET
            params:
              statusCode: 451
          - path: /s499
            methods:
            - GET
            params:
              statusCode: 499
          - path: /s500
            methods:
            - GET
            params:
              statusCode: 500
          - path: /s501
            methods:
            - GET
            params:
              statusCode: 501
          - path: /s502
            methods:
            - GET
            params:
              statusCode: 502
          - path: /s503
            methods:
            - GET
            params:
              statusCode: 503
          - path: /s504
            methods:
            - GET
            params:
              statusCode: 504
          - path: /s599
            methods:
            - GET
            params:
              statusCode: 599
          - path: /s400post
            methods:
            - POST
            params:
              statusCode: 400
          - path: /s200
            methods:
            - GET
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: text/plain
              body: ok
          - path: /s200json
            methods:
            - GET
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":"fine"}'
          - path: /s204
            methods:
            - GET
            params:
              statusCode: 204
          - path: /s399
            methods:
            - GET
            params:
              statusCode: 399
              headers:
              - name: Content-Type
                value: text/plain
              body: three-nine-nine
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-mock-status/v1.0/get" to be ready
    And I wait for the endpoint "http://localhost:8080/oef-status/get" to be ready

    # 400 becomes invalid_request_error
    When I send a GET request to "http://localhost:8080/oef-status/s400"
    Then the response status code should be 400
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Bad Request"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 401 becomes authentication_error
    When I send a GET request to "http://localhost:8080/oef-status/s401"
    Then the response status code should be 401
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "authentication_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Unauthorized"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 403 becomes permission_error
    When I send a GET request to "http://localhost:8080/oef-status/s403"
    Then the response status code should be 403
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "permission_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Forbidden"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 404 becomes not_found_error
    When I send a GET request to "http://localhost:8080/oef-status/s404"
    Then the response status code should be 404
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "not_found_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Not Found"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 405 becomes invalid_request_error
    When I send a GET request to "http://localhost:8080/oef-status/s405"
    Then the response status code should be 405
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Method Not Allowed"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 409 becomes invalid_request_error
    When I send a GET request to "http://localhost:8080/oef-status/s409"
    Then the response status code should be 409
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Conflict"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 413 becomes invalid_request_error
    When I send a GET request to "http://localhost:8080/oef-status/s413"
    Then the response status code should be 413
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Request Entity Too Large"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 418 becomes invalid_request_error
    When I send a GET request to "http://localhost:8080/oef-status/s418"
    Then the response status code should be 418
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "I'm a teapot"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 422 becomes invalid_request_error
    When I send a GET request to "http://localhost:8080/oef-status/s422"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Unprocessable Entity"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 429 becomes rate_limit_error
    When I send a GET request to "http://localhost:8080/oef-status/s429"
    Then the response status code should be 429
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "rate_limit_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Too Many Requests"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 451 becomes invalid_request_error
    When I send a GET request to "http://localhost:8080/oef-status/s451"
    Then the response status code should be 451
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Unavailable For Legal Reasons"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 499 becomes invalid_request_error
    When I send a GET request to "http://localhost:8080/oef-status/s499"
    Then the response status code should be 499
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Request failed"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 500 becomes server_error
    When I send a GET request to "http://localhost:8080/oef-status/s500"
    Then the response status code should be 500
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "server_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Internal Server Error"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 501 becomes server_error
    When I send a GET request to "http://localhost:8080/oef-status/s501"
    Then the response status code should be 501
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "server_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Not Implemented"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 502 becomes server_error
    When I send a GET request to "http://localhost:8080/oef-status/s502"
    Then the response status code should be 502
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "server_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Bad Gateway"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 503 becomes server_error
    When I send a GET request to "http://localhost:8080/oef-status/s503"
    Then the response status code should be 503
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "server_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Service Unavailable"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 504 becomes server_error
    When I send a GET request to "http://localhost:8080/oef-status/s504"
    Then the response status code should be 504
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "server_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Gateway Timeout"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # 599 becomes server_error
    When I send a GET request to "http://localhost:8080/oef-status/s599"
    Then the response status code should be 599
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "server_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Request failed"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # a POST rejection is formatted too
    When I send a POST request to "http://localhost:8080/oef-status/s400post" with body:
      """
      {}
      """
    Then the response status code should be 400
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Bad Request"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    # 200 text is untouched
    When I send a GET request to "http://localhost:8080/oef-status/s200"
    Then the response status code should be 200
    And the response body should contain "ok"

    # 200 JSON with a message field is untouched
    When I send a GET request to "http://localhost:8080/oef-status/s200json"
    Then the response status code should be 200

    # 204 is untouched
    When I send a GET request to "http://localhost:8080/oef-status/s204"
    Then the response status code should be 204

    # 399 is untouched
    When I send a GET request to "http://localhost:8080/oef-status/s399"
    Then the response status code should be 399
    And the response body should contain "three-nine-nine"

    # a proxied success is untouched
    When I send a GET request to "http://localhost:8080/oef-status/get"
    Then the response status code should be 200

    # a proxied POST success is untouched
    When I send a POST request to "http://localhost:8080/oef-status/post" with body:
      """
      hello
      """
    Then the response status code should be 200

    When I delete the LLM provider "oef-status"
    Then the response status code should be 200
    When I delete the API "oef-mock-status-v1.0"
    Then the response should be successful

  # Priority: the fault's message, then the body (error.message, message, message.actionReason, error_description, detail, error), then the status text.
  Scenario: US3: the rejecting policy's own message is recovered from every body shape
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-mock-message-v1.0
      spec:
        displayName: oef-mock-message-v1.0
        version: v1.0
        context: /oef-mock-message/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /get
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /basic
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /apikey
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /ratelimit
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: POST
          path: /post
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /chat/completions
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /embeddings
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"object":"list","data":[]}'
        - method: GET
          path: /openai-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"The model `nope` does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
        - method: GET
          path: /legacy-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"backend legacy detail"}'
        - method: GET
          path: /plain-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 503
              headers:
              - name: Content-Type
                value: text/plain
              body: backend is down
        - method: GET
          path: /empty-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 500
        - method: GET
          path: /rate-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '7'
              body: '{"error":{"message":"backend rate limit","type":"rate_limit_error"}}'
      """
    Then the response should be successful
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-msg
      spec:
        displayName: oef-msg
        version: v1.0
        template: openai
        context: /oef-msg
        upstream:
          url: http://gateway-runtime:8080/oef-mock-message/v1.0
        accessControl:
          mode: allow_all
        operationPolicies:
        - name: respond
          version: v1
          paths:
          - path: /m-nested
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"nested message"}}'
          - path: /m-message
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":"top-level message"}'
          - path: /m-guardrail
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"type":"WORD_COUNT_GUARDRAIL","message":{"action":"GUARDRAIL_INTERVENED","interveningGuardrail":"word-count-guardrail","direction":"REQUEST","actionReason":"Violation of applied word count constraints detected","assessments":"Expected between 1 and 5 words."}}'
          - path: /m-desc
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":"invalid_token","error_description":"token has expired"}'
          - path: /m-detail
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"detail message"}'
          - path: /m-errorstr
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":"plain error string"}'
          - path: /m-order1
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":"first","detail":"second"}'
          - path: /m-order2
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"nested wins"},"message":"top"}'
          - path: /m-nonstring
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":123,"detail":"after non-string"}'
          - path: /m-blank
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":"   ","detail":"after blank"}'
          - path: /m-trim
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":"  padded  "}'
          - path: /m-unicode
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json; charset=utf-8
              body: '{"message":"café ✓ 日本語"}'
          - path: /m-unknown
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"foo":"bar"}'
          - path: /m-array
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '["a","b"]'
          - path: /m-invalid
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":'
          - path: /m-plain
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: text/plain
              body: plain text reason
          - path: /m-plain-charset
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: text/plain; charset=utf-8
              body: charset reason
          - path: /m-multiline
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: text/plain
              body: 'line one

                line two'
          - path: /m-html
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: text/html
              body: <html><body>oops</body></html>
          - path: /m-long
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: text/plain
              body: xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
          - path: /m-escape
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":"quote \" backslash \\ tab\t end"}'
          - path: /m-empty
            methods:
            - GET
            params:
              statusCode: 500
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-mock-message/v1.0/get" to be ready
    And I wait for the endpoint "http://localhost:8080/oef-msg/get" to be ready

    # nested error.message
    When I send a GET request to "http://localhost:8080/oef-msg/m-nested"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "nested message"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # top-level message
    When I send a GET request to "http://localhost:8080/oef-msg/m-message"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "top-level message"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # a shipped guardrail's actionReason
    When I send a GET request to "http://localhost:8080/oef-msg/m-guardrail"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Violation of applied word count constraints detected"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    # error_description outranks an error string
    When I send a GET request to "http://localhost:8080/oef-msg/m-desc"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "token has expired"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # detail
    When I send a GET request to "http://localhost:8080/oef-msg/m-detail"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "detail message"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # error as a string
    When I send a GET request to "http://localhost:8080/oef-msg/m-errorstr"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "plain error string"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # message outranks detail
    When I send a GET request to "http://localhost:8080/oef-msg/m-order1"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "first"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # error.message outranks message
    When I send a GET request to "http://localhost:8080/oef-msg/m-order2"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "nested wins"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # a non-string message is skipped
    When I send a GET request to "http://localhost:8080/oef-msg/m-nonstring"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "after non-string"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # a blank message is skipped
    When I send a GET request to "http://localhost:8080/oef-msg/m-blank"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "after blank"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # the message is trimmed
    When I send a GET request to "http://localhost:8080/oef-msg/m-trim"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "padded"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # unicode survives
    When I send a GET request to "http://localhost:8080/oef-msg/m-unicode"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "café ✓ 日本語"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # JSON with no known field falls back to the status
    When I send a GET request to "http://localhost:8080/oef-msg/m-unknown"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Unprocessable Entity"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # a JSON array falls back to the status
    When I send a GET request to "http://localhost:8080/oef-msg/m-array"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Unprocessable Entity"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # invalid JSON falls back to the status
    When I send a GET request to "http://localhost:8080/oef-msg/m-invalid"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Unprocessable Entity"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # single-line plain text
    When I send a GET request to "http://localhost:8080/oef-msg/m-plain"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "plain text reason"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # text/plain with a charset
    When I send a GET request to "http://localhost:8080/oef-msg/m-plain-charset"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "charset reason"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # multi-line text is not a message
    When I send a GET request to "http://localhost:8080/oef-msg/m-multiline"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Unprocessable Entity"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # HTML is not a message
    When I send a GET request to "http://localhost:8080/oef-msg/m-html"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Unprocessable Entity"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # plain text over 1024 characters is not a message
    When I send a GET request to "http://localhost:8080/oef-msg/m-long"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Unprocessable Entity"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail" should not exist

    # guardrail detail is carried inside error
    When I send a GET request to "http://localhost:8080/oef-msg/m-guardrail"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Violation of applied word count constraints detected"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail.interveningGuardrail" should be "word-count-guardrail"
    And the JSON response field "error.guardrail.action" should be "GUARDRAIL_INTERVENED"
    And the JSON response field "error.guardrail.actionReason" should be "Violation of applied word count constraints detected"
    And the JSON response should have field "error.guardrail.assessments"
    And the JSON response field "type" should not exist
    And the JSON response field "message" should not exist

    # quotes, backslashes and tabs round-trip
    When I send a GET request to "http://localhost:8080/oef-msg/m-escape"
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the response body should contain "quote"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    # an empty body gets the status text
    When I send a GET request to "http://localhost:8080/oef-msg/m-empty"
    Then the response status code should be 500
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "server_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Internal Server Error"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    When I delete the LLM provider "oef-msg"
    Then the response status code should be 200
    When I delete the API "oef-mock-message-v1.0"
    Then the response should be successful

  # Only content-type is set (and content-encoding removed when present); WWW-Authenticate, Retry-After and custom headers survive. A body that is already the OpenAI envelope is left byte for byte.
  Scenario: US1/US5: headers are kept, encodings handled, and an existing envelope is never rewritten
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-mock-headers-v1.0
      spec:
        displayName: oef-mock-headers-v1.0
        version: v1.0
        context: /oef-mock-headers/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /get
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /basic
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /apikey
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /ratelimit
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: POST
          path: /post
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /chat/completions
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /embeddings
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"object":"list","data":[]}'
        - method: GET
          path: /openai-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"The model `nope` does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
        - method: GET
          path: /legacy-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"backend legacy detail"}'
        - method: GET
          path: /plain-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 503
              headers:
              - name: Content-Type
                value: text/plain
              body: backend is down
        - method: GET
          path: /empty-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 500
        - method: GET
          path: /rate-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '7'
              body: '{"error":{"message":"backend rate limit","type":"rate_limit_error"}}'
      """
    Then the response should be successful
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-hdr
      spec:
        displayName: oef-hdr
        version: v1.0
        template: openai
        context: /oef-hdr
        upstream:
          url: http://gateway-runtime:8080/oef-mock-headers/v1.0
        accessControl:
          mode: allow_all
        operationPolicies:
        - name: respond
          version: v1
          paths:
          - path: /h-www
            methods:
            - GET
            params:
              statusCode: 401
              headers:
              - name: Content-Type
                value: application/json
              - name: WWW-Authenticate
                value: Bearer realm="oef"
              body: '{"message":"need token"}'
          - path: /h-retry
            methods:
            - GET
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '30'
              - name: X-RateLimit-Remaining-Requests
                value: '0'
              body: '{"message":"slow down"}'
          - path: /h-custom
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              - name: X-Custom-Trace
                value: abc123
              body: '{"message":"custom header"}'
          - path: /h-ct
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: text/plain
              body: content type test
          - path: /h-gzip
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              - name: Content-Encoding
                value: gzip
              body: not-really-gzip
          - path: /h-identity
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              - name: Content-Encoding
                value: identity
              body: '{"message":"identity ok"}'
          - path: /i-envelope
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"already formatted","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
          - path: /i-envelope-extra
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"has a sibling","type":"t"},"extra":true}'
          - path: /i-envelope-notype
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"no type here"}}'
          - path: /i-envelope-numeric
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":1,"type":"t"}}'
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-mock-headers/v1.0/get" to be ready
    And I wait for the endpoint "http://localhost:8080/oef-hdr/get" to be ready

    # WWW-Authenticate survives a 401
    When I send a GET request to "http://localhost:8080/oef-hdr/h-www"
    Then the response status code should be 401
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "authentication_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "need token"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the response header "WWW-Authenticate" should contain "Bearer realm="

    # Retry-After and rate-limit headers survive a 429
    When I send a GET request to "http://localhost:8080/oef-hdr/h-retry"
    Then the response status code should be 429
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "rate_limit_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "slow down"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the response header "Retry-After" should contain "30"
    And the response header "X-RateLimit-Remaining-Requests" should contain "0"

    # custom headers survive
    When I send a GET request to "http://localhost:8080/oef-hdr/h-custom"
    Then the response status code should be 400
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "custom header"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the response header "X-Custom-Trace" should contain "abc123"

    # text/plain becomes application/json
    When I send a GET request to "http://localhost:8080/oef-hdr/h-ct"
    Then the response status code should be 400
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "content type test"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    # an encoded body is not read and its encoding is dropped
    When I send a GET request to "http://localhost:8080/oef-hdr/h-gzip"
    Then the response status code should be 400
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Bad Request"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the response header "Content-Encoding" should not exist

    # identity encoding is read like plain JSON
    When I send a GET request to "http://localhost:8080/oef-hdr/h-identity"
    Then the response status code should be 400
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "identity ok"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    # an existing OpenAI envelope is left as it is
    When I send a GET request to "http://localhost:8080/oef-hdr/i-envelope"
    Then the response status code should be 400
    And the JSON response field "error.param" should be "model"
    And the JSON response field "error.code" should be "model_not_found"

    # an envelope with a top-level sibling is reshaped
    When I send a GET request to "http://localhost:8080/oef-hdr/i-envelope-extra"
    Then the response status code should be 400
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "has a sibling"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "extra" should not exist

    # an error object without a type is reshaped
    When I send a GET request to "http://localhost:8080/oef-hdr/i-envelope-notype"
    Then the response status code should be 400
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "no type here"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    # an error object with a non-string message is reshaped
    When I send a GET request to "http://localhost:8080/oef-hdr/i-envelope-numeric"
    Then the response status code should be 400
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Bad Request"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    When I delete the LLM provider "oef-hdr"
    Then the response status code should be 200
    When I delete the API "oef-mock-headers-v1.0"
    Then the response should be successful

  # A body larger than maxInspectBytes falls back to the status text; 0 disables recovery entirely; an existing envelope is kept in both cases.
  Scenario: US3/US5: maxInspectBytes bounds message recovery but never the existing-envelope check
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-mock-cap-v1.0
      spec:
        displayName: oef-mock-cap-v1.0
        version: v1.0
        context: /oef-mock-cap/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /get
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /basic
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /apikey
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /ratelimit
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: POST
          path: /post
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /chat/completions
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /embeddings
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"object":"list","data":[]}'
        - method: GET
          path: /openai-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"The model `nope` does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
        - method: GET
          path: /legacy-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"backend legacy detail"}'
        - method: GET
          path: /plain-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 503
              headers:
              - name: Content-Type
                value: text/plain
              body: backend is down
        - method: GET
          path: /empty-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 500
        - method: GET
          path: /rate-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '7'
              body: '{"error":{"message":"backend rate limit","type":"rate_limit_error"}}'
      """
    Then the response should be successful
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-cap16
      spec:
        displayName: oef-cap16
        version: v1.0
        template: openai
        context: /oef-cap16
        upstream:
          url: http://gateway-runtime:8080/oef-mock-cap/v1.0
        accessControl:
          mode: allow_all
        operationPolicies:
        - name: respond
          version: v1
          paths:
          - path: /cap
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":"this message is longer than sixteen bytes"}'
          - path: /cap-small
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":"tiny"}'
          - path: /cap-envelope
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"keep me","type":"invalid_request_error"}}'
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
          params:
            fault:
              maxInspectBytes: 20
      """
    Then the response status code should be 201
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-cap0
      spec:
        displayName: oef-cap0
        version: v1.0
        template: openai
        context: /oef-cap0
        upstream:
          url: http://gateway-runtime:8080/oef-mock-cap/v1.0
        accessControl:
          mode: allow_all
        operationPolicies:
        - name: respond
          version: v1
          paths:
          - path: /cap
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":"this message is longer than sixteen bytes"}'
          - path: /cap-small
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":"tiny"}'
          - path: /cap-envelope
            methods:
            - GET
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"keep me","type":"invalid_request_error"}}'
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
          params:
            fault:
              maxInspectBytes: 0
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-mock-cap/v1.0/get" to be ready
    And I wait for the endpoint "http://localhost:8080/oef-cap16/get" to be ready
    And I wait for the endpoint "http://localhost:8080/oef-cap0/get" to be ready

    # a body over the limit falls back to the status text
    When I send a GET request to "http://localhost:8080/oef-cap16/cap"
    Then the response status code should be 400
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Bad Request"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    # a body within the limit is read
    When I send a GET request to "http://localhost:8080/oef-cap16/cap-small"
    Then the response status code should be 400
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "tiny"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    # an envelope over the limit is still kept
    When I send a GET request to "http://localhost:8080/oef-cap16/cap-envelope"
    Then the response status code should be 400
    And the JSON response field "error.message" should be "keep me"
    And the JSON response field "error.param" should not exist

    # 0 disables message recovery
    When I send a GET request to "http://localhost:8080/oef-cap0/cap-small"
    Then the response status code should be 400
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Bad Request"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    # 0 still keeps an existing envelope
    When I send a GET request to "http://localhost:8080/oef-cap0/cap-envelope"
    Then the response status code should be 400
    And the JSON response field "error.message" should be "keep me"
    And the JSON response field "error.param" should not exist

    When I delete the LLM provider "oef-cap0"
    Then the response status code should be 200
    When I delete the LLM provider "oef-cap16"
    Then the response status code should be 200
    When I delete the API "oef-mock-cap-v1.0"
    Then the response should be successful

  # Byte-identical control for the same rejections.
  Scenario: US2: a provider without the policy keeps every error body it returned before
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-mock-control-v1.0
      spec:
        displayName: oef-mock-control-v1.0
        version: v1.0
        context: /oef-mock-control/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /get
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /basic
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /apikey
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /ratelimit
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: POST
          path: /post
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /chat/completions
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /embeddings
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"object":"list","data":[]}'
        - method: GET
          path: /openai-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"The model `nope` does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
        - method: GET
          path: /legacy-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"backend legacy detail"}'
        - method: GET
          path: /plain-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 503
              headers:
              - name: Content-Type
                value: text/plain
              body: backend is down
        - method: GET
          path: /empty-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 500
        - method: GET
          path: /rate-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '7'
              body: '{"error":{"message":"backend rate limit","type":"rate_limit_error"}}'
      """
    Then the response should be successful
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-control
      spec:
        displayName: oef-control
        version: v1.0
        template: openai
        context: /oef-control
        upstream:
          url: http://gateway-runtime:8080/oef-mock-control/v1.0
        accessControl:
          mode: allow_all
        globalPolicies:
        - name: word-count-guardrail
          version: v1
          params:
            request:
              min: 1
              max: 5
              jsonPath: ''
        operationPolicies:
        - name: respond
          version: v1
          paths:
          - path: /m-message
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":"top-level message"}'
          - path: /m-plain
            methods:
            - GET
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: text/plain
              body: plain text reason
          - path: /s404
            methods:
            - GET
            params:
              statusCode: 404
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-mock-control/v1.0/get" to be ready
    And I wait for the endpoint "http://localhost:8080/oef-control/post" to be ready with method "POST" and body 'hello'

    # guardrail body is the guardrail's own
    When I send a POST request to "http://localhost:8080/oef-control/post" with body:
      """
      this request body has far more than five words in it
      """
    Then the response status code should be 422
    And the JSON response field "type" should be "WORD_COUNT_GUARDRAIL"
    And the JSON response field "error.type" should not exist

    # a JSON rejection is untouched
    When I send a GET request to "http://localhost:8080/oef-control/m-message"
    Then the response status code should be 422

    # a text rejection is untouched
    When I send a GET request to "http://localhost:8080/oef-control/m-plain"
    Then the response status code should be 422
    And the response body should contain "plain text reason"
    And the response header "Content-Type" should contain "text/plain"

    # an empty 404 stays empty
    When I send a GET request to "http://localhost:8080/oef-control/s404"
    Then the response status code should be 404

    When I delete the LLM provider "oef-control"
    Then the response status code should be 200
    When I delete the API "oef-mock-control-v1.0"
    Then the response should be successful

  # Authentication, rate limiting, request guardrails and a RESPONSE-phase guardrail.
  Scenario: US1/US3: rejections from real shipped policies become OpenAI errors
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-mock-real-v1.0
      spec:
        displayName: oef-mock-real-v1.0
        version: v1.0
        context: /oef-mock-real/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /get
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /basic
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /apikey
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /ratelimit
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: POST
          path: /post
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /chat/completions
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /embeddings
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"object":"list","data":[]}'
        - method: GET
          path: /openai-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"The model `nope` does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
        - method: GET
          path: /legacy-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"backend legacy detail"}'
        - method: GET
          path: /plain-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 503
              headers:
              - name: Content-Type
                value: text/plain
              body: backend is down
        - method: GET
          path: /empty-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 500
        - method: GET
          path: /rate-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '7'
              body: '{"error":{"message":"backend rate limit","type":"rate_limit_error"}}'
      """
    Then the response should be successful
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-real
      spec:
        displayName: oef-real
        version: v1.0
        template: openai
        context: /oef-real
        upstream:
          url: http://gateway-runtime:8080/oef-mock-real/v1.0
        accessControl:
          mode: allow_all
        operationPolicies:
        - name: basic-auth
          version: v1
          paths:
          - path: /basic
            methods:
            - GET
            params:
              username: oef
              password: oef-pass
              realm: oef-realm
        - name: api-key-auth
          version: v1
          paths:
          - path: /apikey
            methods:
            - GET
            params:
              key: x-oef-key
              in: header
        - name: basic-ratelimit
          version: v1
          paths:
          - path: /ratelimit
            methods:
            - GET
            params:
              limits:
              - requests: 1
                duration: 1h
        - name: regex-guardrail
          version: v1
          paths:
          - path: /regex
            methods:
            - POST
            params:
              request:
                regex: ^[a-z ]+$
                jsonPath: $.messages[-1].content
                showAssessment: true
        - name: json-schema-guardrail
          version: v1
          paths:
          - path: /schema
            methods:
            - POST
            params:
              request:
                enabled: true
                jsonPath: ''
                schema: '{"type":"object","required":["model"]}'
        - name: content-length-guardrail
          version: v1
          paths:
          - path: /length
            methods:
            - POST
            params:
              request:
                min: 1
                max: 10
                jsonPath: ''
        - name: word-count-guardrail
          version: v1
          paths:
          - path: /post
            methods:
            - POST
            params:
              response:
                enabled: true
                min: 1
                max: 5
                jsonPath: ''
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-mock-real/v1.0/get" to be ready
    And I wait for the endpoint "http://localhost:8080/oef-real/get" to be ready

    # basic-auth without credentials
    When I send a GET request to "http://localhost:8080/oef-real/basic"
    Then the response status code should be 401
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "authentication_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Authentication required"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the response header "WWW-Authenticate" should contain "Basic realm="

    # basic-auth with wrong credentials
    When I send a GET request to "http://localhost:8080/oef-real/basic" with header "Authorization" value "Basic d3Jvbmc6d3Jvbmc="
    Then the response status code should be 401
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "authentication_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Authentication required"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    # basic-auth with the right credentials passes
    When I send a GET request to "http://localhost:8080/oef-real/basic" with header "Authorization" value "Basic b2VmOm9lZi1wYXNz"
    Then the response status code should be 200

    # api-key-auth without a key
    When I send a GET request to "http://localhost:8080/oef-real/apikey"
    Then the response status code should be 401
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "authentication_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the response body should match pattern "\Wcode\W\s*:\s*null"

    # basic-ratelimit: the first request passes
    When I send a GET request to "http://localhost:8080/oef-real/ratelimit"
    Then the response status code should be 200

    # basic-ratelimit: the second is a rate_limit_error
    When I send a GET request to "http://localhost:8080/oef-real/ratelimit"
    Then the response status code should be 429
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "rate_limit_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Rate limit exceeded. Please try again later."
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the response header "Retry-After" should exist

    # regex-guardrail with its assessment shown
    When I send a POST request to "http://localhost:8080/oef-real/regex" with body:
      """
      {"messages":[{"role":"user","content":"Has Digits 123"}]}
      """
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail.interveningGuardrail" should be "regex-guardrail"
    And the JSON response field "error.guardrail.action" should be "GUARDRAIL_INTERVENED"
    And the JSON response should have field "error.guardrail.assessments"

    # json-schema-guardrail
    When I send a POST request to "http://localhost:8080/oef-real/schema" with body:
      """
      {"messages":[]}
      """
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail.interveningGuardrail" should be "json-schema-guardrail"

    # content-length-guardrail
    When I send a POST request to "http://localhost:8080/oef-real/length" with body:
      """
      {"messages":[{"content":"too long"}]}
      """
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail.interveningGuardrail" should be "content-length-guardrail"

    # a RESPONSE-phase guardrail rejection is formatted too
    When I send a POST request to "http://localhost:8080/oef-real/post" with body:
      """
      hello
      """
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Violation of applied word count constraints detected"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail.interveningGuardrail" should be "word-count-guardrail"

    When I delete the LLM provider "oef-real"
    Then the response status code should be 200
    When I delete the API "oef-mock-real-v1.0"
    Then the response should be successful

  # Needs handle_upstream_faults = true (on in the IT config). With it off they keep the proxy's reply.
  Scenario: US4: router failures become OpenAI errors with the gateway's own codes
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-mock-router-v1.0
      spec:
        displayName: oef-mock-router-v1.0
        version: v1.0
        context: /oef-mock-router/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /get
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /basic
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /apikey
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /ratelimit
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: POST
          path: /post
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /chat/completions
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /embeddings
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"object":"list","data":[]}'
        - method: GET
          path: /openai-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"The model `nope` does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
        - method: GET
          path: /legacy-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"backend legacy detail"}'
        - method: GET
          path: /plain-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 503
              headers:
              - name: Content-Type
                value: text/plain
              body: backend is down
        - method: GET
          path: /empty-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 500
        - method: GET
          path: /rate-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '7'
              body: '{"error":{"message":"backend rate limit","type":"rate_limit_error"}}'
      """
    Then the response should be successful
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-refused
      spec:
        displayName: oef-refused
        version: v1.0
        template: openai
        context: /oef-refused
        upstream:
          url: http://127.0.0.1:39999
        accessControl:
          mode: allow_all
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-nodns
      spec:
        displayName: oef-nodns
        version: v1.0
        template: openai
        context: /oef-nodns
        upstream:
          url: http://oef-no-such-host.invalid:80
        accessControl:
          mode: allow_all
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-mock-router/v1.0/get" to be ready
    And I wait for policy snapshot sync
    And I wait for 3 seconds

    # connection refused
    When I send a GET request to "http://localhost:8080/oef-refused/get"
    Then the response status code should be 503
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "server_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "The upstream service could not be reached."
    And the JSON response field "error.code" should be "101503"

    # connection refused on POST
    When I send a POST request to "http://localhost:8080/oef-refused/chat/completions" with body:
      """
      {"model":"m"}
      """
    Then the response status code should be 503
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "server_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "The upstream service could not be reached."
    And the JSON response field "error.code" should be "101503"

    # unresolvable upstream host
    When I send a GET request to "http://localhost:8080/oef-nodns/get"
    Then the response status code should be 503
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "server_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "The upstream service is unavailable."
    And the JSON response field "error.code" should be "303001"

    When I delete the LLM provider "oef-nodns"
    Then the response status code should be 200
    When I delete the LLM provider "oef-refused"
    Then the response status code should be 200
    When I delete the API "oef-mock-router-v1.0"
    Then the response should be successful

  # Status, body and headers of an upstream error are the provider's, whatever their shape.
  Scenario: US4: the backend provider's own errors pass through untouched
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-mock-backend-v1.0
      spec:
        displayName: oef-mock-backend-v1.0
        version: v1.0
        context: /oef-mock-backend/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /get
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /basic
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /apikey
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /ratelimit
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: POST
          path: /post
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /chat/completions
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /embeddings
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"object":"list","data":[]}'
        - method: GET
          path: /openai-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"The model `nope` does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
        - method: GET
          path: /legacy-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"backend legacy detail"}'
        - method: GET
          path: /plain-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 503
              headers:
              - name: Content-Type
                value: text/plain
              body: backend is down
        - method: GET
          path: /empty-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 500
        - method: GET
          path: /rate-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '7'
              body: '{"error":{"message":"backend rate limit","type":"rate_limit_error"}}'
      """
    Then the response should be successful
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-backend
      spec:
        displayName: oef-backend
        version: v1.0
        template: openai
        context: /oef-backend
        upstream:
          url: http://gateway-runtime:8080/oef-mock-backend/v1.0
        accessControl:
          mode: allow_all
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-mock-backend/v1.0/get" to be ready
    And I wait for the endpoint "http://localhost:8080/oef-backend/get" to be ready

    # an OpenAI-shaped backend error
    When I send a GET request to "http://localhost:8080/oef-backend/openai-err"
    Then the response status code should be 400
    And the JSON response field "error.param" should be "model"

    # a legacy JSON backend error is NOT reshaped
    When I send a GET request to "http://localhost:8080/oef-backend/legacy-err"
    Then the response status code should be 422
    And the JSON response field "error" should not exist

    # a plain-text backend error is NOT reshaped
    When I send a GET request to "http://localhost:8080/oef-backend/plain-err"
    Then the response status code should be 503
    And the response body should contain "backend is down"

    # an empty backend 500 stays empty
    When I send a GET request to "http://localhost:8080/oef-backend/empty-err"
    Then the response status code should be 500

    # a backend 429 keeps its body and Retry-After
    When I send a GET request to "http://localhost:8080/oef-backend/rate-err"
    Then the response status code should be 429
    And the JSON response field "error.message" should be "backend rate limit"
    And the response header "Retry-After" should contain "7"

    When I delete the LLM provider "oef-backend"
    Then the response status code should be 200
    When I delete the API "oef-mock-backend-v1.0"
    Then the response should be successful

  # A proxy reaches its provider over the gateway's loopback, so the provider's response is the proxy's BACKEND response.
  Scenario: US1/US4: LLM proxies — opt in at the proxy, or inherit the provider's envelope
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-mock-proxy-v1.0
      spec:
        displayName: oef-mock-proxy-v1.0
        version: v1.0
        context: /oef-mock-proxy/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /get
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /basic
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /apikey
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /ratelimit
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: POST
          path: /post
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /chat/completions
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /embeddings
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"object":"list","data":[]}'
        - method: GET
          path: /openai-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"The model `nope` does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
        - method: GET
          path: /legacy-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"backend legacy detail"}'
        - method: GET
          path: /plain-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 503
              headers:
              - name: Content-Type
                value: text/plain
              body: backend is down
        - method: GET
          path: /empty-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 500
        - method: GET
          path: /rate-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '7'
              body: '{"error":{"message":"backend rate limit","type":"rate_limit_error"}}'
      """
    Then the response should be successful
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-px-refused
      spec:
        displayName: oef-px-refused
        version: v1.0
        template: openai
        context: /oef-px-refused
        upstream:
          url: http://127.0.0.1:39999
        accessControl:
          mode: allow_all
      """
    Then the response status code should be 201
    When I deploy this LLM proxy configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProxy
      metadata:
        name: oef-px-backend
      spec:
        displayName: oef-px-backend
        version: v1.0
        context: /oef-px-backend
        provider:
          id: oef-px-refused
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-px-plain
      spec:
        displayName: oef-px-plain
        version: v1.0
        template: openai
        context: /oef-px-plain
        upstream:
          url: http://gateway-runtime:8080/oef-mock-proxy/v1.0
        accessControl:
          mode: allow_all
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    When I deploy this LLM proxy configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProxy
      metadata:
        name: oef-px-opt-in
      spec:
        displayName: oef-px-opt-in
        version: v1.0
        context: /oef-px-opt-in
        provider:
          id: oef-px-plain
        globalPolicies:
        - name: word-count-guardrail
          version: v1
          params:
            request:
              min: 1
              max: 5
              jsonPath: ''
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-px-fmt
      spec:
        displayName: oef-px-fmt
        version: v1.0
        template: openai
        context: /oef-px-fmt
        upstream:
          url: http://gateway-runtime:8080/oef-mock-proxy/v1.0
        accessControl:
          mode: allow_all
        globalPolicies:
        - name: word-count-guardrail
          version: v1
          params:
            request:
              min: 1
              max: 5
              jsonPath: ''
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    When I deploy this LLM proxy configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProxy
      metadata:
        name: oef-px-inherit
      spec:
        displayName: oef-px-inherit
        version: v1.0
        context: /oef-px-inherit
        provider:
          id: oef-px-fmt
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-mock-proxy/v1.0/get" to be ready
    And I wait for the endpoint "http://localhost:8080/oef-px-opt-in/post" to be ready with method "POST" and body 'hello'
    And I wait for the endpoint "http://localhost:8080/oef-px-inherit/post" to be ready with method "POST" and body 'hello'
    And I wait for policy snapshot sync
    And I wait for 3 seconds

    # a proxy with the policy formats its own rejection
    When I send a POST request to "http://localhost:8080/oef-px-opt-in/post" with body:
      """
      this request body has far more than five words in it
      """
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Violation of applied word count constraints detected"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail.interveningGuardrail" should be "word-count-guardrail"
    And the JSON response field "error.guardrail.action" should be "GUARDRAIL_INTERVENED"

    # a proxy without the policy passes on its provider's envelope
    When I send a POST request to "http://localhost:8080/oef-px-inherit/post" with body:
      """
      this request body has far more than five words in it
      """
    Then the response status code should be 422
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response field "error.message" should be "Violation of applied word count constraints detected"

    # a provider's unformatted router failure is a backend error to the proxy
    When I send a POST request to "http://localhost:8080/oef-px-backend/chat/completions" with body:
      """
      {"model":"m"}
      """
    Then the response status code should be 503
    And the response body should not contain "server_error"
    And the response body should not contain "invalid_request_error"

    # a proxied success is untouched
    When I send a POST request to "http://localhost:8080/oef-px-opt-in/post" with body:
      """
      hi
      """
    Then the response status code should be 200

    When I send a DELETE request to the "gateway-controller" service at "/llm-proxies/oef-px-inherit"
    Then the response should be successful
    When I delete the LLM provider "oef-px-fmt"
    Then the response status code should be 200
    When I send a DELETE request to the "gateway-controller" service at "/llm-proxies/oef-px-opt-in"
    Then the response should be successful
    When I delete the LLM provider "oef-px-plain"
    Then the response status code should be 200
    When I send a DELETE request to the "gateway-controller" service at "/llm-proxies/oef-px-backend"
    Then the response should be successful
    When I delete the LLM provider "oef-px-refused"
    Then the response status code should be 200
    When I delete the API "oef-mock-proxy-v1.0"
    Then the response should be successful

  # RestApi and Mcp keep their own error bodies.
  Scenario: US2: attached to other API kinds the policy changes nothing
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-rest-v1.0
      spec:
        displayName: oef-rest-v1.0
        version: v1.0
        context: /oef-rest/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /ok
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: text/plain
              body: rest ok
        - method: GET
          path: /reject
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"message":"rest reject"}'
        faultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response should be successful
    When I deploy this MCP configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: Mcp
      metadata:
        name: oef-mcp
      spec:
        displayName: oef-mcp
        version: v1.0
        context: /oef-mcp
        specVersion: '2025-06-18'
        upstream:
          url: http://mcp-server-backend:3001/mcp
        policies:
        - name: respond
          version: v1
          params:
            statusCode: 401
            body: '{"message":"mcp reject"}'
            headers:
            - name: Content-Type
              value: application/json
        tools: []
        resources: []
        prompts: []
        faultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response should be successful
    And I wait for policy snapshot sync
    And I wait for 2 seconds
    And I wait for the endpoint "http://localhost:8080/oef-rest/v1.0/ok" to be ready

    # a RestApi rejection is untouched
    When I send a GET request to "http://localhost:8080/oef-rest/v1.0/reject"
    Then the response status code should be 422

    # an Mcp rejection is untouched
    When I send a POST request to "http://localhost:8080/oef-mcp/mcp" with body:
      """
      {"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}
      """
    Then the response status code should be 401
    And the JSON response field "message" should be "mcp reject"
    And the JSON response field "error.type" should not exist

    When I delete the MCP proxy "oef-mcp"
    Then the response should be successful
    When I delete the API "oef-rest-v1.0"
    Then the response should be successful

  # Only operations that carry the policy are formatted; attached twice it formats once; listed under globalPolicies (not a fault list) it never runs.
  Scenario: US2/US5: operation-level attachment, double attachment, and the wrong list
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-mock-attach-v1.0
      spec:
        displayName: oef-mock-attach-v1.0
        version: v1.0
        context: /oef-mock-attach/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /get
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /basic
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /apikey
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /ratelimit
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: POST
          path: /post
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /chat/completions
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /embeddings
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"object":"list","data":[]}'
        - method: GET
          path: /openai-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"The model `nope` does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
        - method: GET
          path: /legacy-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"backend legacy detail"}'
        - method: GET
          path: /plain-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 503
              headers:
              - name: Content-Type
                value: text/plain
              body: backend is down
        - method: GET
          path: /empty-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 500
        - method: GET
          path: /rate-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '7'
              body: '{"error":{"message":"backend rate limit","type":"rate_limit_error"}}'
      """
    Then the response should be successful
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-oplevel
      spec:
        displayName: oef-oplevel
        version: v1.0
        template: openai
        context: /oef-oplevel
        upstream:
          url: http://gateway-runtime:8080/oef-mock-attach/v1.0
        accessControl:
          mode: allow_all
        globalPolicies:
        - name: word-count-guardrail
          version: v1
          params:
            request:
              min: 1
              max: 5
              jsonPath: ''
        operationFaultPolicies:
        - name: openai-error-format
          version: v0
          paths:
          - path: /chat/completions
            methods:
            - POST
            params: {}
      """
    Then the response status code should be 201
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-twice
      spec:
        displayName: oef-twice
        version: v1.0
        template: openai
        context: /oef-twice
        upstream:
          url: http://gateway-runtime:8080/oef-mock-attach/v1.0
        accessControl:
          mode: allow_all
        globalPolicies:
        - name: word-count-guardrail
          version: v1
          params:
            request:
              min: 1
              max: 5
              jsonPath: ''
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
        operationFaultPolicies:
        - name: openai-error-format
          version: v0
          paths:
          - path: /chat/completions
            methods:
            - POST
            params: {}
      """
    Then the response status code should be 201
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-wronglist
      spec:
        displayName: oef-wronglist
        version: v1.0
        template: openai
        context: /oef-wronglist
        upstream:
          url: http://gateway-runtime:8080/oef-mock-attach/v1.0
        accessControl:
          mode: allow_all
        globalPolicies:
        - name: word-count-guardrail
          version: v1
          params:
            request:
              min: 1
              max: 5
              jsonPath: ''
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-mock-attach/v1.0/get" to be ready
    And I wait for the endpoint "http://localhost:8080/oef-oplevel/chat/completions" to be ready with method "POST" and body 'hello'
    And I wait for the endpoint "http://localhost:8080/oef-twice/chat/completions" to be ready with method "POST" and body 'hello'
    And I wait for the endpoint "http://localhost:8080/oef-wronglist/post" to be ready with method "POST" and body 'hello'

    # the operation with the policy is formatted
    When I send a POST request to "http://localhost:8080/oef-oplevel/chat/completions" with body:
      """
      this request body has far more than five words in it
      """
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Violation of applied word count constraints detected"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail.interveningGuardrail" should be "word-count-guardrail"
    And the JSON response field "error.guardrail.action" should be "GUARDRAIL_INTERVENED"

    # another operation is not
    When I send a POST request to "http://localhost:8080/oef-oplevel/embeddings" with body:
      """
      this request body has far more than five words in it
      """
    Then the response status code should be 422
    And the JSON response field "type" should be "WORD_COUNT_GUARDRAIL"
    And the JSON response field "error.type" should not exist

    # attached at API and operation level it formats once
    When I send a POST request to "http://localhost:8080/oef-twice/chat/completions" with body:
      """
      this request body has far more than five words in it
      """
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Violation of applied word count constraints detected"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail.interveningGuardrail" should be "word-count-guardrail"
    And the JSON response field "error.guardrail.action" should be "GUARDRAIL_INTERVENED"
    And the JSON response field "error.error" should not exist

    # listed under globalPolicies it does nothing
    When I send a POST request to "http://localhost:8080/oef-wronglist/post" with body:
      """
      this request body has far more than five words in it
      """
    Then the response status code should be 422
    And the JSON response field "type" should be "WORD_COUNT_GUARDRAIL"
    And the JSON response field "error.type" should not exist

    When I delete the LLM provider "oef-wronglist"
    Then the response status code should be 200
    When I delete the LLM provider "oef-twice"
    Then the response status code should be 200
    When I delete the LLM provider "oef-oplevel"
    Then the response status code should be 200
    When I delete the API "oef-mock-attach-v1.0"
    Then the response should be successful

  # No API means no fault policies.
  Scenario: Edge: a request that matches no API is untouched
    Given I authenticate using basic auth as "admin"

    # unmatched path
    When I send a GET request to "http://localhost:8080/oef-no-such-api/anything"
    Then the response status code should be 404
    And the JSON response field "error" should be "Not Found"
    And the response body should not contain "not_found_error"
    And the response body should not contain "invalid_request_error"
    And the response body should not contain "server_error"


  # A route timeout is a router failure with its own code. IT-only: it needs httpbin's /delay.
  Scenario: US4: an upstream timeout becomes an OpenAI error
    Given I authenticate using basic auth as "admin"
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-timeout
      spec:
        displayName: oef-timeout
        version: v1.0
        template: openai
        context: /oef-timeout
        upstream:
          url: http://echo-backend:80
        accessControl:
          mode: allow_all
        resilience:
          timeout: 2s
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-timeout/get" to be ready

    When I send a GET request to "http://localhost:8080/oef-timeout/delay/5"
    Then the response status code should be 504
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "server_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "The upstream service did not respond in time."
    And the JSON response field "error.code" should be "101504"

    When I send a GET request to "http://localhost:8080/oef-timeout/stream/2"
    Then the response status code should be 200
    And the response body should contain "url"

    When I send a GET request to "http://localhost:8080/oef-timeout/status/418"
    Then the response status code should be 418
    And the response body should not contain "invalid_request_error"

    When I send a GET request to "http://localhost:8080/oef-timeout/status/500"
    Then the response status code should be 500
    And the response body should not contain "server_error"

    When I delete the LLM provider "oef-timeout"
    Then the response status code should be 200

  # The opt-in is live configuration: removing the entry restores the legacy body, re-adding it
  # restores the envelope, with no restart.
  Scenario: US2: removing and re-adding the policy at runtime switches the error body
    Given I authenticate using basic auth as "admin"
    When I deploy this API configuration:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: RestApi
      metadata:
        name: oef-mock-toggle-v1.0
      spec:
        displayName: oef-mock-toggle-v1.0
        version: v1.0
        context: /oef-mock-toggle/$version
        upstream:
          main:
            url: http://gateway-runtime:8080/oef-unused
        operations:
        - method: GET
          path: /get
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /basic
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /apikey
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: GET
          path: /ratelimit
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"ok":true,"source":"oef-mock-backend"}'
        - method: POST
          path: /post
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /chat/completions
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"choices":[{"message":{"content":"one two three four five six seven eight"}}]}'
        - method: POST
          path: /embeddings
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 200
              headers:
              - name: Content-Type
                value: application/json
              body: '{"object":"list","data":[]}'
        - method: GET
          path: /openai-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 400
              headers:
              - name: Content-Type
                value: application/json
              body: '{"error":{"message":"The model `nope` does not exist","type":"invalid_request_error","param":"model","code":"model_not_found"}}'
        - method: GET
          path: /legacy-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 422
              headers:
              - name: Content-Type
                value: application/json
              body: '{"detail":"backend legacy detail"}'
        - method: GET
          path: /plain-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 503
              headers:
              - name: Content-Type
                value: text/plain
              body: backend is down
        - method: GET
          path: /empty-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 500
        - method: GET
          path: /rate-err
          policies:
          - name: respond
            version: v1
            params:
              statusCode: 429
              headers:
              - name: Content-Type
                value: application/json
              - name: Retry-After
                value: '7'
              body: '{"error":{"message":"backend rate limit","type":"rate_limit_error"}}'
      """
    Then the response should be successful
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-toggle
      spec:
        displayName: oef-toggle
        version: v1.0
        template: openai
        context: /oef-toggle
        upstream:
          url: http://gateway-runtime:8080/oef-mock-toggle/v1.0
        accessControl:
          mode: allow_all
        globalPolicies:
        - name: word-count-guardrail
          version: v1
          params:
            request:
              min: 1
              max: 5
              jsonPath: ''
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response status code should be 201
    And I wait for the endpoint "http://localhost:8080/oef-mock-toggle/v1.0/get" to be ready
    And I wait for the endpoint "http://localhost:8080/oef-toggle/post" to be ready with method "POST" and body 'hello'

    When I send a POST request to "http://localhost:8080/oef-toggle/post" with body:
      """
      this request body has far more than five words in it
      """
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Violation of applied word count constraints detected"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail.interveningGuardrail" should be "word-count-guardrail"
    And the JSON response field "error.guardrail.action" should be "GUARDRAIL_INTERVENED"

    When I update the LLM provider "oef-toggle" with:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-toggle
      spec:
        displayName: oef-toggle
        version: v1.0
        template: openai
        context: /oef-toggle
        upstream:
          url: http://gateway-runtime:8080/oef-mock-toggle/v1.0
        accessControl:
          mode: allow_all
        globalPolicies:
        - name: word-count-guardrail
          version: v1
          params:
            request:
              min: 1
              max: 5
              jsonPath: ''
      """
    Then the response should be successful
    And I wait for policy snapshot sync
    When I send a POST request to "http://localhost:8080/oef-toggle/post" with body:
      """
      this request body has far more than five words in it
      """
    Then the response status code should be 422
    And the JSON response field "type" should be "WORD_COUNT_GUARDRAIL"
    And the JSON response field "error.type" should not exist

    When I update the LLM provider "oef-toggle" with:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-toggle
      spec:
        displayName: oef-toggle
        version: v1.0
        template: openai
        context: /oef-toggle
        upstream:
          url: http://gateway-runtime:8080/oef-mock-toggle/v1.0
        accessControl:
          mode: allow_all
        globalPolicies:
        - name: word-count-guardrail
          version: v1
          params:
            request:
              min: 1
              max: 5
              jsonPath: ''
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
      """
    Then the response should be successful
    And I wait for policy snapshot sync
    When I send a POST request to "http://localhost:8080/oef-toggle/post" with body:
      """
      this request body has far more than five words in it
      """
    Then the response status code should be 422
    And the response header "Content-Type" should contain "application/json"
    And the response should be valid JSON
    And the JSON response field "error.type" should be "invalid_request_error"
    And the JSON response should have field "error.message"
    And the response body should match pattern "\Wparam\W\s*:\s*null"
    And the JSON response field "error.message" should be "Violation of applied word count constraints detected"
    And the response body should match pattern "\Wcode\W\s*:\s*null"
    And the JSON response field "error.guardrail.interveningGuardrail" should be "word-count-guardrail"
    And the JSON response field "error.guardrail.action" should be "GUARDRAIL_INTERVENED"

    When I delete the LLM provider "oef-toggle"
    Then the response status code should be 200
    When I delete the API "oef-mock-toggle-v1.0"
    Then the response should be successful

  # The definition schema is enforced at deployment.
  Scenario Outline: A provider whose openai-error-format entry has <case> is rejected
    Given I authenticate using basic auth as "admin"
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-badparams
      spec:
        displayName: oef-badparams
        version: v1.0
        template: openai
        context: /oef-badparams
        upstream:
          url: http://gateway-runtime:8080/oef-mock-__MOCK__/v1.0
        accessControl:
          mode: allow_all
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
          params: <params>
      """
    Then the response status code should be 400

    Examples:
      | case | params |
      | a negative maxInspectBytes | {"fault": {"maxInspectBytes": -1}} |
      | a string maxInspectBytes | {"fault": {"maxInspectBytes": "abc"}} |
      | a fractional maxInspectBytes | {"fault": {"maxInspectBytes": 1.5}} |
      | an unknown fault parameter | {"fault": {"foo": 1}} |
      | an unknown top-level parameter | {"bar": 1} |
      | a non-object fault block | {"fault": "x"} |

  Scenario: A provider with valid openai-error-format parameters is accepted
    Given I authenticate using basic auth as "admin"
    When I create this LLM provider:
      """
      apiVersion: gateway.api-platform.wso2.com/v1
      kind: LlmProvider
      metadata:
        name: oef-goodparams
      spec:
        displayName: oef-goodparams
        version: v1.0
        template: openai
        context: /oef-goodparams
        upstream:
          url: http://gateway-runtime:8080/oef-mock-__MOCK__/v1.0
        accessControl:
          mode: allow_all
        globalFaultPolicies:
        - name: openai-error-format
          version: v0
          params:
            fault:
              maxInspectBytes: 0
      """
    Then the response status code should be 201
    When I delete the LLM provider "oef-goodparams"
    Then the response status code should be 200

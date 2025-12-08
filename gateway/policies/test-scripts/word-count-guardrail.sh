#!/bin/bash

# Comprehensive test script for WordCountGuardrail policy

echo "Testing WordCountGuardrail Policy"
echo "=================================="

# Test Case 1: Basic configuration - POST operation
echo -e "\n=== Test Case 1: Basic Configuration (POST) ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: WordCountGuardrail-Test-API-1
  version: v1.0
  context: /word-count-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /message
      policies:
        - name: WordCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 5
              max: 500
            response:
              min: 5
              max: 500
EOF

sleep 3

echo -e "\nTest 1.1: Valid request (10 words - within 5-500 range)"
curl -X POST http://localhost:8080/word-count-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a test message with exactly ten words"}'

echo -e "\n\nTest 1.2: Invalid request (3 words - below minimum of 5)"
curl -X POST http://localhost:8080/word-count-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "Too few words"}'

echo -e "\n\nTest 1.3: Invalid request (over 500 words)"
long_message=""
for i in {1..600}; do
  long_message="${long_message}word "
done
curl -X POST http://localhost:8080/word-count-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d "{\"message\": \"$long_message\"}"

# Test Case 2: With JSONPath
echo -e "\n\n=== Test Case 2: With JSONPath Configuration ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: WordCountGuardrail-Test-API-2
  version: v1.0
  context: /word-count-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /jsonpath
      policies:
        - name: WordCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 3
              max: 500
              jsonPath: "$.message"
            response:
              min: 5
              max: 500
EOF

sleep 3

echo -e "\nTest 2.1: Valid request (JSONPath extracts valid field)"
curl -X POST http://localhost:8080/word-count-guardrail/v1.0/jsonpath \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a valid message with enough words", "other": "ignored content"}'

echo -e "\n\nTest 2.2: Invalid request (JSONPath field has too few words)"
curl -X POST http://localhost:8080/word-count-guardrail/v1.0/jsonpath \
  -H "Content-Type: application/json" \
  -d '{"message": "Too few", "other": "This field has many words but is ignored"}'

# Test Case 3: With invert=true
echo -e "\n\n=== Test Case 3: Inverted Configuration ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: WordCountGuardrail-Test-API-3
  version: v1.0
  context: /word-count-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /inverted
      policies:
        - name: WordCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 5
              max: 500
              invert: true
            response:
              min: 5
              max: 500
EOF

sleep 3

echo -e "\nTest 3.1: Request within range (should fail with invert=true)"
curl -X POST http://localhost:8080/word-count-guardrail/v1.0/inverted \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a valid message with ten words"}'

echo -e "\n\nTest 3.2: Request outside range (should pass with invert=true)"
curl -X POST http://localhost:8080/word-count-guardrail/v1.0/inverted \
  -H "Content-Type: application/json" \
  -d '{"message": "Too few"}'

# Test Case 4: Different HTTP methods
echo -e "\n\n=== Test Case 4: Different HTTP Methods ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: WordCountGuardrail-Test-API-4
  version: v1.0
  context: /word-count-guardrail-methods/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: PUT
      path: /update
      policies:
        - name: WordCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 5
              max: 500
            response:
              min: 5
              max: 500
    - method: PATCH
      path: /patch
      policies:
        - name: WordCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 3
              max: 500
            response:
              min: 3
              max: 500
EOF

sleep 3

echo -e "\nTest 4.1: PUT request (valid)"
curl -X PUT http://localhost:8080/word-count-guardrail-methods/v1.0/update \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a valid PUT request with enough words"}'

echo -e "\n\nTest 4.2: PATCH request (valid)"
curl -X PATCH http://localhost:8080/word-count-guardrail-methods/v1.0/patch \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a valid patch request with enough words"}'

# Test Case 5: With showAssessment
echo -e "\n\n=== Test Case 5: With showAssessment ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: WordCountGuardrail-Test-API-5
  version: v1.0
  context: /word-count-guardrail-assessment/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /assessment
      policies:
        - name: WordCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 5
              max: 500
              showAssessment: true
            response:
              min: 5
              max: 500
EOF

sleep 3

echo -e "\nTest 5.1: Request with showAssessment (should include assessment details)"
curl -X POST http://localhost:8080/word-count-guardrail-assessment/v1.0/assessment \
  -H "Content-Type: application/json" \
  -d '{"message": "This message has exactly ten words in total"}'

echo -e "\n\nTest 5.2: Invalid request with showAssessment"
curl -X POST http://localhost:8080/word-count-guardrail-assessment/v1.0/assessment \
  -H "Content-Type: application/json" \
  -d '{"message": "Too few"}'

echo -e "\n\n=== All Tests Completed ==="

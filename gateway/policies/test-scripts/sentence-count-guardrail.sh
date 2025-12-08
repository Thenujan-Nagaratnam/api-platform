#!/bin/bash

# Comprehensive test script for SentenceCountGuardrail policy

echo "Testing SentenceCountGuardrail Policy"
echo "======================================"

# Test Case 1: Basic configuration - POST operation
echo -e "\n=== Test Case 1: Basic Configuration (POST) ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: SentenceCountGuardrail-Test-API-1
  version: v1.0
  context: /sentence-count-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /message
      policies:
        - name: SentenceCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 1
              max: 10
            # No response validation - httpbin responses contain many sentences
EOF

sleep 2

echo -e "\nTest 1.1: Valid request (2 sentences - within 1-10 range)"
curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "This is the first sentence. This is the second sentence."}'

echo -e "\n\nTest 1.2: Invalid request (0 sentences - empty message)"
curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d ''

echo -e "\n\nTest 1.3: Invalid request (over 10 sentences)"
many_sentences=""
for i in {1..15}; do
  many_sentences="${many_sentences}This is sentence number $i. "
done
curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d "{\"message\": \"$many_sentences\"}"

echo -e "\n\nTest 1.4: Edge case - exactly at minimum (1 sentence)"
curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a single sentence."}'

# Test Case 2: With JSONPath
echo -e "\n\n=== Test Case 2: With JSONPath Configuration ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: SentenceCountGuardrail-Test-API-2
  version: v1.0
  context: /sentence-count-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /jsonpath
      policies:
        - name: SentenceCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 2
              max: 5
              jsonPath: "$.content"
            # No response validation - httpbin responses contain many sentences
EOF

sleep 2

echo -e "\nTest 2.1: Valid request (JSONPath extracts valid field)"
curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/jsonpath \
  -H "Content-Type: application/json" \
  -d '{"content": "First sentence. Second sentence. Third sentence.", "other": "This field has many sentences but is ignored."}'

echo -e "\n\nTest 2.2: Invalid request (JSONPath field has too few sentences)"
curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/jsonpath \
  -H "Content-Type: application/json" \
  -d '{"content": "Only one sentence.", "other": "This field has many sentences but is ignored."}'

# Test Case 3: With invert=true
echo -e "\n\n=== Test Case 3: Inverted Configuration ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: SentenceCountGuardrail-Test-API-3
  version: v1.0
  context: /sentence-count-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /inverted
      policies:
        - name: SentenceCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 2
              max: 5
              invert: true
            # No response validation - httpbin responses contain many sentences
EOF

sleep 2

echo -e "\nTest 3.1: Request within range (should fail with invert=true)"
curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/inverted \
  -H "Content-Type: application/json" \
  -d '{"message": "First sentence. Second sentence. Third sentence."}'

echo -e "\n\nTest 3.2: Request outside range (should pass with invert=true)"
curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/inverted \
  -H "Content-Type: application/json" \
  -d '{"message": ""}'

# Test Case 4: Different HTTP methods
echo -e "\n\n=== Test Case 4: Different HTTP Methods ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: SentenceCountGuardrail-Test-API-4
  version: v1.0
  context: /sentence-count-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: PUT
      path: /update
      policies:
        - name: SentenceCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 1
              max: 8
            # No response validation - httpbin responses contain many sentences
    - method: PATCH
      path: /patch
      policies:
        - name: SentenceCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 2
              max: 6
            # No response validation - httpbin responses contain many sentences
EOF

sleep 2

echo -e "\nTest 4.1: PUT request (valid)"
curl -X PUT http://localhost:8080/sentence-count-guardrail/v1.0/update \
  -H "Content-Type: application/json" \
  -d '{"message": "First sentence. Second sentence."}'

echo -e "\n\nTest 4.2: PATCH request (valid)"
curl -X PATCH http://localhost:8080/sentence-count-guardrail/v1.0/patch \
  -H "Content-Type: application/json" \
  -d '{"message": "First sentence. Second sentence. Third sentence."}'

# Test Case 5: With showAssessment
echo -e "\n\n=== Test Case 5: With showAssessment ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: SentenceCountGuardrail-Test-API-5
  version: v1.0
  context: /sentence-count-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /assessment
      policies:
        - name: SentenceCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 2
              max: 5
              showAssessment: true
              jsonPath: "$.message"
            # No response validation - httpbin responses contain many sentences
EOF

sleep 2

echo -e "\nTest 5.1: Request with showAssessment (should include assessment details)"
curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/assessment \
  -H "Content-Type: application/json" \
  -d '{"message": "First sentence. Second sentence. Third sentence."}'

echo -e "\n\nTest 5.2: Invalid request with showAssessment"
curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/assessment \
  -H "Content-Type: application/json" \
  -d '{"message": "Only one sentence."}'

echo -e "\n\n=== All Tests Completed ==="

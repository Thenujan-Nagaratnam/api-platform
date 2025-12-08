#!/bin/bash

# Comprehensive test script for ContentLengthGuardrail policy

echo "Testing ContentLengthGuardrail Policy"
echo "======================================"

# Test Case 1: Basic configuration - POST operation
echo -e "\n=== Test Case 1: Basic Configuration (POST) ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: ContentLengthGuardrail-Test-API-1
  version: v1.0
  context: /content-length-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /message
      policies:
        - name: ContentLengthGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 20
              max: 1500
            # No response validation - httpbin responses are too verbose
EOF

sleep 2

echo -e "\nTest 1.1: Valid request (20 bytes - within 20-1500 range)"
curl -X POST http://localhost:8080/content-length-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "This "}'

echo -e "\n\nTest 1.2: Invalid request (9 bytes - below minimum of 20)"
curl -X POST http://localhost:8080/content-length-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"x":"h"}'

echo -e "\n\nTest 1.3: Invalid request (over 1500 bytes - above maximum)"
long_message=""
for i in {1..350}; do
  long_message="${long_message}word "
done
curl -X POST http://localhost:8080/content-length-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d "{\"message\": \"$long_message\"}"

echo -e "\n\nTest 1.4: Edge case - exactly at minimum (20 bytes)"
curl -X POST http://localhost:8080/content-length-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "This "}'

# Test Case 2: With JSONPath
echo -e "\n\n=== Test Case 2: With JSONPath Configuration ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: ContentLengthGuardrail-Test-API-2
  version: v1.0
  context: /content-length-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /jsonpath
      policies:
        - name: ContentLengthGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 20
              max: 100
              jsonPath: "$.message"
            # No response validation - httpbin responses are too verbose
EOF

sleep 2

echo -e "\nTest 2.1: Valid request (JSONPath extracts valid field)"
curl -X POST http://localhost:8080/content-length-guardrail/v1.0/jsonpath \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a valid test message with enough bytes", "other": "ignored content that is very long"}'

echo -e "\n\nTest 2.2: Invalid request (JSONPath field too short)"
curl -X POST http://localhost:8080/content-length-guardrail/v1.0/jsonpath \
  -H "Content-Type: application/json" \
  -d '{"message": "Short", "other": "This field has many bytes but is ignored"}'

# Test Case 3: With invert=true
echo -e "\n\n=== Test Case 3: Inverted Configuration ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: ContentLengthGuardrail-Test-API-3
  version: v1.0
  context: /content-length-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /inverted
      policies:
        - name: ContentLengthGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 10
              max: 100
              invert: true
            # No response validation - httpbin responses are too verbose
EOF

sleep 2

echo -e "\nTest 3.1: Request within range (should fail with invert=true)"
curl -X POST http://localhost:8080/content-length-guardrail/v1.0/inverted \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a valid message with enough bytes"}'

echo -e "\n\nTest 3.2: Request outside range (should pass with invert=true)"
curl -X POST http://localhost:8080/content-length-guardrail/v1.0/inverted \
  -H "Content-Type: application/json" \
  -d '{"x":"h"}'

# Test Case 4: Different HTTP methods
echo -e "\n\n=== Test Case 4: Different HTTP Methods ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: ContentLengthGuardrail-Test-API-4
  version: v1.0
  context: /content-length-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: PUT
      path: /update
      policies:
        - name: ContentLengthGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 10
              max: 500
            # No response validation - httpbin responses are too verbose
    - method: PATCH
      path: /patch
      policies:
        - name: ContentLengthGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 15
              max: 200
            # No response validation - httpbin responses are too verbose
EOF

sleep 2

echo -e "\nTest 4.1: PUT request (valid)"
curl -X PUT http://localhost:8080/content-length-guardrail/v1.0/update \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a valid PUT request with enough bytes"}'

echo -e "\n\nTest 4.2: PATCH request (valid)"
curl -X PATCH http://localhost:8080/content-length-guardrail/v1.0/patch \
  -H "Content-Type: application/json" \
  -d '{"message": "Valid patch request with content"}'

# Test Case 5: With showAssessment
echo -e "\n\n=== Test Case 5: With showAssessment ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: ContentLengthGuardrail-Test-API-5
  version: v1.0
  context: /content-length-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /assessment
      policies:
        - name: ContentLengthGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              min: 10
              max: 100
              showAssessment: true
            # No response validation - httpbin responses are too verbose
EOF

sleep 2

echo -e "\nTest 5.1: Request with showAssessment (should include assessment details)"
curl -X POST http://localhost:8080/content-length-guardrail/v1.0/assessment \
  -H "Content-Type: application/json" \
  -d '{"message": "This message has enough bytes to pass validation"}'

echo -e "\n\nTest 5.2: Invalid request with showAssessment"
curl -X POST http://localhost:8080/content-length-guardrail/v1.0/assessment \
  -H "Content-Type: application/json" \
  -d '{"x":"h"}'

echo -e "\n\n=== All Tests Completed ==="

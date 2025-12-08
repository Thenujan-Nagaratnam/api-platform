#!/bin/bash

# Comprehensive test script for RegexGuardrail policy

echo "Testing RegexGuardrail Policy"
echo "============================="

# Test Case 1: Basic configuration - POST operation
echo -e "\n=== Test Case 1: Basic Configuration (POST) ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: RegexGuardrail-Test-API-1
  version: v1.0
  context: /regex-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /message
      policies:
        - name: RegexGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              regex: "(?i).*password.*"
              invert: true
            response:
              regex: "(?i).*password.*"
              invert: true
EOF

sleep 2

echo -e "\nTest 1.1: Valid request (no password mention)"
curl -X POST http://localhost:8080/regex-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a safe message without sensitive data"}'

echo -e "\n\nTest 1.2: Invalid request (contains password)"
curl -X POST http://localhost:8080/regex-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "My password is secret123"}'

echo -e "\n\nTest 1.3: Invalid request (contains PASSWORD in uppercase)"
curl -X POST http://localhost:8080/regex-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "The PASSWORD field is required"}'

# Test Case 2: With JSONPath
echo -e "\n\n=== Test Case 2: With JSONPath Configuration ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: RegexGuardrail-Test-API-2
  version: v1.0
  context: /regex-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /jsonpath
      policies:
        - name: RegexGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              regex: ".*@.*"
              jsonPath: "$.email"
            # No response validation - only validate request
EOF

sleep 2

echo -e "\nTest 2.1: Valid request (JSONPath extracts email field with @)"
curl -X POST http://localhost:8080/regex-guardrail/v1.0/jsonpath \
  -H "Content-Type: application/json" \
  -d '{"email": "user@example.com", "message": "password here but ignored"}'

echo -e "\n\nTest 2.2: Invalid request (JSONPath field doesn't match pattern)"
curl -X POST http://localhost:8080/regex-guardrail/v1.0/jsonpath \
  -H "Content-Type: application/json" \
  -d '{"email": "invalid-email", "message": "user@example.com in message but ignored"}'

# Test Case 3: With invert=true
echo -e "\n\n=== Test Case 3: Inverted Configuration ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: RegexGuardrail-Test-API-3
  version: v1.0
  context: /regex-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /inverted
      policies:
        - name: RegexGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              regex: "(?i).*admin.*"
              invert: true
            response:
              regex: "(?i).*admin.*"
              invert: true
EOF

sleep 2

echo -e "\nTest 3.1: Request matches pattern (should fail with invert=true)"
curl -X POST http://localhost:8080/regex-guardrail/v1.0/inverted \
  -H "Content-Type: application/json" \
  -d '{"message": "This is an admin message"}'

echo -e "\n\nTest 3.2: Request doesn't match pattern (should pass with invert=true)"
curl -X POST http://localhost:8080/regex-guardrail/v1.0/inverted \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a regular user message"}'

# Test Case 4: Different regex patterns
echo -e "\n\n=== Test Case 4: Different Regex Patterns ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: RegexGuardrail-Test-API-4
  version: v1.0
  context: /regex-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /email
      policies:
        - name: RegexGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              regex: "[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\\.[a-zA-Z]{2,}"
            # No response validation - only validate request email format
    - method: POST
      path: /phone
      policies:
        - name: RegexGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              regex: "\\+?[1-9]\\d{1,14}"
            # No response validation - only validate request phone format
EOF

sleep 2

echo -e "\nTest 4.1: Email pattern validation (valid email)"
curl -X POST http://localhost:8080/regex-guardrail/v1.0/email \
  -H "Content-Type: application/json" \
  -d '{"message": "Contact me at user@example.com"}'

echo -e "\n\nTest 4.2: Email pattern validation (invalid email)"
curl -X POST http://localhost:8080/regex-guardrail/v1.0/email \
  -H "Content-Type: application/json" \
  -d '{"message": "Contact me at invalid-email"}'

echo -e "\n\nTest 4.3: Phone pattern validation (valid phone)"
curl -X POST http://localhost:8080/regex-guardrail/v1.0/phone \
  -H "Content-Type: application/json" \
  -d '{"message": "Call me at +1234567890"}'

# Test Case 5: With showAssessment
echo -e "\n\n=== Test Case 5: With showAssessment ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: RegexGuardrail-Test-API-5
  version: v1.0
  context: /regex-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /assessment
      policies:
        - name: RegexGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              regex: "(?i).*credit.*card.*"
              invert: true
              showAssessment: true
            response:
              regex: "(?i).*credit.*card.*"
              invert: true
EOF

sleep 2

echo -e "\nTest 5.1: Request with showAssessment (should include assessment details)"
curl -X POST http://localhost:8080/regex-guardrail/v1.0/assessment \
  -H "Content-Type: application/json" \
  -d '{"message": "This message contains credit card information"}'

echo -e "\n\nTest 5.2: Valid request with showAssessment"
curl -X POST http://localhost:8080/regex-guardrail/v1.0/assessment \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a safe message without sensitive data"}'

# Test Case 6: Different HTTP methods
echo -e "\n\n=== Test Case 6: Different HTTP Methods ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: RegexGuardrail-Test-API-6
  version: v1.0
  context: /regex-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: PUT
      path: /update
      policies:
        - name: RegexGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              regex: "(?i).*secret.*"
              invert: true
            response:
              regex: "(?i).*secret.*"
              invert: true
    - method: PATCH
      path: /patch
      policies:
        - name: RegexGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              regex: "(?i).*token.*"
              invert: true
            response:
              regex: "(?i).*token.*"
              invert: true
EOF

sleep 2

echo -e "\nTest 6.1: PUT request (valid)"
curl -X PUT http://localhost:8080/regex-guardrail/v1.0/update \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a safe update message"}'

echo -e "\n\nTest 6.2: PATCH request (invalid - contains token)"
curl -X PATCH http://localhost:8080/regex-guardrail/v1.0/patch \
  -H "Content-Type: application/json" \
  -d '{"message": "This message contains an access token"}'

echo -e "\n\n=== All Tests Completed ==="

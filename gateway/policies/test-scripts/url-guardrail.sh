#!/bin/bash

# Comprehensive test script for URLGuardrail policy

echo "Testing URLGuardrail Policy"
echo "==========================="

# Test Case 1: Basic configuration - POST operation (HTTP HEAD validation)
echo -e "\n=== Test Case 1: Basic Configuration (POST) - HTTP HEAD Validation ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: URLGuardrail-Test-API-1
  version: v1.0
  context: /url-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /message
      policies:
        - name: URLGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              onlyDNS: false
              timeout: 3000
            # No response validation - httpbin responses contain URLs that might fail validation
EOF

sleep 2

echo -e "\nTest 1.1: Valid request (contains valid URL)"
curl --max-time 15 -X POST http://localhost:8080/url-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "Visit https://example.com for more info"}'

echo -e "\n\nTest 1.2: Invalid request (contains invalid URL)"
curl --max-time 15 -X POST http://localhost:8080/url-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "Visit https://invalid-domain-that-does-not-exist-12345.com"}'

# Test Case 2: With onlyDNS=true
echo -e "\n\n=== Test Case 2: DNS-Only Validation ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: URLGuardrail-Test-API-2
  version: v1.0
  context: /url-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /dns
      policies:
        - name: URLGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              onlyDNS: true
              timeout: 2000
            # No response validation - httpbin responses contain URLs that might fail validation
EOF

sleep 2

echo -e "\nTest 2.1: Valid request (valid DNS)"
curl --max-time 15 -X POST http://localhost:8080/url-guardrail/v1.0/dns \
  -H "Content-Type: application/json" \
  -d '{"message": "Check out https://google.com"}'

echo -e "\n\nTest 2.2: Invalid request (invalid DNS)"
curl --max-time 15 -X POST http://localhost:8080/url-guardrail/v1.0/dns \
  -H "Content-Type: application/json" \
  -d '{"message": "Visit https://this-domain-does-not-exist-xyz123.com"}'

# Test Case 3: With JSONPath
echo -e "\n\n=== Test Case 3: With JSONPath Configuration ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: URLGuardrail-Test-API-3
  version: v1.0
  context: /url-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /jsonpath
      policies:
        - name: URLGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              jsonPath: "$.link"
              onlyDNS: false
            # No response validation - httpbin responses contain URLs that might fail validation
EOF

sleep 2

echo -e "\nTest 3.1: Valid request (JSONPath extracts valid URL field)"
curl --max-time 15 -X POST http://localhost:8080/url-guardrail/v1.0/jsonpath \
  -H "Content-Type: application/json" \
  -d '{"link": "https://example.com", "message": "Invalid URL in message but ignored: https://invalid-url-xyz.com"}'

echo -e "\n\nTest 3.2: Invalid request (JSONPath field has invalid URL)"
curl --max-time 15 -X POST http://localhost:8080/url-guardrail/v1.0/jsonpath \
  -H "Content-Type: application/json" \
  -d '{"link": "https://invalid-domain-xyz123.com", "message": "Valid URL in message but ignored: https://example.com"}'

# Test Case 4: With custom timeout
echo -e "\n\n=== Test Case 4: Custom Timeout Configuration ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: URLGuardrail-Test-API-4
  version: v1.0
  context: /url-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /timeout
      policies:
        - name: URLGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              onlyDNS: false
              timeout: 1000
            # No response validation - httpbin responses contain URLs that might fail validation
EOF

sleep 2

echo -e "\nTest 4.1: Request with short timeout (valid URL)"
curl --max-time 15 -X POST http://localhost:8080/url-guardrail/v1.0/timeout \
  -H "Content-Type: application/json" \
  -d '{"message": "Visit https://example.com quickly"}'

# Test Case 5: With showAssessment
echo -e "\n\n=== Test Case 5: With showAssessment ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: URLGuardrail-Test-API-5
  version: v1.0
  context: /url-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /assessment
      policies:
        - name: URLGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              onlyDNS: false
              showAssessment: true
            # No response validation - httpbin responses contain URLs that might fail validation
EOF

sleep 2

echo -e "\nTest 5.1: Request with showAssessment (should include assessment details)"
curl --max-time 15 -X POST http://localhost:8080/url-guardrail/v1.0/assessment \
  -H "Content-Type: application/json" \
  -d '{"message": "Check out https://example.com and https://google.com"}'

echo -e "\n\nTest 5.2: Invalid request with showAssessment"
curl --max-time 15 -X POST http://localhost:8080/url-guardrail/v1.0/assessment \
  -H "Content-Type: application/json" \
  -d '{"message": "Visit https://invalid-url-xyz123.com"}'

# Test Case 6: Different HTTP methods
echo -e "\n\n=== Test Case 6: Different HTTP Methods ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: URLGuardrail-Test-API-6
  version: v1.0
  context: /url-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: PUT
      path: /update
      policies:
        - name: URLGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              onlyDNS: true
            # No response validation - httpbin responses contain URLs that might fail validation
    - method: PATCH
      path: /patch
      policies:
        - name: URLGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              onlyDNS: false
              timeout: 3000
            # No response validation - httpbin responses contain URLs that might fail validation
EOF

sleep 2

echo -e "\nTest 6.1: PUT request (valid URL)"
curl --max-time 15 -X PUT http://localhost:8080/url-guardrail/v1.0/update \
  -H "Content-Type: application/json" \
  -d '{"message": "Update with https://example.com"}'

echo -e "\n\nTest 6.2: PATCH request (valid URL)"
curl --max-time 15 -X PATCH http://localhost:8080/url-guardrail/v1.0/patch \
  -H "Content-Type: application/json" \
  -d '{"message": "Patch with https://google.com"}'

# Test Case 7: Multiple URLs in message
echo -e "\n\n=== Test Case 7: Multiple URLs ==="
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: URLGuardrail-Test-API-7
  version: v1.0
  context: /url-guardrail/v1.0
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /multiple
      policies:
        - name: URLGuardrail
          version: v1.0.0
          enabled: true
          params:
            request:
              onlyDNS: true
              timeout: 2000
            # No response validation - httpbin responses contain URLs that might fail validation
            # Using DNS-only validation for faster tests with multiple URLs
EOF

sleep 2

echo -e "\nTest 7.1: Multiple valid URLs"
curl --max-time 15 -X POST http://localhost:8080/url-guardrail/v1.0/multiple \
  -H "Content-Type: application/json" \
  -d '{"message": "Visit https://example.com and https://google.com for more info"}'

echo -e "\n\nTest 7.2: Mix of valid and invalid URLs"
curl --max-time 15 -X POST http://localhost:8080/url-guardrail/v1.0/multiple \
  -H "Content-Type: application/json" \
  -d '{"message": "Visit https://example.com and https://invalid-url-xyz123.com"}'

echo -e "\n\n=== All Tests Completed ==="

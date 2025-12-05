#!/bin/bash

# Test script for RegexGuardrail policy

echo "Testing RegexGuardrail Policy"
echo "============================="

# Deploy API with RegexGuardrail
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: RegexGuardrail-Test-API
  version: v1.0
  context: /regex-guardrail/$version
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
            pattern: ".*password.*"
            invert: false
EOF

# echo -e "\n\nTest 1: Valid request (no password mention)"
# curl -X POST http://localhost:8080/regex-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "This is a safe message without sensitive data"}'

# echo -e "\n\nTest 2: Invalid request (contains password)"
# curl -X POST http://localhost:8080/regex-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "My password is secret123"}'

# echo -e "\n\nTest 3: JSONPath validation"
# curl -X POST http://localhost:8080/regex-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "Safe content", "other": "password here"}'

# echo -e "\n\nTest completed!"

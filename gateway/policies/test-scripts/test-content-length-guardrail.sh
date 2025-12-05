#!/bin/bash

# Test script for ContentLengthGuardrail policy

echo "Testing ContentLengthGuardrail Policy"
echo "======================================"

# Deploy API with ContentLengthGuardrail
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: ContentLengthGuardrail-Test-API
  version: v1.0
  context: /content-length-guardrail/$version
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
            min: 10
            max: 1000
EOF

# echo -e "\n\nTest 1: Valid request (50 bytes - within 10-1000 range)"
# curl -X POST http://localhost:8080/content-length-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "This is a valid test message"}'

# echo -e "\n\nTest 2: Invalid request (5 bytes - below minimum of 10)"
# curl -X POST http://localhost:8080/content-length-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"x": "hi"}'

# echo -e "\n\nTest 3: JSONPath validation"
# curl -X POST http://localhost:8080/content-length-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "Valid content", "other": "ignored"}'

# echo -e "\n\nTest completed!"

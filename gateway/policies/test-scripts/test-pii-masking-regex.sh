#!/bin/bash

# Test script for PIIMaskingRegex policy

echo "Testing PIIMaskingRegex Policy"
echo "==============================="

# Deploy API with PIIMaskingRegex
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: PIIMaskingRegex-Test-API
  version: v1.0
  context: /pii-masking-regex/$version
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /message
      policies:
        - name: PIIMaskingRegex
          version: v1.0.0
          enabled: true
          params:
            piiEntities:
              - piiEntity: "EMAIL"
                piiRegex: "[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\\.[a-zA-Z]{2,}"
              - piiEntity: "PHONE"
                piiRegex: "\\+?[1-9]\\d{1,14}"
            redactPII: false
EOF

# echo -e "\n\nTest 1: Request with email (should be masked)"
# curl -X POST http://localhost:8080/pii-masking-regex/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "Contact me at user@example.com for details"}'

# echo -e "\n\nTest 2: Request with phone (should be masked)"
# curl -X POST http://localhost:8080/pii-masking-regex/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "Call me at +1234567890"}'

# echo -e "\n\nTest 3: JSONPath validation"
# curl -X POST http://localhost:8080/pii-masking-regex/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "Contact user@example.com", "other": "ignored"}'

# echo -e "\n\nTest completed!"

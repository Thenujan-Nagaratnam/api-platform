#!/bin/bash

# Test script for URLGuardrail policy

echo "Testing URLGuardrail Policy"
echo "==========================="

# Deploy API with URLGuardrail
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: URLGuardrail-Test-API
  version: v1.0
  context: /url-guardrail/$version
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
            allowedURLs:
              - "https://example.com"
              - "https://trusted-site.com"
            blockAllURLs: false
EOF

# echo -e "\n\nTest 1: Valid request (contains allowed URL)"
# curl -X POST http://localhost:8080/url-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "Visit https://example.com for more info"}'

# echo -e "\n\nTest 2: Invalid request (contains blocked URL)"
# curl -X POST http://localhost:8080/url-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "Visit https://malicious-site.com"}'

# echo -e "\n\nTest 3: JSONPath validation"
# curl -X POST http://localhost:8080/url-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "Safe content", "link": "https://example.com"}'

# echo -e "\n\nTest completed!"

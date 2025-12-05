#!/bin/bash

# Test script for PIIMaskingGuardrailsAI policy
# NOTE: Requires PII detection service

echo "Testing PIIMaskingGuardrailsAI Policy"
echo "======================================="

# Deploy API with PIIMaskingGuardrailsAI
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: PIIMaskingAI-Test-API
  version: v1.0
  context: /pii-masking-ai/$version
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /message
      policies:
        - name: PIIMaskingGuardrailsAI
          version: v1.0.0
          enabled: true
          params:
            piiServiceURL: "https://your-pii-service.com/validate"
            piiServiceAPIKey: "YOUR_API_KEY"
            piiEntities:
              - "EMAIL"
              - "PHONE"
              - "SSN"
            redactPII: false
EOF

# echo -e "\n\nTest 1: Request with PII (should be masked by AI service)"
# curl -X POST http://localhost:8080/pii-masking-ai/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "Contact me at user@example.com"}'

# echo -e "\n\nNOTE: Update piiServiceURL and piiServiceAPIKey with your PII service credentials"
# echo -e "Test completed!"

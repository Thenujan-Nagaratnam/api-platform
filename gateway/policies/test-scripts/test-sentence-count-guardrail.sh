#!/bin/bash

# Test script for SentenceCountGuardrail policy

echo "Testing SentenceCountGuardrail Policy"
echo "======================================"

# Deploy API with SentenceCountGuardrail
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: SentenceCountGuardrail-Test-API
  version: v1.0
  context: /sentence-count-guardrail/$version
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
            min: 1
            max: 10
EOF

# echo -e "\n\nTest 1: Valid request (2 sentences - within 1-10 range)"
# curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "This is the first sentence. This is the second sentence."}'

# echo -e "\n\nTest 2: Invalid request (0 sentences)"
# curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "No sentence here"}'

# echo -e "\n\nTest 3: JSONPath validation"
# curl -X POST http://localhost:8080/sentence-count-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "First sentence. Second sentence.", "other": "ignored"}'

# echo -e "\n\nTest completed!"

#!/bin/bash

# Test script for WordCountGuardrail policy

echo "Testing WordCountGuardrail Policy"
echo "=================================="

# Deploy API with WordCountGuardrail
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: WordCountGuardrail-Test-API
  version: v1.0
  context: /word-count-guardrail/$version
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /message
      policies:
        - name: WordCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            min: 5
            max: 500
EOF

# echo -e "\n\nTest 1: Valid request (10 words - within 5-500 range)"
# curl -X POST http://localhost:8080/word-count-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "This is a test message with exactly ten words in total"}'

# echo -e "\n\nTest 2: Invalid request (3 words - below minimum of 5)"
# curl -X POST http://localhost:8080/word-count-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "Too few words"}'

# echo -e "\n\nTest 3: JSONPath validation - valid (5 words in $.message field)"
# curl -X POST http://localhost:8080/word-count-guardrail/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "This message has five words", "other": "This field should be ignored"}'

# echo -e "\n\nTest completed!"

#!/bin/bash

# Test script for AzureContentSafetyContentModeration policy
# NOTE: Requires Azure Content Safety API credentials

echo "Testing AzureContentSafetyContentModeration Policy"
echo "=================================================="

# Deploy API with AzureContentSafetyContentModeration
curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: AzureContentSafety-Test-API
  version: v1.0
  context: /azure-content-safety/$version
  upstreams:
    - url: https://httpbin.org/anything
  operations:
    - method: POST
      path: /message
      policies:
        - name: AzureContentSafetyContentModeration
          version: v1.0.0
          enabled: true
          params:
            azureContentSafetyEndpoint: "https://your-resource.cognitiveservices.azure.com"
            azureContentSafetyKey: "YOUR_API_KEY"
            hateCategory: 2
            sexualCategory: 2
            selfHarmCategory: 2
            violenceCategory: 2
            passthroughOnError: false
EOF

# echo -e "\n\nTest 1: Valid request (safe content)"
# curl -X POST http://localhost:8080/azure-content-safety/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "This is a safe and friendly message"}'

# echo -e "\n\nTest 2: JSONPath validation"
# curl -X POST http://localhost:8080/azure-content-safety/v1.0/message \
#   -H "Content-Type: application/json" \
#   -d '{"message": "Safe content", "other": "ignored"}'

# echo -e "\n\nNOTE: Update azureContentSafetyEndpoint and azureContentSafetyKey with your Azure credentials"
# echo -e "Test completed!"

docker compose down -v


docker run --rm \
    -v $(pwd)/../policies:/workspace/policies \
    -v $(pwd)/../policies/policy-manifest.yaml:/workspace/policy-manifest.yaml \
    -v $(pwd)/../policies/output:/workspace/output \
    -v $(pwd)/../policy-engine/policy-engine:/workspace/policy-engine \
    ghcr.io/wso2/api-platform/gateway-builder:0.0.1 \
    --gateway-controller-base-image ghcr.io/wso2/api-platform/gateway-controller:0.0.1 \
    --router-base-image ghcr.io/wso2/api-platform/gateway-router:0.0.1


cd ../policies/output
cd policy-engine
docker build -t myregistry/policy-engine:v1.0.0 .
cd ../gateway-controller
docker build -t myregistry/gateway-controller:v1.0.0 .
cd ../router
docker build -t myregistry/router:v1.0.0 .
cd ../../


curl -X POST http://localhost:9090/apis \
  -H "Content-Type: application/yaml" \
  --data-binary @- <<'EOF'
version: api-platform.wso2.com/v1
kind: http/rest
spec:
  name: WordCountGuardrail-API
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
    - method: POST
      path: /message/jsonpath
      policies:
        - name: WordCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            min: 3
            max: 500
            jsonPath: $.message
    - method: GET
      path: /alerts/active
      policies:
        - name: WordCountGuardrail
          version: v1.0.0
          enabled: true
          params:
            min: 10
            max: 500
EOF


# Test 1: Valid request body (within word count range)
echo "Test 1: Valid request (10 words - within 5-500 range)"
curl -X POST http://localhost:8080/word-count-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a test message with exactly ten words in total"}'

# Test 2: Request body too short (below minimum)
echo -e "\n\nTest 2: Invalid request (3 words - below minimum of 5)"
curl -X POST http://localhost:8080/word-count-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d '{"message": "Too few words"}'

# Test 3: Request body too long (above maximum)  
echo -e "\n\nTest 3: Invalid request (many words - above maximum of 500)"
# Create a long message with many words
LONG_MSG="This is a very long message that contains way too many words"
for i in {1..50}; do LONG_MSG="$LONG_MSG and this is word number $i"; done
curl -X POST http://localhost:8080/word-count-guardrail/v1.0/message \
  -H "Content-Type: application/json" \
  -d "{\"message\": \"$LONG_MSG\"}"

# Test 4: JSONPath validation - valid
echo -e "\n\nTest 4: JSONPath validation - valid (5 words in $.message field)"
curl -X POST http://localhost:8080/word-count-guardrail/v1.0/message/jsonpath \
  -H "Content-Type: application/json" \
  -d '{"message": "This message has five words", "other": "This field should be ignored"}'

# Test 5: JSONPath validation - invalid (too many words)
echo -e "\n\nTest 5: JSONPath validation - invalid (many words in $.message - exceeds max of 500)"
# Create a long message with many words
LONG_MSG2="This message has way too many words"
for i in {1..50}; do LONG_MSG2="$LONG_MSG2 and this is word number $i"; done
curl -X POST http://localhost:8080/word-count-guardrail/v1.0/message/jsonpath \
  -H "Content-Type: application/json" \
  -d "{\"message\": \"$LONG_MSG2\", \"other\": \"ignored\"}"

# Test 6: Response body validation (GET endpoint)
echo -e "\n\nTest 6: Response body validation (GET endpoint)"
curl http://localhost:8080/word-count-guardrail/v1.0/alerts/active

# Test HTTPS endpoint
echo -e "\n\nTest HTTPS endpoint:"
curl https://localhost:5443/word-count-guardrail/v1.0/message -k \
  -X POST \
  -H "Content-Type: application/json" \
  -d '{"message": "This is a valid test message"}'

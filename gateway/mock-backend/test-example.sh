#!/bin/bash

# Example test script using the mock backend

echo "Testing with Mock Backend"
echo "========================"

# Start mock backend (if not already running)
# docker-compose up -d mock-backend

# Wait for mock backend to be ready
sleep 2

# Test 1: Echo endpoint
echo -e "\nTest 1: Echo endpoint"
curl -X POST http://localhost:8081/echo \
  -H "Content-Type: application/json" \
  -d '{"message": "Hello from test"}'

# Test 2: Generate response with 10 words
echo -e "\n\nTest 2: Response with 10 words"
curl -X GET "http://localhost:8081/mock?word_count=10"

# Test 3: Generate response with 3 sentences
echo -e "\n\nTest 3: Response with 3 sentences"
curl -X GET "http://localhost:8081/mock?sentence_count=3"

# Test 4: Generate response with 500 bytes
echo -e "\n\nTest 4: Response with 500 bytes"
curl -X GET "http://localhost:8081/mock?content_length=500"

# Test 5: Include PII data
echo -e "\n\nTest 5: Response with PII data"
curl -X GET "http://localhost:8081/mock?include_pii=true"

# Test 6: Include password
echo -e "\n\nTest 6: Response with password"
curl -X GET "http://localhost:8081/mock?include_password=true"

# Test 7: Include URLs
echo -e "\n\nTest 7: Response with URLs"
curl -X GET "http://localhost:8081/mock?include_urls=https://example.com,https://google.com"

# Test 8: Custom configuration via POST
echo -e "\n\nTest 8: Custom configuration"
curl -X POST http://localhost:8081/mock \
  -H "Content-Type: application/json" \
  -d '{
    "status_code": 200,
    "word_count": 15,
    "include_pii": true,
    "delay_ms": 100
  }'

echo -e "\n\nAll tests completed!"

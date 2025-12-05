#!/bin/bash

# Master test script for all guardrail policies
# This script runs tests for all converted guardrail policies

echo "=========================================="
echo "Testing All Guardrail Policies"
echo "=========================================="
echo ""

# Test WordCountGuardrail
echo "1. Testing WordCountGuardrail..."
bash test-scripts/test-word-count-guardrail.sh
echo ""
sleep 2

# Test ContentLengthGuardrail
echo "2. Testing ContentLengthGuardrail..."
bash test-scripts/test-content-length-guardrail.sh
echo ""
sleep 2

# Test RegexGuardrail
echo "3. Testing RegexGuardrail..."
bash test-scripts/test-regex-guardrail.sh
echo ""
sleep 2

# Test SentenceCountGuardrail
echo "4. Testing SentenceCountGuardrail..."
bash test-scripts/test-sentence-count-guardrail.sh
echo ""
sleep 2

# Test URLGuardrail
echo "5. Testing URLGuardrail..."
bash test-scripts/test-url-guardrail.sh
echo ""
sleep 2

# Test AzureContentSafetyContentModeration
echo "6. Testing AzureContentSafetyContentModeration..."
echo "   (Requires Azure credentials - skipping actual API calls)"
bash test-scripts/test-azure-content-safety.sh
echo ""
sleep 2

# Test PIIMaskingRegex
echo "7. Testing PIIMaskingRegex..."
bash test-scripts/test-pii-masking-regex.sh
echo ""
sleep 2

# Test PIIMaskingGuardrailsAI
echo "8. Testing PIIMaskingGuardrailsAI..."
echo "   (Requires PII service credentials - skipping actual API calls)"
bash test-scripts/test-pii-masking-ai.sh
echo ""

echo "=========================================="
echo "All guardrail tests completed!"
echo "=========================================="

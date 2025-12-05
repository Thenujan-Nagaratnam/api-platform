# Inbuilt Policies - New Format

This directory contains guardrail policies converted from the old format to the new policy SDK format.

## Converted Policies

All policies have been converted from `gateway/examples/inbuilt-policies-old/` to the new format:

1. **WordCountGuardrail** - Validates word count in request/response bodies
2. **ContentLengthGuardrail** - Validates content length (bytes) in request/response bodies
3. **RegexGuardrail** - Validates content against regex patterns
4. **SentenceCountGuardrail** - Validates sentence count in request/response bodies
5. **URLGuardrail** - Validates and filters URLs in content
6. **AWSBedrockGuardrail** - AWS Bedrock guardrail integration (simplified)
7. **AzureContentSafetyContentModeration** - Azure Content Safety API integration
8. **PIIMaskingRegex** - PII masking using regex patterns
9. **PIIMaskingGuardrailsAI** - PII masking using AI service
10. **SemanticCache** - Semantic caching for LLM responses (simplified)

## Policy Structure

Each policy follows the new format:
```
policy-name/
  v1.0.0/
    policy-definition.yaml  # Policy metadata and parameter definitions
    policyname.go           # Policy implementation
    go.mod                  # Go module definition
```

## Testing

Individual test scripts are available for each policy:
- `test-word-count-guardrail.sh`
- `test-content-length-guardrail.sh`
- `test-regex-guardrail.sh`
- `test-sentence-count-guardrail.sh`
- `test-url-guardrail.sh`
- `test-azure-content-safety.sh`
- `test-pii-masking-regex.sh`
- `test-pii-masking-ai.sh`

Run all tests:
```bash
./test-all-guardrails.sh
```

## Policy Manifest

All policies are registered in:
- `gateway/policies/policy-manifest.yaml`
- `gateway/policy-engine/policy-manifest.yaml`

## Notes

- **AzureContentSafetyContentModeration**: Requires Azure Content Safety API credentials
- **PIIMaskingGuardrailsAI**: Requires PII detection service URL and API key
- **SemanticCache**: Simplified implementation - full version requires embedding providers and vector DB libraries
- **AWSBedrockGuardrail**: Simplified implementation - full version requires AWS SDK

## Building

To build policies with the gateway builder:
```bash
cd gateway/policies
./run.sh
```

This will:
1. Build the policy-engine with all policies
2. Create Docker images
3. Deploy test APIs
4. Run test cases

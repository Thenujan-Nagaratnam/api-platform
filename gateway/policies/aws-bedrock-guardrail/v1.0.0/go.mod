module github.com/policy-engine/policies/aws-bedrock-guardrail

go 1.23.0

require github.com/policy-engine/sdk v1.0.0

replace github.com/policy-engine/sdk => ../../../sdk

// NOTE: Full implementation requires AWS SDK dependencies:
// require (
//     github.com/aws/aws-sdk-go-v2/config v1.x.x
//     github.com/aws/aws-sdk-go-v2/service/bedrockruntime v1.x.x
//     github.com/aws/aws-sdk-go-v2/service/sts v1.x.x
//     github.com/aws/aws-sdk-go-v2/credentials/stscreds v1.x.x
// )


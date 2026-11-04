# Baseline and per-app Amazon Bedrock Guardrails with WSO2 AI Gateway

Sample code for the AWS Machine Learning Blog post *Apply baseline and per-app Amazon Bedrock Guardrails with WSO2 AI Gateway*.

- **AWS side, as code:** Terraform creates two Amazon Bedrock guardrails (an organization baseline and a support-assistant guardrail), their versions, and a least-privilege IAM role for the gateway.
- **WSO2 side, in AI Workspace:** you configure the LLM provider, the App LLM proxies, and their guardrail policies in AI Workspace, which deploys them to a gateway that runs on your machine.
- **No stored AWS keys:** your AWS CLI session's temporary credentials only call `sts:AssumeRole` on the gateway role. Every Amazon Bedrock call runs under that role.

![High-level architecture](images/figure-1-architecture.png)

## Layout

| Path | Contents |
| --- | --- |
| `terraform/` | The guardrails, their versions, and the gateway's IAM role |
| `compose/` | `docker-compose.override.yaml`, which passes your session credentials to the gateway runtime, and the guardrail allowlist |
| `scripts/` | Start the gateway, store application keys, test, and clean up |
| `gateway/` | Provider and proxy definitions for the code-only path |

## Prerequisites

- An AWS account and an AWS CLI session (AWS IAM Identity Center recommended).
- An AWS Region with Amazon Bedrock and Amazon Bedrock Guardrails. The post uses `us-east-2` with the `us.amazon.nova-2-lite-v1:0` inference profile, which routes to `us-east-1`, `us-east-2`, and `us-west-2`. Set `model_regions` if you use another profile.
- An AI Workspace account.
- Docker with Docker Compose 2.30 or later, Terraform 1.9 or later, AWS CLI 2.9 or later, curl, jq, and envsubst.

## Run

```bash
aws sso login
export AWS_REGION=us-east-2 BEDROCK_MODEL_ID=us.amazon.nova-2-lite-v1:0
TF_VARS=(-var="region=${AWS_REGION}" -var="model_id=${BEDROCK_MODEL_ID}")

terraform -chdir=terraform init
terraform -chdir=terraform apply "${TF_VARS[@]}"
terraform -chdir=terraform output              # IDs and ARNs for AI Workspace

# In AI Workspace: add a gateway and download it with the Get Started commands. Then:
./scripts/run-gateway.sh <gateway-folder>

# In AI Workspace: add the provider and both App LLM proxies (see the post), then store each proxy key:
./scripts/store-app-key.sh support-assistant
./scripts/store-app-key.sh employee-assistant

./scripts/test-guardrails.sh <gateway-folder>   # calls https://localhost:8443/default/<proxy>
```

`run-gateway.sh` starts the gateway in the background, confirms that `gateway-runtime` received your AWS credentials (it prints variable names only), and returns. Set `FOLLOW_LOGS=1` to follow the logs. Stop the gateway with `docker compose --env-file api-platform.env down` in the gateway folder.

## Code-only path (no AI Workspace)

To configure everything as code instead of in AI Workspace, use the standalone gateway and its management API. The Terraform, test, and cleanup steps are the same.

```bash
./scripts/start-gateway.sh        # downloads the gateway (SHA-256 verified), sets it up, and starts it
./scripts/deploy-artifacts.sh     # provider, both proxies, and one key per app in Secrets Manager
./scripts/test-guardrails.sh wso2apip-ai-gateway
```

`deploy-artifacts.sh` deploys `gateway/bedrock-provider.yaml` and the two proxy files, fills in the Terraform outputs, and stores each application's key in Secrets Manager without printing it. The admin password for the management API is generated once and kept in Secrets Manager. Standalone proxies live at `https://localhost:8443/<proxy>`, without the `/default` project prefix, and the test script picks that up automatically.

## Clean up

If you used AI Workspace, delete the proxies, the provider, and the gateway there first. Then:

```bash
./scripts/cleanup.sh <gateway-folder>
terraform -chdir=terraform destroy "${TF_VARS[@]}"
```

## Scan results

| Tool | Scope | Result |
| --- | --- | --- |
| Checkov 3 | `terraform/` | 8 passed, 0 failed |
| ShellCheck | `scripts/*.sh` | No findings |
| yamllint | `compose/` | No findings |

## License

Apache License 2.0. See [LICENSE](LICENSE).

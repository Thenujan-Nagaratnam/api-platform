# Safeguard LLM traffic with Amazon Bedrock Guardrails and WSO2 AI Gateway on Amazon EKS

Sample code for running WSO2 AI Gateway on Amazon EKS, configured from Agent Control Plane, with Amazon Bedrock Guardrails.

- **AWS side, as code:** Terraform creates two Amazon Bedrock guardrails, the VPC, the EKS cluster, an Amazon RDS for PostgreSQL database, Amazon ECR repositories, a least-privilege IAM role for the gateway through EKS Pod Identity, and an Amazon CloudWatch dashboard.
- **Gateway side, in Agent Control Plane:** you configure the LLM provider, the App LLM proxies, and their guardrail policies in Agent Control Plane, which deploys them to the gateway in your cluster.
- **No stored AWS keys:** the gateway pods get temporary credentials from EKS Pod Identity.

## Layout

| Path | Contents |
| --- | --- |
| `terraform/` | The guardrails, network, cluster, database, image repositories, IAM role, and dashboard |
| `helm/` | Helm values and the guardrail allowlist |
| `scripts/` | Build images, store the gateway token and application keys, install the gateway, port-forward, test, and clean up |
| `gateway/` | Provider and proxy definitions, for reference |

## Prerequisites

- An AWS account and an AWS CLI session (AWS IAM Identity Center recommended).
- An Agent Control Plane account.
- Docker, Terraform 1.9 or later, AWS CLI 2.9 or later, kubectl, Helm 3.12 or later, curl, jq, envsubst, openssl, and unzip.

## Run

```bash
aws sso login
export AWS_REGION=us-east-2 BEDROCK_MODEL_ID=us.amazon.nova-2-lite-v1:0
MY_IP="$(curl -s https://checkip.amazonaws.com)/32"
TF_VARS=(-var="region=${AWS_REGION}" -var="model_id=${BEDROCK_MODEL_ID}" -var="eks_public_access_cidrs=[\"${MY_IP}\"]")

terraform -chdir=terraform init
terraform -chdir=terraform apply "${TF_VARS[@]}"
./scripts/build-gateway-image.sh

# In Agent Control Plane: add a gateway with the URL https://localhost:8443, and copy its control plane host and token. Then:
export AGENT_CONTROL_PLANE_HOST=<control-plane-host>
./scripts/store-gateway-token.sh
./scripts/install-gateway.sh

# In a second terminal, and keep it running:
./scripts/port-forward.sh

# In Agent Control Plane: add the provider and both App LLM proxies, with the default-credential-chain authentication type. Then store each proxy key:
./scripts/store-app-key.sh support-assistant
./scripts/store-app-key.sh employee-assistant

./scripts/test-guardrails.sh   # calls https://localhost:8443/default/<proxy>
terraform -chdir=terraform output -raw dashboard_url
```

## Clean up

In Agent Control Plane, delete the proxies, the provider, and the gateway first. Then:

```bash
./scripts/cleanup.sh
terraform -chdir=terraform destroy "${TF_VARS[@]}"
```

## License

Apache License 2.0. See [LICENSE](LICENSE).

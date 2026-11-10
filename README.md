# Safeguard LLM traffic with Amazon Bedrock Guardrails and WSO2 AI Gateway on Amazon EKS

Code-only sample for running WSO2 AI Gateway on Amazon EKS with Amazon Bedrock Guardrails. It configures the gateway through its management API.

- **AWS side, as code:** Terraform creates two Amazon Bedrock guardrails, the VPC, the EKS cluster, an Amazon RDS for PostgreSQL database, Amazon ECR repositories, a least-privilege IAM role for the gateway through EKS Pod Identity, and an Amazon CloudWatch dashboard.
- **Gateway side, as code:** YAML files define the LLM provider, both App LLM proxies, and their guardrail policies. `deploy-artifacts.sh` deploys them through the gateway's management API.
- **No stored AWS keys:** the gateway pods get temporary credentials from EKS Pod Identity.

## Layout

| Path | Contents |
| --- | --- |
| `terraform/` | The guardrails, network, cluster, database, image repositories, IAM role, and dashboard |
| `helm/` | Helm values and the guardrail allowlist |
| `scripts/` | Build and install the gateway, port-forward, deploy the provider and proxies, test, and clean up |
| `gateway/` | Provider and proxy definitions, and the policy versions built into the gateway images |

## Prerequisites

- An AWS account and an AWS CLI session (AWS IAM Identity Center recommended).
- Docker, Terraform 1.9 or later, AWS CLI 2.9 or later, kubectl, Helm 3.12 or later, curl, jq, envsubst, openssl, and unzip.

## Run

```bash
aws sso login
export AWS_REGION=us-east-1 BEDROCK_MODEL_ID=us.amazon.nova-2-lite-v1:0
MY_IP="$(curl -s https://checkip.amazonaws.com)/32"
TF_VARS=(-var="region=${AWS_REGION}" -var="model_id=${BEDROCK_MODEL_ID}" -var="eks_public_access_cidrs=[\"${MY_IP}\"]")

terraform -chdir=terraform init
terraform -chdir=terraform apply "${TF_VARS[@]}"

./scripts/build-gateway-image.sh   # builds the gateway images for linux/amd64 and pushes them to Amazon ECR
./scripts/install-gateway.sh       # installs cert-manager, creates the database tables, and installs the gateway

# In a second terminal, and keep it running:
./scripts/port-forward.sh

./scripts/deploy-artifacts.sh      # provider, both proxies, and one key per app in Secrets Manager
./scripts/test-guardrails.sh       # calls https://localhost:8443/<proxy>
terraform -chdir=terraform output -raw dashboard_url
```

`install-gateway.sh` generates the management API admin password and keeps it in Secrets Manager. It also generates the key that encrypts the gateway's secrets at rest, and keeps it in a Kubernetes Secret. Rerun it to apply changes. `deploy-artifacts.sh` deploys `gateway/bedrock-provider.yaml` and the two proxy files, fills in the Terraform outputs, and stores each application's key in Secrets Manager without printing it.

## Clean up

```bash
./scripts/cleanup.sh
terraform -chdir=terraform destroy "${TF_VARS[@]}"
```

## License

Apache License 2.0. See [LICENSE](LICENSE).

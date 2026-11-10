# Amazon CloudWatch dashboard for the shared guardrail. It graphs how many checks the guardrail runs,
# and how many requests and responses it blocks or masks. Set create_dashboard = false to
# skip it. After the first three dashboards in an account, each one costs about $3 a month, and
# terraform destroy removes it with the guardrails.
variable "create_dashboard" {
  description = "Create an Amazon CloudWatch dashboard that graphs guardrail checks and interventions."
  type        = bool
  default     = true
}

locals {
  guardrail_namespace = "AWS/Bedrock/Guardrails"
}

resource "aws_cloudwatch_dashboard" "guardrails" {
  count          = var.create_dashboard ? 1 : 0
  dashboard_name = "${var.name_prefix}-guardrails"

  dashboard_body = jsonencode({
    start = "-PT1H"
    widgets = [{
      type   = "metric"
      x      = 0
      y      = 0
      width  = 24
      height = 8
      properties = {
        title   = "Amazon Bedrock Guardrails: checks and interventions per minute"
        region  = var.region
        view    = "timeSeries"
        stacked = false
        stat    = "Sum"
        period  = 60
        # Each guardrail is labelled by name, so the graph shows no account ID.
        metrics = [
          [local.guardrail_namespace, "Invocations",
            "GuardrailArn", aws_bedrock_guardrail.baseline.guardrail_arn,
            "GuardrailVersion", aws_bedrock_guardrail_version.baseline.version,
          { label = "${aws_bedrock_guardrail.baseline.name} checks (every request)", color = "#1f77b4" }],
          [local.guardrail_namespace, "InvocationsIntervened", "Operation", "ApplyGuardrail",
          { label = "Interventions: blocked or masked", color = "#d62728" }],
        ]
        yAxis  = { left = { min = 0, label = "Count", showUnits = false } }
        legend = { position = "bottom" }
      }
    }]
  })
}

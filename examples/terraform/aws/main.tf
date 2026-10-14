terraform {
  required_version = ">= 1.6.0"
}

variable "lambda_image" {
  type        = string
  description = "OCI image for the Lambda lite launcher."
}

variable "status_table_name" {
  type        = string
  description = "DynamoDB table the Lambda launcher writes execution status to (STATUS_TABLE)."
}

variable "runner_github_app_secret_arn" {
  type        = string
  description = "Secrets Manager ARN holding the runner GitHub App JSON (app_id, installation_id, private_key). Exposed to the Lambda as RUNNER_GITHUB_APP_SECRET_ARN; the execution role needs secretsmanager:GetSecretValue on it."
}

locals {
  # Pass this map as the Lambda function's environment.variables block.
  lambda_environment = {
    STATUS_TABLE                 = var.status_table_name
    RUNNER_GITHUB_APP_SECRET_ARN = var.runner_github_app_secret_arn
  }

  # Attach to the Lambda execution role.
  lambda_secret_read_statement = {
    Effect   = "Allow"
    Action   = ["secretsmanager:GetSecretValue"]
    Resource = [var.runner_github_app_secret_arn]
  }
}

output "lambda_environment" {
  value = local.lambda_environment
}

output "lambda_secret_read_statement" {
  value = local.lambda_secret_read_statement
}

output "next_step" {
  value = "Create Lambda launcher resources using lambda_environment and lambda_secret_read_statement, wire a backend secret ref, and point the broker config at the published launcher image."
}

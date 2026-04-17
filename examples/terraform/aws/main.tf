terraform {
  required_version = ">= 1.6.0"
}

variable "lambda_image" {
  type        = string
  description = "OCI image for the Lambda lite launcher."
}

output "next_step" {
  value = "Create Lambda launcher resources, wire a backend secret ref, and point the broker config at the published launcher image."
}


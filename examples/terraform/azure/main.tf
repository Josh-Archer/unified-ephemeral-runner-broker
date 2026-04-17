terraform {
  required_version = ">= 1.6.0"
}

variable "functions_image" {
  type        = string
  description = "OCI image for the Azure Functions lite launcher."
}

output "next_step" {
  value = "Create the Azure Functions lite launcher resources, wire a backend secret ref, and point the broker config at the published launcher image."
}


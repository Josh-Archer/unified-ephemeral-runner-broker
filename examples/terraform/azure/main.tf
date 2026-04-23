terraform {
  required_version = ">= 1.6.0"
}

variable "functions_image" {
  type        = string
  description = "OCI image for the Azure Functions lite launcher."
}

output "next_step" {
  value = "Create a Linux custom-container Function App on a Premium or Dedicated plan, point it at the published azure-functions image, wire the backend secret ref, and enable the azure-functions backend in broker config."
}

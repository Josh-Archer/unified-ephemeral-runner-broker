terraform {
  required_version = ">= 1.6.0"
}

variable "cloud_run_image" {
  type        = string
  description = "OCI image for the Cloud Run lite launcher."
}

output "next_step" {
  value = "Create Cloud Run Job resources, wire a backend secret ref, and point the broker config at the published launcher image."
}


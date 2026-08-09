# ---- Hetzner Cloud ----

# Read & Write API token. Supplied via the environment, NOT terraform.tfvars:
#   export TF_VAR_hcloud_token="..."
variable "hcloud_token" {
  description = "Hetzner Cloud API token (Read & Write)."
  type        = string
  sensitive   = true
}

# ---- AWS (pyparser dataset bucket + IAM) ----

variable "region" {
  description = "AWS region."
  type        = string
  default     = "eu-north-1"
}

variable "aws_profile" {
  description = "Named AWS CLI/SSO profile. Empty string uses the default credential chain."
  type        = string
  default     = ""
}

variable "dataset_bucket_name" {
  description = "Name of the EXISTING S3 bucket that holds the dataset (PDFs/markdown/assets)."
  type        = string
}

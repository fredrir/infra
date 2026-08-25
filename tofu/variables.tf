# ---- Hetzner Cloud ----
variable "hcloud_token" {
  description = "Hetzner Cloud API token (Read & Write)."
  type        = string
  sensitive   = true
}

# ---- AWS ----
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

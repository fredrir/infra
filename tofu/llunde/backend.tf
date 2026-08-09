# Remote state (ADR 002): S3 backend replaces local state + manual copies.
# Credentials come from the environment (AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY),
# e.g. `doppler run --project pyparser --config prd -- tofu ...`.
terraform {
  backend "s3" {
    bucket = "llunde-pyparser-bucket"
    key    = "tofu-state/llunde.tfstate"
    region = "eu-north-1"
  }
}

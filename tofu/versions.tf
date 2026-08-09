terraform {
  required_version = ">= 1.6.0"

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.48"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
    }
  }

  # Remote state (ADR 002): one flat root, one state, since phase-3 step 7.
  # The per-project keys (tofu-state/llunde.tfstate, tofu-state/pyparser.tfstate)
  # remain in the versioned bucket as the merge's rollback anchors.
  # Credentials come from the environment (AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY).
  backend "s3" {
    bucket       = "llunde-pyparser-bucket"
    key          = "tofu-state/infra.tfstate"
    region       = "eu-north-1"
    encrypt      = true
    use_lockfile = true
  }
}

provider "hcloud" {
  token = var.hcloud_token
}

provider "aws" {
  region  = var.region
  profile = var.aws_profile != "" ? var.aws_profile : null

  # All AWS resources here are pyparser's (dataset bucket + IAM); the tags
  # predate the flat root and are kept verbatim to avoid churn.
  default_tags {
    tags = {
      Project   = "pyparser"
      ManagedBy = "terraform"
    }
  }
}

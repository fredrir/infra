terraform {
  required_version = ">= 1.6"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.48"
    }
  }

  # Remote state (ADR 002): S3 backend, same bucket as the llunde root.
  # Credentials come from the environment (AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY).
  backend "s3" {
    bucket       = "llunde-pyparser-bucket"
    key          = "tofu-state/pyparser.tfstate"
    region       = "eu-north-1"
    encrypt      = true
    use_lockfile = true
  }
}

provider "aws" {
  region  = var.region
  profile = var.aws_profile != "" ? var.aws_profile : null

  default_tags {
    tags = {
      Project   = "pyparser"
      ManagedBy = "terraform"
    }
  }
}

provider "hcloud" {
  token = var.hcloud_token
}

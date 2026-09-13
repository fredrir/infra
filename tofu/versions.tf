terraform {
  required_version = ">= 1.11.0, < 2.0.0"

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "1.68.0"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "5.100.0"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "5.23.0"
    }
  }
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

  default_tags {
    tags = {
      Project   = "pyparser"
      ManagedBy = "terraform"
    }
  }
}

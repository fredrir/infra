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

  # State is LOCAL (terraform.tfstate in this dir, gitignored). Back it up after
  # the adoption import — it's the only Terraform record of the live server. To
  # move to remote, locked S3 state later: create a state bucket, uncomment below
  # (the modern S3 backend supports use_lockfile = true — no DynamoDB needed),
  # then `terraform init -migrate-state`.
  #
  # backend "s3" {
  #   bucket       = "<your-tf-state-bucket>"
  #   key          = "pyparser/terraform.tfstate"
  #   region       = "eu-north-1"
  #   encrypt      = true
  #   use_lockfile = true
  # }
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

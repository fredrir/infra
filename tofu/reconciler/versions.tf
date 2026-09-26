terraform {
  required_version = ">= 1.11.0, < 2.0.0"

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "1.69.0"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "5.100.0"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "5.25.0"
    }
  }
  backend "s3" {
    bucket       = "llunde-pyparser-bucket"
    key          = "tofu-state/reconciler.tfstate"
    region       = "eu-north-1"
    encrypt      = true
    use_lockfile = true
  }
}

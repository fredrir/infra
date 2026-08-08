terraform {
  required_version = ">= 1.6"
  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.48"
    }
  }
  # hcloud ONLY — no aws provider (this module has no AWS resources; declaring aws
  # would force AWS credential resolution for nothing). Local state, gitignored.
}

provider "hcloud" {
  token = var.hcloud_token
}

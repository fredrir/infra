terraform {
  required_version = ">= 1.11.0, < 2.0.0"
  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "1.68.0"
    }
  }
}

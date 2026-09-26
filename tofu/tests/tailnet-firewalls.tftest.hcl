mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "123456789012" }
  }
  mock_data "aws_s3_bucket" {
    defaults = { arn = "arn:aws:s3:::dataset" }
  }
  mock_data "aws_iam_policy_document" {
    defaults = { json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}" }
  }
  mock_resource "aws_iam_policy" {
    defaults = { arn = "arn:aws:iam::123456789012:policy/platform/workload" }
  }
  mock_resource "aws_sesv2_email_identity" {
    defaults = {
      arn                     = "arn:aws:ses:eu-north-1:123456789012:identity/example.com"
      dkim_signing_attributes = { tokens = ["first-selector", "second-selector", "third-selector"] }
    }
  }
}

mock_provider "hcloud" {}

mock_provider "cloudflare" {}

variables {
  hcloud_token            = "mock"
  dataset_bucket_name     = "dataset"
  platform_mail_recipient = "operator@example.net"
  platform_mail = {
    domain  = "example.com"
    zone_id = "0123456789abcdef0123456789abcdef"
    sender  = "alerts@example.com"
  }
  platform_control_planes = {
    "fredrir-07" = { name = "fredrir-07", private_ip = "10.60.0.7", ssh_keys = ["admin"] }
    "fredrir-08" = { name = "fredrir-08", private_ip = "10.60.0.8", ssh_keys = ["admin"] }
  }
}

run "root_firewalls_accept_direct_tailnet_paths" {
  command = plan

  plan_options {
    target = [hcloud_firewall.control_05, hcloud_firewall.platform_control_planes]
  }

  assert {
    condition = alltrue([
      for firewall in [hcloud_firewall.control_05, hcloud_firewall.platform_control_planes[0]] : anytrue([
        for rule in firewall.rule : rule.direction == "in" && rule.protocol == "udp" && rule.port == "41641" && toset(rule.source_ips) == toset(["0.0.0.0/0", "::/0"])
      ])
    ])
    error_message = "Every fleet firewall must accept Tailscale's UDP port so NATed peers avoid DERP relays."
  }
}

run "server_module_firewall_accepts_direct_tailnet_paths" {
  command = plan

  module {
    source = "./modules/hetzner"
  }

  providers = {
    hcloud = hcloud
  }

  plan_options {
    target = [hcloud_firewall.this]
  }

  variables {
    name          = "fredrir-99"
    server_type   = "cx33"
    location      = "hel1"
    firewall_name = "fredrir-99-fw"
    inbound_rules = [{ port = "443", description = "HTTPS" }]
  }

  assert {
    condition = anytrue([
      for rule in hcloud_firewall.this.rule : rule.direction == "in" && rule.protocol == "udp" && rule.port == "41641" && toset(rule.source_ips) == toset(["0.0.0.0/0", "::/0"])
    ]) && length(hcloud_firewall.this.rule) == 2
    error_message = "Every fleet firewall must accept Tailscale's UDP port so NATed peers avoid DERP relays."
  }
}

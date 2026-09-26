mock_provider "hcloud" {
  mock_resource "hcloud_primary_ip" {
    defaults = { id = 4242, ip_address = "203.0.113.10" }
  }
  mock_resource "hcloud_firewall" {
    defaults = { id = "77" }
  }
  mock_data "hcloud_servers" {
    defaults = { servers = [] }
  }
}

mock_provider "aws" {}

mock_provider "cloudflare" {}

override_data {
  target = data.cloudflare_account_api_token_permission_groups_list.account
  values = {
    result = [
      { id = "zone-read", name = "Zone Read", scopes = ["com.cloudflare.api.account.zone"] },
      { id = "zone-write", name = "Zone Write", scopes = ["com.cloudflare.api.account.zone"] },
      { id = "dns-read", name = "DNS Read", scopes = ["com.cloudflare.api.account.zone"] },
      { id = "dns-write", name = "DNS Write", scopes = ["com.cloudflare.api.account.zone"] },
      { id = "tunnel-read", name = "Cloudflare Tunnel Read", scopes = ["com.cloudflare.api.account"] },
      { id = "tunnel-write", name = "Cloudflare Tunnel Write", scopes = ["com.cloudflare.api.account"] },
      { id = "logs-read-account", name = "Logs Read", scopes = ["com.cloudflare.api.account"] },
      { id = "logs-read-zone", name = "Logs Read", scopes = ["com.cloudflare.api.account.zone"] },
    ]
  }
}

variables {
  reconciler_hcloud_token = "mock"
}

run "dedicated_host_without_inbound_access" {
  command = plan

  assert {
    condition     = length(hcloud_firewall.reconciler.rule) == 0 && hcloud_server.reconciler.firewall_ids == toset([77])
    error_message = "The reconciler firewall must allow no inbound traffic by default."
  }

  assert {
    condition = (
      hcloud_server.reconciler.name == "fredrir-11" && hcloud_server.reconciler.server_type == "cx33" &&
      hcloud_server.reconciler.location == "hel1" && hcloud_server.reconciler.image == "ubuntu-26.04" &&
      hcloud_server.reconciler.delete_protection && hcloud_server.reconciler.rebuild_protection
    )
    error_message = "The reconciler must be a protected cx33 in hel1 running Ubuntu 26.04."
  }

  assert {
    condition = (
      length(hcloud_server.reconciler.public_net) == 1 &&
      one(hcloud_server.reconciler.public_net).ipv4_enabled && one(hcloud_server.reconciler.public_net).ipv4 == 4242 &&
      !one(hcloud_server.reconciler.public_net).ipv6_enabled
    )
    error_message = "The reconciler must egress only through its primary IPv4 address."
  }

  assert {
    condition = (
      hcloud_primary_ip.reconciler.type == "ipv4" && hcloud_primary_ip.reconciler.location == "hel1" &&
      !hcloud_primary_ip.reconciler.auto_delete && hcloud_primary_ip.reconciler.delete_protection
    )
    error_message = "The primary IPv4 address must outlive the server and resist deletion."
  }

  assert {
    condition     = keys(hcloud_ssh_key.admin) == ["archie", "macie"] && startswith(hcloud_ssh_key.admin["macie"].public_key, "ssh-ed25519 ")
    error_message = "Only the declared administrator keys may reach the reconciler."
  }

  assert {
    condition     = aws_iam_access_key.verify.user == "infra-reconciliation-verify"
    error_message = "The access key belongs to the verify identity."
  }

  assert {
    condition     = cloudflare_account_token.verify.condition.request_ip.in == tolist(["203.0.113.10/32"]) && cloudflare_account_token.verify.condition.request_ip.not_in == null
    error_message = "The Cloudflare token must be usable only from the reconciler's address."
  }

  assert {
    condition = (
      cloudflare_account_token.verify.account_id == "8786559b30fcebd08d0c594b6e899eef" &&
      [for policy in cloudflare_account_token.verify.policies : [for group in policy.permission_groups : group.id]] == [["zone-read", "dns-read"], ["tunnel-read"]] &&
      [for policy in cloudflare_account_token.verify.policies : jsondecode(policy.resources)] == [
        { "com.cloudflare.api.account.8786559b30fcebd08d0c594b6e899eef" = { "com.cloudflare.api.account.zone.*" = "*" } },
        { "com.cloudflare.api.account.8786559b30fcebd08d0c594b6e899eef" = "*" },
      ] &&
      alltrue([for policy in cloudflare_account_token.verify.policies : policy.effect == "allow"])
    )
    error_message = "The Cloudflare token may only read zones, DNS and tunnels of the managed account."
  }

}

run "bootstrap_ssh_from_administrators" {
  command = plan

  variables {
    bootstrap_admin_cidrs = ["62.92.106.173/32"]
  }

  assert {
    condition = length(hcloud_firewall.reconciler.rule) == 1 && alltrue([
      for rule in hcloud_firewall.reconciler.rule :
      rule.direction == "in" && rule.protocol == "tcp" && rule.port == "22" && rule.source_ips == toset(["62.92.106.173/32"])
    ])
    error_message = "Bootstrap access must be SSH from the declared administrator ranges only."
  }
}

run "refuse_shared_project" {
  command = plan

  override_data {
    target = data.hcloud_servers.project
    values = {
      servers = [{
        id                 = 141119325
        name               = "fredrir-04"
        server_type        = "ccx23"
        location           = "hel1"
        datacenter         = "hel1-dc2"
        image              = "ubuntu-26.04"
        status             = "running"
        backup_window      = ""
        backups            = false
        delete_protection  = true
        rebuild_protection = true
        firewall_ids       = []
        labels             = {}
        iso                = ""
        rescue             = ""
        ipv4_address       = "95.217.135.164"
        ipv6_address       = ""
        ipv6_network       = ""
        network            = []
        placement_group_id = 0
        primary_disk_size  = 160
      }]
    }
  }

  expect_failures = [hcloud_primary_ip.reconciler]
}

run "accept_existing_reconciler" {
  command = plan

  override_data {
    target = data.hcloud_servers.project
    values = {
      servers = [{
        id                 = 150000011
        name               = "fredrir-11"
        server_type        = "cx33"
        location           = "hel1"
        datacenter         = "hel1-dc2"
        image              = "ubuntu-26.04"
        status             = "running"
        backup_window      = ""
        backups            = false
        delete_protection  = true
        rebuild_protection = true
        firewall_ids       = []
        labels             = {}
        iso                = ""
        rescue             = ""
        ipv4_address       = "203.0.113.10"
        ipv6_address       = ""
        ipv6_network       = ""
        network            = []
        placement_group_id = 0
        primary_disk_size  = 80
      }]
    }
  }

  assert {
    condition     = hcloud_server.reconciler.name == "fredrir-11"
    error_message = "A project holding only the reconciler must plan."
  }
}

run "reject_open_bootstrap" {
  command = plan

  variables {
    bootstrap_admin_cidrs = ["0.0.0.0/0"]
  }

  expect_failures = [var.bootstrap_admin_cidrs]
}

run "reject_wide_bootstrap" {
  command = plan

  variables {
    bootstrap_admin_cidrs = ["62.92.0.0/16"]
  }

  expect_failures = [var.bootstrap_admin_cidrs]
}

run "reject_ipv6_bootstrap" {
  command = plan

  variables {
    bootstrap_admin_cidrs = ["2001:db8::/64"]
  }

  expect_failures = [var.bootstrap_admin_cidrs]
}

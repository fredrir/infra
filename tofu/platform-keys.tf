resource "cloudflare_workers_script" "keys" {
  account_id         = local.cf_account
  script_name        = "platform-keys"
  main_module        = "keys.js"
  content            = file("${path.module}/workers/keys.js")
  compatibility_date = "2026-09-01"

  bindings = [
    {
      name = "KEYS"
      type = "plain_text"
      text = file("${path.module}/../ssh/admin_keys")
    },
  ]
}

resource "cloudflare_workers_route" "keys" {
  zone_id = var.platform_dns_zones["fredrir.com"]
  pattern = "keys.fredrir.com/*"
  script  = cloudflare_workers_script.keys.script_name
}

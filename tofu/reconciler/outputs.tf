output "reconciler_ipv4" {
  value = hcloud_primary_ip.reconciler.ip_address
}

output "reconciler_server_id" {
  value = hcloud_server.reconciler.id
}

output "verify_aws_access_key_id" {
  value = aws_iam_access_key.verify.id
}

output "verify_aws_secret_access_key" {
  value     = aws_iam_access_key.verify.secret
  sensitive = true
}

output "verify_cloudflare_api_token" {
  value     = cloudflare_account_token.verify.value
  sensitive = true
}

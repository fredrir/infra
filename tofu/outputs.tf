# ---- llunde-01 ----

output "ipv4" {
  description = "Public IPv4 of llunde-01"
  value       = hcloud_server.llunde_01.ipv4_address
}

output "ipv6" {
  description = "Public IPv6 network of llunde-01"
  value       = hcloud_server.llunde_01.ipv6_address
}

# ---- llunde-parser (pyparser) ----

output "server_ipv4" {
  description = "Public IPv4 of the pyparser server."
  value       = module.parser.ipv4_address
}

output "dataset_bucket" {
  description = "The pyparser dataset bucket."
  value       = data.aws_s3_bucket.dataset.bucket
}

output "dataset_access_policy_arn" {
  description = "IAM policy granting read/write to the dataset bucket (attached to the leploy user)."
  value       = aws_iam_policy.dataset_access.arn
}

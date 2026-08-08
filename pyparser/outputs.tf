output "dataset_bucket" {
  description = "The pyparser dataset bucket."
  value       = data.aws_s3_bucket.dataset.bucket
}

output "dataset_access_policy_arn" {
  description = "IAM policy granting read/write to the dataset bucket (attached to the leploy user)."
  value       = aws_iam_policy.dataset_access.arn
}

output "server_ipv4" {
  description = "Public IPv4 of the pyparser server."
  value       = module.server.ipv4_address
}

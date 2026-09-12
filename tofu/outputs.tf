output "control_05_ipv4" {
  value = hcloud_server.control_05.ipv4_address
}

output "control_05_ipv6_network" {
  value = hcloud_server.control_05.ipv6_address
}

output "worker_04_ipv4" {
  value = module.worker_04.ipv4_address
}

output "dataset_bucket" {
  description = "The pyparser dataset bucket."
  value       = data.aws_s3_bucket.dataset.bucket
}

output "dataset_access_policy_arn" {
  description = "IAM policy granting read/write to the dataset bucket (attached to the leploy user)."
  value       = aws_iam_policy.dataset_access.arn
}

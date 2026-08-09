variable "hcloud_token" {
  description = "Hetzner Cloud API token (Read & Write). Supply via TF_VAR_hcloud_token."
  type        = string
  sensitive   = true
}

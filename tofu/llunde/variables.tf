variable "hcloud_token" {
  description = "Hetzner Cloud API token (Read & Write). Supplied via TF_VAR_hcloud_token; same project as pyparser."
  type        = string
  sensitive   = true
}

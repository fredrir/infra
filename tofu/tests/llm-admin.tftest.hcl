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
  reconciler_ipv4         = "203.0.113.10"
  hcloud_token            = "mock"
  dataset_bucket_name     = "dataset"
  platform_mail_recipient = "operator@example.net"
  platform_mail = {
    domain  = "example.com"
    zone_id = "0123456789abcdef0123456789abcdef"
    sender  = "alerts@example.com"
  }
}

run "llm_admin_requires_access" {
  command = plan

  plan_options {
    target = [cloudflare_zero_trust_access_application.llm_admin]
  }

  assert {
    condition = (
      cloudflare_zero_trust_access_application.llm_admin.domain == "llm-admin.fredrir.com" &&
      cloudflare_zero_trust_access_application.llm_admin.type == "self_hosted" &&
      toset(cloudflare_zero_trust_access_application.llm_admin.allowed_idps) == toset(["ae4b9d76-c28a-44a9-9056-b6da60405ccb"]) &&
      cloudflare_zero_trust_access_policy.llm_admin.decision == "allow" &&
      toset([for rule in cloudflare_zero_trust_access_policy.llm_admin.include : rule.email.email]) == toset(["fhansteen@gmail.com"])
    )
    error_message = "Only the operator's GitHub identity may reach llm-admin."
  }
}

mock_provider "aws" {
  mock_resource "aws_sesv2_email_identity" {
    defaults = {
      arn                         = "arn:aws:ses:eu-north-1:123456789012:identity/example.com"
      verified_for_sending_status = false
      dkim_signing_attributes     = { tokens = ["third-selector", "first-selector", "second-selector"] }
    }
  }
  mock_resource "aws_iam_policy" {
    defaults = { arn = "arn:aws:iam::123456789012:policy/platform/test-alerts-send" }
  }
}

mock_provider "cloudflare" {}

variables {
  domain               = "example.com"
  zone_id              = "0123456789abcdef0123456789abcdef"
  sender               = "alerts@example.com"
  recipient            = "operator@example.net"
  smtp_user_name       = "test-alerts"
  permissions_boundary = "arn:aws:iam::123456789012:policy/boundary/test-workload-boundary"
}

run "scoped_smtp_identity" {
  command = plan
  module { source = "./modules/platform-mail" }

  assert {
    condition = (
      length(jsondecode(aws_iam_policy.sender.policy).Statement) == 1 &&
      jsondecode(aws_iam_policy.sender.policy).Statement[0].Action == ["ses:SendRawEmail"] &&
      jsondecode(aws_iam_policy.sender.policy).Statement[0].Resource == [
        "arn:aws:ses:eu-north-1:123456789012:identity/example.com",
        "arn:aws:ses:eu-north-1:123456789012:identity/operator@example.net",
      ]
    )
    error_message = "SMTP authority must contain only the sending domain and exact verified recipient identity in the same account and region."
  }
  assert {
    condition = (
      jsondecode(aws_iam_policy.sender.policy).Statement[0].Condition.StringEquals["ses:FromAddress"] == "alerts@example.com" &&
      jsondecode(aws_iam_policy.sender.policy).Statement[0].Condition["ForAllValues:StringEquals"]["ses:Recipients"] == ["operator@example.net"] &&
      jsondecode(aws_iam_policy.sender.policy).Statement[0].Condition.Null["ses:Recipients"] == "false"
    )
    error_message = "Every recipient and the From address must match exactly."
  }
  assert {
    condition = (
      aws_iam_user.sender.permissions_boundary == var.permissions_boundary &&
      aws_iam_user.sender.permissions_boundary != aws_iam_policy.sender.arn &&
      aws_iam_user_policy_attachment.sender.policy_arn == aws_iam_policy.sender.arn &&
      aws_iam_user_policy_attachment.sender.user == aws_iam_user.sender.name &&
      !aws_iam_user.sender.force_destroy
    )
    error_message = "The sender grant must stay separate from the fixed workload boundary, with access keys protected from forced deletion."
  }
  assert {
    condition = (
      length(cloudflare_dns_record.dkim) == 3 &&
      alltrue([for record in cloudflare_dns_record.dkim : record.type == "CNAME" && !record.proxied && record.zone_id == var.zone_id]) &&
      cloudflare_dns_record.dkim["first"].name == "first-selector._domainkey.example.com" &&
      cloudflare_dns_record.dkim["first"].content == "first-selector.dkim.amazonses.com" &&
      aws_sesv2_email_identity.sender.dkim_signing_attributes[0].next_signing_key_length == "RSA_2048_BIT" &&
      !output.sender.verified_for_sending
    )
    error_message = "Easy DKIM requires three DNS-only selectors, strong AWS-managed signing keys and truthful verification status."
  }
}

run "reject_sender_on_another_domain" {
  command = plan
  module { source = "./modules/platform-mail" }
  variables { sender = "alerts@example.net" }
  expect_failures = [var.sender]
}

run "reject_sender_header_injection" {
  command = plan
  module { source = "./modules/platform-mail" }
  variables { sender = "alerts@example.com\nBcc:other@example.net" }
  expect_failures = [var.sender]
}

run "reject_wildcard_recipient" {
  command = plan
  module { source = "./modules/platform-mail" }
  variables { recipient = "*@example.net" }
  expect_failures = [var.recipient]
}

run "reject_recipient_list" {
  command = plan
  module { source = "./modules/platform-mail" }
  variables { recipient = "operator@example.net,other@example.net" }
  expect_failures = [var.recipient]
}

run "reject_invalid_zone_id" {
  command = plan
  module { source = "./modules/platform-mail" }
  variables { zone_id = "unknown" }
  expect_failures = [var.zone_id]
}

run "reject_wildcard_domain" {
  command = plan
  module { source = "./modules/platform-mail" }
  variables {
    domain = "*.example.com"
    sender = "alerts@*.example.com"
  }
  expect_failures = [var.domain]
}

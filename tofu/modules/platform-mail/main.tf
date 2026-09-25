locals {
  tags = {
    Project   = "fredrir-platform"
    ManagedBy = "terraform"
    Service   = "alerts"
  }
  dkim_slots = { first = 0, second = 1, third = 2 }
  sending_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = ["ses:SendRawEmail"]
      Resource = [
        aws_sesv2_email_identity.sender.arn,
        replace(aws_sesv2_email_identity.sender.arn, "/${var.domain}", "/${var.recipient}"),
      ]
      Condition = {
        StringEquals                = { "ses:FromAddress" = var.sender }
        "ForAllValues:StringEquals" = { "ses:Recipients" = [var.recipient] }
        Null                        = { "ses:Recipients" = "false" }
      }
    }]
  })
}

resource "aws_sesv2_email_identity" "sender" {
  email_identity = var.domain

  dkim_signing_attributes {
    next_signing_key_length = "RSA_2048_BIT"
  }

  tags = local.tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "cloudflare_dns_record" "dkim" {
  for_each = local.dkim_slots

  zone_id = var.zone_id
  name    = "${sort(aws_sesv2_email_identity.sender.dkim_signing_attributes[0].tokens)[each.value]}._domainkey.${var.domain}"
  type    = "CNAME"
  content = "${sort(aws_sesv2_email_identity.sender.dkim_signing_attributes[0].tokens)[each.value]}.dkim.amazonses.com"
  ttl     = 300
  proxied = false

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_iam_policy" "sender" {
  name   = "${var.smtp_user_name}-send"
  path   = "/platform/"
  policy = local.sending_policy
  tags   = local.tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_iam_user" "sender" {
  name                 = var.smtp_user_name
  path                 = "/platform/"
  permissions_boundary = var.permissions_boundary
  force_destroy        = false
  tags                 = local.tags

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_iam_user_policy_attachment" "sender" {
  user       = aws_iam_user.sender.name
  policy_arn = aws_iam_policy.sender.arn
}

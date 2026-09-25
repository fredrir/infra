output "sender" {
  value = {
    domain               = aws_sesv2_email_identity.sender.email_identity
    identity_arn         = aws_sesv2_email_identity.sender.arn
    verified_for_sending = aws_sesv2_email_identity.sender.verified_for_sending_status
    iam_user_name        = aws_iam_user.sender.name
    iam_user_arn         = aws_iam_user.sender.arn
    iam_user_boundary    = aws_iam_user.sender.permissions_boundary
    from                 = var.sender
    dkim_records = {
      for name, record in cloudflare_dns_record.dkim : name => {
        id      = record.id
        name    = record.name
        type    = record.type
        content = record.content
      }
    }
  }
}

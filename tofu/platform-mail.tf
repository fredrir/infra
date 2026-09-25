variable "platform_mail" {
  type = object({
    domain         = string
    zone_id        = string
    sender         = string
    smtp_user_name = optional(string, "fredrir-platform-alerts-smtp")
  })
  default = null
}

variable "platform_mail_recipient" {
  type      = string
  default   = null
  sensitive = true

  validation {
    condition     = var.platform_mail == null || var.platform_mail_recipient != null
    error_message = "An enabled mail identity requires the private recipient input."
  }
}

module "platform_mail" {
  count  = var.platform_mail == null ? 0 : 1
  source = "./modules/platform-mail"

  domain               = var.platform_mail.domain
  zone_id              = var.platform_mail.zone_id
  sender               = var.platform_mail.sender
  recipient            = var.platform_mail_recipient
  smtp_user_name       = var.platform_mail.smtp_user_name
  permissions_boundary = aws_iam_policy.workload_boundary.arn
}

output "platform_mail" {
  value = var.platform_mail == null ? null : module.platform_mail[0].sender
}

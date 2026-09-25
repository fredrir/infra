variable "domain" {
  type = string

  validation {
    condition     = length(var.domain) <= 253 && can(regex("^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z]{2,63}$", var.domain))
    error_message = "Use an owned lowercase DNS domain without a wildcard or trailing dot."
  }
}

variable "zone_id" {
  type = string

  validation {
    condition     = can(regex("^[a-f0-9]{32}$", var.zone_id))
    error_message = "Use the verified existing Cloudflare zone ID for the sending domain."
  }
}

variable "sender" {
  type = string

  validation {
    condition = (
      length(var.sender) <= 254 &&
      can(regex("^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}@[a-z0-9.-]+$", var.sender)) &&
      endswith(var.sender, "@${var.domain}")
    )
    error_message = "Use one bare sender mailbox on the exact sending domain."
  }
}

variable "recipient" {
  type      = string
  sensitive = true
  nullable  = false

  validation {
    condition = (
      length(var.recipient) <= 254 &&
      can(regex("^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}@([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z]{2,63}$", var.recipient))
    )
    error_message = "Use one exact bare recipient mailbox without wildcard or display name."
  }
}

variable "smtp_user_name" {
  type    = string
  default = "fredrir-platform-alerts-smtp"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,59}$", var.smtp_user_name))
    error_message = "Use a stable lowercase IAM user name of 3 to 60 characters."
  }
}

variable "permissions_boundary" {
  type     = string
  nullable = false
}

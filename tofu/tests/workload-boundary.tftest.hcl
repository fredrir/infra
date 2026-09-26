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

override_resource {
  target = aws_iam_policy.workload_boundary
  values = { arn = "arn:aws:iam::123456789012:policy/boundary/infra-workload-boundary" }
}

variables {
  hcloud_token            = "mock"
  dataset_bucket_name     = "dataset"
  platform_mail_recipient = "operator@example.net"
  platform_mail = {
    domain  = "example.com"
    zone_id = "0123456789abcdef0123456789abcdef"
    sender  = "alerts@example.com"
  }
}

run "workload_boundary" {
  command = plan

  plan_options {
    target = [aws_iam_policy.reconciliation, aws_iam_user.platform_backup, aws_iam_user_policy_attachment.platform_backup, aws_iam_user.dataset, module.platform_mail, aws_s3_bucket_lifecycle_configuration.dataset]
  }

  assert {
    condition = (
      aws_iam_policy.workload_boundary.path == "/boundary/" &&
      alltrue([
        for statement in jsondecode(aws_iam_policy.workload_boundary.policy).Statement : statement.Effect == "Deny" || (
          alltrue([for action in statement.Action : can(regex("^(s3|ses):[A-Za-z]+$", action))]) &&
          alltrue([for resource in statement.Resource : startswith(resource, "arn:aws:s3:::dataset") || startswith(resource, "arn:aws:ses:eu-north-1:123456789012:identity/")])
        )
      ]) &&
      contains([
        for statement in jsondecode(aws_iam_policy.workload_boundary.policy).Statement : statement.Resource if statement.Effect == "Deny" && statement.Action == ["s3:*"]
      ], ["arn:aws:s3:::dataset/tofu-state/*", "arn:aws:s3:::dataset/tf-state-backups/*", "arn:aws:s3:::dataset/reconciliation/*"])
    )
    error_message = "The workload boundary must grant only named dataset S3 and SES actions and keep OpenTofu state out of reach."
  }

  assert {
    condition = (
      alltrue([
        for user in concat(values(aws_iam_user.platform_backup), [aws_iam_user.dataset]) :
        user.permissions_boundary == aws_iam_policy.workload_boundary.arn
      ]) &&
      output.platform_mail.iam_user_boundary == aws_iam_policy.workload_boundary.arn &&
      alltrue([for attachment in aws_iam_user_policy_attachment.platform_backup : attachment.policy_arn != aws_iam_policy.workload_boundary.arn]) &&
      alltrue([for user in aws_iam_user.reconciliation : user.path == "/automation/"])
    )
    error_message = "Every non-automation user must carry the fixed workload boundary separately from its own grant."
  }

  assert {
    condition = alltrue([
      for identity, policy in aws_iam_policy.reconciliation : anytrue([
        for statement in jsondecode(policy.policy).Statement :
        statement.Effect == "Allow" && contains(statement.Action, "iam:GetPolicyVersion") && contains(statement.Resource, aws_iam_policy.workload_boundary.arn)
      ])
    ])
    error_message = "Reconciliation identities must be able to read the workload boundary."
  }

  assert {
    condition = alltrue([
      for statement in jsondecode(aws_iam_policy.reconciliation["apply"].policy).Statement : statement.Effect == "Deny" || alltrue([
        for resource in statement.Resource : !can(regex("^${replace(resource, "*", ".*")}$", aws_iam_policy.workload_boundary.arn)) || alltrue([
          for action in statement.Action : can(regex("^iam:(Get|List)", action))
        ])
      ])
    ])
    error_message = "The apply identity must not be granted write access to the workload boundary."
  }

  assert {
    condition = alltrue([
      for statement in jsondecode(aws_iam_policy.reconciliation["apply"].policy).Statement : !contains(statement.Action, "iam:AttachUserPolicy") || statement.Effect == "Deny" || (
        !anytrue([for policy in statement.Condition.ArnLike["iam:PolicyARN"] : can(regex("^${replace(policy, "*", ".*")}$", aws_iam_policy.workload_boundary.arn))])
      )
    ])
    error_message = "The apply identity must attach only managed workload grants, never the boundary."
  }

  assert {
    condition = anytrue([
      for statement in jsondecode(aws_iam_policy.reconciliation["apply"].policy).Statement :
      try(statement.Sid, "") == "FixedWorkloadBoundary" && statement.Effect == "Deny" &&
      statement.Resource == [aws_iam_policy.workload_boundary.arn] &&
      length(setsubtract(["iam:CreatePolicy", "iam:CreatePolicyVersion", "iam:SetDefaultPolicyVersion", "iam:DeletePolicy", "iam:DeletePolicyVersion", "iam:TagPolicy", "iam:UntagPolicy"], statement.Action)) == 0
    ])
    error_message = "The apply identity must be denied every write to the workload boundary policy."
  }

  assert {
    condition = anytrue([
      for statement in jsondecode(aws_iam_policy.reconciliation["apply"].policy).Statement :
      try(statement.Sid, "") == "KeepWorkloadBoundary" && statement.Effect == "Deny" &&
      statement.Resource == ["*"] && statement.Action == ["iam:DeleteUserPermissionsBoundary"]
    ])
    error_message = "The apply identity must be denied removing any user permissions boundary."
  }

  assert {
    condition = anytrue([
      for statement in jsondecode(aws_iam_policy.reconciliation["apply"].policy).Statement :
      try(statement.Sid, "") == "RequireWorkloadBoundary" && statement.Effect == "Deny" && statement.Resource == ["*"] &&
      length(setsubtract(["iam:CreateUser", "iam:PutUserPermissionsBoundary", "iam:AttachUserPolicy"], statement.Action)) == 0 &&
      statement.Condition == { ArnNotEquals = { "iam:PermissionsBoundary" = aws_iam_policy.workload_boundary.arn } }
    ])
    error_message = "The apply identity must create, rebound or grant users only under exactly the workload boundary."
  }

  assert {
    condition = alltrue([
      for identity, policy in aws_iam_policy.reconciliation : anytrue([
        for statement in jsondecode(policy.policy).Statement : statement.Effect == "Allow" && contains(statement.Action, "s3:GetLifecycleConfiguration")
        ]) && !anytrue([
        for statement in jsondecode(policy.policy).Statement : statement.Effect == "Allow" && contains(statement.Action, "s3:PutLifecycleConfiguration")
      ])
    ])
    error_message = "Reconciliation identities must read but never rewrite bucket lifecycle rules."
  }

  assert {
    condition = alltrue([
      for rule in aws_s3_bucket_lifecycle_configuration.dataset.rule : length(rule.filter) == 1 && (
        startswith(rule.filter[0].prefix, "restic/llunde-") ||
        (rule.filter[0].prefix == "reconciliation/production/runs/" && rule.expiration[0].days == 30 && length(rule.noncurrent_version_expiration) == 0) ||
        (rule.filter[0].prefix == "reconciliation/production/" && rule.noncurrent_version_expiration[0].noncurrent_days == 7 && rule.expiration[0].expired_object_delete_marker && coalesce(rule.expiration[0].days, 0) == 0)
      )
    ])
    error_message = "Lifecycle expiration must stay scoped to the retired host repositories, reconciliation run reports and superseded reconciliation state."
  }
}

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
    defaults = { arn = "arn:aws:iam::123456789012:policy/automation/reconciliation" }
  }
  mock_resource "aws_iam_user" {
    defaults = { arn = "arn:aws:iam::123456789012:user/automation/reconciliation" }
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

run "no_reconciler_declared" {
  command = plan

  plan_options {
    target = [aws_iam_policy.reconciliation, aws_iam_user.reconciliation, aws_iam_user_policy_attachment.reconciliation]
  }

  assert {
    condition     = keys(aws_iam_user.reconciliation) == ["apply", "plan"] && keys(aws_iam_policy.reconciliation) == ["apply", "plan"] && keys(aws_iam_user_policy_attachment.reconciliation) == ["apply", "plan"]
    error_message = "Without a declared reconciler only the plan and apply identities exist."
  }

  assert {
    condition = (
      length(jsondecode(aws_iam_policy.reconciliation["plan"].policy).Statement) == 5 &&
      length(jsondecode(aws_iam_policy.reconciliation["apply"].policy).Statement) == 14 &&
      alltrue([
        for policy in values(aws_iam_policy.reconciliation) : !anytrue([
          for statement in jsondecode(policy.policy).Statement :
          try(statement.Sid, "") == "RequireReconcilerAddress" || contains(statement.Resource, "arn:aws:s3:::dataset/reconciliation/production/runs/*")
        ])
      ])
    )
    error_message = "Without a declared reconciler the plan and apply policies keep their statements."
  }
}

run "reconciler_declared" {
  command = plan

  variables {
    reconciler_ipv4 = "203.0.113.10"
  }

  plan_options {
    target = [aws_iam_policy.reconciliation, aws_iam_user.reconciliation, aws_iam_user_policy_attachment.reconciliation]
  }

  assert {
    condition = (
      keys(aws_iam_user.reconciliation) == ["apply", "plan", "verify"] &&
      aws_iam_user.reconciliation["verify"].name == "infra-reconciliation-verify" &&
      aws_iam_user.reconciliation["verify"].path == "/automation/" &&
      aws_iam_policy.reconciliation["verify"].path == "/automation/" &&
      aws_iam_user_policy_attachment.reconciliation["verify"].user == "infra-reconciliation-verify"
    )
    error_message = "A declared reconciler adds the automation verify identity with its own attached policy."
  }

  assert {
    condition = (
      slice(jsondecode(aws_iam_policy.reconciliation["verify"].policy).Statement, 0, 4) == [
        for statement in jsondecode(aws_iam_policy.reconciliation["plan"].policy).Statement : statement
        if !contains(statement.Resource, "arn:aws:s3:::dataset/tofu-state/infra.tfstate.tflock")
      ] &&
      length(jsondecode(aws_iam_policy.reconciliation["verify"].policy).Statement) == 6
    )
    error_message = "The verify policy is the plan policy without the OpenTofu lock, plus the run report grant and the address condition."
  }

  assert {
    condition = anytrue([
      for statement in jsondecode(aws_iam_policy.reconciliation["verify"].policy).Statement :
      statement.Effect == "Allow" && statement.Action == ["s3:PutObject"] && statement.Resource == ["arn:aws:s3:::dataset/reconciliation/production/runs/*"]
    ])
    error_message = "The verify identity must upload run reports."
  }

  assert {
    condition = anytrue([
      for statement in jsondecode(aws_iam_policy.reconciliation["verify"].policy).Statement :
      try(statement.Sid, "") == "RequireReconcilerAddress" && statement.Effect == "Deny" && statement.Action == ["*"] && statement.Resource == ["*"] &&
      statement.Condition == { NotIpAddress = { "aws:SourceIp" = ["203.0.113.10/32"] }, Bool = { "aws:ViaAWSService" = "false" } }
    ])
    error_message = "The verify identity must be denied every request from another address."
  }

  assert {
    condition = alltrue([
      for statement in jsondecode(aws_iam_policy.reconciliation["verify"].policy).Statement : statement.Effect == "Deny" || alltrue([
        for action in statement.Action : can(regex("^(s3|iam|ses):(Get|List)", action))
      ]) || (statement.Action == ["s3:PutObject"] && statement.Resource == ["arn:aws:s3:::dataset/reconciliation/production/runs/*"])
    ])
    error_message = "The verify identity may write only run reports."
  }

  assert {
    condition = !anytrue([
      for statement in jsondecode(aws_iam_policy.reconciliation["verify"].policy).Statement :
      statement.Effect == "Allow" && anytrue([for resource in statement.Resource : startswith(resource, "arn:aws:s3:::dataset/tofu-state/")]) && !alltrue([for action in statement.Action : action == "s3:GetObject"])
    ])
    error_message = "The verify identity compares OpenTofu without its lock, so it must not write OpenTofu state or the lock."
  }

  assert {
    condition = alltrue([
      for identity in ["plan", "apply"] : anytrue([
        for statement in jsondecode(aws_iam_policy.reconciliation[identity].policy).Statement :
        statement.Action == ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"] && statement.Resource == ["arn:aws:s3:::dataset/tofu-state/infra.tfstate.tflock"]
      ])
    ])
    error_message = "Plans and applies keep the OpenTofu lock."
  }

  assert {
    condition = alltrue([
      for identity in ["plan", "apply"] : !anytrue([
        for statement in jsondecode(aws_iam_policy.reconciliation[identity].policy).Statement :
        try(statement.Sid, "") == "RequireReconcilerAddress" || contains(statement.Resource, "arn:aws:s3:::dataset/reconciliation/production/runs/*")
      ])
    ])
    error_message = "Only the verify identity carries the reconciler grants."
  }

  assert {
    condition = alltrue([
      for policy in values(aws_iam_policy.reconciliation) : anytrue([
        for statement in jsondecode(policy.policy).Statement :
        contains(statement.Action, "iam:GetUser") && contains(statement.Resource, "arn:aws:iam::123456789012:policy/automation/infra-reconciliation-verify")
      ])
    ])
    error_message = "Every reconciliation identity must read the verify identity's policy for drift comparison."
  }
}

run "reject_address_range" {
  command = plan

  variables {
    reconciler_ipv4 = "203.0.113.0/24"
  }

  plan_options {
    target = [aws_iam_policy.reconciliation]
  }

  expect_failures = [var.reconciler_ipv4]
}

run "reject_private_address" {
  command = plan

  variables {
    reconciler_ipv4 = "10.60.0.11"
  }

  plan_options {
    target = [aws_iam_policy.reconciliation]
  }

  expect_failures = [var.reconciler_ipv4]
}

run "reject_tailnet_address" {
  command = plan

  variables {
    reconciler_ipv4 = "100.64.0.11"
  }

  plan_options {
    target = [aws_iam_policy.reconciliation]
  }

  expect_failures = [var.reconciler_ipv4]
}

run "reject_non_address" {
  command = plan

  variables {
    reconciler_ipv4 = "reconciler.example.com"
  }

  plan_options {
    target = [aws_iam_policy.reconciliation]
  }

  expect_failures = [var.reconciler_ipv4]
}

run "reject_ipv6_address" {
  command = plan

  variables {
    reconciler_ipv4 = "2001:db8::10"
  }

  plan_options {
    target = [aws_iam_policy.reconciliation]
  }

  expect_failures = [var.reconciler_ipv4]
}

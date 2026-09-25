data "aws_caller_identity" "reconciliation" {}

locals {
  reconciliation_identities = toset(["plan", "apply"])
  reconciliation_iam        = "arn:aws:iam::${data.aws_caller_identity.reconciliation.account_id}"
  reconciliation_users      = ["${local.reconciliation_iam}:user/platform/*"]
  reconciliation_policies   = [for policy in ["platform/*", "pyparser-dataset-*"] : "${local.reconciliation_iam}:policy/${policy}"]
  iam_policy_writes = [
    "iam:CreatePolicy", "iam:DeletePolicy", "iam:CreatePolicyVersion", "iam:DeletePolicyVersion",
    "iam:SetDefaultPolicyVersion", "iam:TagPolicy", "iam:UntagPolicy",
  ]
}

resource "aws_iam_policy" "workload_boundary" {
  name = "infra-workload-boundary"
  path = "/boundary/"
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Effect   = "Allow"
        Action   = ["s3:ListBucket", "s3:GetBucketLocation"]
        Resource = [data.aws_s3_bucket.dataset.arn]
      },
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
        Resource = ["${data.aws_s3_bucket.dataset.arn}/*"]
      },
      {
        Effect   = "Deny"
        Action   = ["s3:*"]
        Resource = [for prefix in ["tofu-state", "tf-state-backups", "reconciliation"] : "${data.aws_s3_bucket.dataset.arn}/${prefix}/*"]
      },
      ], var.platform_mail == null ? [] : [
      {
        Effect    = "Allow"
        Action    = ["ses:SendRawEmail"]
        Resource  = ["arn:aws:ses:${var.region}:${data.aws_caller_identity.reconciliation.account_id}:identity/*"]
        Condition = { StringEquals = { "ses:FromAddress" = var.platform_mail.sender } }
      },
    ])
  })

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_iam_user" "reconciliation" {
  for_each = local.reconciliation_identities
  name     = "infra-reconciliation-${each.key}"
  path     = "/automation/"
}

resource "aws_iam_policy" "reconciliation" {
  for_each = local.reconciliation_identities
  name     = "infra-reconciliation-${each.key}"
  path     = "/automation/"
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Effect   = "Allow"
        Action   = ["s3:ListBucket", "s3:GetBucketLocation", "s3:GetBucketVersioning", "s3:GetEncryptionConfiguration", "s3:GetBucketPublicAccessBlock", "s3:GetLifecycleConfiguration"]
        Resource = [data.aws_s3_bucket.dataset.arn]
      },
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject"]
        Resource = ["${data.aws_s3_bucket.dataset.arn}/tofu-state/infra.tfstate", "${data.aws_s3_bucket.dataset.arn}/reconciliation/production/*"]
      },
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
        Resource = ["${data.aws_s3_bucket.dataset.arn}/tofu-state/infra.tfstate.tflock"]
      },
      {
        Effect = "Allow"
        Action = [
          "iam:GetUser", "iam:GetPolicy", "iam:GetPolicyVersion", "iam:ListPolicyVersions",
          "iam:ListAttachedUserPolicies", "iam:ListUserPolicies", "iam:ListAccessKeys",
          "iam:GetUserPolicy", "iam:ListUserTags", "iam:ListPolicyTags",
        ]
        Resource = concat(local.reconciliation_users, local.reconciliation_policies, [aws_iam_policy.workload_boundary.arn], [for user in aws_iam_user.reconciliation : user.arn], [
          for identity in local.reconciliation_identities : "${local.reconciliation_iam}:policy/automation/infra-reconciliation-${identity}"
        ])
      },
      {
        Effect   = "Allow"
        Action   = ["ses:GetEmailIdentity", "ses:GetEmailIdentityPolicies", "ses:ListTagsForResource"]
        Resource = ["arn:aws:ses:${var.region}:${data.aws_caller_identity.reconciliation.account_id}:identity/fredrir.com"]
      }
      ], [for statement in [
        {
          Effect   = "Allow"
          Action   = ["s3:PutObject", "s3:DeleteObject"]
          Resource = ["${data.aws_s3_bucket.dataset.arn}/tofu-state/infra.tfstate", "${data.aws_s3_bucket.dataset.arn}/reconciliation/production/*"]
        },
        {
          Effect   = "Allow"
          Action   = ["s3:PutBucketVersioning", "s3:PutEncryptionConfiguration", "s3:PutBucketPublicAccessBlock"]
          Resource = [data.aws_s3_bucket.dataset.arn]
        },
        {
          Effect = "Allow"
          Action = [
            "iam:CreateUser", "iam:UpdateUser", "iam:DeleteUser", "iam:TagUser", "iam:UntagUser",
            "iam:PutUserPermissionsBoundary", "iam:DetachUserPolicy",
          ]
          Resource = local.reconciliation_users
        },
        {
          Effect    = "Allow"
          Action    = ["iam:AttachUserPolicy"]
          Resource  = local.reconciliation_users
          Condition = { ArnLike = { "iam:PolicyARN" = local.reconciliation_policies } }
        },
        {
          Effect   = "Allow"
          Action   = local.iam_policy_writes
          Resource = local.reconciliation_policies
        },
        {
          Effect = "Allow"
          Action = [
            "ses:CreateEmailIdentity", "ses:DeleteEmailIdentity", "ses:PutEmailIdentityDkimAttributes",
            "ses:PutEmailIdentityDkimSigningAttributes", "ses:PutEmailIdentityFeedbackAttributes",
            "ses:PutEmailIdentityMailFromAttributes", "ses:TagResource", "ses:UntagResource",
          ]
          Resource = ["arn:aws:ses:${var.region}:${data.aws_caller_identity.reconciliation.account_id}:identity/fredrir.com"]
        },
        {
          Sid       = "RequireWorkloadBoundary"
          Effect    = "Deny"
          Action    = ["iam:CreateUser", "iam:PutUserPermissionsBoundary", "iam:AttachUserPolicy"]
          Resource  = ["*"]
          Condition = { ArnNotEquals = { "iam:PermissionsBoundary" = aws_iam_policy.workload_boundary.arn } }
        },
        {
          Sid      = "KeepWorkloadBoundary"
          Effect   = "Deny"
          Action   = ["iam:DeleteUserPermissionsBoundary"]
          Resource = ["*"]
        },
        {
          Sid      = "FixedWorkloadBoundary"
          Effect   = "Deny"
          Action   = local.iam_policy_writes
          Resource = [aws_iam_policy.workload_boundary.arn]
        },
    ] : statement if each.key == "apply"])
  })
}

resource "aws_iam_user_policy_attachment" "reconciliation" {
  for_each   = local.reconciliation_identities
  user       = aws_iam_user.reconciliation[each.key].name
  policy_arn = aws_iam_policy.reconciliation[each.key].arn
}

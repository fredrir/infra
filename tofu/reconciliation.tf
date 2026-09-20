data "aws_caller_identity" "reconciliation" {}

locals {
  reconciliation_identities = toset(["plan", "apply"])
  reconciliation_iam_resources = concat([
    for user in ["leploy", "restic-llunde-01", "restic-llunde-parser"] :
    "arn:aws:iam::${data.aws_caller_identity.reconciliation.account_id}:user/${user}"
    ], [
    "arn:aws:iam::${data.aws_caller_identity.reconciliation.account_id}:user/platform/*",
    "arn:aws:iam::${data.aws_caller_identity.reconciliation.account_id}:policy/platform/*",
    "arn:aws:iam::${data.aws_caller_identity.reconciliation.account_id}:policy/pyparser-dataset-*",
    "arn:aws:iam::${data.aws_caller_identity.reconciliation.account_id}:policy/restic-*",
  ])
}

resource "aws_iam_user" "reconciliation" {
  for_each = local.reconciliation_identities
  name     = "infra-reconciliation-${each.key}"
  path     = "/automation/"
}

resource "aws_iam_user_policy" "reconciliation" {
  for_each = local.reconciliation_identities
  name     = "managed-infrastructure"
  user     = aws_iam_user.reconciliation[each.key].name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Effect   = "Allow"
        Action   = ["s3:ListBucket", "s3:GetBucketLocation", "s3:GetBucketVersioning", "s3:GetEncryptionConfiguration", "s3:GetBucketPublicAccessBlock"]
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
        Resource = concat(local.reconciliation_iam_resources, [for user in aws_iam_user.reconciliation : user.arn])
      },
      {
        Effect   = "Allow"
        Action   = ["ses:GetEmailIdentity", "ses:GetEmailIdentityPolicies", "ses:ListTagsForResource"]
        Resource = ["arn:aws:ses:${var.region}:${data.aws_caller_identity.reconciliation.account_id}:identity/fredrir.com"]
      }
      ], each.key == "apply" ? [
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
          "iam:PutUserPermissionsBoundary", "iam:DeleteUserPermissionsBoundary",
          "iam:CreatePolicy", "iam:DeletePolicy", "iam:CreatePolicyVersion", "iam:DeletePolicyVersion",
          "iam:SetDefaultPolicyVersion", "iam:TagPolicy", "iam:UntagPolicy",
          "iam:AttachUserPolicy", "iam:DetachUserPolicy",
        ]
        Resource = local.reconciliation_iam_resources
      },
      {
        Effect = "Allow"
        Action = [
          "ses:CreateEmailIdentity", "ses:DeleteEmailIdentity", "ses:PutEmailIdentityDkimAttributes",
          "ses:PutEmailIdentityDkimSigningAttributes", "ses:PutEmailIdentityFeedbackAttributes",
          "ses:PutEmailIdentityMailFromAttributes", "ses:TagResource", "ses:UntagResource",
        ]
        Resource = ["arn:aws:ses:${var.region}:${data.aws_caller_identity.reconciliation.account_id}:identity/fredrir.com"]
      }
    ] : [])
  })
}

locals {
  platform_backup_projects = toset(["parser", "y", "portfolio", "control", "attic"])
}

data "aws_iam_policy_document" "platform_backup" {
  for_each = local.platform_backup_projects

  statement {
    sid       = "BucketLocation"
    actions   = ["s3:GetBucketLocation"]
    resources = [data.aws_s3_bucket.dataset.arn]
  }

  statement {
    sid       = "ListRepository"
    actions   = ["s3:ListBucket"]
    resources = [data.aws_s3_bucket.dataset.arn]
    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = ["restic/platform/${each.key}", "restic/platform/${each.key}/*"]
    }
  }

  statement {
    sid       = "RepositoryObjects"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
    resources = ["${data.aws_s3_bucket.dataset.arn}/restic/platform/${each.key}/*"]
  }

  statement {
    sid       = "RequireTLS"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [data.aws_s3_bucket.dataset.arn, "${data.aws_s3_bucket.dataset.arn}/*"]
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_iam_policy" "platform_backup" {
  for_each = local.platform_backup_projects
  name     = "platform-restic-${each.key}"
  path     = "/platform/"
  policy   = data.aws_iam_policy_document.platform_backup[each.key].json

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_iam_user" "platform_backup" {
  for_each             = local.platform_backup_projects
  name                 = "platform-restic-${each.key}"
  path                 = "/platform/"
  permissions_boundary = aws_iam_policy.platform_backup[each.key].arn
  force_destroy        = false

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_iam_user_policy_attachment" "platform_backup" {
  for_each   = local.platform_backup_projects
  user       = aws_iam_user.platform_backup[each.key].name
  policy_arn = aws_iam_policy.platform_backup[each.key].arn
}

locals {
  platform_backup_projects    = toset(["parser", "y", "portfolio", "control", "attic"])
  application_backup_projects = toset(["parser", "y", "portfolio"])
  backup_bucket_arns          = { for project in local.platform_backup_projects : project => contains(local.application_backup_projects, project) ? data.aws_s3_bucket.application_backups.arn : data.aws_s3_bucket.dataset.arn }
  backup_prefixes             = { for project in local.platform_backup_projects : project => contains(local.application_backup_projects, project) ? project : "restic/platform/${project}" }
}

data "aws_s3_bucket" "application_backups" {
  bucket = "${var.dataset_bucket_name}-backups"
}

resource "aws_s3_bucket_public_access_block" "application_backups" {
  bucket                  = data.aws_s3_bucket.application_backups.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "application_backups" {
  bucket = data.aws_s3_bucket.application_backups.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "application_backups" {
  bucket = data.aws_s3_bucket.application_backups.id
  rule {
    id     = "abort-incomplete-uploads"
    status = "Enabled"
    filter {}
    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }
}

data "aws_iam_policy_document" "platform_backup" {
  for_each = local.platform_backup_projects

  statement {
    sid       = "BucketLocation"
    actions   = ["s3:GetBucketLocation"]
    resources = [local.backup_bucket_arns[each.key]]
  }

  statement {
    sid       = "ListRepository"
    actions   = ["s3:ListBucket"]
    resources = [local.backup_bucket_arns[each.key]]
    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = [local.backup_prefixes[each.key], "${local.backup_prefixes[each.key]}/*"]
    }
  }

  statement {
    sid       = "RepositoryObjects"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
    resources = ["${local.backup_bucket_arns[each.key]}/${local.backup_prefixes[each.key]}/*"]
  }

  statement {
    sid       = "RequireTLS"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [local.backup_bucket_arns[each.key], "${local.backup_bucket_arns[each.key]}/*"]
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
  permissions_boundary = aws_iam_policy.workload_boundary.arn
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

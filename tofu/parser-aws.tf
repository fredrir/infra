# ---- S3 ----

data "aws_s3_bucket" "dataset" {
  bucket = var.dataset_bucket_name
}

resource "aws_s3_bucket_public_access_block" "dataset" {
  bucket                  = data.aws_s3_bucket.dataset.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "dataset" {
  bucket = data.aws_s3_bucket.dataset.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "dataset" {
  bucket = data.aws_s3_bucket.dataset.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "dataset" {
  bucket = data.aws_s3_bucket.dataset.id

  rule {
    id     = "expire-retired-host-restic"
    status = "Enabled"
    filter {
      prefix = "restic/llunde-"
    }
    expiration {
      date = "2026-12-25T00:00:00Z"
    }
    noncurrent_version_expiration {
      noncurrent_days = 90
    }
  }

  rule {
    id     = "remove-retired-host-restic-delete-markers"
    status = "Enabled"
    filter {
      prefix = "restic/llunde-"
    }
    expiration {
      expired_object_delete_marker = true
    }
  }
}

# ---- IAM ----

data "aws_iam_policy_document" "dataset_access" {
  statement {
    sid       = "ListDatasetBucket"
    actions   = ["s3:ListBucket", "s3:GetBucketLocation"]
    resources = [data.aws_s3_bucket.dataset.arn]
  }
  statement {
    sid       = "ReadWriteDatasetObjects"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["${data.aws_s3_bucket.dataset.arn}/*"]
  }

  # Deny fences
  statement {
    sid       = "DenyTofuState"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = ["${data.aws_s3_bucket.dataset.arn}/tofu-state/*"]
  }
  statement {
    sid    = "DenyPermanentDestroy"
    effect = "Deny"
    actions = [
      "s3:DeleteObjectVersion",
      "s3:PutLifecycleConfiguration",
      "s3:PutBucketVersioning",
    ]
    resources = [
      data.aws_s3_bucket.dataset.arn,
      "${data.aws_s3_bucket.dataset.arn}/*",
    ]
  }

  statement {
    sid       = "DenyRestic"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = ["${data.aws_s3_bucket.dataset.arn}/restic/*"]
  }
}

resource "aws_iam_policy" "dataset_access" {
  name_prefix = "pyparser-dataset-"
  description = "Read/write access to the pyparser dataset bucket"
  policy      = data.aws_iam_policy_document.dataset_access.json
}

resource "aws_iam_user" "dataset" {
  name                 = "platform-dataset-parser"
  path                 = "/platform/"
  permissions_boundary = aws_iam_policy.workload_boundary.arn
  force_destroy        = false

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_iam_user_policy_attachment" "dataset" {
  user       = aws_iam_user.dataset.name
  policy_arn = aws_iam_policy.dataset_access.arn
}

resource "aws_iam_user" "leploy" {
  name                 = "leploy"
  permissions_boundary = aws_iam_policy.workload_boundary.arn
}

resource "aws_iam_user_policy_attachment" "leploy_dataset" {
  user       = aws_iam_user.leploy.name
  policy_arn = aws_iam_policy.dataset_access.arn
}

# ---- Per-host restic keys ----
locals {
  restic_hosts = toset(["llunde-01", "llunde-parser"])
}

data "aws_iam_policy_document" "restic_host" {
  for_each = local.restic_hosts

  statement {
    sid       = "ListOwnResticRepo"
    actions   = ["s3:ListBucket", "s3:GetBucketLocation"]
    resources = [data.aws_s3_bucket.dataset.arn]
    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = ["restic/${each.key}/*"]
    }
  }

  statement {
    sid       = "ReadWriteDeleteOwnResticObjects"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["${data.aws_s3_bucket.dataset.arn}/restic/${each.key}/*"]
  }
}

resource "aws_iam_user" "restic_host" {
  for_each             = local.restic_hosts
  name                 = "restic-${each.key}"
  permissions_boundary = aws_iam_policy.workload_boundary.arn
}

resource "aws_iam_policy" "restic_host" {
  for_each    = local.restic_hosts
  name_prefix = "restic-${each.key}-"
  description = "Scoped restic access to restic/${each.key}/* (own prefix; no cross-host, no version-delete)"
  policy      = data.aws_iam_policy_document.restic_host[each.key].json
}

resource "aws_iam_user_policy_attachment" "restic_host" {
  for_each   = local.restic_hosts
  user       = aws_iam_user.restic_host[each.key].name
  policy_arn = aws_iam_policy.restic_host[each.key].arn
}

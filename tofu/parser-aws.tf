# pyparser AWS: the S3 dataset bucket + IAM, all that is left after the move to
# Hetzner — compute and Postgres run on the Hetzner box (parser-server.tf,
# docs/pyparser/PROD.md).

# ---- S3: harden the EXISTING dataset bucket (reference, don't recreate) ----

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

# ---- IAM: least-privilege read/write policy for the dataset bucket ----

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

  # Deny fences on `leploy`, the pyparser dataset app's key. Neither touches its
  # legitimate use.
  #   1. tofu-state/* — a poisoned tfstate turns the manual `tofu apply` into
  #      the attacker's weapon (a lower-assurance principal than GitHub
  #      Actions, sitting on two public boxes).
  #   2. Permanent backup destruction — versioning (above) IS the recovery, so
  #      deny version-deletes and any lifecycle/versioning change defeating it.
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

  # Hosts use their own restic keys (below), so leploy needs no restic access:
  # a compromised dataset credential cannot reach either host's backups.
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

# The IAM user whose access key the Hetzner stack uses for S3 (AWS_ACCESS_KEY_ID
# in the host secrets.env). The KEY is deliberately unmanaged — its secret is
# unrecoverable and rotating it via tofu would break the live secrets.env.
# Manage it out-of-band (AWS console / CLI).
resource "aws_iam_user" "leploy" {
  name = "leploy"
}

resource "aws_iam_user_policy_attachment" "leploy_dataset" {
  user       = aws_iam_user.leploy.name
  policy_arn = aws_iam_policy.dataset_access.arn
}

# ---- Per-host restic keys ----
# Each key is scoped to its own restic/<host>/* prefix: full read/write/delete
# there so prune stays on the host, useless against the other host's prefix.
# DeleteObject but NOT DeleteObjectVersion, and the leploy denials above lock
# versioning on — so a compromised host leaves only recoverable delete-markers
# and backup *versions* always survive. Preferred over append-only + laptop
# prune, which moves prune off the host for no added protection. Access keys
# unmanaged here, same reason as leploy.

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

  # DeleteObject so prune runs on the host; NOT DeleteObjectVersion, so with
  # versioning locked on a compromised host can only leave recoverable
  # delete-markers, never permanently destroy a backup.
  statement {
    sid       = "ReadWriteDeleteOwnResticObjects"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["${data.aws_s3_bucket.dataset.arn}/restic/${each.key}/*"]
  }
}

resource "aws_iam_user" "restic_host" {
  for_each = local.restic_hosts
  name     = "restic-${each.key}"
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

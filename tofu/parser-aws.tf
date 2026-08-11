# ============================================================
# pyparser AWS — S3 dataset bucket + IAM. This is the only AWS infra left after
# the move to Hetzner: compute + Postgres run on the Hetzner box (see
# parser-server.tf and docs/pyparser/PROD.md).
# ============================================================

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

  # C1/C3 fence (phase-4 review, step 0.1a). `leploy` is today one shared key
  # used by the pyparser dataset app AND both hosts' restic (secrets/restic.yaml,
  # secrets/pyparser-restic.yaml) with whole-bucket access — so root on either
  # internet-facing host can reach the OpenTofu STATE and both hosts' backups.
  # Ahead of the full per-host restic split, deny the two catastrophic vectors:
  #   1. NEVER touch tofu-state/* — a poisoned tfstate turns the manual `tofu
  #      apply` into the attacker's weapon (the state is a lower-assurance
  #      principal than GitHub Actions, sitting on two public boxes).
  #   2. NEVER permanently destroy backups — bucket versioning (parser-aws.tf
  #      versioning block) IS the recovery, so deny version-deletes and any
  #      change to lifecycle/versioning that would defeat it.
  # Both are safe for leploy's legitimate use: the dataset app and restic never
  # touch tofu-state, never delete object *versions* (restic's DeleteObject
  # leaves recoverable delete-markers), and never reconfigure the bucket.
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

  # C1/C3 completion (0.1b): both hosts now use their own per-host restic keys
  # (restic-<host>, migrated + verified with live backups), so leploy — which is
  # ONLY the pyparser dataset app now — never needs restic access. Fence it off
  # so a compromise of the dataset credential can't read or touch either host's
  # backups.
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
# in the host secrets.env). The access KEY itself is deliberately NOT managed
# here — its secret is unrecoverable, and rotating it via Terraform would break
# the live secrets.env. Manage the key out-of-band (AWS console / CLI).
resource "aws_iam_user" "leploy" {
  name = "leploy"
}

resource "aws_iam_user_policy_attachment" "leploy_dataset" {
  user       = aws_iam_user.leploy.name
  policy_arn = aws_iam_policy.dataset_access.arn
}

# ---- Per-host restic keys (C1/C3 step 0.1b) ----
# Each host gets a key scoped to its OWN restic prefix (restic/<host>/*): full
# read/write/delete on its own backups — so prune stays on the host, unchanged —
# but it CANNOT touch the other host's backups (llunde-01's key is useless
# against restic/llunde-parser/*). Permanent destruction stays impossible: the
# key gets DeleteObject (recoverable S3 delete-markers only), NOT
# DeleteObjectVersion, and the leploy policy above already denies version-deletes
# + PutBucketVersioning with versioning on, so backup *versions* always survive.
# (Chosen over strict append-only + laptop prune: versioning already blocks
# permanent loss, so this keeps prune where it is with zero operational burden.)
#
# Rollout is staged: apply creates these users inert → create an access key for
# each out-of-band (like leploy) → rewire each host's sops restic-env in one
# rebuild → verify a real backup runs green → only THEN deny leploy on restic/*
# (separate PR). Access keys are deliberately NOT managed here (unrecoverable
# secret; rotating via tofu would break the live restic-env — same as leploy).

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

  # Full object access to its OWN repo, incl. DeleteObject so prune runs on the
  # host. NOT DeleteObjectVersion — versioning (locked by the leploy policy's
  # denials) keeps every version, so a compromised host can only create
  # recoverable delete-markers, never permanently destroy a backup.
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

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

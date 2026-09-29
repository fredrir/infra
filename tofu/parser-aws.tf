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
    id     = "expire-pruned-platform-restic"
    status = "Enabled"
    filter {
      prefix = "restic/platform/"
    }
    noncurrent_version_expiration {
      noncurrent_days = 90
    }
    expiration {
      expired_object_delete_marker = true
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

  rule {
    id     = "expire-reconciliation-runs"
    status = "Enabled"
    filter {
      prefix = "reconciliation/production/runs/"
    }
    expiration {
      days = 30
    }
  }

  rule {
    id     = "expire-noncurrent-reconciliation-state"
    status = "Enabled"
    filter {
      prefix = "reconciliation/production/"
    }
    noncurrent_version_expiration {
      noncurrent_days = 7
    }
    expiration {
      expired_object_delete_marker = true
    }
  }

  dynamic "rule" {
    for_each = toset(["assets", "convert", "extract", "files"])
    content {
      id     = "expire-retired-dataset-${rule.value}"
      status = "Enabled"
      filter {
        prefix = "${rule.value}/"
      }
      expiration {
        date = "2026-10-13T00:00:00Z"
      }
      noncurrent_version_expiration {
        noncurrent_days = 90
      }
    }
  }

  dynamic "rule" {
    for_each = toset(["assets", "convert", "extract", "files"])
    content {
      id     = "remove-retired-dataset-${rule.value}-delete-markers"
      status = "Enabled"
      filter {
        prefix = "${rule.value}/"
      }
      expiration {
        expired_object_delete_marker = true
      }
    }
  }
}

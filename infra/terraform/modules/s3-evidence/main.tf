# Evidence storage (PART 123/124, ADR-0007): three buckets with versioning,
# SSE-KMS, blocked public access, TLS-only policies and server access logging.
# audit-archive additionally has Object Lock in COMPLIANCE mode with a default
# retention, so no principal (root included) can delete or shorten a locked
# version before it expires. Lifecycle rules follow the CP_RETENTION_* classes.

locals {
  buckets = {
    raw-events        = { object_lock = false }
    provider-evidence = { object_lock = false }
    audit-archive     = { object_lock = true }
  }
  bucket_names     = { for k, v in local.buckets : k => "${var.name_prefix}-${k}-${var.bucket_suffix}" }
  logs_bucket_name = "${var.name_prefix}-access-logs-${var.bucket_suffix}"
}

# ---------------------------------------------------------------------------
# Server access log target
# ---------------------------------------------------------------------------

#trivy:ignore:AVD-AWS-0089 this bucket is the access-log target; S3 does not deliver access logs into a bucket that itself logs to another KMS bucket
resource "aws_s3_bucket" "logs" {
  bucket        = local.logs_bucket_name
  force_destroy = var.force_destroy
  tags          = merge(var.tags, { Name = local.logs_bucket_name })
}

resource "aws_s3_bucket_ownership_controls" "logs" {
  bucket = aws_s3_bucket.logs.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "logs" {
  bucket                  = aws_s3_bucket.logs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "logs" {
  bucket = aws_s3_bucket.logs.id
  versioning_configuration {
    status = "Enabled"
  }
}

#trivy:ignore:AVD-AWS-0132 S3 server access logging cannot deliver into an SSE-KMS bucket; the log target uses SSE-S3 and holds no evidence
resource "aws_s3_bucket_server_side_encryption_configuration" "logs" {
  bucket = aws_s3_bucket.logs.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "logs" {
  bucket = aws_s3_bucket.logs.id
  rule {
    id     = "expire-access-logs"
    status = "Enabled"
    filter {
      prefix = ""
    }
    expiration {
      days = var.access_log_retention_days
    }
    noncurrent_version_expiration {
      noncurrent_days = 30
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

data "aws_iam_policy_document" "logs" {
  statement {
    sid       = "S3ServerAccessLogsPolicy"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.logs.arn}/*"]
    principals {
      type        = "Service"
      identifiers = ["logging.s3.amazonaws.com"]
    }
    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = [for k, v in local.bucket_names : "arn:aws:s3:::${v}"]
    }
  }

  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.logs.arn, "${aws_s3_bucket.logs.arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "logs" {
  bucket = aws_s3_bucket.logs.id
  policy = data.aws_iam_policy_document.logs.json
}

# ---------------------------------------------------------------------------
# Evidence buckets
# ---------------------------------------------------------------------------

resource "aws_s3_bucket" "this" {
  for_each            = local.buckets
  bucket              = local.bucket_names[each.key]
  object_lock_enabled = each.value.object_lock
  force_destroy       = var.force_destroy && !each.value.object_lock
  tags                = merge(var.tags, { Name = local.bucket_names[each.key], EvidenceClass = each.key })
}

resource "aws_s3_bucket_ownership_controls" "this" {
  for_each = local.buckets
  bucket   = aws_s3_bucket.this[each.key].id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "this" {
  for_each                = local.buckets
  bucket                  = aws_s3_bucket.this[each.key].id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "this" {
  for_each = local.buckets
  bucket   = aws_s3_bucket.this[each.key].id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "this" {
  for_each = local.buckets
  bucket   = aws_s3_bucket.this[each.key].id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = var.kms_key_arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_logging" "this" {
  for_each      = local.buckets
  bucket        = aws_s3_bucket.this[each.key].id
  target_bucket = aws_s3_bucket.logs.id
  target_prefix = "${each.key}/"
}

# Object Lock (audit archive only): COMPLIANCE mode, default retention.
resource "aws_s3_bucket_object_lock_configuration" "audit" {
  bucket = aws_s3_bucket.this["audit-archive"].id
  rule {
    default_retention {
      mode = "COMPLIANCE"
      days = var.audit_object_lock_retention_days
    }
  }
  depends_on = [aws_s3_bucket_versioning.this]
}

# Lifecycle per retention class (CP_RETENTION_*).
resource "aws_s3_bucket_lifecycle_configuration" "raw" {
  bucket = aws_s3_bucket.this["raw-events"].id
  rule {
    id     = "raw-market-data-class"
    status = "Enabled"
    filter {
      prefix = ""
    }
    transition {
      days          = var.raw_glacier_after_days
      storage_class = "GLACIER_IR"
    }
    expiration {
      days = var.raw_market_data_retention_days
    }
    noncurrent_version_expiration {
      noncurrent_days = 30
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
  depends_on = [aws_s3_bucket_versioning.this]
}

resource "aws_s3_bucket_lifecycle_configuration" "evidence" {
  bucket = aws_s3_bucket.this["provider-evidence"].id
  rule {
    id     = "financial-record-class"
    status = "Enabled"
    filter {
      prefix = ""
    }
    transition {
      days          = var.evidence_glacier_after_days
      storage_class = "GLACIER"
    }
    expiration {
      days = var.financial_record_retention_days
    }
    noncurrent_version_expiration {
      noncurrent_days = var.financial_record_retention_days
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
  depends_on = [aws_s3_bucket_versioning.this]
}

resource "aws_s3_bucket_lifecycle_configuration" "audit" {
  bucket = aws_s3_bucket.this["audit-archive"].id
  rule {
    id     = "security-audit-class-no-expiry"
    status = "Enabled"
    filter {
      prefix = ""
    }
    transition {
      days          = var.audit_glacier_after_days
      storage_class = "GLACIER"
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
  depends_on = [aws_s3_bucket_versioning.this]
}

# Bucket policies: TLS only, KMS only, and only our key.
data "aws_iam_policy_document" "bucket" {
  for_each = local.buckets

  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.this[each.key].arn, "${aws_s3_bucket.this[each.key].arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }

  statement {
    sid       = "DenyNonKMSEncryptionHeader"
    effect    = "Deny"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.this[each.key].arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "StringNotEqualsIfExists"
      variable = "s3:x-amz-server-side-encryption"
      values   = ["aws:kms"]
    }
  }

  statement {
    sid       = "DenyForeignKMSKey"
    effect    = "Deny"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.this[each.key].arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "StringNotEqualsIfExists"
      variable = "s3:x-amz-server-side-encryption-aws-kms-key-id"
      values   = [var.kms_key_arn]
    }
  }

  # Nobody, root included, may weaken the WORM configuration of the audit archive.
  dynamic "statement" {
    for_each = each.value.object_lock ? [1] : []
    content {
      sid    = "DenyWeakeningObjectLock"
      effect = "Deny"
      actions = [
        "s3:PutBucketObjectLockConfiguration",
        "s3:PutBucketVersioning",
        "s3:BypassGovernanceRetention",
        "s3:DeleteBucket",
      ]
      resources = [aws_s3_bucket.this[each.key].arn, "${aws_s3_bucket.this[each.key].arn}/*"]
      principals {
        type        = "*"
        identifiers = ["*"]
      }
    }
  }
}

resource "aws_s3_bucket_policy" "this" {
  for_each   = local.buckets
  bucket     = aws_s3_bucket.this[each.key].id
  policy     = data.aws_iam_policy_document.bucket[each.key].json
  depends_on = [aws_s3_bucket_public_access_block.this]
}

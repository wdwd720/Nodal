# Customer-managed keys for the platform (PART 99, 123). One symmetric key per
# data class (RDS, S3 evidence/ECR, Secrets Manager, logs/SNS) plus a separate
# asymmetric ECC_NIST_P256 SIGN_VERIFY key for audit checkpoints whose policy
# grants kms:Sign to the audit-worker task role only (SYSTEM.md section 2).

locals {
  root_arn = "arn:aws:iam::${var.aws_account_id}:root"
  admins   = concat([local.root_arn], var.key_admin_principal_arns)
}

# ---------------------------------------------------------------------------
# Symmetric keys
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "symmetric" {
  # Standard account-root statement: lets IAM identity policies in this
  # account grant use of the key. Every task role policy is scoped by
  # kms:ViaService in the ecs-service module.
  statement {
    sid       = "EnableIAMPolicies"
    actions   = ["kms:*"]
    resources = ["*"]
    principals {
      type        = "AWS"
      identifiers = local.admins
    }
  }
}

data "aws_iam_policy_document" "logs" {
  source_policy_documents = [data.aws_iam_policy_document.symmetric.json]

  statement {
    sid = "CloudWatchLogsUse"
    actions = [
      "kms:Encrypt*", "kms:Decrypt*", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:Describe*",
    ]
    resources = ["*"]
    principals {
      type        = "Service"
      identifiers = ["logs.${var.aws_region}.amazonaws.com"]
    }
    condition {
      test     = "ArnLike"
      variable = "kms:EncryptionContext:aws:logs:arn"
      values   = ["arn:aws:logs:${var.aws_region}:${var.aws_account_id}:log-group:*"]
    }
  }

  statement {
    sid       = "SNSAndDeliveryUse"
    actions   = ["kms:GenerateDataKey*", "kms:Decrypt"]
    resources = ["*"]
    principals {
      type        = "Service"
      identifiers = ["sns.amazonaws.com", "cloudwatch.amazonaws.com", "delivery.logs.amazonaws.com"]
    }
  }
}

resource "aws_kms_key" "rds" {
  description             = "${var.name_prefix} RDS storage and Performance Insights"
  deletion_window_in_days = var.deletion_window_in_days
  enable_key_rotation     = true
  policy                  = data.aws_iam_policy_document.symmetric.json
  tags                    = merge(var.tags, { Name = "${var.name_prefix}-rds" })
}

resource "aws_kms_alias" "rds" {
  name          = "alias/${var.name_prefix}-rds"
  target_key_id = aws_kms_key.rds.key_id
}

resource "aws_kms_key" "s3" {
  description             = "${var.name_prefix} S3 evidence buckets and ECR"
  deletion_window_in_days = var.deletion_window_in_days
  enable_key_rotation     = true
  policy                  = data.aws_iam_policy_document.symmetric.json
  tags                    = merge(var.tags, { Name = "${var.name_prefix}-s3" })
}

resource "aws_kms_alias" "s3" {
  name          = "alias/${var.name_prefix}-s3"
  target_key_id = aws_kms_key.s3.key_id
}

resource "aws_kms_key" "secrets" {
  description             = "${var.name_prefix} Secrets Manager"
  deletion_window_in_days = var.deletion_window_in_days
  enable_key_rotation     = true
  policy                  = data.aws_iam_policy_document.symmetric.json
  tags                    = merge(var.tags, { Name = "${var.name_prefix}-secrets" })
}

resource "aws_kms_alias" "secrets" {
  name          = "alias/${var.name_prefix}-secrets"
  target_key_id = aws_kms_key.secrets.key_id
}

resource "aws_kms_key" "logs" {
  description             = "${var.name_prefix} CloudWatch Logs, SNS and flow logs"
  deletion_window_in_days = var.deletion_window_in_days
  enable_key_rotation     = true
  policy                  = data.aws_iam_policy_document.logs.json
  tags                    = merge(var.tags, { Name = "${var.name_prefix}-logs" })
}

resource "aws_kms_alias" "logs" {
  name          = "alias/${var.name_prefix}-logs"
  target_key_id = aws_kms_key.logs.key_id
}

# ---------------------------------------------------------------------------
# Audit checkpoint signing key (asymmetric). Sign is NOT delegated to IAM
# policies: the root statement carries administration only, so no identity
# policy in the account can grant kms:Sign; only the explicit statements here.
# ---------------------------------------------------------------------------

data "aws_iam_policy_document" "audit_signing" {
  statement {
    sid = "KeyAdministrationOnly"
    actions = [
      "kms:Create*", "kms:Describe*", "kms:Enable*", "kms:List*", "kms:Put*", "kms:Update*",
      "kms:Revoke*", "kms:Disable*", "kms:Get*", "kms:Delete*", "kms:TagResource",
      "kms:UntagResource", "kms:ScheduleKeyDeletion", "kms:CancelKeyDeletion",
    ]
    resources = ["*"]
    principals {
      type        = "AWS"
      identifiers = local.admins
    }
  }

  dynamic "statement" {
    for_each = length(var.audit_signer_role_arns) > 0 ? [1] : []
    content {
      sid       = "AuditWorkerSign"
      actions   = ["kms:Sign", "kms:Verify", "kms:GetPublicKey", "kms:DescribeKey"]
      resources = ["*"]
      principals {
        type        = "AWS"
        identifiers = var.audit_signer_role_arns
      }
    }
  }

  dynamic "statement" {
    for_each = length(var.audit_verifier_principal_arns) > 0 ? [1] : []
    content {
      sid       = "AuditorsVerify"
      actions   = ["kms:Verify", "kms:GetPublicKey", "kms:DescribeKey"]
      resources = ["*"]
      principals {
        type        = "AWS"
        identifiers = var.audit_verifier_principal_arns
      }
    }
  }
}

#trivy:ignore:AVD-AWS-0065 asymmetric SIGN_VERIFY keys cannot be auto-rotated; checkpoints record the key id so a manual key roll stays verifiable
resource "aws_kms_key" "audit_signing" {
  description              = "${var.name_prefix} audit checkpoint signing (ECC_NIST_P256, sign/verify only)"
  key_usage                = "SIGN_VERIFY"
  customer_master_key_spec = "ECC_NIST_P256"
  deletion_window_in_days  = var.deletion_window_in_days
  policy                   = data.aws_iam_policy_document.audit_signing.json
  tags                     = merge(var.tags, { Name = "${var.name_prefix}-audit-signing" })
}

resource "aws_kms_alias" "audit_signing" {
  name          = "alias/${var.name_prefix}-audit-signing"
  target_key_id = aws_kms_key.audit_signing.key_id
}

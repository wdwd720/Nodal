output "rds_key_arn" {
  value = aws_kms_key.rds.arn
}

output "s3_key_arn" {
  value = aws_kms_key.s3.arn
}

output "secrets_key_arn" {
  value = aws_kms_key.secrets.arn
}

output "logs_key_arn" {
  value = aws_kms_key.logs.arn
}

output "audit_signing_key_arn" {
  value = aws_kms_key.audit_signing.arn
}

output "audit_signing_key_id" {
  value = aws_kms_key.audit_signing.key_id
}

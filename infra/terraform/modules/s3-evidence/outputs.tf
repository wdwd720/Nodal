output "raw_events_bucket" {
  value = aws_s3_bucket.this["raw-events"].bucket
}

output "raw_events_bucket_arn" {
  value = aws_s3_bucket.this["raw-events"].arn
}

output "provider_evidence_bucket" {
  value = aws_s3_bucket.this["provider-evidence"].bucket
}

output "provider_evidence_bucket_arn" {
  value = aws_s3_bucket.this["provider-evidence"].arn
}

output "audit_archive_bucket" {
  value = aws_s3_bucket.this["audit-archive"].bucket
}

output "audit_archive_bucket_arn" {
  value = aws_s3_bucket.this["audit-archive"].arn
}

output "access_logs_bucket" {
  value = aws_s3_bucket.logs.bucket
}

output "access_logs_bucket_arn" {
  value = aws_s3_bucket.logs.arn
}

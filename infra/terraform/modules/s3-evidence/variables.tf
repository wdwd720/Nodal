variable "name_prefix" {
  type = string
}

variable "bucket_suffix" {
  description = "Globally-unique suffix, normally <account id>-<region>."
  type        = string
}

variable "kms_key_arn" {
  description = "Platform S3 key (SSE-KMS on every evidence bucket)."
  type        = string
}

variable "audit_object_lock_retention_days" {
  description = "Default COMPLIANCE-mode retention on the audit archive (PART 123). 2555 in prod; short in dev so buckets can be torn down."
  type        = number
  validation {
    condition     = var.audit_object_lock_retention_days >= 1
    error_message = "Object Lock default retention must be at least one day."
  }
}

variable "raw_market_data_retention_days" {
  description = "Mirrors CP_RETENTION_RAW_MARKET_DATA_DAYS: expiry of raw provider payloads."
  type        = number
}

variable "financial_record_retention_days" {
  description = "Mirrors CP_RETENTION_FINANCIAL_RECORD_DAYS: expiry of provider evidence."
  type        = number
}

variable "raw_glacier_after_days" {
  type    = number
  default = 30
}

variable "evidence_glacier_after_days" {
  type    = number
  default = 90
}

variable "audit_glacier_after_days" {
  type    = number
  default = 365
}

variable "access_log_retention_days" {
  type    = number
  default = 400
}

variable "force_destroy" {
  description = "Allow terraform destroy to empty buckets (dev only; never applied to the Object-Locked audit archive)."
  type        = bool
  default     = false
}

variable "tags" {
  type    = map(string)
  default = {}
}

variable "name_prefix" {
  type = string
}

variable "subnet_ids" {
  description = "Private data subnets (no route to the internet)."
  type        = list(string)
}

variable "security_group_ids" {
  type = list(string)
}

variable "kms_key_arn" {
  description = "Platform RDS key: storage and Performance Insights."
  type        = string
}

variable "secrets_kms_key_arn" {
  description = "Key for the RDS-managed master user secret in Secrets Manager."
  type        = string
}

variable "engine_version" {
  description = "PostgreSQL major.minor; a bare major lets RDS pick the latest minor at creation."
  type        = string
  default     = "16"
}

variable "parameter_group_family" {
  type    = string
  default = "postgres16"
}

variable "instance_class" {
  type = string
}

variable "allocated_storage" {
  type    = number
  default = 100
}

variable "max_allocated_storage" {
  description = "Storage autoscaling ceiling (GiB)."
  type        = number
  default     = 1000
}

variable "multi_az" {
  type = bool
}

variable "deletion_protection" {
  type = bool
}

variable "skip_final_snapshot" {
  type    = bool
  default = false
}

variable "backup_retention_days" {
  description = "Automated backup retention; PITR covers the same window (BACKUP_RESTORE.md section 2 requires >= 35)."
  type        = number
  default     = 35
  validation {
    condition     = var.backup_retention_days >= 1 && var.backup_retention_days <= 35
    error_message = "RDS supports 1-35 days of automated backup retention."
  }
}

variable "database_name" {
  type    = string
  default = "controlplane"
}

variable "master_username" {
  description = "Master user; its password is generated and rotated by RDS (manage_master_user_password)."
  type        = string
  default     = "cp_master"
}

variable "log_min_duration_statement_ms" {
  type    = number
  default = 500
}

variable "performance_insights_retention_days" {
  type    = number
  default = 7
}

variable "monitoring_interval" {
  description = "Enhanced monitoring interval in seconds (0 disables)."
  type        = number
  default     = 60
}

variable "read_replica_count" {
  description = "Same-region read replicas (analytics/cp_readonly traffic)."
  type        = number
  default     = 0
}

variable "read_replica_instance_class" {
  type    = string
  default = null
}

variable "apply_immediately" {
  type    = bool
  default = false
}

variable "ca_cert_identifier" {
  type    = string
  default = "rds-ca-rsa2048-g1"
}

variable "backup_window" {
  type    = string
  default = "03:00-04:00"
}

variable "maintenance_window" {
  type    = string
  default = "Sun:04:30-Sun:05:30"
}

variable "tags" {
  type    = map(string)
  default = {}
}

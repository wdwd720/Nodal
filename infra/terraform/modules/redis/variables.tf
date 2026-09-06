variable "name_prefix" {
  type = string
}

variable "subnet_ids" {
  description = "Private data subnets."
  type        = list(string)
}

variable "security_group_ids" {
  type = list(string)
}

variable "kms_key_arn" {
  description = "Key for encryption at rest."
  type        = string
}

variable "logs_kms_key_arn" {
  type = string
}

variable "node_type" {
  type = string
}

variable "num_cache_clusters" {
  description = "Nodes in the replication group; >= 2 enables automatic failover and Multi-AZ."
  type        = number
  validation {
    condition     = var.num_cache_clusters >= 1 && var.num_cache_clusters <= 6
    error_message = "num_cache_clusters must be between 1 and 6."
  }
}

variable "engine_version" {
  type    = string
  default = "7.1"
}

variable "parameter_group_family" {
  type    = string
  default = "redis7"
}

variable "snapshot_retention_limit" {
  description = "Days of automatic snapshots (Redis is never financial truth; this is for warm restarts only)."
  type        = number
  default     = 1
}

variable "log_retention_days" {
  type    = number
  default = 30
}

variable "apply_immediately" {
  type    = bool
  default = false
}

variable "tags" {
  type    = map(string)
  default = {}
}

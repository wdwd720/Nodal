variable "name_prefix" {
  type = string
}

variable "aws_region" {
  type = string
}

variable "kms_key_arn" {
  description = "Key for SNS topic encryption."
  type        = string
}

variable "sev1_email_endpoints" {
  type    = list(string)
  default = []
}

variable "sev2_email_endpoints" {
  type    = list(string)
  default = []
}

variable "sev1_https_endpoints" {
  description = "Pager/incident webhooks (PagerDuty, Opsgenie) for SEV1."
  type        = list(string)
  default     = []
}

variable "sev2_https_endpoints" {
  type    = list(string)
  default = []
}

variable "rds_instance_identifier" {
  type = string
}

variable "rds_replica_identifiers" {
  type    = list(string)
  default = []
}

variable "rds_free_storage_threshold_bytes" {
  type    = number
  default = 10737418240 # 10 GiB
}

variable "alb_arn_suffix" {
  type = string
}

variable "target_group_arn_suffix" {
  type = string
}

variable "alb_5xx_threshold" {
  description = "ELB/target 5xx count per 5 minutes that pages SEV2."
  type        = number
  default     = 10
}

variable "ecs_cluster_name" {
  type = string
}

variable "ecs_service_names" {
  description = "Long-running services to watch (not one-shot task definitions)."
  type        = list(string)
}

variable "redis_node_ids" {
  description = "ElastiCache node ids (<replication group>-001, ...)."
  type        = list(string)
  default     = []
}

variable "custom_metric_namespace" {
  description = "Namespace the OTLP exporter publishes application metrics to."
  type        = string
  default     = "ECS/AWSOTel/Application"
}

variable "custom_metric_dimensions" {
  description = "Dimensions attached to the application metrics by the collector (must match exactly for alarms to fire)."
  type        = map(string)
  default     = {}
}

variable "unknown_submissions_threshold" {
  description = "unknown_submissions per 5 minutes that is 'elevated' (PART 135 SEV2)."
  type        = number
  default     = 3
}

variable "oldest_mismatch_sev2_seconds" {
  type    = number
  default = 900
}

variable "oldest_mismatch_sev1_seconds" {
  type    = number
  default = 3600
}

variable "tags" {
  type    = map(string)
  default = {}
}

# ---------------------------------------------------------------------------
# Relay (transactional outbox) thresholds. cmd/relay-worker samples depth and
# lag every CP_RELAY_WORKER_SAMPLE_INTERVAL (15 s by default), so a 60 s
# period always contains samples when a relay is running.
# ---------------------------------------------------------------------------

variable "relay_lag_sev2_seconds" {
  description = "outbox_oldest_unpublished_age that is worth an alert. A healthy relay is single-digit seconds behind."
  type        = number
  default     = 120
}

variable "relay_lag_sev1_seconds" {
  description = "outbox_oldest_unpublished_age at which the event stream is considered stopped."
  type        = number
  default     = 900
}

variable "relay_publish_failure_threshold" {
  description = "outbox_publish_failures per 5 minutes that is worth an alert. One transient rejection is normal; a sustained rate is the bus."
  type        = number
  default     = 20
}

variable "relay_blocked_partition_threshold" {
  description = "outbox_blocked_partitions that is worth an alert. Above zero means at least one aggregate has stopped emitting events."
  type        = number
  default     = 1
}

# ---------------------------------------------------------------------------
# Provider degradation (measured through its consequences; see main.tf).
# ---------------------------------------------------------------------------

variable "execution_failure_rate_threshold_percent" {
  type    = number
  default = 25
}

variable "execution_rate_min_attempts" {
  description = "Attempts in the 5-minute window below which the failure-rate alarm stays silent, so a quiet period cannot page on a 100% rate over two attempts."
  type        = number
  default     = 20
}

variable "unknown_submission_rate_threshold_percent" {
  type    = number
  default = 5
}

variable "unknown_submission_min_submissions" {
  type    = number
  default = 20
}

# ---------------------------------------------------------------------------
# Database connection saturation.
# ---------------------------------------------------------------------------

variable "db_connection_warn_threshold" {
  description = "DatabaseConnections that is worth an alert. Set it from the cp_app role CONNECTION LIMIT (roughly 60%), not from the instance max_connections."
  type        = number
}

variable "db_connection_critical_threshold" {
  description = "DatabaseConnections at which new tasks and the migrate job will start failing to connect (roughly 85% of the cp_app CONNECTION LIMIT)."
  type        = number
}

# ---------------------------------------------------------------------------
# Log-derived signals and the liveness probe.
# ---------------------------------------------------------------------------

variable "api_log_group_name" {
  description = "cmd/api CloudWatch log group. Required for the kill-switch alarm, which reads the access log; null disables that alarm rather than creating one that can never fire."
  type        = string
  default     = null
}

variable "liveness_target_group_arn_suffix" {
  description = "Target group performing the /v1/healthz check. null disables the liveness alarm."
  type        = string
  default     = null
}

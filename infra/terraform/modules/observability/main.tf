# Alerting (PART 135/136): SNS topics per severity, CloudWatch alarms for
# the SEV1/SEV2 signals, and a dashboard skeleton. Application metrics
# (internal/observability/metrics.go) reach CloudWatch through the OTLP
# exporter; their namespace and dimensions are variables because they must
# match the collector configuration exactly.

locals {
  sev1 = [aws_sns_topic.sev1.arn]
  sev2 = [aws_sns_topic.sev2.arn]

  ecs_services = toset(var.ecs_service_names)
  rds_replicas = toset(var.rds_replica_identifiers)

  # A map keyed by position, not a set of the ids themselves.
  #
  # The ids are built from the replication group's name, which does not exist
  # until the group is created -- so on a first apply they are unknown at plan
  # time, and `for_each` over a set of unknown strings cannot be planned at all:
  # Terraform has no way to know which instances it is about to create. The
  # stack could therefore never be planned in one pass on a fresh account, which
  # is the one time a plan matters most.
  #
  # How many nodes there are IS known -- it comes from a variable -- so the
  # position is a stable key and the id becomes an ordinary unknown attribute,
  # which Terraform is perfectly happy with.
  redis_nodes = { for i, id in var.redis_node_ids : tostring(i) => id }

  # Metrics derived from log content, kept out of the OTLP namespace so a
  # collector misconfiguration can never silently shadow one.
  log_metric_namespace = "${var.name_prefix}/logs"
}

# ---------------------------------------------------------------------------
# Topics
# ---------------------------------------------------------------------------

resource "aws_sns_topic" "sev1" {
  name              = "${var.name_prefix}-sev1"
  kms_master_key_id = var.kms_key_arn
  tags              = merge(var.tags, { Severity = "SEV1" })
}

resource "aws_sns_topic" "sev2" {
  name              = "${var.name_prefix}-sev2"
  kms_master_key_id = var.kms_key_arn
  tags              = merge(var.tags, { Severity = "SEV2" })
}

resource "aws_sns_topic_subscription" "sev1_email" {
  for_each  = toset(var.sev1_email_endpoints)
  topic_arn = aws_sns_topic.sev1.arn
  protocol  = "email"
  endpoint  = each.value
}

resource "aws_sns_topic_subscription" "sev2_email" {
  for_each  = toset(var.sev2_email_endpoints)
  topic_arn = aws_sns_topic.sev2.arn
  protocol  = "email"
  endpoint  = each.value
}

resource "aws_sns_topic_subscription" "sev1_https" {
  for_each               = toset(var.sev1_https_endpoints)
  topic_arn              = aws_sns_topic.sev1.arn
  protocol               = "https"
  endpoint               = each.value
  endpoint_auto_confirms = true
}

resource "aws_sns_topic_subscription" "sev2_https" {
  for_each               = toset(var.sev2_https_endpoints)
  topic_arn              = aws_sns_topic.sev2.arn
  protocol               = "https"
  endpoint               = each.value
  endpoint_auto_confirms = true
}

# ---------------------------------------------------------------------------
# RDS
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_metric_alarm" "rds_cpu" {
  alarm_name          = "${var.name_prefix}-rds-cpu-high"
  alarm_description   = "SEV2: RDS CPU above 80% for 15 minutes"
  namespace           = "AWS/RDS"
  metric_name         = "CPUUtilization"
  dimensions          = { DBInstanceIdentifier = var.rds_instance_identifier }
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 3
  threshold           = 80
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "rds_storage" {
  alarm_name          = "${var.name_prefix}-rds-free-storage-low"
  alarm_description   = "SEV1: RDS free storage below threshold (writes will stop)"
  namespace           = "AWS/RDS"
  metric_name         = "FreeStorageSpace"
  dimensions          = { DBInstanceIdentifier = var.rds_instance_identifier }
  statistic           = "Minimum"
  period              = 300
  evaluation_periods  = 2
  threshold           = var.rds_free_storage_threshold_bytes
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = local.sev1
  ok_actions          = local.sev1
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "rds_memory" {
  alarm_name          = "${var.name_prefix}-rds-freeable-memory-low"
  alarm_description   = "SEV2: RDS freeable memory below 512 MiB"
  namespace           = "AWS/RDS"
  metric_name         = "FreeableMemory"
  dimensions          = { DBInstanceIdentifier = var.rds_instance_identifier }
  statistic           = "Minimum"
  period              = 300
  evaluation_periods  = 3
  threshold           = 536870912
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "rds_replica_lag" {
  for_each            = local.rds_replicas
  alarm_name          = "${var.name_prefix}-rds-replica-lag-${each.value}"
  alarm_description   = "SEV2: read replica lag above 30 s (analytics/readonly reads are stale)"
  namespace           = "AWS/RDS"
  metric_name         = "ReplicaLag"
  dimensions          = { DBInstanceIdentifier = each.value }
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 5
  threshold           = 30
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

# ---------------------------------------------------------------------------
# ECS
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_metric_alarm" "ecs_running_tasks" {
  for_each            = local.ecs_services
  alarm_name          = "${var.name_prefix}-ecs-${each.value}-no-running-tasks"
  alarm_description   = "SEV2: ${each.value} has no running task (task failures / circuit breaker)"
  namespace           = "ECS/ContainerInsights"
  metric_name         = "RunningTaskCount"
  dimensions          = { ClusterName = var.ecs_cluster_name, ServiceName = each.value }
  statistic           = "Minimum"
  period              = 60
  evaluation_periods  = 3
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "ecs_cpu" {
  for_each            = local.ecs_services
  alarm_name          = "${var.name_prefix}-ecs-${each.value}-cpu-high"
  alarm_description   = "SEV2: ${each.value} CPU above 85% for 15 minutes"
  namespace           = "AWS/ECS"
  metric_name         = "CPUUtilization"
  dimensions          = { ClusterName = var.ecs_cluster_name, ServiceName = each.value }
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 3
  threshold           = 85
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "ecs_memory" {
  for_each            = local.ecs_services
  alarm_name          = "${var.name_prefix}-ecs-${each.value}-memory-high"
  alarm_description   = "SEV2: ${each.value} memory above 85% for 15 minutes"
  namespace           = "AWS/ECS"
  metric_name         = "MemoryUtilization"
  dimensions          = { ClusterName = var.ecs_cluster_name, ServiceName = each.value }
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 3
  threshold           = 85
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

# ---------------------------------------------------------------------------
# ALB
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_metric_alarm" "alb_5xx" {
  alarm_name          = "${var.name_prefix}-alb-elb-5xx"
  alarm_description   = "SEV2: load balancer generated 5xx (no healthy target / timeouts)"
  namespace           = "AWS/ApplicationELB"
  metric_name         = "HTTPCode_ELB_5XX_Count"
  dimensions          = { LoadBalancer = var.alb_arn_suffix }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = var.alb_5xx_threshold
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "target_5xx" {
  alarm_name          = "${var.name_prefix}-alb-target-5xx"
  alarm_description   = "SEV2: api returned 5xx"
  namespace           = "AWS/ApplicationELB"
  metric_name         = "HTTPCode_Target_5XX_Count"
  dimensions          = { LoadBalancer = var.alb_arn_suffix, TargetGroup = var.target_group_arn_suffix }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = var.alb_5xx_threshold
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "unhealthy_hosts" {
  alarm_name          = "${var.name_prefix}-alb-unhealthy-targets"
  alarm_description   = "SEV2: api targets failing /readyz"
  namespace           = "AWS/ApplicationELB"
  metric_name         = "UnHealthyHostCount"
  dimensions          = { LoadBalancer = var.alb_arn_suffix, TargetGroup = var.target_group_arn_suffix }
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 3
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "target_latency" {
  alarm_name          = "${var.name_prefix}-alb-p99-latency"
  alarm_description   = "SEV2: api p99 latency above 2 s"
  namespace           = "AWS/ApplicationELB"
  metric_name         = "TargetResponseTime"
  dimensions          = { LoadBalancer = var.alb_arn_suffix, TargetGroup = var.target_group_arn_suffix }
  extended_statistic  = "p99"
  period              = 300
  evaluation_periods  = 3
  threshold           = 2
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

# ---------------------------------------------------------------------------
# Redis
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_metric_alarm" "redis_cpu" {
  for_each            = local.redis_nodes
  alarm_name          = "${var.name_prefix}-redis-${each.value}-engine-cpu-high"
  alarm_description   = "SEV2: Redis engine CPU above 80%"
  namespace           = "AWS/ElastiCache"
  metric_name         = "EngineCPUUtilization"
  dimensions          = { CacheClusterId = each.value }
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 3
  threshold           = 80
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "redis_memory" {
  for_each            = local.redis_nodes
  alarm_name          = "${var.name_prefix}-redis-${each.value}-memory-high"
  alarm_description   = "SEV2: Redis memory usage above 85%"
  namespace           = "AWS/ElastiCache"
  metric_name         = "DatabaseMemoryUsagePercentage"
  dimensions          = { CacheClusterId = each.value }
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 3
  threshold           = 85
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

# ---------------------------------------------------------------------------
# Application metrics (PART 132/135). Names match internal/observability/metrics.go.
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_metric_alarm" "ledger_posting_errors" {
  alarm_name          = "${var.name_prefix}-ledger-posting-errors"
  alarm_description   = "SEV1: ledger posting rejected or failed (ledger integrity candidate)"
  namespace           = var.custom_metric_namespace
  metric_name         = "ledger_posting_errors"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev1
  ok_actions          = local.sev1
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "reconciliation_mismatches" {
  alarm_name          = "${var.name_prefix}-reconciliation-mismatches"
  alarm_description   = "SEV1: reconciliation mismatch detected (unexplained financial mismatch)"
  namespace           = var.custom_metric_namespace
  metric_name         = "reconciliation_mismatches"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev1
  ok_actions          = local.sev1
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "unknown_submissions" {
  alarm_name          = "${var.name_prefix}-unknown-submissions-elevated"
  alarm_description   = "SEV2: elevated SUBMISSION_UNKNOWN outcomes"
  namespace           = var.custom_metric_namespace
  metric_name         = "unknown_submissions"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = var.unknown_submissions_threshold
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "oldest_mismatch_sev2" {
  alarm_name          = "${var.name_prefix}-oldest-unresolved-mismatch-sev2"
  alarm_description   = "SEV2: a reconciliation mismatch has been unresolved for ${var.oldest_mismatch_sev2_seconds} s"
  namespace           = var.custom_metric_namespace
  metric_name         = "oldest_unresolved_mismatch"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = var.oldest_mismatch_sev2_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "oldest_mismatch_sev1" {
  alarm_name          = "${var.name_prefix}-oldest-unresolved-mismatch-sev1"
  alarm_description   = "SEV1: a reconciliation mismatch has been unresolved for ${var.oldest_mismatch_sev1_seconds} s (new risk stays blocked)"
  namespace           = var.custom_metric_namespace
  metric_name         = "oldest_unresolved_mismatch"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = var.oldest_mismatch_sev1_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev1
  ok_actions          = local.sev1
  tags                = var.tags
}

# The heartbeat. Every counter alarm above sets treat_missing_data =
# "notBreaching", and correctly: a mismatch counter that never arrives is a
# system with no mismatches. But it is also a system whose instruments were
# never constructed, and for a year that was the case -- both composition
# roots passed NoopMetrics(), so applying this file would have shown a wall of
# green alarms over a system emitting nothing (F-118). This alarm is the one
# that cannot be fooled that way: verification_passes is emitted once per
# completed VerifyInternal pass, every five minutes, so a period with no
# sample at all means nothing is verifying -- the pass is broken, the exporter
# is down, or the instruments are not built -- and every SEV1 above is blind.
resource "aws_cloudwatch_metric_alarm" "verification_heartbeat" {
  alarm_name          = "${var.name_prefix}-verification-heartbeat-missing"
  alarm_description   = "SEV2: no internal verification pass has reported in 15 minutes. Every counter-based SEV1 alarm is blind while this fires: nothing is checking the ledger against its own entries."
  namespace           = var.custom_metric_namespace
  metric_name         = "verification_passes"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Sum"
  period              = 900
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching" # no sample at all IS the condition
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

# ---------------------------------------------------------------------------
# Dashboard skeleton
# ---------------------------------------------------------------------------

locals {
  dims_list = flatten([for k, v in var.custom_metric_dimensions : [k, v]])

  dashboard = {
    widgets = [
      {
        type = "metric", x = 0, y = 0, width = 12, height = 6
        properties = {
          title  = "Financial signals"
          region = var.aws_region
          stat   = "Sum"
          period = 60
          metrics = [
            concat([var.custom_metric_namespace, "ledger_posting_errors"], local.dims_list),
            concat([var.custom_metric_namespace, "reconciliation_mismatches"], local.dims_list),
            concat([var.custom_metric_namespace, "unknown_submissions"], local.dims_list),
          ]
        }
      },
      {
        type = "metric", x = 12, y = 0, width = 12, height = 6
        properties = {
          title   = "Oldest unresolved mismatch (s)"
          region  = var.aws_region
          stat    = "Maximum"
          period  = 60
          metrics = [concat([var.custom_metric_namespace, "oldest_unresolved_mismatch"], local.dims_list)]
        }
      },
      {
        type = "metric", x = 0, y = 6, width = 12, height = 6
        properties = {
          title  = "API edge"
          region = var.aws_region
          stat   = "Sum"
          period = 60
          metrics = [
            ["AWS/ApplicationELB", "RequestCount", "LoadBalancer", var.alb_arn_suffix],
            ["AWS/ApplicationELB", "HTTPCode_Target_5XX_Count", "LoadBalancer", var.alb_arn_suffix],
            ["AWS/ApplicationELB", "HTTPCode_ELB_5XX_Count", "LoadBalancer", var.alb_arn_suffix],
          ]
        }
      },
      {
        type = "metric", x = 12, y = 6, width = 12, height = 6
        properties = {
          title  = "RDS"
          region = var.aws_region
          stat   = "Average"
          period = 60
          metrics = [
            ["AWS/RDS", "CPUUtilization", "DBInstanceIdentifier", var.rds_instance_identifier],
            ["AWS/RDS", "DatabaseConnections", "DBInstanceIdentifier", var.rds_instance_identifier],
            ["AWS/RDS", "FreeStorageSpace", "DBInstanceIdentifier", var.rds_instance_identifier],
          ]
        }
      },
      {
        type = "metric", x = 0, y = 12, width = 12, height = 6
        properties = {
          title  = "Relay lag (s) and outbox depth"
          region = var.aws_region
          stat   = "Maximum"
          period = 60
          metrics = [
            concat([var.custom_metric_namespace, "outbox_oldest_unpublished_age"], local.dims_list),
            concat([var.custom_metric_namespace, "outbox_depth"], local.dims_list),
            concat([var.custom_metric_namespace, "outbox_blocked_partitions"], local.dims_list),
          ]
        }
      },
      {
        type = "metric", x = 12, y = 12, width = 12, height = 6
        properties = {
          title  = "Outbox publish outcome"
          region = var.aws_region
          stat   = "Sum"
          period = 60
          metrics = [
            concat([var.custom_metric_namespace, "outbox_events_published"], local.dims_list),
            concat([var.custom_metric_namespace, "outbox_publish_failures"], local.dims_list),
            concat([var.custom_metric_namespace, "outbox_publish_retries"], local.dims_list),
          ]
        }
      },
      {
        type = "metric", x = 0, y = 18, width = 24, height = 6
        properties = {
          title   = "ECS running tasks"
          region  = var.aws_region
          stat    = "Minimum"
          period  = 60
          metrics = [for s in var.ecs_service_names : ["ECS/ContainerInsights", "RunningTaskCount", "ClusterName", var.ecs_cluster_name, "ServiceName", s]]
        }
      },
    ]
  }
}

resource "aws_cloudwatch_dashboard" "this" {
  dashboard_name = "${var.name_prefix}-overview"
  dashboard_body = jsonencode(local.dashboard)
}

# ---------------------------------------------------------------------------
# Relay (transactional outbox). Names match cmd/relay-worker/metrics.go.
#
# Relay lag is the one number that says whether the rest of the platform is
# looking at the present. Every domain event is committed into outbox_events
# with the state change it describes; until the relay publishes it, the SSE
# stream, the execution worker wake-ups and every event-derived read model
# are that many seconds behind the ledger. Money is still correct (that is
# the point of the outbox), but decisions made on stale reads are not.
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_metric_alarm" "relay_lag_sev2" {
  alarm_name          = "${var.name_prefix}-relay-lag-sev2"
  alarm_description   = "SEV2: the oldest unpublished outbox row is older than ${var.relay_lag_sev2_seconds} s. Read models, SSE and worker wake-ups are behind by at least that much. Check the relay-worker running task count and its publish failures before touching the outbox."
  namespace           = var.custom_metric_namespace
  metric_name         = "outbox_oldest_unpublished_age"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 3
  threshold           = var.relay_lag_sev2_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "breaching" # no sample at all means no relay is running
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "relay_lag_sev1" {
  alarm_name          = "${var.name_prefix}-relay-lag-sev1"
  alarm_description   = "SEV1: outbox lag above ${var.relay_lag_sev1_seconds} s. The event stream has effectively stopped; treat every event-derived read model as stale and do not act on one until it drains."
  namespace           = var.custom_metric_namespace
  metric_name         = "outbox_oldest_unpublished_age"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 3
  threshold           = var.relay_lag_sev1_seconds
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = local.sev1
  ok_actions          = local.sev1
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "relay_publish_failures" {
  alarm_name          = "${var.name_prefix}-relay-publish-failures"
  alarm_description   = "SEV2: the bus rejected ${var.relay_publish_failure_threshold}+ publishes in 5 minutes. Rows stay unpublished and retryable, so nothing is lost; the usual causes are Redpanda unreachable, SASL credentials rotated without a redeploy, or a topic that no longer accepts the payload."
  namespace           = var.custom_metric_namespace
  metric_name         = "outbox_publish_failures"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = var.relay_publish_failure_threshold
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "relay_blocked_partitions" {
  alarm_name          = "${var.name_prefix}-relay-blocked-partitions"
  alarm_description   = "SEV2: ${var.relay_blocked_partition_threshold}+ outbox partitions are stuck behind a failed or backed-off row. This is the dead-letter signal of the outbox: a blocked partition publishes nothing until its head row succeeds, because per-partition order is preserved. relay-worker status -json names them."
  namespace           = var.custom_metric_namespace
  metric_name         = "outbox_blocked_partitions"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 2
  threshold           = var.relay_blocked_partition_threshold
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "relay_unregistered_topics" {
  alarm_name          = "${var.name_prefix}-relay-unregistered-topics"
  alarm_description   = "SEV2: the relay published a row whose topic is not in the event registry. A producer is ahead of the registry, or a rolling deploy is mid-flight with an incompatible contract. The event is still published, never dropped."
  namespace           = var.custom_metric_namespace
  metric_name         = "outbox_unregistered_topic_events"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

# ---------------------------------------------------------------------------
# Provider health. internal/provider tracks HEALTHY/DEGRADED/UNHEALTHY per
# adapter but publishes no metric for the state itself (it is served on
# /v1/admin/providers), so degradation is alarmed on its consequences: the
# share of execution attempts that fail, and the share of submissions whose
# outcome is never learned. Both are PART 133 rate pairs, so the ratio is
# computed at query time from counters that aggregate correctly.
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_metric_alarm" "execution_failure_rate" {
  alarm_name          = "${var.name_prefix}-execution-failure-rate-high"
  alarm_description   = "SEV2: more than ${var.execution_failure_rate_threshold_percent}% of execution attempts failed over 5 minutes. Read it as provider degradation until proven otherwise: check /v1/admin/providers for the adapter that went DEGRADED and decide whether its kill switch should be pulled."
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  threshold           = var.execution_failure_rate_threshold_percent
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags

  metric_query {
    id = "attempts"
    metric {
      namespace   = var.custom_metric_namespace
      metric_name = "execution_failure_rate_attempts"
      dimensions  = var.custom_metric_dimensions
      period      = 300
      stat        = "Sum"
    }
  }

  metric_query {
    id = "failures"
    metric {
      namespace   = var.custom_metric_namespace
      metric_name = "execution_failure_rate_failures"
      dimensions  = var.custom_metric_dimensions
      period      = 300
      stat        = "Sum"
    }
  }

  # The volume guard stops two failures out of three overnight attempts from
  # paging anyone; an alarm that fires on noise is an alarm people mute.
  metric_query {
    id          = "rate"
    expression  = "IF(attempts >= ${var.execution_rate_min_attempts}, 100*failures/attempts, 0)"
    label       = "Execution failure rate (%)"
    return_data = true
  }
}

resource "aws_cloudwatch_metric_alarm" "unknown_submission_rate" {
  alarm_name          = "${var.name_prefix}-unknown-submission-rate-high"
  alarm_description   = "SEV1: more than ${var.unknown_submission_rate_threshold_percent}% of submissions ended with an unknown outcome. The provider or the chain observer is not answering; every one of these is money whose fate is undetermined and which must never be retried blindly."
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 2
  threshold           = var.unknown_submission_rate_threshold_percent
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev1
  ok_actions          = local.sev1
  tags                = var.tags

  metric_query {
    id = "submissions"
    metric {
      namespace   = var.custom_metric_namespace
      metric_name = "unknown_submission_rate_submissions"
      dimensions  = var.custom_metric_dimensions
      period      = 300
      stat        = "Sum"
    }
  }

  metric_query {
    id = "unknown"
    metric {
      namespace   = var.custom_metric_namespace
      metric_name = "unknown_submission_rate_unknown"
      dimensions  = var.custom_metric_dimensions
      period      = 300
      stat        = "Sum"
    }
  }

  metric_query {
    id          = "rate"
    expression  = "IF(submissions >= ${var.unknown_submission_min_submissions}, 100*unknown/submissions, 0)"
    label       = "Unknown submission rate (%)"
    return_data = true
  }
}

# ---------------------------------------------------------------------------
# Database connection saturation. The binding constraint is the cp_app role
# CONNECTION LIMIT, not the instance max_connections: when it is reached the
# next task to start is refused with "too many connections for role", which
# looks like a deploy failure rather than a database problem.
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_metric_alarm" "rds_connections_sev2" {
  alarm_name          = "${var.name_prefix}-rds-connections-high"
  alarm_description   = "SEV2: ${var.db_connection_warn_threshold} database connections in use. The ceiling is CP_DATABASE_MAX_CONNS times task count; a service scaling out, or a leak, hits the role CONNECTION LIMIT next and new tasks then fail to start."
  namespace           = "AWS/RDS"
  metric_name         = "DatabaseConnections"
  dimensions          = { DBInstanceIdentifier = var.rds_instance_identifier }
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 5
  threshold           = var.db_connection_warn_threshold
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "rds_connections_sev1" {
  alarm_name          = "${var.name_prefix}-rds-connections-critical"
  alarm_description   = "SEV1: ${var.db_connection_critical_threshold} database connections in use. At the role limit new tasks cannot open a pool and the migrate job cannot run; recovery is to scale the noisy service in, not to raise the limit under pressure."
  namespace           = "AWS/RDS"
  metric_name         = "DatabaseConnections"
  dimensions          = { DBInstanceIdentifier = var.rds_instance_identifier }
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 3
  threshold           = var.db_connection_critical_threshold
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "missing"
  alarm_actions       = local.sev1
  ok_actions          = local.sev1
  tags                = var.tags
}

# ---------------------------------------------------------------------------
# Kill switch. Activation and release are audited in Postgres (PART 52/93),
# which is the record of authority; this filter exists so the on-call engineer
# learns about it in seconds instead of finding it in an audit query later. A
# kill switch changing state is always an incident: either new risk has just
# been stopped, or it has just been allowed again.
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_log_metric_filter" "kill_switch_change" {
  count          = var.api_log_group_name == null ? 0 : 1
  name           = "${var.name_prefix}-kill-switch-change"
  log_group_name = var.api_log_group_name

  # cmd/api logs one JSON line per request (internal/httpapi/middleware.go,
  # observe): {"msg":"http request","method":...,"route":<chi pattern>,"status":...}.
  pattern = "{ $.msg = \"http request\" && $.method = \"POST\" && $.route = \"/v1/admin/kill-switches\" && $.status < 300 }"

  metric_transformation {
    name          = "kill_switch_changes"
    namespace     = local.log_metric_namespace
    value         = "1"
    default_value = 0
    unit          = "Count"
  }
}

resource "aws_cloudwatch_metric_alarm" "kill_switch_change" {
  count               = var.api_log_group_name == null ? 0 : 1
  alarm_name          = "${var.name_prefix}-kill-switch-changed"
  alarm_description   = "SEV1: a kill switch was activated or released. Activation means new risk of that kind has stopped and someone must decide what happens to the work already in flight; release means it has been allowed again and the reason must already be in the audit record."
  namespace           = local.log_metric_namespace
  metric_name         = aws_cloudwatch_log_metric_filter.kill_switch_change[0].metric_transformation[0].name
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev1
  tags                = var.tags
}

# ---------------------------------------------------------------------------
# Liveness. The readiness group (/v1/readyz) goes unhealthy whenever Postgres
# or Redis is unreachable, which is correct and already alarmed above. The
# liveness group (/v1/healthz) is answered by the process alone, so it going
# unhealthy means the process itself is wedged or gone: a different response.
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_metric_alarm" "liveness_unhealthy" {
  # Not `liveness_target_group_arn_suffix == null`. That suffix comes from a
  # target group created in the same apply, so the comparison is unknown at plan
  # time and `count` cannot be unknown. Whether the alarm exists is a decision
  # the caller can state up front; what it points at is not.
  count               = var.liveness_alarm_enabled ? 1 : 0
  alarm_name          = "${var.name_prefix}-api-liveness-unhealthy"
  alarm_description   = "SEV1: api targets are failing /v1/healthz, which the process answers without touching any dependency. The container is wedged or dying, not waiting on the database."
  namespace           = "AWS/ApplicationELB"
  metric_name         = "UnHealthyHostCount"
  dimensions          = { LoadBalancer = var.alb_arn_suffix, TargetGroup = var.liveness_target_group_arn_suffix }
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 3
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev1
  ok_actions          = local.sev1
  tags                = var.tags
}

# Alarms on the advanced (sandbox) capabilities' metrics, recorded from the
# database by the advanced worker's collector (internal/advancedmetrics; the
# instruments are internal/observability/advanced.go). Every figure behind
# them is value domain SIMULATED, so none of these is a real-capital event;
# they are paged because a sandbox whose books stop reconciling is a defect
# in the controls that would one day guard real value.

resource "aws_cloudwatch_metric_alarm" "advanced_reserve_mismatches" {
  alarm_name          = "${var.name_prefix}-advanced-reserve-mismatches"
  alarm_description   = "SEV1: a sandbox stable asset's supply or reserve does not reconcile; its circuit breaker has paused issuance"
  namespace           = var.custom_metric_namespace
  metric_name         = "advanced_reserve_mismatches"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev1
  ok_actions          = local.sev1
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "advanced_unmatched_effects" {
  alarm_name          = "${var.name_prefix}-advanced-unmatched-effects"
  alarm_description   = "SEV2: an advanced economic effect has stayed UNKNOWN or unmatched for 15 minutes; reconciliation has not resolved it and the account takes no new risk"
  namespace           = var.custom_metric_namespace
  metric_name         = "advanced_unmatched_economic_effects"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Minimum"
  period              = 300
  evaluation_periods  = 3
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

resource "aws_cloudwatch_metric_alarm" "advanced_chain_mismatches" {
  alarm_name          = "${var.name_prefix}-advanced-chain-mismatches"
  alarm_description   = "SEV2: the sandbox ledger and a simulated chain disagree (bridge route or stable asset supply); the affected route or asset is paused"
  namespace           = var.custom_metric_namespace
  metric_name         = "advanced_chain_mismatches"
  dimensions          = var.custom_metric_dimensions
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = local.sev2
  ok_actions          = local.sev2
  tags                = var.tags
}

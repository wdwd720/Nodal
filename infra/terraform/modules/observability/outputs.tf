output "sev1_topic_arn" {
  value = aws_sns_topic.sev1.arn
}

output "sev2_topic_arn" {
  value = aws_sns_topic.sev2.arn
}

output "dashboard_name" {
  value = aws_cloudwatch_dashboard.this.dashboard_name
}

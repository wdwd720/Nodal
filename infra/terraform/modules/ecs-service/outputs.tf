output "task_role_arn" {
  value = aws_iam_role.task.arn
}

output "task_role_name" {
  value = aws_iam_role.task.name
}

output "execution_role_arn" {
  value = aws_iam_role.execution.arn
}

output "execution_role_name" {
  value = aws_iam_role.execution.name
}

output "task_definition_arn" {
  value = aws_ecs_task_definition.this.arn
}

output "task_definition_family" {
  value = aws_ecs_task_definition.this.family
}

output "service_name" {
  description = "null for one-shot task definitions (migrate, db-bootstrap)."
  value       = one(concat(aws_ecs_service.this[*].name, aws_ecs_service.scaled[*].name))
}

output "service_arn" {
  value = one(concat(aws_ecs_service.this[*].id, aws_ecs_service.scaled[*].id))
}

output "autoscaling_target_resource_id" {
  description = "Application Auto Scaling resource id, or null when the count is fixed."
  value       = one(aws_appautoscaling_target.this[*].resource_id)
}

output "log_group_name" {
  value = aws_cloudwatch_log_group.this.name
}

output "log_group_arn" {
  value = aws_cloudwatch_log_group.this.arn
}

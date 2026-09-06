output "secret_arns" {
  description = "Secret name (without prefix) -> ARN."
  value       = { for k, s in aws_secretsmanager_secret.this : k => s.arn }
}

output "secret_names" {
  description = "Secret name (without prefix) -> full Secrets Manager name."
  value       = { for k, s in aws_secretsmanager_secret.this : k => s.name }
}

output "secrets_by_service" {
  description = "Service key -> list of secret ARNs its task role may read."
  value       = { for svc, names in local.secrets_of : svc => [for n in names : aws_secretsmanager_secret.this[n].arn] }
}

output "secret_name_prefix" {
  value = var.secret_name_prefix
}

output "identifier" {
  value = aws_db_instance.this.identifier
}

output "arn" {
  value = aws_db_instance.this.arn
}

output "address" {
  description = "Writer hostname (no credentials)."
  value       = aws_db_instance.this.address
}

output "port" {
  value = aws_db_instance.this.port
}

output "endpoint" {
  description = "Writer host:port (no credentials)."
  value       = aws_db_instance.this.endpoint
}

output "database_name" {
  value = aws_db_instance.this.db_name
}

output "master_user_secret_arn" {
  description = "RDS-managed master credentials secret (read only by the db-bootstrap task)."
  value       = aws_db_instance.this.master_user_secret[0].secret_arn
}

output "replica_identifiers" {
  value = aws_db_instance.replica[*].identifier
}

output "replica_addresses" {
  value = aws_db_instance.replica[*].address
}

output "bootstrap_roles_sql" {
  description = "Contents of bootstrap/roles.sql, fed to the db-bootstrap one-shot task."
  value       = file("${path.module}/bootstrap/roles.sql")
}

# RDS PostgreSQL 16 (PART 138/139, BACKUP_RESTORE.md section 2): Multi-AZ,
# encrypted with the platform key, 35-day automated backups with PITR,
# deletion protection, Performance Insights, enhanced monitoring, TLS forced
# by the parameter group. The master password is generated and rotated by
# RDS itself (manage_master_user_password): it never enters Terraform state.
#
# Application roles (cp_migrate, cp_app, cp_readonly, cp_ops) are NOT created
# here: there is deliberately no null_resource/provisioner. They are created
# by the documented db-bootstrap one-shot ECS task running bootstrap/roles.sql
# (DEPLOYMENT.md section 4).

resource "aws_db_subnet_group" "this" {
  name        = "${var.name_prefix}-postgres"
  description = "${var.name_prefix} private data subnets"
  subnet_ids  = var.subnet_ids
  tags        = var.tags
}

resource "aws_db_parameter_group" "this" {
  name_prefix = "${var.name_prefix}-${var.parameter_group_family}-"
  family      = var.parameter_group_family
  description = "${var.name_prefix}: TLS required, slow statement logging, pg_stat_statements"

  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }

  parameter {
    name  = "log_min_duration_statement"
    value = tostring(var.log_min_duration_statement_ms)
  }

  parameter {
    name  = "log_statement"
    value = "ddl"
  }

  parameter {
    name  = "log_connections"
    value = "1"
  }

  parameter {
    name  = "log_disconnections"
    value = "1"
  }

  parameter {
    name  = "log_lock_waits"
    value = "1"
  }

  parameter {
    name  = "idle_in_transaction_session_timeout"
    value = "60000"
  }

  parameter {
    name         = "shared_preload_libraries"
    value        = "pg_stat_statements"
    apply_method = "pending-reboot"
  }

  lifecycle {
    create_before_destroy = true
  }

  tags = var.tags
}

# Enhanced monitoring role.
data "aws_iam_policy_document" "monitoring_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["monitoring.rds.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "monitoring" {
  permissions_boundary = var.permissions_boundary_arn
  count                = var.monitoring_interval > 0 ? 1 : 0
  name                 = "${var.name_prefix}-rds-monitoring"
  assume_role_policy   = data.aws_iam_policy_document.monitoring_assume.json
  tags                 = var.tags
}

resource "aws_iam_role_policy_attachment" "monitoring" {
  count      = var.monitoring_interval > 0 ? 1 : 0
  role       = aws_iam_role.monitoring[0].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonRDSEnhancedMonitoringRole"
}

resource "aws_db_instance" "this" {
  identifier = "${var.name_prefix}-postgres"

  engine         = "postgres"
  engine_version = var.engine_version
  instance_class = var.instance_class

  allocated_storage     = var.allocated_storage
  max_allocated_storage = var.max_allocated_storage
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = var.kms_key_arn

  db_name                       = var.database_name
  username                      = var.master_username
  manage_master_user_password   = true
  master_user_secret_kms_key_id = var.secrets_kms_key_arn

  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = var.security_group_ids
  publicly_accessible    = false
  multi_az               = var.multi_az
  parameter_group_name   = aws_db_parameter_group.this.name
  ca_cert_identifier     = var.ca_cert_identifier

  backup_retention_period  = var.backup_retention_days
  backup_window            = var.backup_window
  maintenance_window       = var.maintenance_window
  copy_tags_to_snapshot    = true
  delete_automated_backups = false

  deletion_protection       = var.deletion_protection
  skip_final_snapshot       = var.skip_final_snapshot
  final_snapshot_identifier = "${var.name_prefix}-postgres-final"

  performance_insights_enabled          = true
  performance_insights_kms_key_id       = var.kms_key_arn
  performance_insights_retention_period = var.performance_insights_retention_days
  monitoring_interval                   = var.monitoring_interval
  monitoring_role_arn                   = var.monitoring_interval > 0 ? aws_iam_role.monitoring[0].arn : null
  enabled_cloudwatch_logs_exports       = ["postgresql", "upgrade"]

  iam_database_authentication_enabled = true
  auto_minor_version_upgrade          = true
  apply_immediately                   = var.apply_immediately

  tags = merge(var.tags, { Name = "${var.name_prefix}-postgres" })
}

resource "aws_db_instance" "replica" {
  count = var.read_replica_count

  identifier          = "${var.name_prefix}-postgres-replica-${count.index + 1}"
  replicate_source_db = aws_db_instance.this.identifier
  instance_class      = coalesce(var.read_replica_instance_class, var.instance_class)

  vpc_security_group_ids = var.security_group_ids
  publicly_accessible    = false
  parameter_group_name   = aws_db_parameter_group.this.name
  ca_cert_identifier     = var.ca_cert_identifier

  performance_insights_enabled          = true
  performance_insights_kms_key_id       = var.kms_key_arn
  performance_insights_retention_period = var.performance_insights_retention_days
  monitoring_interval                   = var.monitoring_interval
  monitoring_role_arn                   = var.monitoring_interval > 0 ? aws_iam_role.monitoring[0].arn : null
  enabled_cloudwatch_logs_exports       = ["postgresql", "upgrade"]

  # Encryption and backups are inherited from the source instance.
  skip_final_snapshot        = true
  auto_minor_version_upgrade = true
  apply_immediately          = var.apply_immediately

  tags = merge(var.tags, { Name = "${var.name_prefix}-postgres-replica-${count.index + 1}" })
}

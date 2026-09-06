# ElastiCache for Redis (PART 102/137): private subnets only, encryption in
# transit (required) and at rest (platform key), AUTH token. Redis holds rate
# limits and short-lived caches only; it is never financial truth
# (SYSTEM.md section 4), so snapshot retention is minimal.
#
# The AUTH token is generated here and published to Secrets Manager by the
# environment (secrets module, "redis/url"). Known residual: random_password
# keeps the token in Terraform state; the state bucket is KMS-encrypted and
# access-restricted (DEPLOYMENT.md section 2), and the token can be rotated
# with auth_token_update_strategy = ROTATE.

resource "random_password" "auth_token" {
  length  = 64
  special = false
}

resource "aws_elasticache_subnet_group" "this" {
  name        = "${var.name_prefix}-redis"
  description = "${var.name_prefix} private data subnets"
  subnet_ids  = var.subnet_ids
  tags        = var.tags
}

resource "aws_elasticache_parameter_group" "this" {
  name        = "${var.name_prefix}-${var.parameter_group_family}"
  family      = var.parameter_group_family
  description = "${var.name_prefix}: cache-only eviction policy"

  parameter {
    name  = "maxmemory-policy"
    value = "volatile-lru"
  }

  tags = var.tags
}

resource "aws_cloudwatch_log_group" "slow" {
  name              = "/aws/elasticache/${var.name_prefix}/slow-log"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.logs_kms_key_arn
  tags              = var.tags
}

resource "aws_cloudwatch_log_group" "engine" {
  name              = "/aws/elasticache/${var.name_prefix}/engine-log"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.logs_kms_key_arn
  tags              = var.tags
}

resource "aws_elasticache_replication_group" "this" {
  replication_group_id = "${var.name_prefix}-redis"
  description          = "${var.name_prefix} rate limits and read caches"

  engine         = "redis"
  engine_version = var.engine_version
  node_type      = var.node_type
  port           = 6379

  num_cache_clusters         = var.num_cache_clusters
  automatic_failover_enabled = var.num_cache_clusters > 1
  multi_az_enabled           = var.num_cache_clusters > 1

  at_rest_encryption_enabled = true
  kms_key_id                 = var.kms_key_arn
  transit_encryption_enabled = true
  transit_encryption_mode    = "required"
  auth_token                 = random_password.auth_token.result
  auth_token_update_strategy = "ROTATE"

  subnet_group_name    = aws_elasticache_subnet_group.this.name
  security_group_ids   = var.security_group_ids
  parameter_group_name = aws_elasticache_parameter_group.this.name

  snapshot_retention_limit   = var.snapshot_retention_limit
  snapshot_window            = "02:00-03:00"
  maintenance_window         = "sun:05:00-sun:06:00"
  auto_minor_version_upgrade = true
  apply_immediately          = var.apply_immediately

  log_delivery_configuration {
    destination      = aws_cloudwatch_log_group.slow.name
    destination_type = "cloudwatch-logs"
    log_format       = "json"
    log_type         = "slow-log"
  }

  log_delivery_configuration {
    destination      = aws_cloudwatch_log_group.engine.name
    destination_type = "cloudwatch-logs"
    log_format       = "json"
    log_type         = "engine-log"
  }

  tags = merge(var.tags, { Name = "${var.name_prefix}-redis" })
}

# STAGING environment composition (PART 144: separate account, database, secrets,
# provider config and capability state). Multi-AZ, per-AZ NAT, deletion protection
# and no force-destroy are fixed here, not variables.

locals {
  environment   = "STAGING"
  env_lower     = "staging"
  name_prefix   = "${var.project}-${local.env_lower}"
  secret_prefix = "${var.project}/${local.env_lower}"
  bucket_suffix = "${var.aws_account_id}-${var.aws_region}"

  tags = merge({
    Project     = var.project
    Environment = local.environment
    ManagedBy   = "terraform"
    Stack       = "infra/terraform/environments/${local.env_lower}"
  }, var.extra_tags)

  long_running = [for k in keys(var.service_sizing) : k if k != "migrate"]

  # --- task role policy fragments (S3 evidence, KMS) ---
  s3_kms = {
    sid       = "S3KMS"
    actions   = ["kms:GenerateDataKey", "kms:Decrypt"]
    resources = [module.kms.s3_key_arn]
  }
  evidence_rw = {
    sid       = "ProviderEvidenceRW"
    actions   = ["s3:PutObject", "s3:GetObject", "s3:ListBucket"]
    resources = [module.s3_evidence.provider_evidence_bucket_arn, "${module.s3_evidence.provider_evidence_bucket_arn}/*"]
  }
  raw_rw = {
    sid       = "RawEventsRW"
    actions   = ["s3:PutObject", "s3:GetObject", "s3:ListBucket"]
    resources = [module.s3_evidence.raw_events_bucket_arn, "${module.s3_evidence.raw_events_bucket_arn}/*"]
  }
  audit_rw = {
    sid       = "AuditArchiveWriteVerify"
    actions   = ["s3:PutObject", "s3:GetObject", "s3:GetObjectRetention", "s3:ListBucket", "s3:GetBucketObjectLockConfiguration"]
    resources = [module.s3_evidence.audit_archive_bucket_arn, "${module.s3_evidence.audit_archive_bucket_arn}/*"]
  }
  evidence_ro = {
    sid       = "ProviderEvidenceRead"
    actions   = ["s3:GetObject", "s3:ListBucket"]
    resources = [module.s3_evidence.provider_evidence_bucket_arn, "${module.s3_evidence.provider_evidence_bucket_arn}/*"]
  }
  audit_sign = {
    sid       = "AuditCheckpointSign"
    actions   = ["kms:Sign", "kms:Verify", "kms:GetPublicKey", "kms:DescribeKey"]
    resources = [module.kms.audit_signing_key_arn]
  }

  # Per-binary IAM beyond secrets (SYSTEM.md section 2 credential scope).
  # relay-worker is deliberately empty: it reads outbox_events and publishes to
  # the bus, so it needs no bucket and no key beyond resolving its own secrets.
  task_policies = {
    api                   = [local.evidence_rw, local.s3_kms]
    relay-worker          = []
    execution-worker      = [local.evidence_rw, local.s3_kms]
    reconciliation-worker = [local.evidence_rw, local.s3_kms]
    market-ingest-worker  = [local.raw_rw, local.s3_kms]
    agent-worker          = [local.evidence_rw, local.s3_kms]
    workflow-worker       = [local.evidence_rw, local.s3_kms]
    audit-worker          = [local.audit_rw, local.evidence_ro, local.s3_kms, local.audit_sign]
    migrate               = []
  }

  # The runtime image entrypoint is the bare binary, so every worker needs its
  # subcommand: `relay-worker` with no arguments prints usage and exits 2,
  # which the deployment circuit breaker would read as a crash loop. cmd/api is
  # the only binary that takes no subcommand.
  service_command = {
    api                   = []
    relay-worker          = ["run"]
    execution-worker      = ["run"]
    reconciliation-worker = ["run"]
    market-ingest-worker  = ["run"]
    agent-worker          = ["run"]
    workflow-worker       = ["run"]
    audit-worker          = ["run"]
    migrate               = ["up"]
  }

  service_env_overrides = {
    audit-worker = { CP_DATABASE_READONLY_URL = module.app_config.readonly_database_url_ref }
  }

  otlp_endpoint = var.otel_sidecar_enabled ? "127.0.0.1:4317" : var.otlp_endpoint
  otlp_insecure = var.otel_sidecar_enabled ? true : var.otlp_insecure
}

provider "aws" {
  region              = var.aws_region
  allowed_account_ids = [var.aws_account_id]
  default_tags {
    tags = local.tags
  }
}

provider "aws" {
  alias               = "us_east_1"
  region              = "us-east-1"
  allowed_account_ids = [var.aws_account_id]
  default_tags {
    tags = local.tags
  }
}

# ---------------------------------------------------------------------------
# Foundations
# ---------------------------------------------------------------------------

module "kms" {
  source = "../../modules/kms"

  name_prefix    = local.name_prefix
  aws_account_id = var.aws_account_id
  aws_region     = var.aws_region
  # Only when the audit worker is deployed. The key still exists at every stage
  # -- it signs the audit chain and the chain outlives any one binary -- but a
  # stage that runs no audit worker has no role to name here.
  audit_signer_role_arns        = contains(keys(var.service_sizing), "audit-worker") ? [module.services["audit-worker"].task_role_arn] : []
  audit_verifier_principal_arns = var.auditor_principal_arns
  key_admin_principal_arns      = var.key_admin_principal_arns
  deletion_window_in_days       = 30
  tags                          = local.tags
}

module "network" {
  source                   = "../../modules/network"
  permissions_boundary_arn = var.permissions_boundary_arn

  name_prefix                      = local.name_prefix
  aws_region                       = var.aws_region
  vpc_cidr                         = var.vpc_cidr
  availability_zones               = var.availability_zones
  single_nat_gateway               = false
  logs_kms_key_arn                 = module.kms.logs_key_arn
  flow_log_retention_days          = 365
  alb_ingress_from_cloudfront_only = var.cloudfront_enabled
  tags                             = local.tags
}

module "s3_evidence" {
  source = "../../modules/s3-evidence"

  name_prefix                      = local.name_prefix
  bucket_suffix                    = local.bucket_suffix
  kms_key_arn                      = module.kms.s3_key_arn
  audit_object_lock_retention_days = var.audit_object_lock_retention_days
  raw_market_data_retention_days   = var.retention_days.raw_market_data
  financial_record_retention_days  = var.retention_days.financial_record
  force_destroy                    = false
  tags                             = local.tags
}

# ---------------------------------------------------------------------------
# Data stores
# ---------------------------------------------------------------------------

module "rds" {
  source                   = "../../modules/rds"
  permissions_boundary_arn = var.permissions_boundary_arn

  name_prefix           = local.name_prefix
  subnet_ids            = module.network.private_data_subnet_ids
  security_group_ids    = [module.network.db_security_group_id]
  kms_key_arn           = module.kms.rds_key_arn
  secrets_kms_key_arn   = module.kms.secrets_key_arn
  instance_class        = var.rds_instance_class
  allocated_storage     = var.rds_allocated_storage
  max_allocated_storage = var.rds_allocated_storage * 4
  multi_az              = true
  deletion_protection   = true
  skip_final_snapshot   = false
  backup_retention_days = 35
  read_replica_count    = var.rds_read_replica_count
  apply_immediately     = false
  tags                  = local.tags
}

module "redis" {
  source = "../../modules/redis"

  name_prefix        = local.name_prefix
  subnet_ids         = module.network.private_data_subnet_ids
  security_group_ids = [module.network.redis_security_group_id]
  kms_key_arn        = module.kms.rds_key_arn
  logs_kms_key_arn   = module.kms.logs_key_arn
  node_type          = var.redis_node_type
  num_cache_clusters = var.redis_num_cache_clusters
  apply_immediately  = false
  tags               = local.tags
}

# ---------------------------------------------------------------------------
# Compute platform, edge, configuration
# ---------------------------------------------------------------------------

module "ecs_cluster" {
  source = "../../modules/ecs-cluster"

  name_prefix      = local.name_prefix
  ecr_kms_key_arn  = module.kms.s3_key_arn
  ecr_force_delete = false
  tags             = local.tags
}

module "waf_edge" {
  source = "../../modules/waf-edge"
  providers = {
    aws           = aws
    aws.us_east_1 = aws.us_east_1
  }

  name_prefix                    = local.name_prefix
  vpc_id                         = module.network.vpc_id
  public_subnet_ids              = module.network.public_subnet_ids
  alb_security_group_id          = module.network.alb_security_group_id
  acm_certificate_arn            = var.acm_certificate_arn
  bucket_suffix                  = local.bucket_suffix
  logs_kms_key_arn               = module.kms.logs_key_arn
  deletion_protection            = true
  waf_rate_limit                 = var.waf_rate_limit
  cloudfront_enabled             = var.cloudfront_enabled
  cloudfront_acm_certificate_arn = var.cloudfront_acm_certificate_arn
  cloudfront_aliases             = var.cloudfront_aliases
  cloudfront_origin_domain_name  = var.cloudfront_origin_domain_name
  cloudfront_origin_secret       = var.cloudfront_origin_secret
  force_destroy_logs             = false
  tags                           = local.tags
}

module "app_config" {
  source = "../../modules/app-config"

  environment         = local.environment
  aws_region          = var.aws_region
  http_replicas       = try(var.service_sizing["api"].desired_count, 1)
  secret_name_prefix  = local.secret_prefix
  public_product_name = var.public_product_name
  public_base_url     = var.public_base_url
  cors_origins        = var.cors_origins
  trusted_proxy_cidrs = [module.network.vpc_cidr]
  database            = var.database_pool
  redpanda            = var.redpanda
  clickhouse          = var.clickhouse
  temporal            = var.temporal
  archive = {
    raw_bucket      = module.s3_evidence.raw_events_bucket
    evidence_bucket = module.s3_evidence.provider_evidence_bucket
    audit_bucket    = module.s3_evidence.audit_archive_bucket
  }
  kms_audit_signing_key_id = module.kms.audit_signing_key_arn
  auth                     = var.auth
  provider_slots           = var.provider_slots
  api                      = var.api
  relay_worker             = var.relay_worker
  execution_worker         = var.execution_worker
  workflow_worker          = var.workflow_worker
  telemetry = {
    otlp_endpoint      = local.otlp_endpoint
    otlp_insecure      = local.otlp_insecure
    trace_sample_ratio = var.trace_sample_ratio
  }
  retention_days = var.retention_days
}

# ---------------------------------------------------------------------------
# Secrets (least-privilege matrix lives in the module)
# ---------------------------------------------------------------------------

module "secrets" {
  source = "../../modules/secrets"

  secret_name_prefix = local.secret_prefix
  kms_key_arn        = module.kms.secrets_key_arn
  task_role_arns = merge(
    { for k, s in module.services : k => s.task_role_arn },
    { "db-bootstrap" = module.db_bootstrap.execution_role_arn },
  )
  admin_principal_arns    = var.secret_admin_principal_arns
  managed_secret_keys     = ["redis/url"]
  managed_secret_values   = { "redis/url" = module.redis.redis_url }
  rotation                = var.secret_rotation
  recovery_window_in_days = 30
  tags                    = local.tags
}

# ---------------------------------------------------------------------------
# Services: one task role + execution role per binary
# ---------------------------------------------------------------------------

module "services" {
  source                   = "../../modules/ecs-service"
  permissions_boundary_arn = var.permissions_boundary_arn
  for_each                 = var.service_sizing

  name_prefix    = local.name_prefix
  service_name   = each.key
  aws_region     = var.aws_region
  aws_account_id = var.aws_account_id
  cluster_arn    = module.ecs_cluster.cluster_arn
  cluster_name   = module.ecs_cluster.cluster_name

  image          = "${module.ecs_cluster.ecr_repository_urls[each.key]}:${var.image_tag}"
  cpu            = each.value.cpu
  memory         = each.value.memory
  desired_count  = each.value.desired_count
  create_service = each.key != "migrate"
  command        = local.service_command[each.key]

  subnet_ids         = module.network.private_app_subnet_ids
  security_group_ids = [each.key == "api" ? module.network.api_security_group_id : module.network.worker_security_group_id]

  environment = merge(
    module.app_config.env,
    lookup(module.app_config.service_env, each.key, {}),
    { CP_SERVICE_NAME = "controlplane-${each.key}" },
    lookup(local.service_env_overrides, each.key, {}),
  )
  runtime_secret_arns = lookup(module.secrets.secrets_by_service, each.key, [])
  secrets_kms_key_arn = module.kms.secrets_key_arn
  ecr_repository_arns = [module.ecs_cluster.ecr_repository_arns[each.key]]

  logs_kms_key_arn   = module.kms.logs_key_arn
  log_retention_days = var.retention_days.operational_log

  container_port          = each.key == "api" ? 8080 : null
  target_group_arn        = each.key == "api" ? module.waf_edge.target_group_arn : null
  extra_target_group_arns = each.key == "api" ? [module.waf_edge.liveness_target_group_arn] : []

  # Autoscaling is for the api alone: the workers claim their own work from
  # Postgres, so their throughput is a sizing decision, not a load signal.
  autoscaling_max_capacity        = each.key == "api" ? var.api_autoscaling.max_capacity : null
  autoscaling_min_capacity        = var.api_autoscaling.min_capacity
  autoscaling_cpu_target          = var.api_autoscaling.cpu_target
  autoscaling_requests_per_target = var.api_autoscaling.requests_per_target
  autoscaling_alb_resource_label  = each.key == "api" ? module.waf_edge.autoscaling_resource_label : null

  task_policy_statements = lookup(local.task_policies, each.key, [])
  otel_sidecar_enabled   = var.otel_sidecar_enabled
  tags                   = local.tags

  depends_on = [module.waf_edge]
}

# One-shot role bootstrap (PART 101): runs bootstrap/roles.sql as the RDS
# master user with role passwords injected from Secrets Manager.
module "db_bootstrap" {
  source                   = "../../modules/ecs-service"
  permissions_boundary_arn = var.permissions_boundary_arn

  name_prefix    = local.name_prefix
  service_name   = "db-bootstrap"
  aws_region     = var.aws_region
  aws_account_id = var.aws_account_id
  cluster_arn    = module.ecs_cluster.cluster_arn
  cluster_name   = module.ecs_cluster.cluster_name

  image          = var.db_bootstrap_image
  cpu            = 256
  memory         = 512
  desired_count  = 0
  create_service = false
  container_user = "70:70"

  subnet_ids         = module.network.private_app_subnet_ids
  security_group_ids = [module.network.worker_security_group_id]

  environment = {
    PGHOST                  = module.rds.address
    PGPORT                  = tostring(module.rds.port)
    PGDATABASE              = module.rds.database_name
    PGSSLMODE               = "require"
    ROLES_SQL               = module.rds.bootstrap_roles_sql
    CP_APP_CONNECTION_LIMIT = tostring(var.db_app_connection_limit)
  }
  injected_secrets = {
    PGUSER               = "${module.rds.master_user_secret_arn}:username::"
    PGPASSWORD           = "${module.rds.master_user_secret_arn}:password::"
    CP_MIGRATE_PASSWORD  = module.secrets.secret_arns["database/roles/cp_migrate-password"]
    CP_APP_PASSWORD      = module.secrets.secret_arns["database/roles/cp_app-password"]
    CP_READONLY_PASSWORD = module.secrets.secret_arns["database/roles/cp_readonly-password"]
    CP_OPS_PASSWORD      = module.secrets.secret_arns["database/roles/cp_ops-password"]
  }
  command = [
    "sh", "-c",
    "printf '%s' \"$ROLES_SQL\" | psql -v ON_ERROR_STOP=1 -v cp_migrate_password=\"$CP_MIGRATE_PASSWORD\" -v cp_app_password=\"$CP_APP_PASSWORD\" -v cp_readonly_password=\"$CP_READONLY_PASSWORD\" -v cp_ops_password=\"$CP_OPS_PASSWORD\" -v cp_app_connection_limit=\"$CP_APP_CONNECTION_LIMIT\" -f -",
  ]

  secrets_kms_key_arn = module.kms.secrets_key_arn
  logs_kms_key_arn    = module.kms.logs_key_arn
  log_retention_days  = var.retention_days.operational_log
  tags                = local.tags
}

# ---------------------------------------------------------------------------
# Deployment identity and alerting
# ---------------------------------------------------------------------------

# Created only once the repository is known. count rather than a comment,
# because a module that must not be applied yet should be impossible to apply
# rather than merely discouraged.
module "iam_deploy" {
  count                    = (var.github_org != "" && var.github_repo != "") ? 1 : 0
  source                   = "../../modules/iam-deploy"
  permissions_boundary_arn = var.permissions_boundary_arn

  name_prefix          = local.name_prefix
  aws_region           = var.aws_region
  aws_account_id       = var.aws_account_id
  github_org           = var.github_org
  github_repo          = var.github_repo
  allowed_environments = var.github_deploy_environments
  create_oidc_provider = var.create_github_oidc_provider
  oidc_provider_arn    = var.github_oidc_provider_arn
  ecr_repository_arns  = values(module.ecs_cluster.ecr_repository_arns)
  ecs_cluster_arn      = module.ecs_cluster.cluster_arn
  ecs_cluster_name     = module.ecs_cluster.cluster_name
  ecs_service_arns     = [for k, s in module.services : s.service_arn if s.service_arn != null]
  pass_role_arns = concat(
    flatten([for k, s in module.services : [s.task_role_arn, s.execution_role_arn]]),
    [module.db_bootstrap.task_role_arn, module.db_bootstrap.execution_role_arn],
  )
  migrate_task_definition_family = module.services["migrate"].task_definition_family
  migrate_log_group_arn          = module.services["migrate"].log_group_arn
  tags                           = local.tags
}

module "observability" {
  source = "../../modules/observability"

  name_prefix              = local.name_prefix
  aws_region               = var.aws_region
  kms_key_arn              = module.kms.logs_key_arn
  sev1_email_endpoints     = var.sev1_email_endpoints
  sev2_email_endpoints     = var.sev2_email_endpoints
  sev1_https_endpoints     = var.sev1_https_endpoints
  sev2_https_endpoints     = var.sev2_https_endpoints
  rds_instance_identifier  = module.rds.identifier
  rds_replica_identifiers  = module.rds.replica_identifiers
  alb_arn_suffix           = module.waf_edge.alb_arn_suffix
  target_group_arn_suffix  = module.waf_edge.target_group_arn_suffix
  ecs_cluster_name         = module.ecs_cluster.cluster_name
  ecs_service_names        = [for k in local.long_running : module.services[k].service_name]
  redis_node_ids           = [for i in range(var.redis_num_cache_clusters) : format("%s-%03d", module.redis.replication_group_id, i + 1)]
  custom_metric_dimensions = var.custom_metric_dimensions

  api_log_group_name               = module.services["api"].log_group_name
  liveness_target_group_arn_suffix = module.waf_edge.liveness_target_group_arn_suffix

  # The cp_app role CONNECTION LIMIT is the real ceiling, not the instance's
  # max_connections: hitting it stops new tasks from starting.
  db_connection_warn_threshold     = ceil(var.db_app_connection_limit * 0.6)
  db_connection_critical_threshold = ceil(var.db_app_connection_limit * 0.85)

  relay_lag_sev2_seconds = var.relay_lag_thresholds.sev2_seconds
  relay_lag_sev1_seconds = var.relay_lag_thresholds.sev1_seconds

  tags = local.tags
}

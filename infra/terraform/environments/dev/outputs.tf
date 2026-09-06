output "alb_dns_name" {
  value = module.waf_edge.alb_dns_name
}

output "cloudfront_domain_name" {
  value = module.waf_edge.cloudfront_domain_name
}

output "rds_endpoint" {
  description = "Writer host:port. Credentials live only in Secrets Manager."
  value       = module.rds.endpoint
}

output "rds_replica_addresses" {
  value = module.rds.replica_addresses
}

output "redis_primary_endpoint" {
  value = module.redis.primary_endpoint_address
}

output "buckets" {
  value = {
    raw_events        = module.s3_evidence.raw_events_bucket
    provider_evidence = module.s3_evidence.provider_evidence_bucket
    audit_archive     = module.s3_evidence.audit_archive_bucket
    access_logs       = module.s3_evidence.access_logs_bucket
    alb_logs          = module.waf_edge.alb_logs_bucket
  }
}

output "kms_key_arns" {
  value = {
    rds           = module.kms.rds_key_arn
    s3            = module.kms.s3_key_arn
    secrets       = module.kms.secrets_key_arn
    logs          = module.kms.logs_key_arn
    audit_signing = module.kms.audit_signing_key_arn
  }
}

output "task_role_arns" {
  value = merge(
    { for k, s in module.services : k => s.task_role_arn },
    { "db-bootstrap" = module.db_bootstrap.task_role_arn },
  )
}

output "deploy_role_arn" {
  value = module.iam_deploy.deploy_role_arn
}

output "ecr_repository_urls" {
  value = module.ecs_cluster.ecr_repository_urls
}

output "ecs_cluster_name" {
  value = module.ecs_cluster.cluster_name
}

output "migrate_task_definition" {
  value = module.services["migrate"].task_definition_arn
}

output "db_bootstrap_task_definition" {
  value = module.db_bootstrap.task_definition_arn
}

output "secret_name_prefix" {
  value = module.secrets.secret_name_prefix
}

output "sns_topics" {
  value = {
    sev1 = module.observability.sev1_topic_arn
    sev2 = module.observability.sev2_topic_arn
  }
}

output "fake_provider_slots" {
  description = "Non-empty only in DEV."
  value       = module.app_config.fake_provider_slots
}

output "service_names" {
  description = "Long-running ECS services (migrate is a task definition, not a service)."
  value       = { for k in local.long_running : k => module.services[k].service_name }
}

output "enabled_capabilities" {
  description = "Capabilities this deployment's configuration permits. Empty means no capability can pass condition 1 of the gate check, whatever the gate rows say."
  value       = module.app_config.enabled_capabilities
}

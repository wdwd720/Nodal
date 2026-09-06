output "env" {
  description = "CP_* map shared by every binary (merge per-service overrides such as CP_SERVICE_NAME on top)."
  value       = merge(local.common_env, local.provider_env)
}

output "readonly_database_url_ref" {
  description = "SecretRef for cp_readonly; only the audit-worker receives it."
  value       = "${local.ref}/database/readonly-url"
}

output "fake_provider_slots" {
  description = "Slots configured with mode=fake (must be empty in STAGING/PROD; config.Validate refuses otherwise)."
  value       = [for slot, p in var.provider_slots : slot if p.mode == "fake"]
}

output "service_env" {
  description = "Binary -> the CP_* variables only that binary reads. Merge on top of `env` for that service and on no other."
  value = {
    api              = local.api_env
    relay-worker     = local.relay_worker_env
    execution-worker = local.execution_worker_env
    workflow-worker  = local.workflow_worker_env
  }
}

output "enabled_capabilities" {
  description = "Capabilities this deployment's configuration permits. Necessary but never sufficient: activation still needs a persisted, dual-approved, in-window gate row (PART 54)."
  value       = var.api.enabled_capabilities
}

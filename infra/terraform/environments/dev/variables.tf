# DEV: small, single NAT, fake providers permitted (config accepts fake only
# in LOCAL/TEST/DEV), short Object Lock retention so the environment can be
# torn down. Everything else mirrors production shape.

variable "aws_region" {
  type    = string
  default = "us-east-1"
}

variable "aws_account_id" {
  description = "Dev account. The provider refuses to run against any other account."
  type        = string
  validation {
    condition     = can(regex("^[0-9]{12}$", var.aws_account_id))
    error_message = "aws_account_id must be a 12-digit AWS account id."
  }
}

# Every resource name and the Project tag derive from this, and the deployment
# identity is scoped to both: nodal-terraform may only create names beginning
# "nodal-" and may only mutate resources tagged Project=nodal. Changing it
# without changing infra/aws/*.json produces an AccessDenied at apply time, not
# a rename. It was "cp" until 2026-09-08, which no policy anywhere allowed.
variable "project" {
  type    = string
  default = "nodal"

  validation {
    condition     = var.project == "nodal"
    error_message = "The deployment identity is scoped to nodal-* names and the Project=nodal tag; see infra/aws/BOOTSTRAP.md."
  }
}

variable "extra_tags" {
  type    = map(string)
  default = {}
}

variable "image_tag" {
  description = "Git SHA of the release to run (release.yml tags every image with it)."
  type        = string
}

variable "availability_zones" {
  type    = list(string)
  default = ["us-east-1a", "us-east-1b", "us-east-1c"]
}

variable "vpc_cidr" {
  type    = string
  default = "10.10.0.0/16"
}

variable "acm_certificate_arn" {
  description = "Regional ACM certificate for the ALB."
  type        = string
}

variable "public_product_name" {
  type = string
}

variable "public_base_url" {
  type = string
  validation {
    condition     = startswith(var.public_base_url, "https://")
    error_message = "public_base_url must be https."
  }
}

variable "cors_origins" {
  type    = list(string)
  default = []
}

variable "auth" {
  type = object({
    issuer          = string
    client_id       = string
    redirect_url    = string
    cookie_name     = optional(string, "cp_session")
    cookie_domain   = optional(string, "")
    session_ttl     = optional(string, "12h")
    step_up_max_age = optional(string, "5m")
  })
}

variable "provider_slots" {
  description = "Provider slots. DEV may use fake adapters for development flows; staging/prod refuse them (PRODUCTION_GATES.md section 4)."
  type = map(object({
    mode     = string
    name     = string
    base_url = optional(string, "")
    timeout  = optional(string, "10s")
  }))
}

variable "redpanda" {
  type = object({
    brokers        = string
    sasl_mechanism = optional(string, "SCRAM-SHA-256")
  })
}

variable "clickhouse" {
  type = object({
    addr     = string
    database = optional(string, "controlplane")
  })
}

variable "temporal" {
  type = object({
    host_port         = string
    namespace         = string
    task_queue_prefix = optional(string, "cp")
  })
}

variable "otel_sidecar_enabled" {
  description = "ADOT sidecar on 127.0.0.1:4317 (insecure OTLP is accepted outside PROD)."
  type        = bool
  default     = true
}

variable "otlp_endpoint" {
  description = "External OTLP gRPC endpoint when the sidecar is disabled."
  type        = string
  default     = ""
}

variable "otlp_insecure" {
  type    = bool
  default = false
}

variable "trace_sample_ratio" {
  type    = string
  default = "1"
}

variable "retention_days" {
  type = object({
    financial_record = number
    security_audit   = number
    raw_market_data  = number
    social_data      = number
    model_io         = number
    operational_log  = number
  })
  default = {
    financial_record = 2555
    security_audit   = 2555
    raw_market_data  = 30
    social_data      = 30
    model_io         = 30
    operational_log  = 14
  }
}

variable "audit_object_lock_retention_days" {
  description = "COMPLIANCE-mode default retention on the audit archive. Short in dev so the bucket can eventually be deleted."
  type        = number
  default     = 30
}

variable "rds_instance_class" {
  type    = string
  default = "db.t4g.medium"
}

variable "rds_allocated_storage" {
  type    = number
  default = 50
}

variable "rds_multi_az" {
  type    = bool
  default = false
}

variable "rds_read_replica_count" {
  type    = number
  default = 0
}

variable "deletion_protection" {
  type    = bool
  default = false
}

variable "redis_node_type" {
  type    = string
  default = "cache.t4g.small"
}

variable "redis_num_cache_clusters" {
  type    = number
  default = 1
}

variable "database_pool" {
  type = object({
    max_conns         = optional(number, 10)
    min_conns         = optional(number, 1)
    statement_timeout = optional(string, "30s")
    lock_timeout      = optional(string, "5s")
  })
  default = {}
}

variable "db_app_connection_limit" {
  type    = number
  default = 100
}

variable "db_bootstrap_image" {
  description = "Image with psql for the one-shot role bootstrap task; pin to a digest before running in staging/prod."
  type        = string
  default     = "public.ecr.aws/docker/library/postgres:16-alpine"
}

variable "service_sizing" {
  description = "Fargate sizing per binary (cpu/memory units, desired tasks). migrate is a one-shot task definition."
  type = map(object({
    cpu           = number
    memory        = number
    desired_count = number
  }))
  default = {
    api                   = { cpu = 512, memory = 1024, desired_count = 1 }
    relay-worker          = { cpu = 512, memory = 1024, desired_count = 1 }
    execution-worker      = { cpu = 512, memory = 1024, desired_count = 1 }
    reconciliation-worker = { cpu = 512, memory = 1024, desired_count = 1 }
    market-ingest-worker  = { cpu = 512, memory = 1024, desired_count = 1 }
    agent-worker          = { cpu = 512, memory = 1024, desired_count = 1 }
    workflow-worker       = { cpu = 512, memory = 1024, desired_count = 1 }
    audit-worker          = { cpu = 256, memory = 512, desired_count = 1 }
    migrate               = { cpu = 256, memory = 512, desired_count = 0 }
  }
  validation {
    condition = toset(keys(var.service_sizing)) == toset([
      "api", "relay-worker", "execution-worker", "reconciliation-worker", "market-ingest-worker",
      "agent-worker", "workflow-worker", "audit-worker", "migrate",
    ])
    error_message = "service_sizing must cover exactly the nine binaries of SYSTEM.md section 2."
  }
}

variable "auditor_principal_arns" {
  description = "Principals allowed kms:Verify on the audit signing key."
  type        = list(string)
  default     = []
}

variable "key_admin_principal_arns" {
  type    = list(string)
  default = []
}

variable "secret_admin_principal_arns" {
  description = "Operators who populate secret values after bootstrap."
  type        = list(string)
  default     = []
}

variable "secret_rotation" {
  type = map(object({
    lambda_arn               = string
    automatically_after_days = number
  }))
  default = {}
}

variable "cloudfront_enabled" {
  type    = bool
  default = false
}

variable "cloudfront_acm_certificate_arn" {
  type    = string
  default = null
}

variable "cloudfront_aliases" {
  type    = list(string)
  default = []
}

variable "cloudfront_origin_domain_name" {
  type    = string
  default = null
}

variable "cloudfront_origin_secret" {
  type      = string
  default   = null
  sensitive = true
}

variable "waf_rate_limit" {
  type    = number
  default = 2000
}

variable "github_org" {
  type = string
}

variable "github_repo" {
  type = string
}

variable "create_github_oidc_provider" {
  type    = bool
  default = true
}

variable "github_oidc_provider_arn" {
  type    = string
  default = null
}

variable "sev1_email_endpoints" {
  type    = list(string)
  default = []
}

variable "sev2_email_endpoints" {
  type    = list(string)
  default = []
}

variable "sev1_https_endpoints" {
  type    = list(string)
  default = []
}

variable "sev2_https_endpoints" {
  type    = list(string)
  default = []
}

variable "custom_metric_dimensions" {
  description = "Dimensions the collector attaches to application metrics (must match for alarms to fire)."
  type        = map(string)
  default     = {}
}

# ---------------------------------------------------------------------------
# Per-binary configuration (modules/app-config `service_env`). Each block is
# read by exactly one command; its package comment is the reference.
# ---------------------------------------------------------------------------

variable "api" {
  description = "cmd/api. enabled_capabilities is condition 1 of five and activates nothing on its own: the persisted, dual-approved, in-window gate row decides (PART 54)."
  type = object({
    settlement_chain     = optional(string, "")
    settlement_mint      = optional(string, "")
    funding_network      = optional(string, "solana")
    funding_currency     = optional(string, "usdc")
    enabled_capabilities = optional(list(string), [])
    shutdown_timeout     = optional(string, "25s")
    request_timeout      = optional(string, "")
    rate_limit_general   = optional(string, "600/1m")
    rate_limit_auth      = optional(string, "30/1m")
    rate_limit_quote     = optional(string, "120/1m")
    rate_limit_command   = optional(string, "120/1m")
  })
  default = {}
}

variable "relay_worker" {
  description = "cmd/relay-worker. Multiple replicas are safe and are the intended shape (D-036); exclusive = true is an operator switch for a deliberate single publisher and costs drain rate and failover latency (D-037)."
  type = object({
    batch_size         = optional(number, 100)
    poll_interval      = optional(string, "250ms")
    max_interval       = optional(string, "10s")
    retry_backoff_base = optional(string, "1s")
    retry_backoff_max  = optional(string, "5m")
    run_timeout        = optional(string, "30s")
    drain_timeout      = optional(string, "25s")
    sample_interval    = optional(string, "15s")
    exclusive          = optional(bool, false)
  })
  default = {}
  validation {
    condition     = var.relay_worker.exclusive == false
    error_message = "CP_RELAY_WORKER_EXCLUSIVE is off by default and protects nothing since D-036; turning it on here needs a decision-register entry first."
  }
}

variable "execution_worker" {
  description = "cmd/execution-worker."
  type = object({
    concurrency   = optional(number, 4)
    poll_interval = optional(string, "500ms")
    lease_ttl     = optional(string, "60s")
    drain_timeout = optional(string, "25s")
  })
  default = {}
}

variable "workflow_worker" {
  description = "cmd/workflow-worker. Temporal connectivity itself comes from var.temporal."
  type = object({
    max_activities = optional(number, 16)
    max_workflows  = optional(number, 16)
    drain_timeout  = optional(string, "25s")
  })
  default = {}
}

variable "relay_lag_thresholds" {
  description = "Seconds of outbox lag that raise SEV2 and SEV1. A healthy relay is single-digit seconds behind commit."
  type = object({
    sev2_seconds = optional(number, 120)
    sev1_seconds = optional(number, 900)
  })
  default = {}
  validation {
    condition     = var.relay_lag_thresholds.sev1_seconds > var.relay_lag_thresholds.sev2_seconds
    error_message = "The SEV1 lag threshold must be above the SEV2 one."
  }
}

variable "api_autoscaling" {
  description = "cmd/api is the only autoscaled service. Dev keeps the ceiling low: this environment exists to prove the wiring, not to absorb load. Application Auto Scaling owns the count once max_capacity is set, so service_sizing.api.desired_count is only the starting point."
  type = object({
    min_capacity        = optional(number, 1)
    max_capacity        = optional(number, 2)
    cpu_target          = optional(number, 70)
    requests_per_target = optional(number, 800)
  })
  default = {}
  validation {
    condition     = var.api_autoscaling.max_capacity >= var.api_autoscaling.min_capacity
    error_message = "api_autoscaling.max_capacity must be at least min_capacity."
  }
}

# Required, with no default. The deployment identity cannot create a role
# without it, so a missing value must fail at plan time and not at apply time
# with an AccessDenied nobody expects.
variable "permissions_boundary_arn" {
  type        = string
  description = "IAM permissions boundary applied to every role this environment creates."
}

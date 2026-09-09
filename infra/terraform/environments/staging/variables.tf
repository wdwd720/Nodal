# STAGING: production-shaped (Multi-AZ, per-AZ NAT, deletion protection are
# fixed in main.tf), in its own AWS account with its own database, secrets,
# provider configuration and capability state (PART 144). Providers run in
# sandbox mode where officially supported; fake adapters are refused both
# here (validation) and at runtime (config.Validate NO_FAKE_PROVIDERS).

variable "aws_region" {
  type    = string
  default = "us-east-1"
}

variable "aws_account_id" {
  description = "Staging account id. Must differ from the production account; the provider refuses any other account."
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
  default = "10.20.0.0/16"
}

variable "acm_certificate_arn" {
  description = "Regional ACM certificate for the ALB."
  type        = string
}

variable "public_product_name" {
  type = string
  validation {
    condition     = length(trimspace(var.public_product_name)) > 0
    error_message = "public_product_name must be non-empty (config.Validate PUBLIC_PRODUCT_NAME)."
  }
}

variable "public_base_url" {
  type = string
  validation {
    condition     = startswith(var.public_base_url, "https://")
    error_message = "public_base_url must be https (config.Validate PUBLIC_BASE_URL_HTTPS)."
  }
}

variable "cors_origins" {
  type    = list(string)
  default = []
  validation {
    condition     = !contains(var.cors_origins, "*")
    error_message = "CORS wildcard is rejected in STAGING/PROD (config.Validate NO_CORS_WILDCARD)."
  }
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
  validation {
    condition     = startswith(var.auth.issuer, "https://") && startswith(var.auth.redirect_url, "https://")
    error_message = "OIDC issuer and redirect URL must be https in STAGING/PROD."
  }
}

variable "provider_slots" {
  description = "Provider slots. Sandbox or live only: fake adapters are forbidden in STAGING (PRODUCTION_GATES.md section 4)."
  type = map(object({
    mode     = string
    name     = string
    base_url = optional(string, "")
    timeout  = optional(string, "10s")
  }))
  validation {
    condition     = alltrue([for p in values(var.provider_slots) : p.mode != "fake"])
    error_message = "mode = \"fake\" is not allowed in STAGING (config.Validate NO_FAKE_PROVIDERS would refuse to start)."
  }
  validation {
    condition     = alltrue([for p in values(var.provider_slots) : length(trimspace(p.name)) > 0])
    error_message = "Every non-fake provider slot needs an adapter name."
  }
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
  description = "ADOT sidecar on 127.0.0.1:4317. Insecure localhost OTLP is accepted in STAGING (NO_INSECURE_OTLP applies to PROD only)."
  type        = bool
  default     = true
}

variable "otlp_endpoint" {
  type    = string
  default = ""
}

variable "otlp_insecure" {
  type    = bool
  default = false
}

variable "trace_sample_ratio" {
  type    = string
  default = "0.25"
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
    raw_market_data  = 90
    social_data      = 30
    model_io         = 90
    operational_log  = 30
  }
  validation {
    condition     = var.retention_days.financial_record > 0 && var.retention_days.security_audit > 0
    error_message = "Financial-record and security-audit retention must be > 0 (config.Validate RETENTION_NON_ZERO)."
  }
}

variable "audit_object_lock_retention_days" {
  description = "COMPLIANCE-mode default retention on the audit archive. Shorter than prod so staging drills can eventually be cleaned up."
  type        = number
  default     = 90
}

variable "rds_instance_class" {
  type    = string
  default = "db.r6g.large"
}

variable "rds_allocated_storage" {
  type    = number
  default = 100
}

variable "rds_read_replica_count" {
  type    = number
  default = 0
}

variable "redis_node_type" {
  type    = string
  default = "cache.r7g.large"
}

variable "redis_num_cache_clusters" {
  type    = number
  default = 2
  validation {
    condition     = var.redis_num_cache_clusters >= 2
    error_message = "Staging is production-shaped: at least two Redis nodes (automatic failover, Multi-AZ)."
  }
}

variable "database_pool" {
  type = object({
    max_conns         = optional(number, 20)
    min_conns         = optional(number, 2)
    statement_timeout = optional(string, "30s")
    lock_timeout      = optional(string, "5s")
  })
  default = {}
}

variable "db_app_connection_limit" {
  type    = number
  default = 200
}

variable "db_bootstrap_image" {
  description = "Image with psql for the one-shot role bootstrap task. Pin to a digest (image@sha256:...)."
  type        = string
  default     = "public.ecr.aws/docker/library/postgres:16-alpine"
}

variable "service_sizing" {
  type = map(object({
    cpu           = number
    memory        = number
    desired_count = number
  }))
  default = {
    api                   = { cpu = 1024, memory = 2048, desired_count = 2 }
    relay-worker          = { cpu = 512, memory = 1024, desired_count = 2 }
    execution-worker      = { cpu = 1024, memory = 2048, desired_count = 1 }
    reconciliation-worker = { cpu = 1024, memory = 2048, desired_count = 1 }
    market-ingest-worker  = { cpu = 1024, memory = 2048, desired_count = 1 }
    agent-worker          = { cpu = 1024, memory = 2048, desired_count = 1 }
    workflow-worker       = { cpu = 1024, memory = 2048, desired_count = 1 }
    audit-worker          = { cpu = 512, memory = 1024, desired_count = 1 }
    migrate               = { cpu = 512, memory = 1024, desired_count = 0 }
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
  type    = list(string)
  default = []
}

variable "key_admin_principal_arns" {
  type    = list(string)
  default = []
}

variable "secret_admin_principal_arns" {
  type    = list(string)
  default = []
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

# Empty until the Nodal GitHub repository exists. While either is empty the
# iam-deploy module is not instantiated at all, so a deployment can be applied
# without inventing a repository name -- and the CI trust arrives later by
# setting two variables rather than by editing code.
variable "github_org" {
  type    = string
  default = ""
}

variable "github_repo" {
  type    = string
  default = ""
}

variable "github_deploy_environments" {
  description = "GitHub Environments whose jobs may assume the deploy role."
  type        = list(string)
  default     = ["production"]
}

# Default false, and it matters. Account 049286562577 already has
# arn:aws:iam::049286562577:oidc-provider/token.actions.githubusercontent.com,
# created 2026-09-04 and trusted by bdg-github-deployer for another project's
# CI. There is one such provider per account per issuer, so creating it here
# would either fail with EntityAlreadyExists or, worse, take ownership of it --
# and `terraform destroy` would then delete the thing another project's
# deployments authenticate against.
variable "create_github_oidc_provider" {
  type    = bool
  default = false
}

# The existing provider, referenced rather than managed.
variable "github_oidc_provider_arn" {
  type    = string
  default = "arn:aws:iam::049286562577:oidc-provider/token.actions.githubusercontent.com"
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
  type    = map(string)
  default = {}
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
  validation {
    condition     = trimspace(var.api.settlement_chain) != "" && trimspace(var.api.settlement_mint) != ""
    error_message = "cmd/api refuses to start without CP_API_SETTLEMENT_CHAIN and CP_API_SETTLEMENT_MINT outside DEV (wire.go resolveSettlementAsset)."
  }
  validation {
    condition = length(setsubtract(toset(var.api.enabled_capabilities), toset([
      "LIVE_FUNDING", "LIVE_MANUAL_TRADING", "LIVE_AGENT_TRADING", "WITHDRAWALS", "SOCIAL_DATA_PERSISTENCE",
      "MARKETPLACE", "CROSS_CHAIN", "PREDICTION_MARKETS", "SECURITIES", "CEX_TRADING",
    ]))) == 0
    error_message = "enabled_capabilities must name capabilities from internal/gates; an unknown name is silently ignored by the binary and would look enabled here."
  }
  validation {
    condition     = alltrue([for v in [var.api.rate_limit_general, var.api.rate_limit_auth, var.api.rate_limit_quote, var.api.rate_limit_command] : can(regex("^[1-9][0-9]*/[0-9]+(ms|s|m|h)$", trimspace(v)))])
    error_message = "Every transport rate limit must be \"<requests>/<window>\" with a positive count, e.g. 600/1m. \"off\", \"0\" and a zero count all disable the limiter, which cmd/api refuses in STAGING and PROD."
  }
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
  description = "cmd/api is the only autoscaled service. Staging holds two tasks so an AZ can be lost while a load run is in flight. Application Auto Scaling owns the count once max_capacity is set, so service_sizing.api.desired_count is only the starting point."
  type = object({
    min_capacity        = optional(number, 2)
    max_capacity        = optional(number, 4)
    cpu_target          = optional(number, 65)
    requests_per_target = optional(number, 600)
  })
  default = {}
  validation {
    condition     = var.api_autoscaling.max_capacity >= var.api_autoscaling.min_capacity
    error_message = "api_autoscaling.max_capacity must be at least min_capacity."
  }
  validation {
    condition     = var.api_autoscaling.min_capacity >= 2
    error_message = "The api floor is at least two tasks so losing one AZ is not an outage."
  }
}

# Required, with no default. The deployment identity cannot create a role
# without it, so a missing value must fail at plan time and not at apply time
# with an AccessDenied nobody expects.
variable "permissions_boundary_arn" {
  type        = string
  description = "IAM permissions boundary applied to every role this environment creates."
}

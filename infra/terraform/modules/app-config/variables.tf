variable "environment" {
  description = "CP_ENV: DEV, STAGING or PROD (LOCAL/TEST never run on AWS)."
  type        = string
  validation {
    condition     = contains(["DEV", "STAGING", "PROD"], var.environment)
    error_message = "environment must be DEV, STAGING or PROD."
  }
}

variable "aws_region" {
  type = string
}

variable "secret_name_prefix" {
  description = "Secrets Manager prefix used for every aws-sm:// reference (cp/<env>)."
  type        = string
}

variable "public_product_name" {
  type = string
}

variable "public_base_url" {
  type = string
}

variable "cors_origins" {
  type    = list(string)
  default = []
}

variable "trusted_proxy_cidrs" {
  description = "CIDRs whose X-Forwarded-For is trusted (the VPC CIDR: ALB nodes live there)."
  type        = list(string)
  default     = []
}

variable "http" {
  type = object({
    addr           = optional(string, "0.0.0.0:8080")
    read_timeout   = optional(string, "10s")
    write_timeout  = optional(string, "30s")
    idle_timeout   = optional(string, "120s")
    max_body_bytes = optional(number, 1048576)
  })
  default = {}
}

variable "database" {
  type = object({
    max_conns         = optional(number, 20)
    min_conns         = optional(number, 2)
    statement_timeout = optional(string, "30s")
    lock_timeout      = optional(string, "5s")
  })
  default = {}
}

# An empty address means the deployment does not have this system, and none of
# its CP_* variables are then set on any task. It is not an oversight and it is
# not a placeholder: internal/config requires each of these only of the binaries
# that dial it, so an API-only deployment leaves all three empty and every
# binary it runs still starts.
variable "redpanda" {
  type = object({
    brokers        = optional(string, "")
    sasl_mechanism = optional(string, "SCRAM-SHA-256")
  })
  default = {}
}

variable "clickhouse" {
  type = object({
    addr     = optional(string, "")
    database = optional(string, "controlplane")
  })
  default = {}
}

variable "temporal" {
  type = object({
    host_port         = optional(string, "")
    namespace         = optional(string, "")
    task_queue_prefix = optional(string, "cp")
  })
  default = {}
}

variable "archive" {
  type = object({
    raw_bucket      = string
    evidence_bucket = string
    audit_bucket    = string
  })
}

variable "kms_audit_signing_key_id" {
  type = string
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
  description = "Provider slot -> mode/name/base_url/timeout. Keys must be exactly the twelve config slots."
  type = map(object({
    mode     = string
    name     = string
    base_url = optional(string, "")
    timeout  = optional(string, "10s")
  }))
  validation {
    condition = toset(keys(var.provider_slots)) == toset([
      "funding", "wallet", "signing", "execution", "market_data", "chain_observer",
      "chain_observer_fallback", "model", "event_bus", "workflow", "archive", "notification",
    ])
    error_message = "provider_slots must define exactly the twelve slots: funding, wallet, signing, execution, market_data, chain_observer, chain_observer_fallback, model, event_bus, workflow, archive, notification."
  }
  validation {
    condition     = alltrue([for p in values(var.provider_slots) : contains(["fake", "sandbox", "live"], p.mode)])
    error_message = "Provider mode must be fake, sandbox or live."
  }
}

variable "telemetry" {
  type = object({
    otlp_endpoint      = string
    otlp_insecure      = bool
    trace_sample_ratio = optional(string, "0.1")
    metrics_interval   = optional(string, "30s")
  })
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
}

# ---------------------------------------------------------------------------
# Per-binary variables. These are read by one command each (its package
# comment is the reference, not .env.example) and are therefore kept out of
# the shared env map; modules/app-config exposes them as `service_env`.
# ---------------------------------------------------------------------------

variable "api" {
  description = "cmd/api only. settlement_chain/settlement_mint identify the USD-pegged settlement asset and are REQUIRED in STAGING and PROD (wire.go resolveSettlementAsset). enabled_capabilities is condition 1 of the five-condition gate check and can never activate anything on its own."
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
    condition     = alltrue([for v in [var.api.rate_limit_general, var.api.rate_limit_auth, var.api.rate_limit_quote, var.api.rate_limit_command] : lower(trimspace(v)) != "off"])
    error_message = "A transport rate limit may not be \"off\" on AWS: cmd/api refuses it in STAGING and PROD (errRateLimitDisabledInProduction)."
  }
  validation {
    condition     = alltrue([for v in [var.api.rate_limit_general, var.api.rate_limit_auth, var.api.rate_limit_quote, var.api.rate_limit_command] : can(regex("^[1-9][0-9]*/[0-9]+(ms|s|m|h)$", trimspace(v)))])
    error_message = "Rate limits are \"<requests>/<window>\" with a positive request count, e.g. 600/1m. A zero count disables the limit exactly like \"off\", which STAGING and PROD refuse."
  }
}

variable "relay_worker" {
  description = "cmd/relay-worker only. exclusive=true elects a single publisher through a Postgres advisory lock; it is an operator switch, not a safety control, and costs drain rate and failover latency (D-037), so it stays false."
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
    condition     = var.relay_worker.sample_interval != "0"
    error_message = "The depth/lag sampler feeds outbox_oldest_unpublished_age, which every relay-lag alarm is written against; it may not be disabled on AWS."
  }
}

variable "execution_worker" {
  description = "cmd/execution-worker only. Concurrency is plans in flight per task; the lease TTL bounds how long a crashed task's plan stays unclaimable."
  type = object({
    concurrency   = optional(number, 4)
    poll_interval = optional(string, "500ms")
    lease_ttl     = optional(string, "60s")
    drain_timeout = optional(string, "25s")
  })
  default = {}
}

variable "workflow_worker" {
  description = "cmd/workflow-worker only (Temporal host/namespace/task queue come from var.temporal)."
  type = object({
    max_activities = optional(number, 16)
    max_workflows  = optional(number, 16)
    drain_timeout  = optional(string, "25s")
  })
  default = {}
}

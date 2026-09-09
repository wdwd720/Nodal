# Assembles the CP_* environment for every binary from typed inputs. The
# variable table is internal/config/load.go (rendered as .env.example); this
# module mirrors it so an environment is explicit about every value and no
# default is applied outside LOCAL/TEST (goal PART 98). Secrets are always
# aws-sm:// references; values never appear here.

locals {
  ref = "aws-sm://${var.secret_name_prefix}"

  slots = [
    "funding", "wallet", "signing", "execution", "market_data", "chain_observer",
    "chain_observer_fallback", "model", "event_bus", "workflow", "archive", "notification",
  ]
  # Slots whose credentials exist as Secrets Manager entries (secrets module matrix).
  api_key_slots = ["funding", "wallet", "signing", "execution", "market_data", "chain_observer", "chain_observer_fallback", "model", "notification"]
  webhook_slots = ["funding", "wallet"]

  provider_env = merge([
    for slot in local.slots : merge(
      {
        "CP_PROVIDER_${upper(slot)}_MODE"     = var.provider_slots[slot].mode
        "CP_PROVIDER_${upper(slot)}_NAME"     = var.provider_slots[slot].name
        "CP_PROVIDER_${upper(slot)}_BASE_URL" = var.provider_slots[slot].base_url
        "CP_PROVIDER_${upper(slot)}_TIMEOUT"  = var.provider_slots[slot].timeout
      },
      contains(local.api_key_slots, slot) ? {
        "CP_PROVIDER_${upper(slot)}_API_KEY_REF" = "${local.ref}/provider/${replace(slot, "_", "-")}/api-key"
      } : {},
      contains(local.webhook_slots, slot) ? {
        "CP_PROVIDER_${upper(slot)}_WEBHOOK_SECRET_REF" = "${local.ref}/provider/${replace(slot, "_", "-")}/webhook-secret"
      } : {},
    )
  ]...)

  common_env = {
    # Core
    CP_ENV                 = var.environment
    CP_PUBLIC_PRODUCT_NAME = var.public_product_name

    # HTTP (api; harmless for workers)
    CP_HTTP_ADDR                = var.http.addr
    CP_HTTP_PUBLIC_BASE_URL     = var.public_base_url
    CP_HTTP_CORS_ORIGINS        = join(",", var.cors_origins)
    CP_HTTP_READ_TIMEOUT        = var.http.read_timeout
    CP_HTTP_WRITE_TIMEOUT       = var.http.write_timeout
    CP_HTTP_IDLE_TIMEOUT        = var.http.idle_timeout
    CP_HTTP_MAX_BODY_BYTES      = tostring(var.http.max_body_bytes)
    CP_HTTP_TRUSTED_PROXY_CIDRS = join(",", var.trusted_proxy_cidrs)

    # Database (URLs are SecretRefs; the readonly URL is set per service)
    CP_DATABASE_APP_URL           = "${local.ref}/database/app-url"
    CP_DATABASE_MIGRATE_URL       = "${local.ref}/database/migrate-url"
    CP_DATABASE_REQUIRE_TLS       = "true"
    CP_DATABASE_MAX_CONNS         = tostring(var.database.max_conns)
    CP_DATABASE_MIN_CONNS         = tostring(var.database.min_conns)
    CP_DATABASE_STATEMENT_TIMEOUT = var.database.statement_timeout
    CP_DATABASE_LOCK_TIMEOUT      = var.database.lock_timeout

    # Rate limiting. The API autoscales, so its counters cannot live in each
    # task's memory: a limit of 100 across three tasks would admit 300. This is
    # also what makes CP_REDIS_* required of the API rather than merely present.
    CP_RATELIMIT_BACKEND = "redis"
    # Declared, not guessed: internal/config refuses process-local rate-limit
    # counters unless exactly one process serves HTTP, and it cannot see the
    # task count itself. This must agree with api_autoscaling.min_capacity.
    CP_HTTP_REPLICAS = tostring(var.http_replicas)

    # Redis
    CP_REDIS_URL         = "${local.ref}/redis/url"
    CP_REDIS_REQUIRE_TLS = "true"

    # Redpanda, ClickHouse and Temporal are merged in below, and only when the
    # deployment actually has them.

    # Archive (S3 via the task role: no static keys, PART 99)
    CP_ARCHIVE_ENDPOINT             = "https://s3.${var.aws_region}.amazonaws.com"
    CP_ARCHIVE_REGION               = var.aws_region
    CP_ARCHIVE_RAW_BUCKET           = var.archive.raw_bucket
    CP_ARCHIVE_EVIDENCE_BUCKET      = var.archive.evidence_bucket
    CP_ARCHIVE_AUDIT_BUCKET         = var.archive.audit_bucket
    CP_ARCHIVE_OBJECT_LOCK_REQUIRED = "true"
    CP_ARCHIVE_FORCE_PATH_STYLE     = "false"

    # KMS
    CP_KMS_AUDIT_SIGNING_KEY_ID = var.kms_audit_signing_key_id
    CP_KMS_REGION               = var.aws_region

    # Auth (OIDC only; dev/debug auth never reaches AWS)
    CP_AUTH_MODE              = "oidc"
    CP_AUTH_ISSUER            = var.auth.issuer
    CP_AUTH_CLIENT_ID         = var.auth.client_id
    CP_AUTH_CLIENT_SECRET_REF = "${local.ref}/auth/oidc-client-secret"
    CP_AUTH_REDIRECT_URL      = var.auth.redirect_url
    CP_AUTH_COOKIE_NAME       = var.auth.cookie_name
    CP_AUTH_COOKIE_DOMAIN     = var.auth.cookie_domain
    CP_AUTH_COOKIE_SECURE     = "true"
    CP_AUTH_SESSION_TTL       = var.auth.session_ttl
    CP_AUTH_STEP_UP_MAX_AGE   = var.auth.step_up_max_age
    CP_AUTH_DEBUG_ENABLED     = "false"

    # Telemetry
    CP_TELEMETRY_OTLP_ENDPOINT      = var.telemetry.otlp_endpoint
    CP_TELEMETRY_OTLP_INSECURE      = tostring(var.telemetry.otlp_insecure)
    CP_TELEMETRY_TRACE_SAMPLE_RATIO = var.telemetry.trace_sample_ratio
    CP_TELEMETRY_METRICS_INTERVAL   = var.telemetry.metrics_interval

    # Seed: never on AWS
    CP_SEED_ENABLED = "false"

    # Retention classes
    CP_RETENTION_FINANCIAL_RECORD_DAYS = tostring(var.retention_days.financial_record)
    CP_RETENTION_SECURITY_AUDIT_DAYS   = tostring(var.retention_days.security_audit)
    CP_RETENTION_RAW_MARKET_DATA_DAYS  = tostring(var.retention_days.raw_market_data)
    CP_RETENTION_SOCIAL_DATA_DAYS      = tostring(var.retention_days.social_data)
    CP_RETENTION_MODEL_IO_DAYS         = tostring(var.retention_days.model_io)
    CP_RETENTION_OPERATIONAL_LOG_DAYS  = tostring(var.retention_days.operational_log)
  }
}

# ---------------------------------------------------------------------------
# The dependencies a deployment may not have.
#
# internal/config declares dependencies per binary: a variable belonging to an
# external system is required only of a service that dials it, and cmd/api dials
# none of these three. An API-only deployment therefore does not need a broker
# list, an analytics address or a workflow endpoint -- and if this module set
# them anyway, the task definition would contain four endpoints that will never
# be contacted, which is configuration that lies and which the next person
# reads and believes.
#
# So each block appears only when its address is non-empty. Leaving one empty is
# how a deployment says "not this one", and the binaries that genuinely need it
# still refuse to start without it.
# ---------------------------------------------------------------------------

locals {
  redpanda_env = var.redpanda.brokers == "" ? {} : {
    CP_REDPANDA_BROKERS           = var.redpanda.brokers
    CP_REDPANDA_REQUIRE_TLS       = "true"
    CP_REDPANDA_SASL_MECHANISM    = var.redpanda.sasl_mechanism
    CP_REDPANDA_SASL_USERNAME_REF = "${local.ref}/redpanda/sasl-username"
    CP_REDPANDA_SASL_PASSWORD_REF = "${local.ref}/redpanda/sasl-password"
  }

  clickhouse_env = var.clickhouse.addr == "" ? {} : {
    CP_CLICKHOUSE_ADDR         = var.clickhouse.addr
    CP_CLICKHOUSE_DATABASE     = var.clickhouse.database
    CP_CLICKHOUSE_USERNAME_REF = "${local.ref}/clickhouse/username"
    CP_CLICKHOUSE_PASSWORD_REF = "${local.ref}/clickhouse/password"
    CP_CLICKHOUSE_REQUIRE_TLS  = "true"
  }

  temporal_env = var.temporal.host_port == "" ? {} : {
    CP_TEMPORAL_HOST_PORT         = var.temporal.host_port
    CP_TEMPORAL_NAMESPACE         = var.temporal.namespace
    CP_TEMPORAL_TASK_QUEUE_PREFIX = var.temporal.task_queue_prefix
    CP_TEMPORAL_REQUIRE_TLS       = "true"
  }

  optional_dependency_env = merge(local.redpanda_env, local.clickhouse_env, local.temporal_env)
}

# ---------------------------------------------------------------------------
# Per-binary environment. Each map is merged on top of `env` for exactly one
# service; a variable a binary does not read is never set on another's task.
# ---------------------------------------------------------------------------

locals {
  api_env = merge(
    {
      CP_API_SETTLEMENT_CHAIN     = var.api.settlement_chain
      CP_API_SETTLEMENT_MINT      = var.api.settlement_mint
      CP_API_FUNDING_NETWORK      = var.api.funding_network
      CP_API_FUNDING_CURRENCY     = var.api.funding_currency
      CP_API_ENABLED_CAPABILITIES = join(",", var.api.enabled_capabilities)
      CP_API_SHUTDOWN_TIMEOUT     = var.api.shutdown_timeout
      CP_API_RATE_LIMIT_GENERAL   = var.api.rate_limit_general
      CP_API_RATE_LIMIT_AUTH      = var.api.rate_limit_auth
      CP_API_RATE_LIMIT_QUOTE     = var.api.rate_limit_quote
      CP_API_RATE_LIMIT_COMMAND   = var.api.rate_limit_command
    },
    # Unset means "the configured HTTP write timeout", which is what we want
    # unless an operator deliberately shortens it.
    trimspace(var.api.request_timeout) == "" ? {} : { CP_API_REQUEST_TIMEOUT = var.api.request_timeout },
  )

  relay_worker_env = {
    CP_RELAY_WORKER_BATCH_SIZE         = tostring(var.relay_worker.batch_size)
    CP_RELAY_WORKER_POLL_INTERVAL      = var.relay_worker.poll_interval
    CP_RELAY_WORKER_MAX_INTERVAL       = var.relay_worker.max_interval
    CP_RELAY_WORKER_RETRY_BACKOFF_BASE = var.relay_worker.retry_backoff_base
    CP_RELAY_WORKER_RETRY_BACKOFF_MAX  = var.relay_worker.retry_backoff_max
    CP_RELAY_WORKER_RUN_TIMEOUT        = var.relay_worker.run_timeout
    CP_RELAY_WORKER_DRAIN_TIMEOUT      = var.relay_worker.drain_timeout
    CP_RELAY_WORKER_SAMPLE_INTERVAL    = var.relay_worker.sample_interval
    CP_RELAY_WORKER_EXCLUSIVE          = tostring(var.relay_worker.exclusive)
    # The in-process loopback bus has no subscriber outside the process. The
    # opt-in is refused outside LOCAL/TEST/DEV by the binary; it is pinned
    # false here so no environment can ever hand it a silent sink.
    CP_RELAY_WORKER_ALLOW_LOOPBACK_BUS = "false"
  }

  execution_worker_env = {
    CP_EXECUTION_WORKER_CONCURRENCY   = tostring(var.execution_worker.concurrency)
    CP_EXECUTION_WORKER_POLL_INTERVAL = var.execution_worker.poll_interval
    CP_EXECUTION_WORKER_LEASE_TTL     = var.execution_worker.lease_ttl
    CP_EXECUTION_WORKER_DRAIN_TIMEOUT = var.execution_worker.drain_timeout
    # CP_EXECUTION_WORKER_OWNER is deliberately unset: the default host:pid is
    # unique per task, and a shared owner id would let two tasks steal each
    # other's plan leases.
  }

  workflow_worker_env = {
    CP_WORKFLOW_WORKER_MAX_ACTIVITIES = tostring(var.workflow_worker.max_activities)
    CP_WORKFLOW_WORKER_MAX_WORKFLOWS  = tostring(var.workflow_worker.max_workflows)
    CP_WORKFLOW_WORKER_DRAIN_TIMEOUT  = var.workflow_worker.drain_timeout
  }
}

# The settlement asset is required by cmd/api outside DEV; catching it here
# turns a crash-looping service into a plan-time error.
check "api_settlement_asset" {
  assert {
    condition     = var.environment == "DEV" || (trimspace(var.api.settlement_chain) != "" && trimspace(var.api.settlement_mint) != "")
    error_message = "api.settlement_chain and api.settlement_mint are required in ${var.environment}: cmd/api refuses to start without them."
  }
}

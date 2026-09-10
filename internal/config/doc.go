// Package config loads, validates and describes the typed service
// configuration.
//
// Responsibility
//
//   - Read every CP_* environment variable into the typed Config tree (no
//     map[string]string escape hatches) via Load.
//   - Decide, from the Service the caller declares, WHICH external
//     dependencies that binary must be given. Load takes a Service and has no
//     default: a caller that has not said what it is cannot be told what it
//     needs. A variable tagged with a Dependency is required only of a service
//     that declares that dependency, and is still parsed for any service that
//     supplies it -- so a malformed value fails closed everywhere while a
//     missing one is only demanded of the binary that would dial it.
//   - Decide, from the explicit Environment (LOCAL, TEST, DEV, STAGING, PROD),
//     which conveniences are permitted. Development defaults are applied only
//     in LOCAL and TEST; a missing required variable in DEV/STAGING/PROD is a
//     startup error, never a silent default (goal PART 98).
//   - Enforce the production rules with Validate: no fake providers, no dev or
//     debug auth, no seeding, TLS to every data store, no CORS wildcard,
//     archive buckets with Object Lock, KMS signing key, secure cookies, https
//     public URLs, non-zero financial/security-audit retention, non-empty
//     public product name, a configured capability store, no plain-text
//     secrets (goal PARTs 97, 98, 122, 123).
//   - Model secrets as SecretRef (env://, aws-sm://, file://, or a plain value
//     in LOCAL/TEST only) resolved through a Resolver chain, so secret values
//     never live in the Config itself (goal PART 99).
//   - Expose a stable Hash of the non-secret configuration and a Redacted copy
//     for logs (goal PART 222).
//
// This package must never
//
//   - Apply a default outside LOCAL/TEST.
//   - Accept an undeclared Service, or treat one as "requires everything".
//   - Let a dependency's absence pass for a binary that declares it.
//   - Include a SecretRef (value or location) in Hash, or a plain secret in
//     Redacted output.
//   - Parse floating point numbers: TraceSampleRatio stays a decimal string
//     and is converted only inside the telemetry sampler (telemetry is not
//     money, but the config package still never touches float64).
//   - Panic on malformed input; ParseSecretRef is fuzzed.
//   - Import provider, database or other domain packages.
//
// # Environment variables
//
// The authoritative table is Vars (internal/config/load.go); ExampleEnv
// renders it as the repository's .env.example and a test keeps that file and
// this list in sync. All names are prefixed CP_.
//
// Core:
//
//	CP_ENV                          LOCAL|TEST|DEV|STAGING|PROD (required, no default)
//	CP_SERVICE_NAME                 telemetry/log service name
//	CP_PUBLIC_PRODUCT_NAME          customer-facing product name
//
// HTTP:
//
//	CP_HTTP_ADDR                    listen address
//	CP_HTTP_PUBLIC_BASE_URL         externally visible base URL (https in STAGING/PROD)
//	CP_HTTP_CORS_ORIGINS            comma-separated origins ("*" rejected in STAGING/PROD)
//	CP_HTTP_READ_TIMEOUT            duration
//	CP_HTTP_WRITE_TIMEOUT           duration
//	CP_HTTP_IDLE_TIMEOUT            duration
//	CP_HTTP_MAX_BODY_BYTES          integer bytes
//	CP_HTTP_TRUSTED_PROXY_CIDRS     comma-separated CIDRs
//
// Database (Postgres):
//
//	CP_DATABASE_APP_URL             SecretRef: application-role URL
//	CP_DATABASE_MIGRATE_URL         SecretRef: migration-role URL
//	CP_DATABASE_READONLY_URL        SecretRef: read-only URL (optional)
//	CP_DATABASE_OPS_URL             SecretRef: operations-role URL (optional; the retention passes need it)
//	CP_DATABASE_REQUIRE_TLS         bool (true in STAGING/PROD)
//	CP_DATABASE_MAX_CONNS           int32
//	CP_DATABASE_MIN_CONNS           int32
//	CP_DATABASE_STATEMENT_TIMEOUT   duration
//	CP_DATABASE_LOCK_TIMEOUT        duration
//
// Rate limiting (cmd/api only):
//
// API (cmd/api only; both required in STAGING/PROD):
//
//	CP_API_SETTLEMENT_CHAIN            chain of the USD-pegged settlement asset
//	CP_API_SETTLEMENT_MINT             its mint address
//
// Capacity ceilings (cmd/api only; every one 0 is refused):
//
//	CP_CAPACITY_MAX_ACCOUNTS           int64
//	CP_CAPACITY_MAX_PURCHASES_PER_DAY  int64 (rolling 24h)
//	CP_CAPACITY_MAX_AT_RISK_MINOR      int64 (non-terminal funding)
//	CP_CAPACITY_MAX_DATABASE_BYTES     int64 (0 where there is no quota)
//
// Rate limiting (cmd/api only):
//
//	CP_RATELIMIT_BACKEND            memory | redis
//	CP_HTTP_REPLICAS                int; memory counters need exactly 1
//
// Redis (required of cmd/api when CP_RATELIMIT_BACKEND is redis):
//
//	CP_REDIS_URL                    SecretRef: redis URL
//	CP_REDIS_REQUIRE_TLS            bool (true in STAGING/PROD)
//
// Redpanda:
//
//	CP_REDPANDA_BROKERS             comma-separated host:port
//	CP_REDPANDA_REQUIRE_TLS         bool (true in STAGING/PROD)
//	CP_REDPANDA_SASL_MECHANISM      e.g. SCRAM-SHA-256 (optional)
//	CP_REDPANDA_SASL_USERNAME_REF   SecretRef
//	CP_REDPANDA_SASL_PASSWORD_REF   SecretRef
//
// ClickHouse:
//
//	CP_CLICKHOUSE_ADDR              host:port
//	CP_CLICKHOUSE_DATABASE          database name
//	CP_CLICKHOUSE_USERNAME_REF      SecretRef
//	CP_CLICKHOUSE_PASSWORD_REF      SecretRef
//	CP_CLICKHOUSE_REQUIRE_TLS       bool (true in STAGING/PROD)
//
// Temporal:
//
//	CP_TEMPORAL_HOST_PORT           host:port
//	CP_TEMPORAL_NAMESPACE           namespace
//	CP_TEMPORAL_TASK_QUEUE_PREFIX   task queue prefix
//	CP_TEMPORAL_REQUIRE_TLS         bool (true in STAGING/PROD)
//
// Archive (S3-compatible):
//
//	CP_ARCHIVE_BACKEND              s3 | postgres (postgres needs no object store)
//	CP_ARCHIVE_ENDPOINT             endpoint URL
//	CP_ARCHIVE_REGION               region
//	CP_ARCHIVE_RAW_BUCKET           raw payload bucket
//	CP_ARCHIVE_EVIDENCE_BUCKET      provider evidence bucket
//	CP_ARCHIVE_AUDIT_BUCKET         WORM audit bucket
//	CP_ARCHIVE_OBJECT_LOCK_REQUIRED bool (true in STAGING/PROD)
//	CP_ARCHIVE_FORCE_PATH_STYLE     bool
//	CP_ARCHIVE_ACCESS_KEY_REF       SecretRef (optional; prefer IAM roles)
//	CP_ARCHIVE_SECRET_KEY_REF       SecretRef (optional; prefer IAM roles)
//
// KMS:
//
//	CP_KMS_AUDIT_SIGNING_KEY_ID     key id/ARN (required in STAGING/PROD)
//	CP_KMS_REGION                   region
//
// Auth:
//
//	CP_AUTH_MODE                    oidc|dev (dev rejected in STAGING/PROD)
//	CP_AUTH_ISSUER                  OIDC issuer URL
//	CP_AUTH_CLIENT_ID               OIDC client id
//	CP_AUTH_CLIENT_SECRET_REF       SecretRef
//	CP_AUTH_REDIRECT_URL            OIDC redirect URL
//	CP_AUTH_COOKIE_NAME             session cookie name
//	CP_AUTH_COOKIE_DOMAIN           cookie domain (optional)
//	CP_AUTH_COOKIE_SECURE           bool (true in STAGING/PROD)
//	CP_AUTH_SESSION_TTL             duration
//	CP_AUTH_STEP_UP_MAX_AGE         duration
//	CP_AUTH_DEBUG_ENABLED           bool (false in STAGING/PROD)
//
// Providers: for each slot X in CREDIT_PURCHASE, PAYOUT, FUNDING, WALLET,
// SIGNING, EXECUTION, MARKET_DATA, CHAIN_OBSERVER, CHAIN_OBSERVER_FALLBACK,
// MODEL, EVENT_BUS, WORKFLOW, ARCHIVE, NOTIFICATION:
//
//	CP_PROVIDER_X_MODE              fake|sandbox|live (fake rejected in STAGING/PROD)
//	CP_PROVIDER_X_NAME              adapter name (required unless fake)
//	CP_PROVIDER_X_BASE_URL          API base URL (optional)
//	CP_PROVIDER_X_API_KEY_REF       SecretRef
//	CP_PROVIDER_X_WEBHOOK_SECRET_REF SecretRef
//	CP_PROVIDER_X_TIMEOUT           duration
//	CP_PROVIDER_X_ACCOUNT_REF       provider-side account id, asserted at startup
//	CP_PROVIDER_X_SHARED_ACCOUNT    the account also serves systems outside this deployment
//	CP_PROVIDER_X_AVAILABILITY      how far the integration is actually approved for use
//	CP_PROVIDER_X_DESCRIPTOR_PREFIX the static descriptor the provider account puts on a statement
//	CP_PROVIDER_X_DESCRIPTOR_SUFFIX what this product is called on a customer's statement
//
// i.e.
// CP_PROVIDER_CREDIT_PURCHASE_MODE, CP_PROVIDER_CREDIT_PURCHASE_NAME,
// CP_PROVIDER_CREDIT_PURCHASE_BASE_URL,
// CP_PROVIDER_CREDIT_PURCHASE_API_KEY_REF,
// CP_PROVIDER_CREDIT_PURCHASE_WEBHOOK_SECRET_REF,
// CP_PROVIDER_CREDIT_PURCHASE_TIMEOUT,
// CP_PROVIDER_CREDIT_PURCHASE_ACCOUNT_REF,
// CP_PROVIDER_CREDIT_PURCHASE_SHARED_ACCOUNT,
// CP_PROVIDER_CREDIT_PURCHASE_AVAILABILITY,
// CP_PROVIDER_CREDIT_PURCHASE_DESCRIPTOR_PREFIX,
// CP_PROVIDER_CREDIT_PURCHASE_DESCRIPTOR_SUFFIX, CP_PROVIDER_PAYOUT_MODE,
// CP_PROVIDER_PAYOUT_NAME, CP_PROVIDER_PAYOUT_BASE_URL,
// CP_PROVIDER_PAYOUT_API_KEY_REF, CP_PROVIDER_PAYOUT_WEBHOOK_SECRET_REF,
// CP_PROVIDER_PAYOUT_TIMEOUT, CP_PROVIDER_PAYOUT_ACCOUNT_REF,
// CP_PROVIDER_PAYOUT_SHARED_ACCOUNT, CP_PROVIDER_PAYOUT_AVAILABILITY,
// CP_PROVIDER_PAYOUT_DESCRIPTOR_PREFIX, CP_PROVIDER_PAYOUT_DESCRIPTOR_SUFFIX,
// CP_PROVIDER_FUNDING_MODE, CP_PROVIDER_FUNDING_NAME,
// CP_PROVIDER_FUNDING_BASE_URL, CP_PROVIDER_FUNDING_API_KEY_REF,
// CP_PROVIDER_FUNDING_WEBHOOK_SECRET_REF, CP_PROVIDER_FUNDING_TIMEOUT,
// CP_PROVIDER_FUNDING_ACCOUNT_REF, CP_PROVIDER_FUNDING_SHARED_ACCOUNT,
// CP_PROVIDER_FUNDING_AVAILABILITY, CP_PROVIDER_FUNDING_DESCRIPTOR_PREFIX,
// CP_PROVIDER_FUNDING_DESCRIPTOR_SUFFIX, CP_PROVIDER_WALLET_MODE,
// CP_PROVIDER_WALLET_NAME, CP_PROVIDER_WALLET_BASE_URL,
// CP_PROVIDER_WALLET_API_KEY_REF, CP_PROVIDER_WALLET_WEBHOOK_SECRET_REF,
// CP_PROVIDER_WALLET_TIMEOUT, CP_PROVIDER_WALLET_ACCOUNT_REF,
// CP_PROVIDER_WALLET_SHARED_ACCOUNT, CP_PROVIDER_WALLET_AVAILABILITY,
// CP_PROVIDER_WALLET_DESCRIPTOR_PREFIX, CP_PROVIDER_WALLET_DESCRIPTOR_SUFFIX,
// CP_PROVIDER_SIGNING_MODE, CP_PROVIDER_SIGNING_NAME,
// CP_PROVIDER_SIGNING_BASE_URL, CP_PROVIDER_SIGNING_API_KEY_REF,
// CP_PROVIDER_SIGNING_WEBHOOK_SECRET_REF, CP_PROVIDER_SIGNING_TIMEOUT,
// CP_PROVIDER_SIGNING_ACCOUNT_REF, CP_PROVIDER_SIGNING_SHARED_ACCOUNT,
// CP_PROVIDER_SIGNING_AVAILABILITY, CP_PROVIDER_SIGNING_DESCRIPTOR_PREFIX,
// CP_PROVIDER_SIGNING_DESCRIPTOR_SUFFIX, CP_PROVIDER_EXECUTION_MODE,
// CP_PROVIDER_EXECUTION_NAME, CP_PROVIDER_EXECUTION_BASE_URL,
// CP_PROVIDER_EXECUTION_API_KEY_REF,
// CP_PROVIDER_EXECUTION_WEBHOOK_SECRET_REF, CP_PROVIDER_EXECUTION_TIMEOUT,
// CP_PROVIDER_EXECUTION_ACCOUNT_REF, CP_PROVIDER_EXECUTION_SHARED_ACCOUNT,
// CP_PROVIDER_EXECUTION_AVAILABILITY,
// CP_PROVIDER_EXECUTION_DESCRIPTOR_PREFIX,
// CP_PROVIDER_EXECUTION_DESCRIPTOR_SUFFIX, CP_PROVIDER_MARKET_DATA_MODE,
// CP_PROVIDER_MARKET_DATA_NAME, CP_PROVIDER_MARKET_DATA_BASE_URL,
// CP_PROVIDER_MARKET_DATA_API_KEY_REF,
// CP_PROVIDER_MARKET_DATA_WEBHOOK_SECRET_REF,
// CP_PROVIDER_MARKET_DATA_TIMEOUT, CP_PROVIDER_MARKET_DATA_ACCOUNT_REF,
// CP_PROVIDER_MARKET_DATA_SHARED_ACCOUNT,
// CP_PROVIDER_MARKET_DATA_AVAILABILITY,
// CP_PROVIDER_MARKET_DATA_DESCRIPTOR_PREFIX,
// CP_PROVIDER_MARKET_DATA_DESCRIPTOR_SUFFIX, CP_PROVIDER_CHAIN_OBSERVER_MODE,
// CP_PROVIDER_CHAIN_OBSERVER_NAME, CP_PROVIDER_CHAIN_OBSERVER_BASE_URL,
// CP_PROVIDER_CHAIN_OBSERVER_API_KEY_REF,
// CP_PROVIDER_CHAIN_OBSERVER_WEBHOOK_SECRET_REF,
// CP_PROVIDER_CHAIN_OBSERVER_TIMEOUT, CP_PROVIDER_CHAIN_OBSERVER_ACCOUNT_REF,
// CP_PROVIDER_CHAIN_OBSERVER_SHARED_ACCOUNT,
// CP_PROVIDER_CHAIN_OBSERVER_AVAILABILITY,
// CP_PROVIDER_CHAIN_OBSERVER_DESCRIPTOR_PREFIX,
// CP_PROVIDER_CHAIN_OBSERVER_DESCRIPTOR_SUFFIX,
// CP_PROVIDER_CHAIN_OBSERVER_FALLBACK_MODE,
// CP_PROVIDER_CHAIN_OBSERVER_FALLBACK_NAME,
// CP_PROVIDER_CHAIN_OBSERVER_FALLBACK_BASE_URL,
// CP_PROVIDER_CHAIN_OBSERVER_FALLBACK_API_KEY_REF,
// CP_PROVIDER_CHAIN_OBSERVER_FALLBACK_WEBHOOK_SECRET_REF,
// CP_PROVIDER_CHAIN_OBSERVER_FALLBACK_TIMEOUT,
// CP_PROVIDER_CHAIN_OBSERVER_FALLBACK_ACCOUNT_REF,
// CP_PROVIDER_CHAIN_OBSERVER_FALLBACK_SHARED_ACCOUNT,
// CP_PROVIDER_CHAIN_OBSERVER_FALLBACK_AVAILABILITY,
// CP_PROVIDER_CHAIN_OBSERVER_FALLBACK_DESCRIPTOR_PREFIX,
// CP_PROVIDER_CHAIN_OBSERVER_FALLBACK_DESCRIPTOR_SUFFIX,
// CP_PROVIDER_MODEL_MODE, CP_PROVIDER_MODEL_NAME, CP_PROVIDER_MODEL_BASE_URL,
// CP_PROVIDER_MODEL_API_KEY_REF, CP_PROVIDER_MODEL_WEBHOOK_SECRET_REF,
// CP_PROVIDER_MODEL_TIMEOUT, CP_PROVIDER_MODEL_ACCOUNT_REF,
// CP_PROVIDER_MODEL_SHARED_ACCOUNT, CP_PROVIDER_MODEL_AVAILABILITY,
// CP_PROVIDER_MODEL_DESCRIPTOR_PREFIX, CP_PROVIDER_MODEL_DESCRIPTOR_SUFFIX,
// CP_PROVIDER_EVENT_BUS_MODE, CP_PROVIDER_EVENT_BUS_NAME,
// CP_PROVIDER_EVENT_BUS_BASE_URL, CP_PROVIDER_EVENT_BUS_API_KEY_REF,
// CP_PROVIDER_EVENT_BUS_WEBHOOK_SECRET_REF, CP_PROVIDER_EVENT_BUS_TIMEOUT,
// CP_PROVIDER_EVENT_BUS_ACCOUNT_REF, CP_PROVIDER_EVENT_BUS_SHARED_ACCOUNT,
// CP_PROVIDER_EVENT_BUS_AVAILABILITY,
// CP_PROVIDER_EVENT_BUS_DESCRIPTOR_PREFIX,
// CP_PROVIDER_EVENT_BUS_DESCRIPTOR_SUFFIX, CP_PROVIDER_WORKFLOW_MODE,
// CP_PROVIDER_WORKFLOW_NAME, CP_PROVIDER_WORKFLOW_BASE_URL,
// CP_PROVIDER_WORKFLOW_API_KEY_REF, CP_PROVIDER_WORKFLOW_WEBHOOK_SECRET_REF,
// CP_PROVIDER_WORKFLOW_TIMEOUT, CP_PROVIDER_WORKFLOW_ACCOUNT_REF,
// CP_PROVIDER_WORKFLOW_SHARED_ACCOUNT, CP_PROVIDER_WORKFLOW_AVAILABILITY,
// CP_PROVIDER_WORKFLOW_DESCRIPTOR_PREFIX,
// CP_PROVIDER_WORKFLOW_DESCRIPTOR_SUFFIX, CP_PROVIDER_ARCHIVE_MODE,
// CP_PROVIDER_ARCHIVE_NAME, CP_PROVIDER_ARCHIVE_BASE_URL,
// CP_PROVIDER_ARCHIVE_API_KEY_REF, CP_PROVIDER_ARCHIVE_WEBHOOK_SECRET_REF,
// CP_PROVIDER_ARCHIVE_TIMEOUT, CP_PROVIDER_ARCHIVE_ACCOUNT_REF,
// CP_PROVIDER_ARCHIVE_SHARED_ACCOUNT, CP_PROVIDER_ARCHIVE_AVAILABILITY,
// CP_PROVIDER_ARCHIVE_DESCRIPTOR_PREFIX,
// CP_PROVIDER_ARCHIVE_DESCRIPTOR_SUFFIX, CP_PROVIDER_NOTIFICATION_MODE,
// CP_PROVIDER_NOTIFICATION_NAME, CP_PROVIDER_NOTIFICATION_BASE_URL,
// CP_PROVIDER_NOTIFICATION_API_KEY_REF,
// CP_PROVIDER_NOTIFICATION_WEBHOOK_SECRET_REF,
// CP_PROVIDER_NOTIFICATION_TIMEOUT, CP_PROVIDER_NOTIFICATION_ACCOUNT_REF,
// CP_PROVIDER_NOTIFICATION_SHARED_ACCOUNT,
// CP_PROVIDER_NOTIFICATION_AVAILABILITY,
// CP_PROVIDER_NOTIFICATION_DESCRIPTOR_PREFIX,
// CP_PROVIDER_NOTIFICATION_DESCRIPTOR_SUFFIX
//
// Telemetry:
//
//	CP_TELEMETRY_OTLP_ENDPOINT      OTLP gRPC host:port (empty = no-op providers)
//	CP_TELEMETRY_OTLP_INSECURE      bool (rejected in PROD)
//	CP_TELEMETRY_TRACE_SAMPLE_RATIO decimal string in [0,1]
//	CP_TELEMETRY_METRICS_INTERVAL   duration
//
// Seed:
//
//	CP_SEED_ENABLED                 bool (false in STAGING/PROD)
//
// Retention (days):
//
//	CP_RETENTION_LOGIN_ATTEMPT_DAYS     days a login_attempts row survives its expiry
//	CP_RETENTION_FINANCIAL_RECORD_DAYS  > 0 in STAGING/PROD
//	CP_RETENTION_SECURITY_AUDIT_DAYS    > 0 in STAGING/PROD
//	CP_RETENTION_RAW_MARKET_DATA_DAYS
//	CP_RETENTION_SOCIAL_DATA_DAYS
//	CP_RETENTION_MODEL_IO_DAYS
//	CP_RETENTION_OPERATIONAL_LOG_DAYS
//
// # Per-service dependencies
//
// CP_REDIS_*, CP_REDPANDA_*, CP_CLICKHOUSE_*, CP_TEMPORAL_* and CP_ARCHIVE_*
// belong to an external dependency and are required only of the binaries that
// use it. The table is Service.Dependencies (service.go), and it is an audit
// of what each binary actually constructs rather than of what it might one day
// want:
//
//	api                     archive
//	audit-worker            archive
//	market-ingest-worker    redpanda, clickhouse, archive
//	relay-worker            redpanda
//	workflow-worker         temporal
//	agent-worker            (postgres only)
//	execution-worker        (postgres only)
//	reconciliation-worker   (postgres only)
//
// Postgres is deliberately not in the table: every binary that loads
// configuration uses it, so making it conditional would model a choice nobody
// has. Redis is in the table and no service declares it, because nothing in
// the repository constructs a Redis client.
//
// Capability.StoreConfigured has no variable: Load derives it from
// CP_DATABASE_APP_URL. BuildVersion has no variable: it is the package
// variable set with -ldflags.
package config

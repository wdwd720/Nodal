package config

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

// prodEnv returns a complete, valid PROD environment. Every production rule
// test starts from this and applies exactly one mutation.
func prodEnv() map[string]string {
	m := map[string]string{
		"CP_ENV":                 "PROD",
		"CP_SERVICE_NAME":        "controlplane",
		"CP_PUBLIC_PRODUCT_NAME": "Example Product",

		"CP_HTTP_ADDR":                "0.0.0.0:8080",
		"CP_HTTP_PUBLIC_BASE_URL":     "https://api.example.com",
		"CP_HTTP_CORS_ORIGINS":        "https://app.example.com,https://admin.example.com",
		"CP_HTTP_READ_TIMEOUT":        "10s",
		"CP_HTTP_WRITE_TIMEOUT":       "30s",
		"CP_HTTP_IDLE_TIMEOUT":        "120s",
		"CP_HTTP_MAX_BODY_BYTES":      "1048576",
		"CP_HTTP_TRUSTED_PROXY_CIDRS": "10.0.0.0/8,fd00::/8",

		"CP_DATABASE_APP_URL":           "aws-sm://cp/prod/db-app-url",
		"CP_DATABASE_MIGRATE_URL":       "aws-sm://arn:aws:secretsmanager:us-east-1:123456789012:secret:cp/prod/db-migrate-url-AbCdEf",
		"CP_DATABASE_READONLY_URL":      "env://CP_SECRET_DB_READONLY_URL",
		"CP_DATABASE_REQUIRE_TLS":       "true",
		"CP_DATABASE_MAX_CONNS":         "20",
		"CP_DATABASE_MIN_CONNS":         "2",
		"CP_DATABASE_STATEMENT_TIMEOUT": "30s",
		"CP_DATABASE_LOCK_TIMEOUT":      "5s",

		// The API is the only binary this is required of, and STAGING/PROD
		// refuse the memory backend, so the production fixture names the
		// distributed one -- which is also what makes CP_REDIS_* required of
		// the API rather than merely present.
		"CP_RATELIMIT_BACKEND": "redis",
		"CP_HTTP_REPLICAS":     "3",

		"CP_CAPACITY_MAX_ACCOUNTS":          "5000",
		"CP_CAPACITY_MAX_PURCHASES_PER_DAY": "20000",
		"CP_CAPACITY_MAX_AT_RISK_MINOR":     "50000000",
		"CP_CAPACITY_MAX_DATABASE_BYTES":    "0",

		"CP_REDIS_URL":         "aws-sm://cp/prod/redis-url",
		"CP_REDIS_REQUIRE_TLS": "true",

		"CP_REDPANDA_BROKERS":           "b1.example.internal:9092,b2.example.internal:9092",
		"CP_REDPANDA_REQUIRE_TLS":       "true",
		"CP_REDPANDA_SASL_MECHANISM":    "SCRAM-SHA-256",
		"CP_REDPANDA_SASL_USERNAME_REF": "aws-sm://cp/prod/redpanda-username",
		"CP_REDPANDA_SASL_PASSWORD_REF": "aws-sm://cp/prod/redpanda-password",

		"CP_CLICKHOUSE_ADDR":         "clickhouse.example.internal:9440",
		"CP_CLICKHOUSE_DATABASE":     "controlplane",
		"CP_CLICKHOUSE_USERNAME_REF": "aws-sm://cp/prod/clickhouse-username",
		"CP_CLICKHOUSE_PASSWORD_REF": "aws-sm://cp/prod/clickhouse-password",
		"CP_CLICKHOUSE_REQUIRE_TLS":  "true",

		"CP_TEMPORAL_HOST_PORT":         "temporal.example.internal:7233",
		"CP_TEMPORAL_NAMESPACE":         "controlplane-prod",
		"CP_TEMPORAL_TASK_QUEUE_PREFIX": "cp-prod",
		"CP_TEMPORAL_REQUIRE_TLS":       "true",

		"CP_API_SETTLEMENT_CHAIN": "solana",
		"CP_API_SETTLEMENT_MINT":  "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",

		"CP_ARCHIVE_BACKEND":              "s3",
		"CP_ARCHIVE_ENDPOINT":             "https://s3.us-east-1.amazonaws.com",
		"CP_ARCHIVE_REGION":               "us-east-1",
		"CP_ARCHIVE_RAW_BUCKET":           "cp-prod-raw",
		"CP_ARCHIVE_EVIDENCE_BUCKET":      "cp-prod-evidence",
		"CP_ARCHIVE_AUDIT_BUCKET":         "cp-prod-audit",
		"CP_ARCHIVE_OBJECT_LOCK_REQUIRED": "true",
		"CP_ARCHIVE_FORCE_PATH_STYLE":     "false",

		"CP_KMS_AUDIT_SIGNING_KEY_ID": "arn:aws:kms:us-east-1:123456789012:key/11111111-2222-3333-4444-555555555555",
		"CP_KMS_REGION":               "us-east-1",

		"CP_AUTH_MODE":              "oidc",
		"CP_AUTH_ISSUER":            "https://login.example.com",
		"CP_AUTH_CLIENT_ID":         "cp-prod",
		"CP_AUTH_CLIENT_SECRET_REF": "aws-sm://cp/prod/oidc-client-secret",
		"CP_AUTH_REDIRECT_URL":      "https://api.example.com/v1/auth/callback",
		"CP_AUTH_COOKIE_NAME":       "cp_session",
		"CP_AUTH_COOKIE_DOMAIN":     "example.com",
		"CP_AUTH_COOKIE_SECURE":     "true",
		"CP_AUTH_SESSION_TTL":       "12h",
		"CP_AUTH_STEP_UP_MAX_AGE":   "5m",
		"CP_AUTH_DEBUG_ENABLED":     "false",

		"CP_TELEMETRY_OTLP_ENDPOINT":      "otel-collector.example.internal:4317",
		"CP_TELEMETRY_OTLP_INSECURE":      "false",
		"CP_TELEMETRY_TRACE_SAMPLE_RATIO": "0.1",
		"CP_TELEMETRY_METRICS_INTERVAL":   "30s",

		"CP_SEED_ENABLED": "false",

		"CP_RETENTION_LOGIN_ATTEMPT_DAYS":    "2",
		"CP_RETENTION_FINANCIAL_RECORD_DAYS": "2555",
		"CP_RETENTION_SECURITY_AUDIT_DAYS":   "2555",
		"CP_RETENTION_RAW_MARKET_DATA_DAYS":  "90",
		"CP_RETENTION_SOCIAL_DATA_DAYS":      "30",
		"CP_RETENTION_MODEL_IO_DAYS":         "90",
		"CP_RETENTION_OPERATIONAL_LOG_DAYS":  "30",
	}
	for _, slot := range providerSlots() {
		p := "CP_PROVIDER_" + slot.Env + "_"
		m[p+"MODE"] = "live"
		m[p+"NAME"] = "vendor-" + slot.Name
		m[p+"BASE_URL"] = "https://" + slot.Name + ".example.com"
		m[p+"API_KEY_REF"] = "aws-sm://cp/prod/" + slot.Name + "-api-key"
		m[p+"WEBHOOK_SECRET_REF"] = "aws-sm://cp/prod/" + slot.Name + "-webhook-secret"
		m[p+"TIMEOUT"] = "10s"
	}
	return m
}

func withVars(base, overrides map[string]string) map[string]string {
	m := maps.Clone(base)
	maps.Copy(m, overrides)
	return m
}

// mustLoadAs loads for a named service. Every test that cares which external
// dependencies are required says so; the ones that do not use mustLoad.
func mustLoadAs(t *testing.T, service Service, vars map[string]string) *Config {
	t.Helper()
	c, err := Load(context.Background(), service, LookupFromMap(vars))
	require.NoError(t, err)
	require.NotNil(t, c)
	return c
}

// mustLoad loads as the API, which is the representative deployed binary: it
// declares the archive and nothing else. A test whose subject is a dependency
// the API does not use must name the service that does, or it will assert
// against a rule that correctly did not run.
func mustLoad(t *testing.T, vars map[string]string) *Config {
	t.Helper()
	return mustLoadAs(t, ServiceAPI, vars)
}

func validProdConfig(t *testing.T) *Config {
	t.Helper()
	return mustLoad(t, prodEnv())
}

func validProdConfigAs(t *testing.T, service Service) *Config {
	t.Helper()
	return mustLoadAs(t, service, prodEnv())
}

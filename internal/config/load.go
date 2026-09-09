package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix is the prefix of every configuration variable.
const EnvPrefix = "CP_"

// EnvVarEnvironment selects the Environment. It is required everywhere and
// has no default.
const EnvVarEnvironment = "CP_ENV"

// ErrMissingRequired is wrapped in the error for each required variable that
// is absent and has no applicable default.
var ErrMissingRequired = errors.New("required variable is not set")

// VarError reports a problem with one variable. Load joins every VarError so
// operators see all problems at once.
type VarError struct {
	Name string
	Err  error
}

func (e *VarError) Error() string { return "config: " + e.Name + ": " + e.Err.Error() }

// Unwrap returns the underlying error.
func (e *VarError) Unwrap() error { return e.Err }

// VarSpec describes one CP_* variable. Vars returns the full list; it drives
// Load, ExampleEnv and the documentation test, so the three cannot drift.
type VarSpec struct {
	Name    string
	Section string
	Doc     string
	// Default is applied only when the variable is absent AND the environment
	// is LOCAL or TEST. Empty means no default.
	Default string
	// Required means an absent variable (with no applicable default) is an
	// error. Every variable with a Default is Required elsewhere: nothing is
	// silently defaulted in DEV, STAGING or PROD.
	Required bool
	// Example overrides the value printed by ExampleEnv (Default otherwise).
	Example string
	// Secret marks SecretRef-typed variables.
	Secret bool
}

type applyFn func(c *Config, raw string) error

type varSpec struct {
	VarSpec
	apply applyFn
}

// Vars returns every configuration variable in documentation order.
func Vars() []VarSpec {
	all := specs()
	out := make([]VarSpec, len(all))
	for i, s := range all {
		out[i] = s.VarSpec
	}
	return out
}

// Load builds a Config from lookup (os.LookupEnv when nil), applies the
// documented LOCAL/TEST defaults where allowed, derives computed fields and
// runs Validate. Every problem is reported; the first error never hides the
// rest. A present-but-blank variable counts as absent.
func Load(ctx context.Context, lookup func(string) (string, bool)) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	rawEnv, ok := lookup(EnvVarEnvironment)
	if !ok || strings.TrimSpace(rawEnv) == "" {
		return nil, &VarError{Name: EnvVarEnvironment, Err: ErrMissingRequired}
	}
	env, err := ParseEnvironment(rawEnv)
	if err != nil {
		return nil, &VarError{Name: EnvVarEnvironment, Err: err}
	}

	c := &Config{Env: env, BuildVersion: BuildVersion}
	var errs []error
	for _, s := range specs() {
		if s.Name == EnvVarEnvironment {
			continue
		}
		raw, present := lookup(s.Name)
		if !present || strings.TrimSpace(raw) == "" {
			switch {
			case env.AllowsDefaults() && s.Default != "":
				raw = s.Default
			case s.Required:
				errs = append(errs, &VarError{Name: s.Name, Err: ErrMissingRequired})
				continue
			default:
				continue
			}
		}
		if err := s.apply(c, raw); err != nil {
			errs = append(errs, &VarError{Name: s.Name, Err: err})
		}
	}
	c.Capability.StoreConfigured = !c.Database.AppURL.IsZero()
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// ---- parsers -------------------------------------------------------------

func setString(dst func(*Config) *string) applyFn {
	return func(c *Config, raw string) error {
		*dst(c) = strings.TrimSpace(raw)
		return nil
	}
}

func setBool(dst func(*Config) *bool) applyFn {
	return func(c *Config, raw string) error {
		v, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid boolean %q (want true|false)", raw)
		}
		*dst(c) = v
		return nil
	}
}

func setDuration(dst func(*Config) *time.Duration) applyFn {
	return func(c *Config, raw string) error {
		d, err := time.ParseDuration(strings.TrimSpace(raw))
		if err != nil {
			return fmt.Errorf("invalid duration %q (want e.g. 30s, 5m, 1h30m)", raw)
		}
		if d < 0 {
			return fmt.Errorf("duration %q must not be negative", raw)
		}
		*dst(c) = d
		return nil
	}
}

func setInt(dst func(*Config) *int) applyFn {
	return func(c *Config, raw string) error {
		v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 0)
		if err != nil {
			return fmt.Errorf("invalid integer %q", raw)
		}
		*dst(c) = int(v)
		return nil
	}
}

func setInt32(dst func(*Config) *int32) applyFn {
	return func(c *Config, raw string) error {
		v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32)
		if err != nil {
			return fmt.Errorf("invalid 32-bit integer %q", raw)
		}
		*dst(c) = int32(v)
		return nil
	}
}

func setInt64(dst func(*Config) *int64) applyFn {
	return func(c *Config, raw string) error {
		v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid 64-bit integer %q", raw)
		}
		*dst(c) = v
		return nil
	}
}

func setList(dst func(*Config) *[]string) applyFn {
	return func(c *Config, raw string) error {
		*dst(c) = splitList(raw)
		return nil
	}
}

func splitList(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func setSecret(dst func(*Config) *SecretRef) applyFn {
	return func(c *Config, raw string) error {
		raw = strings.TrimSpace(raw)
		if _, err := ParseSecretRef(raw); err != nil {
			return err
		}
		*dst(c) = SecretRef(raw)
		return nil
	}
}

func setProviderMode(dst func(*Config) *ProviderMode) applyFn {
	return func(c *Config, raw string) error {
		m, err := ParseProviderMode(strings.ToLower(strings.TrimSpace(raw)))
		if err != nil {
			return err
		}
		*dst(c) = m
		return nil
	}
}

// ---- the variable table ---------------------------------------------------

func req(name, section, doc, def string, apply applyFn) varSpec {
	return varSpec{VarSpec: VarSpec{Name: name, Section: section, Doc: doc, Default: def, Required: true}, apply: apply}
}

func opt(name, section, doc, example string, apply applyFn) varSpec {
	return varSpec{VarSpec: VarSpec{Name: name, Section: section, Doc: doc, Example: example}, apply: apply}
}

func secretVar(s varSpec) varSpec {
	s.Secret = true
	return s
}

const (
	secCore       = "Core"
	secHTTP       = "HTTP"
	secDatabase   = "Database (Postgres)"
	secRedis      = "Redis"
	secRedpanda   = "Redpanda (event bus)" // #nosec G101 -- config section heading, not a credential
	secClickHouse = "ClickHouse (analytics)"
	secTemporal   = "Temporal (workflows)"
	secArchive    = "Archive (S3-compatible evidence/audit storage)"
	secKMS        = "KMS"
	secAuth       = "Auth"
	secProviders  = "Providers"
	secTelemetry  = "Telemetry"
	secSeed       = "Seed"
	secRetention  = "Retention (days per retention class)" // #nosec G101 -- config section heading, not a credential
)

func specs() []varSpec {
	s := []varSpec{
		{
			VarSpec: VarSpec{
				Name: EnvVarEnvironment, Section: secCore, Required: true, Example: "LOCAL",
				Doc: "Deployment environment: LOCAL | TEST | DEV | STAGING | PROD. No default; unknown values are rejected. Only LOCAL and TEST apply the defaults listed below.",
			},
			apply: func(c *Config, raw string) error {
				e, err := ParseEnvironment(raw)
				if err != nil {
					return err
				}
				c.Env = e
				return nil
			},
		},
		req("CP_SERVICE_NAME", secCore, "Service name used in telemetry resource attributes and logs.", "controlplane",
			setString(func(c *Config) *string { return &c.ServiceName })),
		req("CP_PUBLIC_PRODUCT_NAME", secCore, "Customer-facing product name (brand-neutral codebase; goal PART 5). Must be non-empty in STAGING/PROD.", "Control Plane",
			setString(func(c *Config) *string { return &c.PublicProductName })),

		req("CP_HTTP_ADDR", secHTTP, "Listen address for the HTTP API.", "127.0.0.1:8080",
			setString(func(c *Config) *string { return &c.HTTP.Addr })),
		req("CP_HTTP_PUBLIC_BASE_URL", secHTTP, "Externally visible base URL (used for redirects and links). Must be https in STAGING/PROD.", "http://127.0.0.1:8080",
			setString(func(c *Config) *string { return &c.HTTP.PublicBaseURL })),
		opt("CP_HTTP_CORS_ORIGINS", secHTTP, "Comma-separated allowed CORS origins. \"*\" is rejected in STAGING/PROD.", "http://localhost:3000",
			setList(func(c *Config) *[]string { return &c.HTTP.CORSOrigins })),
		req("CP_HTTP_READ_TIMEOUT", secHTTP, "Server read timeout (Go duration).", "10s",
			setDuration(func(c *Config) *time.Duration { return &c.HTTP.ReadTimeout })),
		req("CP_HTTP_WRITE_TIMEOUT", secHTTP, "Server write timeout (Go duration).", "30s",
			setDuration(func(c *Config) *time.Duration { return &c.HTTP.WriteTimeout })),
		req("CP_HTTP_IDLE_TIMEOUT", secHTTP, "Keep-alive idle timeout (Go duration).", "120s",
			setDuration(func(c *Config) *time.Duration { return &c.HTTP.IdleTimeout })),
		req("CP_HTTP_MAX_BODY_BYTES", secHTTP, "Maximum accepted request body in bytes.", "1048576",
			setInt64(func(c *Config) *int64 { return &c.HTTP.MaxBodyBytes })),
		opt("CP_HTTP_TRUSTED_PROXY_CIDRS", secHTTP, "Comma-separated CIDRs of load balancers whose X-Forwarded-For is trusted. Empty means trust none.", "",
			setList(func(c *Config) *[]string { return &c.HTTP.TrustedProxyCIDRs })),

		secretVar(req("CP_DATABASE_APP_URL", secDatabase, "SecretRef to the application-role Postgres URL (cp_app). Plain value only in LOCAL/TEST.",
			"postgres://cp_app:cp_app_local@127.0.0.1:5433/controlplane?sslmode=disable",
			setSecret(func(c *Config) *SecretRef { return &c.Database.AppURL }))),
		secretVar(req("CP_DATABASE_MIGRATE_URL", secDatabase, "SecretRef to the migration-role Postgres URL (cp_migrate).",
			"postgres://cp_migrate:cp_migrate_local@127.0.0.1:5433/controlplane?sslmode=disable",
			setSecret(func(c *Config) *SecretRef { return &c.Database.MigrateURL }))),
		secretVar(opt("CP_DATABASE_READONLY_URL", secDatabase, "SecretRef to the read-only Postgres URL (cp_readonly). Optional; readers fall back to the app URL.", "",
			setSecret(func(c *Config) *SecretRef { return &c.Database.ReadOnlyURL }))),
		secretVar(opt("CP_DATABASE_OPS_URL", secDatabase, "SecretRef to the operations-role Postgres URL (cp_ops). Optional; the retention passes need it, because cp_app holds no DELETE on the rows they remove.",
			"postgres://cp_ops:cp_ops_local@127.0.0.1:5433/controlplane?sslmode=disable",
			setSecret(func(c *Config) *SecretRef { return &c.Database.OpsURL }))),
		req("CP_DATABASE_REQUIRE_TLS", secDatabase, "Refuse Postgres connections that are not TLS with certificate verification. Must be true in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.Database.RequireTLS })),
		req("CP_DATABASE_MAX_CONNS", secDatabase, "Maximum pool connections.", "10",
			setInt32(func(c *Config) *int32 { return &c.Database.MaxConns })),
		req("CP_DATABASE_MIN_CONNS", secDatabase, "Minimum idle pool connections.", "1",
			setInt32(func(c *Config) *int32 { return &c.Database.MinConns })),
		req("CP_DATABASE_STATEMENT_TIMEOUT", secDatabase, "Postgres statement_timeout applied per session.", "30s",
			setDuration(func(c *Config) *time.Duration { return &c.Database.StatementTimeout })),
		req("CP_DATABASE_LOCK_TIMEOUT", secDatabase, "Postgres lock_timeout applied per session.", "5s",
			setDuration(func(c *Config) *time.Duration { return &c.Database.LockTimeout })),

		secretVar(req("CP_REDIS_URL", secRedis, "SecretRef to the Redis URL (may embed a password). Redis is never financial truth.", "redis://127.0.0.1:6380/0",
			setSecret(func(c *Config) *SecretRef { return &c.Redis.URL }))),
		req("CP_REDIS_REQUIRE_TLS", secRedis, "Require rediss:// (TLS). Must be true in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.Redis.RequireTLS })),

		req("CP_REDPANDA_BROKERS", secRedpanda, "Comma-separated Kafka-protocol broker addresses.", "127.0.0.1:19092",
			setList(func(c *Config) *[]string { return &c.Redpanda.Brokers })),
		req("CP_REDPANDA_REQUIRE_TLS", secRedpanda, "Require TLS to brokers. Must be true in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.Redpanda.RequireTLS })),
		opt("CP_REDPANDA_SASL_MECHANISM", secRedpanda, "SASL mechanism (e.g. SCRAM-SHA-256, SCRAM-SHA-512). Empty disables SASL.", "",
			setString(func(c *Config) *string { return &c.Redpanda.SASLMechanism })),
		secretVar(opt("CP_REDPANDA_SASL_USERNAME_REF", secRedpanda, "SecretRef to the SASL username. Required when a SASL mechanism is set.", "",
			setSecret(func(c *Config) *SecretRef { return &c.Redpanda.SASLUsernameRef }))),
		secretVar(opt("CP_REDPANDA_SASL_PASSWORD_REF", secRedpanda, "SecretRef to the SASL password. Required when a SASL mechanism is set.", "",
			setSecret(func(c *Config) *SecretRef { return &c.Redpanda.SASLPasswordRef }))),

		req("CP_CLICKHOUSE_ADDR", secClickHouse, "ClickHouse host:port.", "127.0.0.1:18123",
			setString(func(c *Config) *string { return &c.ClickHouse.Addr })),
		req("CP_CLICKHOUSE_DATABASE", secClickHouse, "ClickHouse database name.", "controlplane",
			setString(func(c *Config) *string { return &c.ClickHouse.Database })),
		secretVar(opt("CP_CLICKHOUSE_USERNAME_REF", secClickHouse, "SecretRef to the ClickHouse username.", "cp",
			setSecret(func(c *Config) *SecretRef { return &c.ClickHouse.UsernameRef }))),
		secretVar(opt("CP_CLICKHOUSE_PASSWORD_REF", secClickHouse, "SecretRef to the ClickHouse password.", "cp_local",
			setSecret(func(c *Config) *SecretRef { return &c.ClickHouse.PasswordRef }))),
		req("CP_CLICKHOUSE_REQUIRE_TLS", secClickHouse, "Require TLS to ClickHouse. Must be true in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.ClickHouse.RequireTLS })),

		req("CP_TEMPORAL_HOST_PORT", secTemporal, "Temporal frontend host:port.", "127.0.0.1:7233",
			setString(func(c *Config) *string { return &c.Temporal.HostPort })),
		req("CP_TEMPORAL_NAMESPACE", secTemporal, "Temporal namespace.", "default",
			setString(func(c *Config) *string { return &c.Temporal.Namespace })),
		req("CP_TEMPORAL_TASK_QUEUE_PREFIX", secTemporal, "Prefix for every task queue name (lets environments share a cluster safely).", "cp",
			setString(func(c *Config) *string { return &c.Temporal.TaskQueuePrefix })),
		req("CP_TEMPORAL_REQUIRE_TLS", secTemporal, "Require TLS to Temporal. Must be true in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.Temporal.RequireTLS })),

		req("CP_ARCHIVE_ENDPOINT", secArchive, "S3-compatible endpoint URL. Empty in AWS means the regional default; LOCAL points at MinIO.", "http://127.0.0.1:9100",
			setString(func(c *Config) *string { return &c.Archive.Endpoint })),
		req("CP_ARCHIVE_REGION", secArchive, "S3 region.", "us-east-1",
			setString(func(c *Config) *string { return &c.Archive.Region })),
		req("CP_ARCHIVE_RAW_BUCKET", secArchive, "Bucket for raw provider/market payloads.", "raw-events",
			setString(func(c *Config) *string { return &c.Archive.RawBucket })),
		req("CP_ARCHIVE_EVIDENCE_BUCKET", secArchive, "Bucket for provider evidence (requests/responses, receipts).", "provider-evidence",
			setString(func(c *Config) *string { return &c.Archive.EvidenceBucket })),
		req("CP_ARCHIVE_AUDIT_BUCKET", secArchive, "WORM bucket for the audit chain. Object Lock must be enabled in STAGING/PROD.", "audit-evidence",
			setString(func(c *Config) *string { return &c.Archive.AuditBucket })),
		req("CP_ARCHIVE_OBJECT_LOCK_REQUIRED", secArchive, "Refuse to start unless the audit bucket has Object Lock. Must be true in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.Archive.ObjectLockRequired })),
		req("CP_ARCHIVE_FORCE_PATH_STYLE", secArchive, "Use path-style S3 addressing (needed for MinIO).", "true",
			setBool(func(c *Config) *bool { return &c.Archive.ForcePathStyle })),
		secretVar(opt("CP_ARCHIVE_ACCESS_KEY_REF", secArchive, "SecretRef to a static S3 access key. Leave empty in AWS to use the task IAM role (preferred; goal PART 99).", "cp_minio",
			setSecret(func(c *Config) *SecretRef { return &c.Archive.AccessKeyRef }))),
		secretVar(opt("CP_ARCHIVE_SECRET_KEY_REF", secArchive, "SecretRef to a static S3 secret key. Leave empty in AWS to use the task IAM role.", "cp_minio_local",
			setSecret(func(c *Config) *SecretRef { return &c.Archive.SecretKeyRef }))),

		opt("CP_KMS_AUDIT_SIGNING_KEY_ID", secKMS, "KMS key id/ARN used to sign audit records. Required in STAGING/PROD.", "",
			setString(func(c *Config) *string { return &c.KMS.AuditSigningKeyID })),
		opt("CP_KMS_REGION", secKMS, "KMS region. Required when a signing key is set.", "",
			setString(func(c *Config) *string { return &c.KMS.Region })),

		req("CP_AUTH_MODE", secAuth, "Authentication mode: oidc | dev. dev is rejected in STAGING/PROD.", "dev",
			setString(func(c *Config) *string { return &c.Auth.Mode })),
		opt("CP_AUTH_ISSUER", secAuth, "OIDC issuer URL. Required when CP_AUTH_MODE=oidc; https in STAGING/PROD.", "",
			setString(func(c *Config) *string { return &c.Auth.Issuer })),
		opt("CP_AUTH_CLIENT_ID", secAuth, "OIDC client id. Required when CP_AUTH_MODE=oidc.", "",
			setString(func(c *Config) *string { return &c.Auth.ClientID })),
		secretVar(opt("CP_AUTH_CLIENT_SECRET_REF", secAuth, "SecretRef to the OIDC client secret. Required when CP_AUTH_MODE=oidc.", "",
			setSecret(func(c *Config) *SecretRef { return &c.Auth.ClientSecretRef }))),
		opt("CP_AUTH_REDIRECT_URL", secAuth, "OIDC redirect URL. Required when CP_AUTH_MODE=oidc; https in STAGING/PROD.", "",
			setString(func(c *Config) *string { return &c.Auth.RedirectURL })),
		req("CP_AUTH_COOKIE_NAME", secAuth, "Session cookie name.", "cp_session",
			setString(func(c *Config) *string { return &c.Auth.CookieName })),
		opt("CP_AUTH_COOKIE_DOMAIN", secAuth, "Session cookie Domain attribute. Empty means host-only.", "",
			setString(func(c *Config) *string { return &c.Auth.CookieDomain })),
		req("CP_AUTH_COOKIE_SECURE", secAuth, "Set the Secure attribute on session cookies. Must be true in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.Auth.CookieSecure })),
		req("CP_AUTH_SESSION_TTL", secAuth, "Session lifetime (Go duration).", "12h",
			setDuration(func(c *Config) *time.Duration { return &c.Auth.SessionTTL })),
		req("CP_AUTH_STEP_UP_MAX_AGE", secAuth, "Maximum age of an MFA/passkey authentication for step-up protected actions (Go duration).", "5m",
			setDuration(func(c *Config) *time.Duration { return &c.Auth.StepUpMaxAge })),
		req("CP_AUTH_DEBUG_ENABLED", secAuth, "Enable debug authentication endpoints. Must be false in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.Auth.DebugAuthEnabled })),
	}

	for _, slot := range providerSlots() {
		slot := slot
		prefix := "CP_PROVIDER_" + slot.Env + "_"
		label := strings.ReplaceAll(slot.Name, "_", " ")
		s = append(s,
			req(prefix+"MODE", secProviders, "Mode of the "+label+" provider: fake | sandbox | live. fake is rejected in STAGING/PROD.", "fake",
				setProviderMode(func(c *Config) *ProviderMode { return &slot.Get(&c.Providers).Mode })),
			opt(prefix+"NAME", secProviders, "Adapter name of the "+label+" provider (e.g. the vendor). Required unless mode is fake.", "",
				setString(func(c *Config) *string { return &slot.Get(&c.Providers).Name })),
			opt(prefix+"BASE_URL", secProviders, "Base URL of the "+label+" provider API. Optional.", "",
				setString(func(c *Config) *string { return &slot.Get(&c.Providers).BaseURL })),
			secretVar(opt(prefix+"API_KEY_REF", secProviders, "SecretRef to the "+label+" provider API key.", "",
				setSecret(func(c *Config) *SecretRef { return &slot.Get(&c.Providers).APIKeyRef }))),
			secretVar(opt(prefix+"WEBHOOK_SECRET_REF", secProviders, "SecretRef to the "+label+" provider webhook signing secret.", "",
				setSecret(func(c *Config) *SecretRef { return &slot.Get(&c.Providers).WebhookSecretRef }))),
			req(prefix+"TIMEOUT", secProviders, "Per-call timeout for the "+label+" provider (Go duration).", "10s",
				setDuration(func(c *Config) *time.Duration { return &slot.Get(&c.Providers).Timeout })),
			opt(prefix+"ACCOUNT_REF", secProviders, "Provider-side account or tenant id for the "+label+" provider (e.g. a Stripe acct_...). Asserted at startup against the account the credentials actually belong to, so a key rotated to the wrong account is caught before it moves money.", "",
				setString(func(c *Config) *string { return &slot.Get(&c.Providers).AccountRef })),
			opt(prefix+"SHARED_ACCOUNT", secProviders, "Whether the "+label+" provider account also serves systems outside this deployment. It is a declaration for operators and for the capability report; rejecting another product's events is unconditional in the adapters that can receive them, so this flag is never the thing that makes that safe. Absent means not shared.", "false",
				setBool(func(c *Config) *bool { return &slot.Get(&c.Providers).Shared })),
			opt(prefix+"AVAILABILITY", secProviders, "How far the "+label+" integration is actually approved for use on this account, as opposed to how finished the code is. Empty means nothing has been granted, which every adapter treats as a refusal.", "",
				setString(func(c *Config) *string { return &slot.Get(&c.Providers).Availability })),
		)
	}

	s = append(s,
		opt("CP_TELEMETRY_OTLP_ENDPOINT", secTelemetry, "OTLP gRPC collector host:port. Empty installs no-op tracer/meter providers.", "",
			setString(func(c *Config) *string { return &c.Telemetry.OTLPEndpoint })),
		req("CP_TELEMETRY_OTLP_INSECURE", secTelemetry, "Export OTLP without TLS. Ignored (rejected) in PROD.", "true",
			setBool(func(c *Config) *bool { return &c.Telemetry.OTLPInsecure })),
		req("CP_TELEMETRY_TRACE_SAMPLE_RATIO", secTelemetry, "Trace sampling probability as a decimal string in [0,1] (e.g. 1, 0.1). Converted to a float only inside the sampler; this is telemetry, not money.", "1",
			setString(func(c *Config) *string { return &c.Telemetry.TraceSampleRatio })),
		req("CP_TELEMETRY_METRICS_INTERVAL", secTelemetry, "Metric export interval (Go duration).", "30s",
			setDuration(func(c *Config) *time.Duration { return &c.Telemetry.MetricsInterval })),

		req("CP_SEED_ENABLED", secSeed, "Allow seeding clearly-labeled fake users/assets/balances. Must be false in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.Seed.Enabled })),

		req("CP_RETENTION_LOGIN_ATTEMPT_DAYS", secRetention, "Days a login_attempts row is kept after it expired. It holds a plaintext OIDC nonce and PKCE verifier; the durable record of a login is a security_events row. Minimum 1.", "2",
			setInt(func(c *Config) *int { return &c.Retention.LoginAttemptDays })),
		req("CP_RETENTION_FINANCIAL_RECORD_DAYS", secRetention, "Retention of the FINANCIAL_RECORD class. Must be > 0 in STAGING/PROD.", "2555",
			setInt(func(c *Config) *int { return &c.Retention.FinancialRecordDays })),
		req("CP_RETENTION_SECURITY_AUDIT_DAYS", secRetention, "Retention of the SECURITY_AUDIT class. Must be > 0 in STAGING/PROD.", "2555",
			setInt(func(c *Config) *int { return &c.Retention.SecurityAuditDays })),
		req("CP_RETENTION_RAW_MARKET_DATA_DAYS", secRetention, "Retention of the RAW_MARKET_DATA class.", "90",
			setInt(func(c *Config) *int { return &c.Retention.RawMarketDataDays })),
		req("CP_RETENTION_SOCIAL_DATA_DAYS", secRetention, "Retention of the SOCIAL_DATA class.", "30",
			setInt(func(c *Config) *int { return &c.Retention.SocialDataDays })),
		req("CP_RETENTION_MODEL_IO_DAYS", secRetention, "Retention of the MODEL_IO class (prompts, tool calls, model outputs).", "90",
			setInt(func(c *Config) *int { return &c.Retention.ModelIODays })),
		req("CP_RETENTION_OPERATIONAL_LOG_DAYS", secRetention, "Retention of the OPERATIONAL_LOG class.", "30",
			setInt(func(c *Config) *int { return &c.Retention.OperationalLogDays })),
	)
	return s
}

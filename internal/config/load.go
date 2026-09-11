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
	// Dep names the external dependency this variable configures, when it
	// configures one. Such a variable is required only of a service that
	// declares that dependency, and is parsed for any service that supplies
	// it. Empty means the variable is unconditional.
	Dep Dependency
	// Svc narrows a required variable to one binary. Most variables that vary
	// by binary vary because of an external dependency, and Dep covers those.
	// This covers the rest: a setting only one binary has any use for, like
	// where the transport rate limiter keeps its counters. Like Dep, it
	// narrows required-ness only -- a supplied value is still parsed.
	Svc Service
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
// The service argument is not optional and has no default. A caller that has
// not said which binary it is cannot be told what it needs, and guessing on its
// behalf is exactly how every binary came to require every dependency.
func Load(ctx context.Context, service Service, lookup func(string) (string, bool)) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !service.Valid() {
		return nil, fmt.Errorf("config: unknown service %q; every binary must declare which one it is", service)
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

	c := &Config{Env: env, Service: service, BuildVersion: BuildVersion}
	var errs []error

	// Two passes, because one of the questions this loop has to answer depends
	// on an answer from the loop itself. Whether the API needs Redis is decided
	// by CP_RATELIMIT_BACKEND, and a single pass would make the outcome depend
	// on the order of the table -- which is a trap for whoever next reorders it.
	//
	// Pass one applies every value that is present (or defaulted where that is
	// allowed) and remembers what was absent. Nothing is judged missing yet.
	var absent []varSpec
	for _, s := range specs() {
		if s.Name == EnvVarEnvironment {
			continue
		}
		raw, present := lookup(s.Name)
		if !present || strings.TrimSpace(raw) == "" {
			if env.AllowsDefaults() && s.Default != "" {
				raw = s.Default
			} else {
				absent = append(absent, s)
				continue
			}
		}
		// A supplied value is always PARSED, whether or not this binary would
		// use it. A malformed broker list is a mistake whoever set it wants to
		// hear about, and hearing about it only from the one binary that dials
		// it means hearing about it late.
		if err := s.apply(c, raw); err != nil {
			errs = append(errs, &VarError{Name: s.Name, Err: err})
		}
	}

	// Pass two: now that the deciding values are loaded, which absences are
	// errors.
	//
	// This is the one place the package's "every problem is reported" promise
	// bends. An absent CP_RATELIMIT_BACKEND is itself an error, and it also
	// leaves the Redis question unanswerable, so its own error is reported and
	// the Redis variables are not judged. Setting it produces the next answer.
	// Guessing on its behalf would be worse: the guess would have to be redis,
	// and then a LOCAL run that simply forgot the variable would be told to
	// configure a Redis it does not need.
	for _, s := range absent {
		if c.requiresVar(s) {
			errs = append(errs, &VarError{Name: s.Name, Err: ErrMissingRequired})
		}
	}
	c.Capability.StoreConfigured = !c.Database.AppURL.IsZero()
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// The two prod-like secrets whose reference cannot stand in for their
	// value. This needs the lookup, which is why it is here rather than in
	// Validate (F-137).
	if err := c.ResolvableSecrets(ctx, lookup); err != nil {
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

func setArchiveBackend(dst func(*Config) *ArchiveBackend) applyFn {
	return func(c *Config, raw string) error {
		v, err := ParseArchiveBackend(raw)
		if err != nil {
			return err
		}
		*dst(c) = v
		return nil
	}
}

func setRateLimitBackend(dst func(*Config) *RateLimitBackend) applyFn {
	return func(c *Config, raw string) error {
		v, err := ParseRateLimitBackend(strings.ToLower(strings.TrimSpace(raw)))
		if err != nil {
			return err
		}
		*dst(c) = v
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

// needs marks a variable as belonging to an external dependency. It is
// required only of a service that declares that dependency, and is parsed for
// every service that supplies it.
func needs(d Dependency, s varSpec) varSpec {
	s.Dep = d
	return s
}

// only marks a variable as belonging to one binary. It is required only of
// that service, and is parsed for every service that supplies it.
func only(svc Service, s varSpec) varSpec {
	s.Svc = svc
	return s
}

const (
	secCore       = "Core"
	secHTTP       = "HTTP"
	secDatabase   = "Database (Postgres)"
	secAPI        = "API (cmd/api only)"
	secCapacity   = "Capacity ceilings"
	secRateLimit  = "Rate limiting"
	secRedis      = "Redis"
	secRedpanda   = "Redpanda (event bus)" // #nosec G101 -- config section heading, not a credential
	secClickHouse = "ClickHouse (analytics)"
	secTemporal   = "Temporal (workflows)"
	secArchive    = "Archive (S3-compatible evidence/audit storage)"
	secKMS        = "KMS"
	secAuth       = "Auth"
	secProviders  = "Providers"
	secTelemetry  = "Telemetry"
	secAlert      = "Alerting"
	secPII        = "Personal data"
	secSeed       = "Seed"
	secCredit     = "Credit (funding lifecycle)"
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
		// Required of TOOLING only, and the change is a security one (F-93).
		//
		// This was declared unconditionally required, so every binary that loads configuration --
		// including the internet-facing one -- had to be given the SCHEMA OWNER
		// credential. The owner can `ALTER TABLE ... DISABLE TRIGGER`, and since
		// 00743-00753 every state machine in this system is enforced by triggers:
		// the transition bindings, forbid_mutation on fifty-three append-only
		// tables, and the eleven triggers that now write state columns the
		// application cannot. Handing that credential to the process exposed to
		// the internet undercuts all of them.
		//
		// Nothing that loads configuration reads it. `Database.MigrateURL` has no
		// reader anywhere in the tree, and `cmd/migrate` -- the only binary that
		// migrates -- resolves the variable from the environment itself, with its
		// own LOCAL default and its own refusal outside LOCAL/TEST. So requiring
		// it bought nothing and cost the credential.
		//
		// It stays declared and keeps its LOCAL default, so `.env.example` and
		// `configcheck` still describe it and a developer's local tooling still
		// works without being told about it. What changed is which binaries are
		// made to hold it.
		only(ServiceTooling, secretVar(req("CP_DATABASE_MIGRATE_URL", secDatabase, "SecretRef to the migration-role Postgres URL (cp_migrate). Required only of tooling; cmd/migrate reads it from the environment itself, and the internet-facing binary must not be given it.",
			"postgres://cp_migrate:cp_migrate_local@127.0.0.1:5433/controlplane?sslmode=disable",
			setSecret(func(c *Config) *SecretRef { return &c.Database.MigrateURL })))),
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
		req("CP_DATABASE_CONNECT_TIMEOUT", secDatabase, "How long one dial to Postgres may take. Bounded below the API request timeout on purpose: a connect that outlives the request it serves is work done for nobody, holding a pool slot the next request wanted.", "5s",
			setDuration(func(c *Config) *time.Duration { return &c.Database.ConnectTimeout })),
		req("CP_DATABASE_MAX_CONN_IDLE_TIME", secDatabase, "Discard a pooled connection idle this long. On a database that suspends when idle -- Neon's free tier scales to zero -- a pooled connection outlives the server on the other end of it, and a connection failure is not retryable by design. 0 leaves pgxpool's 30-minute default.", "0s",
			setDuration(func(c *Config) *time.Duration { return &c.Database.MaxConnIdleTime })),
		req("CP_DATABASE_STATEMENT_TIMEOUT", secDatabase, "Postgres statement_timeout applied per session.", "30s",
			setDuration(func(c *Config) *time.Duration { return &c.Database.StatementTimeout })),
		req("CP_DATABASE_LOCK_TIMEOUT", secDatabase, "Postgres lock_timeout applied per session.", "5s",
			setDuration(func(c *Config) *time.Duration { return &c.Database.LockTimeout })),

		only(ServiceAPI, opt("CP_API_REQUEST_TIMEOUT", secAPI, "Per-request deadline for non-streaming routes (Go duration). Empty takes CP_HTTP_WRITE_TIMEOUT.", "30s",
			setDuration(func(c *Config) *time.Duration { return &c.API.RequestTimeout }))),
		only(ServiceAPI, opt("CP_API_SHUTDOWN_TIMEOUT", secAPI, "Bound on draining in-flight requests before the process exits (Go duration).", "25s",
			setDuration(func(c *Config) *time.Duration { return &c.API.ShutdownTimeout }))),
		only(ServiceAPI, opt("CP_API_ENABLED_CAPABILITIES", secAPI, "Comma-separated capability names this deployment may run, e.g. CREDIT_PURCHASE. A capability absent from the list is inactive whatever its gate row says, and an empty list disables every one of them -- which is the default, and fails closed. Being on the list does not activate a capability: the gate row still requires three distinct principals.", "CREDIT_PURCHASE",
			setString(func(c *Config) *string { return &c.API.EnabledCapabilities }))),
		only(ServiceAPI, opt("CP_API_FUNDING_NETWORK", secAPI, "Chain the funding endpoints quote an onramp on.", "solana",
			setString(func(c *Config) *string { return &c.API.FundingNetwork }))),
		only(ServiceAPI, opt("CP_API_FUNDING_CURRENCY", secAPI, "Currency the funding endpoints quote an onramp in.", "usdc",
			setString(func(c *Config) *string { return &c.API.FundingCurrency }))),
		only(ServiceAPI, opt("CP_API_LEGAL_POLICY", secAPI, "Jurisdiction routing policy to load: CONSERVATIVE (the default), DEVELOPMENT (LOCAL/TEST/DEV only) or SANDBOX (any environment but PROD: declares a sandbox tier that may exercise the gated product against providers that move nothing, ADR-0023). Naming it here is what lets a configuration check see which one was asked for.", "development",
			setString(func(c *Config) *string { return &c.API.LegalPolicy }))),
		only(ServiceAPI, opt("CP_API_PAYOUT_POLICY", secAPI, "Payout policy to load: CLOSED (the default; no origin is withdrawable) or SANDBOX (the rehearsal policy of a sandbox tier; refused in PROD and refused unless CP_API_LEGAL_POLICY is SANDBOX). A real policy is a persisted version with evidence, never a name here.", "closed",
			setString(func(c *Config) *string { return &c.API.PayoutPolicy }))),
		only(ServiceAPI, opt("CP_API_SANDBOX_GATES", secAPI, "Comma-separated capabilities the deployment sandbox-activates at boot, e.g. CREDIT_PURCHASE,NATIVE_MARKET_TRADING. Only on a sandbox tier; each must also be in CP_API_ENABLED_CAPABILITIES. A SANDBOX gate carries no approval, cannot exist in PROD, and is read as active only by a sandbox tier.", "",
			setString(func(c *Config) *string { return &c.API.SandboxGates }))),
		only(ServiceAPI, opt("CP_API_DEMO_DATA", secAPI, "Load the SANDBOX demo catalogue at boot: eight labelled demo markets, a demo Credit balance and the activity feed built from them. Only on a sandbox tier (CP_API_LEGAL_POLICY=SANDBOX) and never in PROD; false by default, so a deployment that says nothing seeds nothing. Distinct from CP_SEED_ENABLED, which governs the developer seed scripts and is refused outright in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.API.DemoData }))),
		only(ServiceAPI, opt("CP_API_SETTLEMENT_CHAIN", secAPI, "Chain of the USD-pegged asset that funds settle into. Required in STAGING/PROD; cmd/api also checks the pair resolves to a known stablecoin, which needs the database and so stays there. The example is the DEVNET pair scripts/seed registers, because that is what README + this file has to boot against; a deployment that settles on mainnet states the mainnet pair and has the providers to back the claim.", "solana-devnet",
			setString(func(c *Config) *string { return &c.API.SettlementChain }))),
		only(ServiceAPI, opt("CP_API_SETTLEMENT_MINT", secAPI, "Mint address of that asset. Required in STAGING/PROD. The example is devnet USDC, the mint scripts/seed registers.", "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
			setString(func(c *Config) *string { return &c.API.SettlementMint }))),
		only(ServiceAPI, req("CP_CAPACITY_MAX_ACCOUNTS", secCapacity, "Most accounts this deployment tier will hold. Reached, it refuses to open more. 0 disables the ceiling, which is only correct where the tier has no such limit.", "50",
			setInt64(func(c *Config) *int64 { return &c.Capacity.MaxAccounts }))),
		only(ServiceAPI, req("CP_CAPACITY_MAX_PURCHASES_PER_DAY", secCapacity, "Most Credit purchases in any rolling 24 hours. It bounds provider webhook volume and database growth together. 0 disables it.", "200",
			setInt64(func(c *Config) *int64 { return &c.Capacity.MaxPurchasesPerDay }))),
		only(ServiceAPI, req("CP_CAPACITY_MAX_AT_RISK_MINOR", secCapacity, "Most money, in minor units, that may sit in a non-terminal funding state at once. This is the ceiling that bounds what a failure COSTS rather than what it consumes. 0 disables it.", "200000",
			setInt64(func(c *Config) *int64 { return &c.Capacity.MaxAtRiskMinor }))),
		only(ServiceAPI, req("CP_CAPACITY_MAX_DATABASE_BYTES", secCapacity, "The database storage quota this tier is subject to. New financial actions stop below it, leaving room to reconcile and export. 0 disables it, which is correct on infrastructure with no quota.", "524288000",
			setInt64(func(c *Config) *int64 { return &c.Capacity.MaxDatabaseBytes }))),
		only(ServiceAPI, req("CP_HTTP_REPLICAS", secRateLimit, "How many processes of this binary serve HTTP. It decides whether process-local rate-limit counters can enforce the configured limit: one process can, more cannot, because each keeps its own copy of the budget. Whatever runs the deployment -- a task count, a replica count, an instance count -- must agree with this, and a value that understates it produces a limit looser than the one configured.", "1",
			setInt(func(c *Config) *int { return &c.RateLimit.Replicas }))),
		only(ServiceAPI, req("CP_API_RATE_LIMIT_GENERAL", secRateLimit, "Transport budget for ordinary requests, as \"<requests>/<window>\" (e.g. 600/1m) or \"off\". Refused as off in STAGING/PROD: switching a transport limit off is a load-testing affordance, never a production one.", "600/1m",
			setString(func(c *Config) *string { return &c.RateLimit.General }))),
		only(ServiceAPI, req("CP_API_RATE_LIMIT_AUTH", secRateLimit, "Transport budget for the authentication routes. Lower than the general one on purpose: these are the routes a credential-stuffing run uses.", "30/1m",
			setString(func(c *Config) *string { return &c.RateLimit.Auth }))),
		only(ServiceAPI, req("CP_API_RATE_LIMIT_QUOTE", secRateLimit, "Transport budget for quote requests.", "120/1m",
			setString(func(c *Config) *string { return &c.RateLimit.Quote }))),
		only(ServiceAPI, req("CP_API_RATE_LIMIT_COMMAND", secRateLimit, "Transport budget for the routes that change something.", "120/1m",
			setString(func(c *Config) *string { return &c.RateLimit.Command }))),
		only(ServiceAPI, req("CP_RATELIMIT_BACKEND", secRateLimit, "Where transport rate-limit counters live: memory | redis. memory keeps them in the process, so the budget is per replica -- three API tasks with a limit of 100 admit 300 -- which is why STAGING and PROD refuse it. redis shares one budget across every replica and makes CP_REDIS_* required of the API.", "memory",
			setRateLimitBackend(func(c *Config) *RateLimitBackend { return &c.RateLimit.Backend }))),

		needs(DepRedis, secretVar(req("CP_REDIS_URL", secRedis, "SecretRef to the Redis URL (may embed a password). Redis is never financial truth.", "redis://127.0.0.1:6380/0",
			setSecret(func(c *Config) *SecretRef { return &c.Redis.URL })))),
		needs(DepRedis, req("CP_REDIS_REQUIRE_TLS", secRedis, "Require rediss:// (TLS). Must be true in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.Redis.RequireTLS }))),

		needs(DepRedpanda, req("CP_REDPANDA_BROKERS", secRedpanda, "Comma-separated Kafka-protocol broker addresses.", "127.0.0.1:19092",
			setList(func(c *Config) *[]string { return &c.Redpanda.Brokers }))),
		needs(DepRedpanda, req("CP_REDPANDA_REQUIRE_TLS", secRedpanda, "Require TLS to brokers. Must be true in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.Redpanda.RequireTLS }))),
		needs(DepRedpanda, opt("CP_REDPANDA_SASL_MECHANISM", secRedpanda, "SASL mechanism (e.g. SCRAM-SHA-256, SCRAM-SHA-512). Empty disables SASL.", "",
			setString(func(c *Config) *string { return &c.Redpanda.SASLMechanism }))),
		needs(DepRedpanda, secretVar(opt("CP_REDPANDA_SASL_USERNAME_REF", secRedpanda, "SecretRef to the SASL username. Required when a SASL mechanism is set.", "",
			setSecret(func(c *Config) *SecretRef { return &c.Redpanda.SASLUsernameRef })))),
		needs(DepRedpanda, secretVar(opt("CP_REDPANDA_SASL_PASSWORD_REF", secRedpanda, "SecretRef to the SASL password. Required when a SASL mechanism is set.", "",
			setSecret(func(c *Config) *SecretRef { return &c.Redpanda.SASLPasswordRef })))),

		needs(DepClickHouse, req("CP_CLICKHOUSE_ADDR", secClickHouse, "ClickHouse host:port.", "127.0.0.1:18123",
			setString(func(c *Config) *string { return &c.ClickHouse.Addr }))),
		needs(DepClickHouse, req("CP_CLICKHOUSE_DATABASE", secClickHouse, "ClickHouse database name.", "controlplane",
			setString(func(c *Config) *string { return &c.ClickHouse.Database }))),
		needs(DepClickHouse, secretVar(opt("CP_CLICKHOUSE_USERNAME_REF", secClickHouse, "SecretRef to the ClickHouse username.", "cp",
			setSecret(func(c *Config) *SecretRef { return &c.ClickHouse.UsernameRef })))),
		needs(DepClickHouse, secretVar(opt("CP_CLICKHOUSE_PASSWORD_REF", secClickHouse, "SecretRef to the ClickHouse password.", "cp_local",
			setSecret(func(c *Config) *SecretRef { return &c.ClickHouse.PasswordRef })))),
		needs(DepClickHouse, req("CP_CLICKHOUSE_REQUIRE_TLS", secClickHouse, "Require TLS to ClickHouse. Must be true in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.ClickHouse.RequireTLS }))),

		needs(DepTemporal, req("CP_TEMPORAL_HOST_PORT", secTemporal, "Temporal frontend host:port.", "127.0.0.1:7233",
			setString(func(c *Config) *string { return &c.Temporal.HostPort }))),
		needs(DepTemporal, req("CP_TEMPORAL_NAMESPACE", secTemporal, "Temporal namespace.", "default",
			setString(func(c *Config) *string { return &c.Temporal.Namespace }))),
		needs(DepTemporal, req("CP_TEMPORAL_TASK_QUEUE_PREFIX", secTemporal, "Prefix for every task queue name (lets environments share a cluster safely).", "cp",
			setString(func(c *Config) *string { return &c.Temporal.TaskQueuePrefix }))),
		needs(DepTemporal, req("CP_TEMPORAL_REQUIRE_TLS", secTemporal, "Require TLS to Temporal. Must be true in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.Temporal.RequireTLS }))),

		needs(DepArchive, req("CP_ARCHIVE_BACKEND", secArchive, "Where evidence objects live: s3 | postgres. s3 makes every CP_ARCHIVE_* value below required and is what a deployment with object storage uses. postgres keeps objects in the application database, write-once by privilege and by trigger, and needs no object store at all -- but it cannot provide S3 Object Lock, so the audit chain still wants s3.", "s3",
			setArchiveBackend(func(c *Config) *ArchiveBackend { return &c.Archive.Backend }))),
		needs(DepArchive, req("CP_ARCHIVE_ENDPOINT", secArchive, "S3-compatible endpoint URL. Empty in AWS means the regional default; LOCAL points at MinIO.", "http://127.0.0.1:9100",
			setString(func(c *Config) *string { return &c.Archive.Endpoint }))),
		needs(DepArchive, req("CP_ARCHIVE_REGION", secArchive, "S3 region.", "us-east-1",
			setString(func(c *Config) *string { return &c.Archive.Region }))),
		needs(DepArchive, req("CP_ARCHIVE_RAW_BUCKET", secArchive, "Bucket for raw provider/market payloads.", "raw-events",
			setString(func(c *Config) *string { return &c.Archive.RawBucket }))),
		needs(DepArchive, req("CP_ARCHIVE_EVIDENCE_BUCKET", secArchive, "Bucket for provider evidence (requests/responses, receipts).", "provider-evidence",
			setString(func(c *Config) *string { return &c.Archive.EvidenceBucket }))),
		needs(DepArchive, req("CP_ARCHIVE_AUDIT_BUCKET", secArchive, "WORM bucket for the audit chain. Object Lock must be enabled in STAGING/PROD.", "audit-evidence",
			setString(func(c *Config) *string { return &c.Archive.AuditBucket }))),
		needs(DepArchive, req("CP_ARCHIVE_OBJECT_LOCK_REQUIRED", secArchive, "Refuse to start unless the audit bucket has Object Lock. Must be true in STAGING/PROD.", "false",
			setBool(func(c *Config) *bool { return &c.Archive.ObjectLockRequired }))),
		needs(DepArchive, req("CP_ARCHIVE_FORCE_PATH_STYLE", secArchive, "Use path-style S3 addressing (needed for MinIO).", "true",
			setBool(func(c *Config) *bool { return &c.Archive.ForcePathStyle }))),
		needs(DepArchive, secretVar(opt("CP_ARCHIVE_ACCESS_KEY_REF", secArchive, "SecretRef to a static S3 access key. Leave empty in AWS to use the task IAM role (preferred; goal PART 99).", "cp_minio",
			setSecret(func(c *Config) *SecretRef { return &c.Archive.AccessKeyRef })))),
		needs(DepArchive, secretVar(opt("CP_ARCHIVE_SECRET_KEY_REF", secArchive, "SecretRef to a static S3 secret key. Leave empty in AWS to use the task IAM role.", "cp_minio_local",
			setSecret(func(c *Config) *SecretRef { return &c.Archive.SecretKeyRef })))),

		needs(DepKMS, opt("CP_KMS_AUDIT_SIGNING_KEY_ID", secKMS, "KMS key id/ARN used to sign audit records. Required in STAGING/PROD of the binary that signs, which is cmd/audit-worker alone.", "",
			setString(func(c *Config) *string { return &c.KMS.AuditSigningKeyID }))),
		needs(DepKMS, opt("CP_KMS_REGION", secKMS, "KMS region. Required when a signing key is set.", "",
			setString(func(c *Config) *string { return &c.KMS.Region }))),

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
		opt("CP_AUTH_POST_LOGIN_URL", secAuth, "Where the OIDC callback sends the browser after setting the session cookie. Empty means a path on the API's own origin, which is what a same-origin development run (the Vite proxy) needs and is only right there. Required in STAGING/PROD with CP_AUTH_MODE=oidc, where the app has its own origin and the API's root is a 404 problem document. The LOCAL/TEST default is empty rather than one deployment's hostname: a default naming app-nodal.actorvia.xyz would send a developer's browser to the internet.", "",
			setString(func(c *Config) *string { return &c.Auth.PostLoginURL })),
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
		opt("CP_AUTH_BOOTSTRAP_OPERATORS", secAuth, "Operator-directory rows this deployment grants at login, as issuer|subject=ROLE entries separated by commas. Nothing else writes operator_roles, so this is how a deployment gets its first operator. Empty declares none. PROD accepts only empty or exactly one ADMIN.", "",
			setString(func(c *Config) *string { return &c.Auth.BootstrapOperators })),
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
			opt(prefix+"DESCRIPTOR_PREFIX", secProviders, "The static descriptor the "+label+" provider's account puts on a customer's statement, e.g. a card statement prefix. It is used to check that the dynamic suffix fits the provider's length limit, because a provider that truncates rather than refuses ships a descriptor nobody chose.", "",
				setString(func(c *Config) *string { return &slot.Get(&c.Providers).DescriptorPrefix })),
			opt(prefix+"DESCRIPTOR_SUFFIX", secProviders, "What this product should be called on a customer's statement. Required when the provider account is shared: a charge that shows another product's name is a charge a customer disputes.", "",
				setString(func(c *Config) *string { return &slot.Get(&c.Providers).DescriptorSuffix })),
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

		secretVar(opt("CP_ALERT_WEBHOOK_URL", secAlert, "Where a raised alert is POSTed as JSON. Empty means alerts are logged and delivered nowhere, which is said at startup rather than assumed. A SecretRef because a Slack or Discord webhook URL is itself the credential.",
			"https://hooks.example.test/services/AAA/BBB/CCC",
			setSecret(func(c *Config) *SecretRef { return &c.Alert.WebhookURL }))),
		req("CP_ALERT_WEBHOOK_FORMAT", secAlert, "Payload shape the destination accepts: auto, generic, slack, discord or ntfy. Slack and Discord refuse a body that is not their own shape with a 400, so a wrong value here is a destination that rejects every alert. auto derives it from the URL's host and is right for hooks.slack.com, discord.com and ntfy.sh; name it explicitly for a self-hosted ntfy or a Mattermost hook.", "auto",
			setString(func(c *Config) *string { return &c.Alert.WebhookFormat })),
		req("CP_ALERT_MIN_SEVERITY", secAlert, "Lowest severity worth delivering: SEV1 or SEV2. SEV2 delivers everything.", "SEV2",
			setString(func(c *Config) *string { return &c.Alert.MinSeverity })),
		req("CP_ALERT_TIMEOUT", secAlert, "Bound on one delivery attempt. Short on purpose: delivery runs behind a small queue and a slow destination delays every alert behind it.", "5s",
			setDuration(func(c *Config) *time.Duration { return &c.Alert.Timeout })),
		secretVar(opt("CP_PII_KEYRING_REF", secPII, "Keyring for personal data at rest, as a JSON document: {\"active\": N, \"keys\": {\"N\": \"<base64 32 bytes>\"}}. internal/pii seals identity_pii's columns with AES-256-GCM under the active version and opens a row under whichever version it names, so rotation is: add a key, make it active, deploy, reseal, then remove the old key. Required in STAGING/PROD. Empty in LOCAL/TEST means no personal data is stored, which is said at startup. A SecretRef because it IS the key.", "env://NODAL_PII_KEYRING",
			setSecret(func(c *Config) *SecretRef { return &c.PII.Keyring }))),

		req("CP_SEED_ENABLED", secSeed, "Allow the developer seed scripts (scripts/seed, scripts/seedeconomy) to write clearly-labelled fake identities, assets and balances. They refuse anything but LOCAL/DEV/TEST anyway; setting this false stops them there too. Must be false in STAGING/PROD, which is why it is not and cannot be the control for a sandbox tier's demo catalogue -- that is CP_API_DEMO_DATA.", "false",
			setBool(func(c *Config) *bool { return &c.Seed.Enabled })),

		req("CP_CREDIT_SETTLEMENT_WINDOW", secCredit, "How long a captured card payment stays reversible before its Credits may be treated as settled. A risk determination, not a default worth trusting: card scheme chargeback windows run to 120 days.", "720h",
			setDuration(func(c *Config) *time.Duration { return &c.Credit.SettlementWindow })),

		req("CP_RETENTION_LOGIN_ATTEMPT_DAYS", secRetention, "Days a login_attempts row is kept after it expired. It holds a plaintext OIDC nonce and PKCE verifier; the durable record of a login is a security_events row. Minimum 1.", "2",
			setInt(func(c *Config) *int { return &c.Retention.LoginAttemptDays })),
		req("CP_RETENTION_SECURITY_EVENT_DAYS", secRetention, "Days a monthly partition of security_events is kept before it is detached and dropped. 0 disables pruning entirely, which is the default because ADR-0020 leaves the period open; when set it must be at least 90.", "0",
			setInt(func(c *Config) *int { return &c.Retention.SecurityEventDays })),
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

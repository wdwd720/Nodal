package config

import (
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"strings"
)

// Rule names a validation rule. Every Violation carries one so that tests and
// operators can identify exactly which rule failed.
type Rule string

// Rules applied in every environment.
const (
	// RuleField is a basic well-formedness violation (empty, unparsable,
	// out of range).
	RuleField Rule = "FIELD"
	// RuleSecretRefScheme rejects SecretRef schemes not permitted in the
	// environment (plain outside LOCAL/TEST, file:// outside LOCAL/TEST/DEV).
	RuleSecretRefScheme Rule = "SECRET_REF_SCHEME" // #nosec G101 -- validation rule name, not a credential
	// RuleOIDCConfigured requires issuer, client id, client secret and
	// redirect URL when auth mode is oidc (https in STAGING/PROD).
	RuleOIDCConfigured Rule = "OIDC_CONFIGURED"
	// RuleNoDevAuth rejects auth mode "dev" outside LOCAL/TEST/DEV.
	RuleNoDevAuth Rule = "NO_DEV_AUTH"
	// RuleNoFakeProviders rejects provider mode "fake" in STAGING/PROD.
	RuleNoFakeProviders Rule = "NO_FAKE_PROVIDERS"
)

// Rules applied only when Environment.IsProductionLike (STAGING, PROD).
const (
	RuleNoDebugAuth        Rule = "NO_DEBUG_AUTH"
	RuleNoSeed             Rule = "NO_SEED"
	RuleDatabaseTLS        Rule = "DATABASE_TLS"
	RuleRedisTLS           Rule = "REDIS_TLS"
	RuleRedpandaTLS        Rule = "REDPANDA_TLS"
	RuleClickHouseTLS      Rule = "CLICKHOUSE_TLS"
	RuleTemporalTLS        Rule = "TEMPORAL_TLS"
	RuleNoCORSWildcard     Rule = "NO_CORS_WILDCARD"
	RuleArchiveConfigured  Rule = "ARCHIVE_CONFIGURED"
	RuleArchiveObjectLock  Rule = "ARCHIVE_OBJECT_LOCK"
	RuleKMSConfigured      Rule = "KMS_CONFIGURED"
	RuleCookieSecure       Rule = "COOKIE_SECURE"
	RulePublicBaseURLHTTPS Rule = "PUBLIC_BASE_URL_HTTPS"
	RuleRetentionNonZero   Rule = "RETENTION_NON_ZERO"
	RulePublicProductName  Rule = "PUBLIC_PRODUCT_NAME"
	RuleCapabilityStore    Rule = "CAPABILITY_STORE_CONFIGURED"
	// RuleNoInsecureOTLP applies to PROD only: telemetry must be exported
	// over TLS.
	RuleNoInsecureOTLP Rule = "NO_INSECURE_OTLP"
)

// Violation is one failed validation rule.
type Violation struct {
	Rule   Rule
	Field  string
	Detail string
}

func (v *Violation) Error() string {
	return fmt.Sprintf("config: rule %s: %s: %s", v.Rule, v.Field, v.Detail)
}

// Violations flattens err (typically the joined error from Validate) into
// the individual violations it contains.
func Violations(err error) []*Violation {
	var out []*Violation
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if v, ok := e.(*Violation); ok {
			out = append(out, v)
			return
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
	return out
}

// HasViolation reports whether err contains a violation of rule.
func HasViolation(err error, rule Rule) bool {
	for _, v := range Violations(err) {
		if v.Rule == rule {
			return true
		}
	}
	return false
}

// Validate checks every rule and returns all violations joined into one
// error (nil when valid). Environment-independent rules run everywhere;
// production rules run when Env.IsProductionLike. Validation never applies a
// default: it only rejects.
func (c *Config) Validate() error {
	var errs []error
	add := func(rule Rule, field, detail string) {
		errs = append(errs, &Violation{Rule: rule, Field: field, Detail: detail})
	}

	if !c.Env.IsValid() {
		add(RuleField, "Env", fmt.Sprintf("unknown environment %q", string(c.Env)))
		return errors.Join(errs...)
	}
	env := c.Env
	prodLike := env.IsProductionLike()

	// Which external systems this binary actually uses. An unknown or unset
	// Service requires nothing, and that is the safe direction here rather than
	// the dangerous one: Load refuses an unknown service before Validate is
	// ever reached, so the only way to arrive here without one is a Config
	// somebody built by hand in a test, which is not a deployment.
	if c.Service != "" && !c.Service.Valid() {
		add(RuleField, "Service", fmt.Sprintf("unknown service %q", string(c.Service)))
		return errors.Join(errs...)
	}
	needsRedis := c.Service.Requires(DepRedis)
	needsRedpanda := c.Service.Requires(DepRedpanda)
	needsClickHouse := c.Service.Requires(DepClickHouse)
	needsTemporal := c.Service.Requires(DepTemporal)
	needsArchive := c.Service.Requires(DepArchive)

	// ---- core --------------------------------------------------------------
	if c.ServiceName == "" {
		add(RuleField, "ServiceName", "must not be empty")
	}
	if prodLike && c.PublicProductName == "" {
		add(RulePublicProductName, "PublicProductName", "must be set in STAGING/PROD")
	}

	// ---- HTTP --------------------------------------------------------------
	if c.HTTP.Addr == "" {
		add(RuleField, "HTTP.Addr", "must not be empty")
	}
	if u, err := parseHTTPURL(c.HTTP.PublicBaseURL); err != nil {
		add(RuleField, "HTTP.PublicBaseURL", err.Error())
	} else if prodLike && u.Scheme != "https" {
		add(RulePublicBaseURLHTTPS, "HTTP.PublicBaseURL", "must use https in STAGING/PROD")
	}
	for name, d := range map[string]int64{
		"HTTP.ReadTimeout":  int64(c.HTTP.ReadTimeout),
		"HTTP.WriteTimeout": int64(c.HTTP.WriteTimeout),
		"HTTP.IdleTimeout":  int64(c.HTTP.IdleTimeout),
		"HTTP.MaxBodyBytes": c.HTTP.MaxBodyBytes,
	} {
		if d <= 0 {
			add(RuleField, name, "must be > 0")
		}
	}
	for _, o := range c.HTTP.CORSOrigins {
		if o == "*" {
			if prodLike {
				add(RuleNoCORSWildcard, "HTTP.CORSOrigins", "wildcard origin is not allowed in STAGING/PROD")
			}
			continue
		}
		if u, err := parseHTTPURL(o); err != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			add(RuleField, "HTTP.CORSOrigins", fmt.Sprintf("%q is not an origin (scheme://host[:port])", o))
		}
	}
	for _, cidr := range c.HTTP.TrustedProxyCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			add(RuleField, "HTTP.TrustedProxyCIDRs", fmt.Sprintf("%q is not a CIDR", cidr))
		}
	}

	// ---- database ----------------------------------------------------------
	if c.Database.AppURL.IsZero() {
		add(RuleField, "Database.AppURL", "must not be empty")
	}
	if c.Database.MigrateURL.IsZero() {
		add(RuleField, "Database.MigrateURL", "must not be empty")
	}
	if c.Database.MaxConns < 1 {
		add(RuleField, "Database.MaxConns", "must be >= 1")
	}
	if c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		add(RuleField, "Database.MinConns", "must be between 0 and MaxConns")
	}
	if c.Database.StatementTimeout <= 0 {
		add(RuleField, "Database.StatementTimeout", "must be > 0")
	}
	if c.Database.LockTimeout <= 0 {
		add(RuleField, "Database.LockTimeout", "must be > 0")
	}
	if prodLike && !c.Database.RequireTLS {
		add(RuleDatabaseTLS, "Database.RequireTLS", "must be true in STAGING/PROD")
	}
	if prodLike && !c.Capability.StoreConfigured {
		add(RuleCapabilityStore, "Capability.StoreConfigured", "capability store (application database) must be configured in STAGING/PROD")
	}

	// ---- redis -------------------------------------------------------------
	//
	// The presence rules below are asked only of a service that declares the
	// dependency. The FORMAT rules are not conditional: a malformed value is a
	// mistake whoever set it wants to hear about, whether or not this binary
	// would have dialled it. That split is why each block tests presence under
	// `needs` and parses unconditionally.
	if needsRedis {
		if c.Redis.URL.IsZero() {
			add(RuleField, "Redis.URL", "must not be empty")
		}
		if prodLike && !c.Redis.RequireTLS {
			add(RuleRedisTLS, "Redis.RequireTLS", "must be true in STAGING/PROD")
		}
	}

	// ---- redpanda ----------------------------------------------------------
	if needsRedpanda {
		if len(c.Redpanda.Brokers) == 0 {
			add(RuleField, "Redpanda.Brokers", "at least one broker is required")
		}
		if prodLike && !c.Redpanda.RequireTLS {
			add(RuleRedpandaTLS, "Redpanda.RequireTLS", "must be true in STAGING/PROD")
		}
	}
	if c.Redpanda.SASLMechanism != "" {
		if c.Redpanda.SASLUsernameRef.IsZero() {
			add(RuleField, "Redpanda.SASLUsernameRef", "required when SASLMechanism is set")
		}
		if c.Redpanda.SASLPasswordRef.IsZero() {
			add(RuleField, "Redpanda.SASLPasswordRef", "required when SASLMechanism is set")
		}
	}

	// ---- clickhouse --------------------------------------------------------
	if needsClickHouse {
		if c.ClickHouse.Addr == "" {
			add(RuleField, "ClickHouse.Addr", "must not be empty")
		}
		if c.ClickHouse.Database == "" {
			add(RuleField, "ClickHouse.Database", "must not be empty")
		}
		if prodLike && !c.ClickHouse.RequireTLS {
			add(RuleClickHouseTLS, "ClickHouse.RequireTLS", "must be true in STAGING/PROD")
		}
	}

	// ---- temporal ----------------------------------------------------------
	if needsTemporal {
		if c.Temporal.HostPort == "" {
			add(RuleField, "Temporal.HostPort", "must not be empty")
		}
		if c.Temporal.Namespace == "" {
			add(RuleField, "Temporal.Namespace", "must not be empty")
		}
		if c.Temporal.TaskQueuePrefix == "" {
			add(RuleField, "Temporal.TaskQueuePrefix", "must not be empty")
		}
		if prodLike && !c.Temporal.RequireTLS {
			add(RuleTemporalTLS, "Temporal.RequireTLS", "must be true in STAGING/PROD")
		}
	}

	// ---- archive -----------------------------------------------------------
	if c.Archive.Endpoint != "" {
		if _, err := parseHTTPURL(c.Archive.Endpoint); err != nil {
			add(RuleField, "Archive.Endpoint", err.Error())
		}
	}
	if prodLike && needsArchive {
		for name, v := range map[string]string{
			"Archive.Region":         c.Archive.Region,
			"Archive.RawBucket":      c.Archive.RawBucket,
			"Archive.EvidenceBucket": c.Archive.EvidenceBucket,
			"Archive.AuditBucket":    c.Archive.AuditBucket,
		} {
			if v == "" {
				add(RuleArchiveConfigured, name, "must be set in STAGING/PROD")
			}
		}
		// Object Lock is a compliance control on the audit bucket, and it is
		// asked of the services that write to an archive. A worker with no
		// archive being required to declare Object Lock on a bucket it never
		// touches is not a control; it is a value somebody sets to true to make
		// a startup error go away, which is how controls stop meaning anything.
		if !c.Archive.ObjectLockRequired {
			add(RuleArchiveObjectLock, "Archive.ObjectLockRequired", "audit bucket Object Lock must be required in STAGING/PROD")
		}
	}

	// ---- kms ---------------------------------------------------------------
	if c.KMS.AuditSigningKeyID != "" && c.KMS.Region == "" {
		add(RuleField, "KMS.Region", "required when AuditSigningKeyID is set")
	}
	if prodLike && c.KMS.AuditSigningKeyID == "" {
		add(RuleKMSConfigured, "KMS.AuditSigningKeyID", "must be set in STAGING/PROD")
	}

	// ---- auth --------------------------------------------------------------
	switch c.Auth.Mode {
	case AuthModeOIDC:
		for name, v := range map[string]string{
			"Auth.Issuer":      c.Auth.Issuer,
			"Auth.ClientID":    c.Auth.ClientID,
			"Auth.RedirectURL": c.Auth.RedirectURL,
		} {
			if v == "" {
				add(RuleOIDCConfigured, name, "required when Auth.Mode is oidc")
			}
		}
		if c.Auth.ClientSecretRef.IsZero() {
			add(RuleOIDCConfigured, "Auth.ClientSecretRef", "required when Auth.Mode is oidc")
		}
		for name, v := range map[string]string{"Auth.Issuer": c.Auth.Issuer, "Auth.RedirectURL": c.Auth.RedirectURL} {
			if v == "" {
				continue
			}
			u, err := parseHTTPURL(v)
			if err != nil {
				add(RuleOIDCConfigured, name, err.Error())
			} else if prodLike && u.Scheme != "https" {
				add(RuleOIDCConfigured, name, "must use https in STAGING/PROD")
			}
		}
	case AuthModeDev:
		if !env.AllowsDevAuth() {
			add(RuleNoDevAuth, "Auth.Mode", "dev auth is only allowed in LOCAL/TEST/DEV")
		}
	default:
		add(RuleField, "Auth.Mode", fmt.Sprintf("unknown auth mode %q (want oidc|dev)", c.Auth.Mode))
	}
	if c.Auth.CookieName == "" {
		add(RuleField, "Auth.CookieName", "must not be empty")
	}
	if c.Auth.SessionTTL <= 0 {
		add(RuleField, "Auth.SessionTTL", "must be > 0")
	}
	if c.Auth.StepUpMaxAge <= 0 {
		add(RuleField, "Auth.StepUpMaxAge", "must be > 0")
	}
	if prodLike && c.Auth.DebugAuthEnabled {
		add(RuleNoDebugAuth, "Auth.DebugAuthEnabled", "must be false in STAGING/PROD")
	}
	if prodLike && !c.Auth.CookieSecure {
		add(RuleCookieSecure, "Auth.CookieSecure", "must be true in STAGING/PROD")
	}

	// ---- providers ---------------------------------------------------------
	for _, slot := range providerSlots() {
		p := slot.Get(&c.Providers)
		field := "Providers." + slot.Name
		if !p.Mode.IsValid() {
			add(RuleField, field+".Mode", fmt.Sprintf("unknown provider mode %q", string(p.Mode)))
			continue
		}
		if p.Mode == ProviderModeFake && !env.AllowsFakeProviders() {
			add(RuleNoFakeProviders, field+".Mode", "fake providers are not allowed in STAGING/PROD")
		}
		if p.Mode != ProviderModeFake && p.Name == "" {
			add(RuleField, field+".Name", "required unless mode is fake")
		}
		if p.Timeout <= 0 {
			add(RuleField, field+".Timeout", "must be > 0")
		}
		if p.BaseURL != "" {
			if _, err := parseHTTPURL(p.BaseURL); err != nil {
				add(RuleField, field+".BaseURL", err.Error())
			}
		}
	}

	// ---- telemetry ---------------------------------------------------------
	if err := validateSampleRatio(c.Telemetry.TraceSampleRatio); err != nil {
		add(RuleField, "Telemetry.TraceSampleRatio", err.Error())
	}
	if c.Telemetry.MetricsInterval <= 0 {
		add(RuleField, "Telemetry.MetricsInterval", "must be > 0")
	}
	if ep := c.Telemetry.OTLPEndpoint; ep != "" {
		if strings.Contains(ep, "://") {
			add(RuleField, "Telemetry.OTLPEndpoint", "must be host:port without a scheme")
		} else if _, _, err := net.SplitHostPort(ep); err != nil {
			add(RuleField, "Telemetry.OTLPEndpoint", "must be host:port")
		}
	}
	if env == EnvProd && c.Telemetry.OTLPInsecure {
		add(RuleNoInsecureOTLP, "Telemetry.OTLPInsecure", "must be false in PROD")
	}

	// ---- seed --------------------------------------------------------------
	if prodLike && c.Seed.Enabled {
		add(RuleNoSeed, "Seed.Enabled", "must be false in STAGING/PROD")
	}

	// ---- retention ---------------------------------------------------------
	for name, d := range map[string]int{
		"Retention.FinancialRecordDays": c.Retention.FinancialRecordDays,
		"Retention.SecurityAuditDays":   c.Retention.SecurityAuditDays,
		"Retention.RawMarketDataDays":   c.Retention.RawMarketDataDays,
		"Retention.SocialDataDays":      c.Retention.SocialDataDays,
		"Retention.ModelIODays":         c.Retention.ModelIODays,
		"Retention.OperationalLogDays":  c.Retention.OperationalLogDays,
	} {
		if d < 0 {
			add(RuleField, name, "must not be negative")
		}
	}
	if prodLike {
		if c.Retention.FinancialRecordDays <= 0 {
			add(RuleRetentionNonZero, "Retention.FinancialRecordDays", "must be > 0 in STAGING/PROD")
		}
		if c.Retention.SecurityAuditDays <= 0 {
			add(RuleRetentionNonZero, "Retention.SecurityAuditDays", "must be > 0 in STAGING/PROD")
		}
	}

	// ---- secret references (structural: every SecretRef field) -------------
	for path, ref := range c.secretRefs() {
		if err := ref.ValidateFor(env); err != nil {
			add(RuleSecretRefScheme, path, err.Error())
		}
	}

	return errors.Join(errs...)
}

func parseHTTPURL(s string) (*url.URL, error) {
	if s == "" {
		return nil, errors.New("must not be empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q", s)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q must be an absolute http(s) URL", s)
	}
	return u, nil
}

// validateSampleRatio checks that s is a decimal in [0, 1] using exact
// rational arithmetic; the config package never parses floats.
func validateSampleRatio(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("must not be empty")
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return fmt.Errorf("%q must be a plain decimal such as 1 or 0.25", s)
		}
	}
	rat, ok := new(big.Rat).SetString(s)
	if !ok {
		return fmt.Errorf("%q is not a decimal", s)
	}
	if rat.Sign() < 0 || rat.Cmp(big.NewRat(1, 1)) > 0 {
		return fmt.Errorf("%q must be between 0 and 1", s)
	}
	return nil
}

package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prodRuleCases lists, for every production rule, one mutation that must
// trigger exactly that rule on an otherwise valid PROD/STAGING config.
func prodRuleCases() []struct {
	name    string
	rule    Rule
	service Service
	field   string
	mutate  func(*Config)
} {
	return []struct {
		name string
		rule Rule
		// service is the binary the rule is tested under. A rule about an
		// external dependency only runs for a service that declares it, so a
		// case with the wrong service here asserts against a rule that
		// correctly did not fire. Empty means the default (the API).
		service Service
		field   string
		mutate  func(*Config)
	}{
		{"fake provider", RuleNoFakeProviders, "", "Providers.execution.Mode", func(c *Config) { c.Providers.Execution.Mode = ProviderModeFake }},
		{"dev auth", RuleNoDevAuth, "", "Auth.Mode", func(c *Config) { c.Auth.Mode = AuthModeDev }},
		{"debug auth", RuleNoDebugAuth, "", "Auth.DebugAuthEnabled", func(c *Config) { c.Auth.DebugAuthEnabled = true }},
		{"seed", RuleNoSeed, "", "Seed.Enabled", func(c *Config) { c.Seed.Enabled = true }},
		{"database tls", RuleDatabaseTLS, "", "Database.RequireTLS", func(c *Config) { c.Database.RequireTLS = false }},
		// "redis tls" is back. It was removed while nothing in the repository
		// constructed a Redis client; cmd/api now builds one whenever the
		// rate-limit backend is redis, which the production fixture selects
		// because STAGING and PROD refuse the per-process alternative.
		{"redis tls", RuleRedisTLS, ServiceAPI, "Redis.RequireTLS", func(c *Config) { c.Redis.RequireTLS = false }},
		{"memory rate limit", RuleDistributedRateLimit, ServiceAPI, "RateLimit.Backend", func(c *Config) { c.RateLimit.Backend = RateLimitMemory }},
		{"redpanda tls", RuleRedpandaTLS, ServiceRelayWorker, "Redpanda.RequireTLS", func(c *Config) { c.Redpanda.RequireTLS = false }},
		{"clickhouse tls", RuleClickHouseTLS, ServiceMarketIngestWorker, "ClickHouse.RequireTLS", func(c *Config) { c.ClickHouse.RequireTLS = false }},
		{"temporal tls", RuleTemporalTLS, ServiceWorkflowWorker, "Temporal.RequireTLS", func(c *Config) { c.Temporal.RequireTLS = false }},
		{"cors wildcard", RuleNoCORSWildcard, "", "HTTP.CORSOrigins", func(c *Config) { c.HTTP.CORSOrigins = append(c.HTTP.CORSOrigins, "*") }},
		{"archive raw bucket", RuleArchiveConfigured, "", "Archive.RawBucket", func(c *Config) { c.Archive.RawBucket = "" }},
		{"archive evidence bucket", RuleArchiveConfigured, "", "Archive.EvidenceBucket", func(c *Config) { c.Archive.EvidenceBucket = "" }},
		{"archive audit bucket", RuleArchiveConfigured, "", "Archive.AuditBucket", func(c *Config) { c.Archive.AuditBucket = "" }},
		{"archive region", RuleArchiveConfigured, "", "Archive.Region", func(c *Config) { c.Archive.Region = "" }},
		{"archive object lock", RuleArchiveObjectLock, "", "Archive.ObjectLockRequired", func(c *Config) { c.Archive.ObjectLockRequired = false }},
		{"kms key", RuleKMSConfigured, "", "KMS.AuditSigningKeyID", func(c *Config) { c.KMS.AuditSigningKeyID = "" }},
		{"cookie secure", RuleCookieSecure, "", "Auth.CookieSecure", func(c *Config) { c.Auth.CookieSecure = false }},
		{"public base url http", RulePublicBaseURLHTTPS, "", "HTTP.PublicBaseURL", func(c *Config) { c.HTTP.PublicBaseURL = "http://api.example.com" }},
		{"financial retention zero", RuleRetentionNonZero, "", "Retention.FinancialRecordDays", func(c *Config) { c.Retention.FinancialRecordDays = 0 }},
		{"security audit retention zero", RuleRetentionNonZero, "", "Retention.SecurityAuditDays", func(c *Config) { c.Retention.SecurityAuditDays = 0 }},
		{"public product name", RulePublicProductName, "", "PublicProductName", func(c *Config) { c.PublicProductName = "" }},
		{"capability store", RuleCapabilityStore, "", "Capability.StoreConfigured", func(c *Config) { c.Capability.StoreConfigured = false }},
		{"plain secret", RuleSecretRefScheme, "", "Database.AppURL", func(c *Config) { c.Database.AppURL = "postgres://u:p@h/db?sslmode=verify-full" }},
		{"file secret", RuleSecretRefScheme, "", "Auth.ClientSecretRef", func(c *Config) { c.Auth.ClientSecretRef = "file:///run/secrets/oidc" }},
		{"oidc issuer http", RuleOIDCConfigured, "", "Auth.Issuer", func(c *Config) { c.Auth.Issuer = "http://login.example.com" }},
		{"oidc redirect http", RuleOIDCConfigured, "", "Auth.RedirectURL", func(c *Config) { c.Auth.RedirectURL = "http://api.example.com/cb" }},
		{"oidc missing client secret", RuleOIDCConfigured, "", "Auth.ClientSecretRef", func(c *Config) { c.Auth.ClientSecretRef = "" }},
	}
}

func TestValidate_ValidProdHasNoViolations(t *testing.T) {
	t.Parallel()
	c := validProdConfig(t)
	require.NoError(t, c.Validate())
	staging := c.Clone()
	staging.Env = EnvStaging
	require.NoError(t, staging.Validate())
}

func TestValidate_ProdRulesIndividually(t *testing.T) {
	t.Parallel()
	for _, env := range []Environment{EnvProd, EnvStaging} {
		for _, tc := range prodRuleCases() {
			t.Run(string(env)+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				service := tc.service
				if service == "" {
					service = ServiceAPI
				}
				c := validProdConfigAs(t, service)
				c.Env = env
				require.NoError(t, c.Validate(), "baseline must be valid")
				tc.mutate(c)
				err := c.Validate()
				require.Error(t, err)
				vs := Violations(err)
				require.Len(t, vs, 1, "exactly one violation expected, got: %v", err)
				assert.Equal(t, tc.rule, vs[0].Rule)
				assert.Equal(t, tc.field, vs[0].Field)
				assert.Contains(t, err.Error(), string(tc.rule), "error text names the rule")
				assert.True(t, HasViolation(err, tc.rule))
			})
		}
	}
}

func TestValidate_NoFakeProvidersCoversEverySlot(t *testing.T) {
	t.Parallel()
	for _, slot := range providerSlots() {
		t.Run(slot.Name, func(t *testing.T) {
			t.Parallel()
			c := validProdConfig(t)
			slot.Get(&c.Providers).Mode = ProviderModeFake
			err := c.Validate()
			require.Error(t, err)
			vs := Violations(err)
			require.Len(t, vs, 1)
			assert.Equal(t, RuleNoFakeProviders, vs[0].Rule)
			assert.Equal(t, "Providers."+slot.Name+".Mode", vs[0].Field)
		})
	}
}

func TestValidate_InsecureOTLPOnlyRejectedInProd(t *testing.T) {
	t.Parallel()
	c := validProdConfig(t)
	c.Telemetry.OTLPInsecure = true
	err := c.Validate()
	require.Error(t, err)
	assert.True(t, HasViolation(err, RuleNoInsecureOTLP))
	assert.Len(t, Violations(err), 1)

	staging := validProdConfig(t)
	staging.Env = EnvStaging
	staging.Telemetry.OTLPInsecure = true
	assert.NoError(t, staging.Validate(), "STAGING may export telemetry without TLS")
}

func TestValidate_JoinsEveryViolation(t *testing.T) {
	t.Parallel()
	c := validProdConfig(t)
	c.Providers.Funding.Mode = ProviderModeFake
	c.Auth.DebugAuthEnabled = true
	c.Seed.Enabled = true
	c.Database.RequireTLS = false
	c.HTTP.CORSOrigins = []string{"*"}
	c.KMS.AuditSigningKeyID = ""
	err := c.Validate()
	require.Error(t, err)
	for _, r := range []Rule{RuleNoFakeProviders, RuleNoDebugAuth, RuleNoSeed, RuleDatabaseTLS, RuleNoCORSWildcard, RuleKMSConfigured} {
		assert.True(t, HasViolation(err, r), "%s must be reported alongside the others", r)
	}
	assert.Len(t, Violations(err), 6)
	assert.Equal(t, 6, strings.Count(err.Error(), "config: rule "))
}

func TestValidate_LocalAndTestPermitDevelopmentSettings(t *testing.T) {
	t.Parallel()
	for _, env := range []Environment{EnvLocal, EnvTest} {
		t.Run(string(env), func(t *testing.T) {
			t.Parallel()
			c := validProdConfig(t)
			c.Env = env
			for _, tc := range prodRuleCases() {
				if tc.rule == RuleOIDCConfigured && strings.Contains(tc.name, "missing") {
					continue // a structural OIDC rule, not a production-only one
				}
				tc.mutate(c)
			}
			c.Telemetry.OTLPInsecure = true
			assert.NoError(t, c.Validate(), "every production-only rule is relaxed in %s", env)
		})
	}
}

func TestValidate_DevAllowsFakesButNotPlainSecrets(t *testing.T) {
	t.Parallel()
	c := validProdConfig(t)
	c.Env = EnvDev
	c.Providers.Funding.Mode = ProviderModeFake
	c.Auth.Mode = AuthModeDev
	c.Database.RequireTLS = false
	c.Auth.ClientSecretRef = "file:///run/secrets/oidc"
	require.NoError(t, c.Validate())

	c.Database.AppURL = "postgres://plain@localhost/db"
	err := c.Validate()
	require.Error(t, err)
	assert.True(t, HasViolation(err, RuleSecretRefScheme))
	assert.Len(t, Violations(err), 1)
}

func TestValidate_FieldRulesApplyEverywhere(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		field  string
		mutate func(*Config)
	}{
		{"empty service name", "ServiceName", func(c *Config) { c.ServiceName = "" }},
		{"bad public url", "HTTP.PublicBaseURL", func(c *Config) { c.HTTP.PublicBaseURL = "api.example.com" }},
		{"zero read timeout", "HTTP.ReadTimeout", func(c *Config) { c.HTTP.ReadTimeout = 0 }},
		{"zero body limit", "HTTP.MaxBodyBytes", func(c *Config) { c.HTTP.MaxBodyBytes = 0 }},
		{"bad cors origin", "HTTP.CORSOrigins", func(c *Config) { c.HTTP.CORSOrigins = []string{"https://app.example.com/path"} }},
		{"bad cidr", "HTTP.TrustedProxyCIDRs", func(c *Config) { c.HTTP.TrustedProxyCIDRs = []string{"10.0.0.1"} }},
		{"min conns above max", "Database.MinConns", func(c *Config) { c.Database.MinConns = c.Database.MaxConns + 1 }},
		{"zero max conns", "Database.MaxConns", func(c *Config) { c.Database.MaxConns = 0; c.Database.MinConns = 0 }},
		// "no brokers" is a presence rule, so it belongs to a service that
		// declares Redpanda. The SASL pairing below is a FORMAT rule and stays
		// unconditional: half a credential is a mistake whoever set it wants
		// to hear about, whether or not this binary would have connected.
		{"sasl without username", "Redpanda.SASLUsernameRef", func(c *Config) { c.Redpanda.SASLUsernameRef = "" }},
		{"kms key without region", "KMS.Region", func(c *Config) { c.KMS.Region = "" }},
		{"unknown auth mode", "Auth.Mode", func(c *Config) { c.Auth.Mode = "basic" }},
		{"zero session ttl", "Auth.SessionTTL", func(c *Config) { c.Auth.SessionTTL = 0 }},
		{"provider name missing", "Providers.wallet.Name", func(c *Config) { c.Providers.Wallet.Name = "" }},
		{"provider zero timeout", "Providers.model.Timeout", func(c *Config) { c.Providers.Model.Timeout = 0 }},
		{"provider bad url", "Providers.model.BaseURL", func(c *Config) { c.Providers.Model.BaseURL = "not a url" }},
		{"sample ratio above one", "Telemetry.TraceSampleRatio", func(c *Config) { c.Telemetry.TraceSampleRatio = "1.5" }},
		{"sample ratio negative", "Telemetry.TraceSampleRatio", func(c *Config) { c.Telemetry.TraceSampleRatio = "-0.1" }},
		{"sample ratio scientific", "Telemetry.TraceSampleRatio", func(c *Config) { c.Telemetry.TraceSampleRatio = "1e-1" }},
		{"sample ratio text", "Telemetry.TraceSampleRatio", func(c *Config) { c.Telemetry.TraceSampleRatio = "half" }},
		{"otlp endpoint with scheme", "Telemetry.OTLPEndpoint", func(c *Config) { c.Telemetry.OTLPEndpoint = "https://otel:4317" }},
		{"otlp endpoint without port", "Telemetry.OTLPEndpoint", func(c *Config) { c.Telemetry.OTLPEndpoint = "otel" }},
		{"negative retention", "Retention.ModelIODays", func(c *Config) { c.Retention.ModelIODays = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := validProdConfig(t)
			tc.mutate(c)
			err := c.Validate()
			require.Error(t, err)
			vs := Violations(err)
			require.NotEmpty(t, vs)
			found := false
			for _, v := range vs {
				if v.Field == tc.field {
					found = true
					assert.Equal(t, RuleField, v.Rule)
				}
			}
			assert.True(t, found, "expected a FIELD violation on %s, got %v", tc.field, err)
		})
	}
}

func TestValidate_SampleRatioAccepts(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"0", "1", "0.5", "0.001", "1.0", "1.000", ".25"} {
		assert.NoError(t, validateSampleRatio(ok), ok)
	}
}

func TestValidate_UnknownEnvironmentShortCircuits(t *testing.T) {
	t.Parallel()
	c := validProdConfig(t)
	c.Env = "PRODUCTION"
	err := c.Validate()
	require.Error(t, err)
	vs := Violations(err)
	require.Len(t, vs, 1)
	assert.Equal(t, "Env", vs[0].Field)
}

func TestViolations_HandlesNilAndForeignErrors(t *testing.T) {
	t.Parallel()
	assert.Empty(t, Violations(nil))
	assert.Empty(t, Violations(assert.AnError))
	assert.False(t, HasViolation(nil, RuleNoSeed))
}

// TestValidate_RedisTLSIsUnreachableUntilSomethingUsesRedis is the honest
// version of the "redis tls" case that used to sit in prodRuleCases.
//
// No service declares DepRedis, because nothing in this repository constructs
// a Redis client: ratelimit.NewRedisStore exists with no caller anywhere, and
// cmd/api explicitly chooses ratelimit.NewMemoryStore. So the rule cannot fire
// through Load, and asserting that it does would have meant inventing a fake
// service purely to make a test pass.
//
// What is worth asserting is the pair of facts that make that true today and
// the mechanism that will make the rule live the moment it stops being true.
// TestValidate_RedisTLSBelongsToWhoeverActuallyUsesRedis.
//
// Redis is the one dependency that is not a property of the binary alone: the
// API needs it when, and only when, its rate-limit counters are shared. So the
// Redis rules follow the configured backend rather than the service table, and
// this is the test that says so in both directions.
func TestValidate_RedisTLSBelongsToWhoeverActuallyUsesRedis(t *testing.T) {
	t.Parallel()

	// Held to the rule: the API with the distributed backend really does dial
	// Redis, so plaintext to it in production is plaintext on the wire.
	c := validProdConfig(t)
	c.Service = ServiceAPI
	c.RateLimit.Backend = RateLimitRedis
	c.Redis.RequireTLS = false
	err := c.Validate()
	require.Error(t, err, "the API dials Redis for its counters, so its TLS setting is its problem")
	assert.True(t, HasViolation(err, RuleRedisTLS))

	// Not held to it: a binary that never dials Redis is not asked how it
	// would have encrypted the connection.
	c = validProdConfig(t)
	c.Service = ServiceRelayWorker
	c.Redis.RequireTLS = false
	require.NoError(t, c.Validate(), "the relay worker does not use Redis")
}

// TestValidate_PresenceRulesBelongToTheServicesThatUseThem: the presence half
// of each dependency block fires for a service that declares it and stays
// silent for one that does not. The values are removed from an otherwise valid
// production configuration, so the only thing separating the two assertions is
// which binary is asking.
func TestValidate_PresenceRulesBelongToTheServicesThatUseThem(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		uses    Service
		notUses Service
		field   string
		mutate  func(*Config)
	}{
		{
			"redpanda brokers", ServiceRelayWorker, ServiceAPI, "Redpanda.Brokers",
			func(c *Config) { c.Redpanda.Brokers = nil },
		},
		{
			"clickhouse addr", ServiceMarketIngestWorker, ServiceAPI, "ClickHouse.Addr",
			func(c *Config) { c.ClickHouse.Addr = "" },
		},
		{
			"temporal host", ServiceWorkflowWorker, ServiceAPI, "Temporal.HostPort",
			func(c *Config) { c.Temporal.HostPort = "" },
		},
		{
			"archive evidence bucket", ServiceAPI, ServiceRelayWorker, "Archive.EvidenceBucket",
			func(c *Config) { c.Archive.EvidenceBucket = "" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			uses := validProdConfigAs(t, tc.uses)
			require.NoError(t, uses.Validate(), "baseline must be valid")
			tc.mutate(uses)
			err := uses.Validate()
			require.Error(t, err, "%s uses this dependency and must refuse it missing", tc.uses)
			require.Equal(t, tc.field, Violations(err)[0].Field)

			notUses := validProdConfigAs(t, tc.notUses)
			tc.mutate(notUses)
			require.NoError(t, notUses.Validate(),
				"%s does not use this dependency and must not be held to it", tc.notUses)
		})
	}
}

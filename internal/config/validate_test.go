package config

import (
	"context"
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
		// "memory rate limit" used to be here and cannot be: the rule is no
		// longer production-only. It fires whenever a binary serving HTTP
		// declares more than one replica, in every environment, because three
		// DEV replicas get the limit just as wrong as three PROD ones. Its own
		// test is TestValidate_TheRateLimitRuleFollowsReplicasNotEnvironment.
		{"redpanda tls", RuleRedpandaTLS, ServiceRelayWorker, "Redpanda.RequireTLS", func(c *Config) { c.Redpanda.RequireTLS = false }},
		{"clickhouse tls", RuleClickHouseTLS, ServiceMarketIngestWorker, "ClickHouse.RequireTLS", func(c *Config) { c.ClickHouse.RequireTLS = false }},
		{"temporal tls", RuleTemporalTLS, ServiceWorkflowWorker, "Temporal.RequireTLS", func(c *Config) { c.Temporal.RequireTLS = false }},
		{"cors wildcard", RuleNoCORSWildcard, "", "HTTP.CORSOrigins", func(c *Config) { c.HTTP.CORSOrigins = append(c.HTTP.CORSOrigins, "*") }},
		{"archive raw bucket", RuleArchiveConfigured, "", "Archive.RawBucket", func(c *Config) { c.Archive.RawBucket = "" }},
		{"archive evidence bucket", RuleArchiveConfigured, "", "Archive.EvidenceBucket", func(c *Config) { c.Archive.EvidenceBucket = "" }},
		{"archive audit bucket", RuleArchiveConfigured, "", "Archive.AuditBucket", func(c *Config) { c.Archive.AuditBucket = "" }},
		{"archive region", RuleArchiveConfigured, "", "Archive.Region", func(c *Config) { c.Archive.Region = "" }},
		{"archive object lock", RuleArchiveObjectLock, "", "Archive.ObjectLockRequired", func(c *Config) { c.Archive.ObjectLockRequired = false }},
		{"kms key", RuleKMSConfigured, ServiceAuditWorker, "KMS.AuditSigningKeyID", func(c *Config) { c.KMS.AuditSigningKeyID = "" }},
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

// TestValidate_AMoneyCeilingMustBeStated (F-97). Zero disables a ceiling in the
// guard, which is right for a library and wrong for a deployment that takes
// money: setting the money-at-risk cap to 0 in a dashboard loaded cleanly,
// validated, and logged "capacity ceilings in force" with it off.
func TestValidate_AMoneyCeilingMustBeStated(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"CP_CAPACITY_MAX_ACCOUNTS", "CP_CAPACITY_MAX_PURCHASES_PER_DAY", "CP_CAPACITY_MAX_AT_RISK_MINOR",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(context.Background(), ServiceAPI, LookupFromMap(withVars(prodEnv(), map[string]string{name: "0"})))
			require.Error(t, err, "%s = 0 disables a ceiling and was accepted", name)
			assert.Contains(t, err.Error(), string(RuleCapacityCeiling))
		})
	}
	// The control: the database ceiling may legitimately be zero, because
	// managed Postgres with no storage quota has nothing to state.
	_, err := Load(context.Background(), ServiceAPI, LookupFromMap(withVars(prodEnv(), map[string]string{"CP_CAPACITY_MAX_DATABASE_BYTES": "0"})))
	require.NoError(t, err)
}

func TestValidate_ValidProdHasNoViolations(t *testing.T) {
	t.Parallel()
	c := validProdConfig(t)
	require.NoError(t, c.Validate())
	require.NoError(t, asStaging(c).Validate())
}

// TestValidate_TheEnvironmentAndTheProviderModeMakeTheSameClaim.
//
// PROD in sandbox mode mints value against test objects nobody paid for.
// STAGING in live mode charges real cards for a rehearsal. Both adapters that
// take money refuse to be built on a mismatch -- but they refuse at startup,
// where the symptom is a WARN and a silently disabled capability, on a service
// that answers 200 on every health check. This is the check that happens
// first, where scripts/configcheck and the deployment tests can see it.
//
// This was reached in a real deployment: every provider slot said sandbox and
// CP_ENV said PROD, so the Credit purchase path was dark and the only visible
// symptom was a 404 on the webhook route.
func TestValidate_TheEnvironmentAndTheProviderModeMakeTheSameClaim(t *testing.T) {
	t.Parallel()

	t.Run("PROD refuses a provider sandbox", func(t *testing.T) {
		t.Parallel()
		c := validProdConfig(t)
		c.Providers.CreditPurchase.Mode = ProviderModeSandbox
		err := c.Validate()
		require.Error(t, err)
		assert.True(t, HasViolation(err, RuleProviderModeMatchesEnv))
		assert.Contains(t, err.Error(), "PROD requires live mode")
	})

	t.Run("STAGING refuses live money", func(t *testing.T) {
		t.Parallel()
		c := asStaging(validProdConfig(t))
		c.Providers.Payout.Mode = ProviderModeLive
		err := c.Validate()
		require.Error(t, err)
		require.Len(t, Violations(err), 1)
		assert.Equal(t, RuleProviderModeMatchesEnv, Violations(err)[0].Rule)
		assert.Equal(t, "Providers.payout.Mode", Violations(err)[0].Field)
	})

	t.Run("every slot is covered in both directions", func(t *testing.T) {
		t.Parallel()
		for _, slot := range providerSlots() {
			prod := validProdConfig(t)
			slot.Get(&prod.Providers).Mode = ProviderModeSandbox
			assert.True(t, HasViolation(prod.Validate(), RuleProviderModeMatchesEnv),
				"PROD accepts a sandbox %s provider", slot.Name)

			staging := asStaging(validProdConfig(t))
			slot.Get(&staging.Providers).Mode = ProviderModeLive
			assert.True(t, HasViolation(staging.Validate(), RuleProviderModeMatchesEnv),
				"STAGING accepts a live %s provider", slot.Name)
		}
	})

	t.Run("DEV is not constrained either way", func(t *testing.T) {
		t.Parallel()
		// Below STAGING the ladder is about developer conveniences, not about
		// whose money moves, and a DEV deployment pointed at a live provider is
		// a decision its operator gets to make.
		c := validProdConfig(t)
		c.Env = EnvDev
		c.Providers.CreditPurchase.Mode = ProviderModeSandbox
		assert.False(t, HasViolation(c.Validate(), RuleProviderModeMatchesEnv))
	})
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
				if env == EnvStaging {
					c = asStaging(c)
				}
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

	staging := asStaging(validProdConfig(t))
	staging.Telemetry.OTLPInsecure = true
	assert.NoError(t, staging.Validate(), "STAGING may export telemetry without TLS")
}

func TestValidate_JoinsEveryViolation(t *testing.T) {
	t.Parallel()
	// Loaded as the audit worker, because one of the six rules below belongs to
	// the binary that signs the audit chain and to no other. cmd/api links the
	// KMS SDK and never calls it.
	c := validProdConfigAs(t, ServiceAuditWorker)
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
			// The rate-limit rule is deliberately not environment-dependent
			// any more, so this test -- which is about the rules that ARE --
			// states the single replica that makes it moot.
			c.RateLimit.Replicas = 1
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

// TestValidate_TheRateLimitRuleFollowsReplicasNotEnvironment.
//
// A rate limit is a budget, and process-local counters give each process its
// own copy of it. So the configured limit is the enforced limit exactly when
// there is one process, and the environment was only ever a proxy for that.
// The proxy was wrong in both directions, and this is the test that says so:
// a single-process PROD deployment is correct, and a three-process LOCAL one
// is not.
func TestValidate_TheRateLimitRuleFollowsReplicasNotEnvironment(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		env      Environment
		backend  RateLimitBackend
		replicas int
		ok       bool
	}{
		{"one process in PROD may count in memory", EnvProd, RateLimitMemory, 1, true},
		{"three processes in PROD may not", EnvProd, RateLimitMemory, 3, false},
		{"three processes in LOCAL may not either", EnvLocal, RateLimitMemory, 3, false},
		{"one process in LOCAL is fine", EnvLocal, RateLimitMemory, 1, true},
		{"redis is fine at any count", EnvProd, RateLimitRedis, 12, true},
		{"redis is fine at one", EnvLocal, RateLimitRedis, 1, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := validProdConfigAs(t, ServiceAPI)
			c.Env = tc.env
			c.RateLimit.Backend = tc.backend
			c.RateLimit.Replicas = tc.replicas
			if tc.env == EnvLocal {
				// LOCAL relaxes the unrelated production rules; this test is
				// about one rule and must not be answered by another.
				c.Telemetry.OTLPInsecure = true
			}
			err := c.Validate()
			if tc.ok {
				assert.False(t, HasViolation(err, RuleDistributedRateLimit),
					"the rate-limit rule must not fire: %v", err)
				return
			}
			require.Error(t, err)
			assert.True(t, HasViolation(err, RuleDistributedRateLimit))
			assert.Contains(t, err.Error(), "keeps counters in the process")
		})
	}

	// And zero replicas is a typo rather than a policy.
	c := validProdConfigAs(t, ServiceAPI)
	c.RateLimit.Replicas = 0
	require.Error(t, c.Validate())
	assert.Contains(t, ruleText(c.Validate()), "at least 1")
}

// ruleText flattens a validation error for substring assertions.
func ruleText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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

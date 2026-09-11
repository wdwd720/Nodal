package config

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func varErrors(err error) map[string]error {
	out := map[string]error{}
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		var ve *VarError
		if errors.As(e, &ve) && !isJoin(e) {
			out[ve.Name] = ve.Err
			return
		}
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, c := range j.Unwrap() {
				walk(c)
			}
		}
	}
	walk(err)
	return out
}

func isJoin(e error) bool {
	_, ok := e.(interface{ Unwrap() []error })
	return ok
}

func TestLoad_LocalAndTestApplyDefaults(t *testing.T) {
	t.Parallel()
	for _, env := range []Environment{EnvLocal, EnvTest} {
		t.Run(string(env), func(t *testing.T) {
			t.Parallel()
			c := mustLoad(t, map[string]string{"CP_ENV": string(env)})
			assert.Equal(t, env, c.Env)
			assert.Equal(t, BuildVersion, c.BuildVersion)
			assert.Equal(t, "controlplane", c.ServiceName)
			assert.Equal(t, "Control Plane", c.PublicProductName)
			assert.Equal(t, "127.0.0.1:8080", c.HTTP.Addr)
			assert.Equal(t, 10*time.Second, c.HTTP.ReadTimeout)
			assert.Equal(t, int64(1048576), c.HTTP.MaxBodyBytes)
			assert.Empty(t, c.HTTP.CORSOrigins, "optional lists have no default")
			assert.Equal(t, SecretSchemePlain, c.Database.AppURL.Scheme())
			assert.Contains(t, string(c.Database.AppURL), "cp_app")
			assert.Contains(t, string(c.Database.MigrateURL), "cp_migrate")
			assert.True(t, c.Database.ReadOnlyURL.IsZero())
			assert.False(t, c.Database.RequireTLS)
			assert.Equal(t, int32(10), c.Database.MaxConns)
			assert.Equal(t, []string{"127.0.0.1:19092"}, c.Redpanda.Brokers)
			assert.Equal(t, "default", c.Temporal.Namespace)
			assert.Equal(t, "audit-evidence", c.Archive.AuditBucket)
			assert.Equal(t, AuthModeDev, c.Auth.Mode)
			assert.Equal(t, 12*time.Hour, c.Auth.SessionTTL)
			assert.False(t, c.Auth.DebugAuthEnabled)
			assert.False(t, c.Seed.Enabled)
			for _, slot := range providerSlots() {
				p := slot.Get(&c.Providers)
				assert.Equal(t, ProviderModeFake, p.Mode, slot.Name)
				assert.Equal(t, 10*time.Second, p.Timeout, slot.Name)
			}
			assert.Equal(t, "", c.Telemetry.OTLPEndpoint)
			assert.Equal(t, "1", c.Telemetry.TraceSampleRatio)
			assert.Equal(t, 2555, c.Retention.FinancialRecordDays)
			assert.True(t, c.Capability.StoreConfigured, "derived from the database URL")
			assert.NoError(t, c.Validate())
		})
	}
}

// TestLoad_DefaultsNeverApplyOutsideLocalTest is now per service, and says
// something stronger than it used to.
//
// Before, every binary required every variable, so the expected set was a
// constant. Now the expected set is exactly the unconditional required
// variables plus the ones belonging to dependencies THIS service declares --
// and the test computes both sides from the same tables the loader uses, so a
// variable tagged with the wrong dependency, or a service given the wrong
// dependency list, fails here rather than in a deployment.
func TestLoad_DefaultsNeverApplyOutsideLocalTest(t *testing.T) {
	t.Parallel()
	for _, service := range AllServices() {
		for _, env := range []Environment{EnvDev, EnvStaging, EnvProd} {
			t.Run(string(service)+"/"+string(env), func(t *testing.T) {
				t.Parallel()

				var want []string
				for _, s := range Vars() {
					if !s.Required || s.Default == "" {
						continue
					}
					if s.Dep != "" && !service.Requires(s.Dep) {
						continue
					}
					if s.Svc != "" && s.Svc != service {
						continue
					}
					want = append(want, s.Name)
				}
				require.NotEmpty(t, want)

				c, err := Load(context.Background(), service, LookupFromMap(map[string]string{"CP_ENV": string(env)}))
				require.Error(t, err)
				assert.Nil(t, c)
				got := varErrors(err)
				for _, name := range want {
					require.Contains(t, got, name, "%s must be reported missing for %s in %s", name, service, env)
					assert.ErrorIs(t, got[name], ErrMissingRequired, name)
				}
				assert.Len(t, got, len(want), "every missing variable is reported at once, and no others")
				assert.NotContains(t, got, "CP_HTTP_CORS_ORIGINS", "optional variables are not required")
				assert.NotContains(t, got, "CP_KMS_AUDIT_SIGNING_KEY_ID", "presence in PROD is a validation rule, not a load error")

				// The point of the exercise: a dependency this service does not
				// use is never demanded of it.
				for _, d := range AllDependencies() {
					if service.Requires(d) {
						continue
					}
					for _, s := range Vars() {
						if s.Dep == d {
							assert.NotContains(t, got, s.Name,
								"%s does not use %s and must not be asked for %s", service, d, s.Name)
						}
					}
				}
			})
		}
	}
}

func TestLoad_EnvironmentIsRequiredAndFailsClosed(t *testing.T) {
	t.Parallel()
	_, err := Load(context.Background(), ServiceAPI, LookupFromMap(map[string]string{}))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMissingRequired)
	assert.Contains(t, err.Error(), "CP_ENV")

	_, err = Load(context.Background(), ServiceAPI, LookupFromMap(map[string]string{"CP_ENV": "  "}))
	assert.ErrorIs(t, err, ErrMissingRequired)

	_, err = Load(context.Background(), ServiceAPI, LookupFromMap(map[string]string{"CP_ENV": "production"}))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownEnvironment)
}

func TestLoad_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Load(ctx, ServiceAPI, LookupFromMap(map[string]string{"CP_ENV": "LOCAL"}))
	assert.ErrorIs(t, err, context.Canceled)
}

func TestLoad_NilLookupUsesProcessEnvironment(t *testing.T) {
	// Not parallel: touches the process environment.
	t.Setenv("CP_ENV", "TEST")
	t.Setenv("CP_SERVICE_NAME", "from-process-env")
	c, err := Load(context.Background(), ServiceAPI, nil)
	require.NoError(t, err)
	assert.Equal(t, "from-process-env", c.ServiceName)
	_, hasEnv := os.LookupEnv("CP_ENV")
	assert.True(t, hasEnv)
}

func TestLoad_InvalidValuesAreAllReported(t *testing.T) {
	t.Parallel()
	vars := withVars(map[string]string{"CP_ENV": "LOCAL"}, map[string]string{
		"CP_DATABASE_REQUIRE_TLS":       "yes",
		"CP_HTTP_READ_TIMEOUT":          "10 seconds",
		"CP_HTTP_WRITE_TIMEOUT":         "-5s",
		"CP_DATABASE_MAX_CONNS":         "ten",
		"CP_DATABASE_MIN_CONNS":         "99999999999",
		"CP_HTTP_MAX_BODY_BYTES":        "1.5",
		"CP_PROVIDER_FUNDING_MODE":      "mock",
		"CP_DATABASE_APP_URL":           "env://",
		"CP_RETENTION_SOCIAL_DATA_DAYS": "30d",
		"CP_AUTH_CLIENT_SECRET_REF":     "aws-sm://has space",
	})
	c, err := Load(context.Background(), ServiceAPI, LookupFromMap(vars))
	require.Error(t, err)
	assert.Nil(t, c)
	got := varErrors(err)
	for _, name := range []string{
		"CP_DATABASE_REQUIRE_TLS", "CP_HTTP_READ_TIMEOUT", "CP_HTTP_WRITE_TIMEOUT", "CP_DATABASE_MAX_CONNS",
		"CP_DATABASE_MIN_CONNS", "CP_HTTP_MAX_BODY_BYTES", "CP_PROVIDER_FUNDING_MODE", "CP_DATABASE_APP_URL",
		"CP_RETENTION_SOCIAL_DATA_DAYS", "CP_AUTH_CLIENT_SECRET_REF",
	} {
		assert.Contains(t, got, name)
		assert.NotErrorIs(t, got[name], ErrMissingRequired, name)
	}
	assert.Len(t, got, 10)
	assert.ErrorIs(t, got["CP_DATABASE_APP_URL"], ErrInvalidSecretRef)
}

func TestLoad_BlankValueCountsAsAbsent(t *testing.T) {
	t.Parallel()
	c := mustLoad(t, map[string]string{"CP_ENV": "LOCAL", "CP_HTTP_ADDR": "   ", "CP_HTTP_CORS_ORIGINS": ""})
	assert.Equal(t, "127.0.0.1:8080", c.HTTP.Addr, "blank falls back to the LOCAL default")
	assert.Empty(t, c.HTTP.CORSOrigins)

	_, err := Load(context.Background(), ServiceAPI, LookupFromMap(withVars(prodEnv(), map[string]string{"CP_HTTP_ADDR": ""})))
	require.Error(t, err)
	assert.ErrorIs(t, varErrors(err)["CP_HTTP_ADDR"], ErrMissingRequired, "blank is missing in PROD, never defaulted")
}

func TestLoad_FullProdEnvironment(t *testing.T) {
	t.Parallel()
	c := validProdConfig(t)
	assert.Equal(t, EnvProd, c.Env)
	assert.Equal(t, []string{"https://app.example.com", "https://admin.example.com"}, c.HTTP.CORSOrigins)
	assert.Equal(t, []string{"10.0.0.0/8", "fd00::/8"}, c.HTTP.TrustedProxyCIDRs)
	assert.Equal(t, SecretSchemeAWSSM, c.Database.AppURL.Scheme())
	assert.Equal(t, SecretSchemeEnv, c.Database.ReadOnlyURL.Scheme())
	assert.True(t, c.Database.RequireTLS)
	assert.Equal(t, int32(20), c.Database.MaxConns)
	assert.Equal(t, []string{"b1.example.internal:9092", "b2.example.internal:9092"}, c.Redpanda.Brokers)
	assert.Equal(t, "SCRAM-SHA-256", c.Redpanda.SASLMechanism)
	assert.True(t, c.Archive.ObjectLockRequired)
	assert.True(t, c.Archive.AccessKeyRef.IsZero(), "IAM role, no static keys")
	assert.Equal(t, AuthModeOIDC, c.Auth.Mode)
	assert.True(t, c.Auth.CookieSecure)
	assert.Equal(t, "0.1", c.Telemetry.TraceSampleRatio)
	assert.True(t, c.Capability.StoreConfigured)
	assert.NoError(t, c.Validate())
}

func TestLoad_ProviderVariablesMapToSlots(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"CP_ENV": "LOCAL"}
	for i, slot := range providerSlots() {
		p := "CP_PROVIDER_" + slot.Env + "_"
		vars[p+"MODE"] = "sandbox"
		vars[p+"NAME"] = "name-" + slot.Name
		vars[p+"BASE_URL"] = "https://" + strings.ReplaceAll(slot.Name, "_", "-") + ".example.com"
		vars[p+"API_KEY_REF"] = "env://KEY_" + slot.Env
		vars[p+"WEBHOOK_SECRET_REF"] = "env://WH_" + slot.Env
		vars[p+"TIMEOUT"] = time.Duration(i + 1).String()
	}
	c := mustLoad(t, vars)
	for i, slot := range providerSlots() {
		p := slot.Get(&c.Providers)
		assert.Equal(t, ProviderModeSandbox, p.Mode, slot.Name)
		assert.Equal(t, "name-"+slot.Name, p.Name, slot.Name)
		assert.Equal(t, SecretRef("env://KEY_"+slot.Env), p.APIKeyRef, slot.Name)
		assert.Equal(t, SecretRef("env://WH_"+slot.Env), p.WebhookSecretRef, slot.Name)
		assert.Equal(t, time.Duration(i+1), p.Timeout, slot.Name)
	}
}

func TestLoad_ValidationErrorsSurface(t *testing.T) {
	t.Parallel()
	// A syntactically fine PROD environment with a semantic violation.
	vars := withVars(prodEnv(), map[string]string{"CP_PROVIDER_SIGNING_MODE": "fake"})
	c, err := Load(context.Background(), ServiceAPI, LookupFromMap(vars))
	require.Error(t, err)
	assert.Nil(t, c)
	assert.True(t, HasViolation(err, RuleNoFakeProviders))
}

func TestVars_TableIsWellFormed(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, s := range Vars() {
		assert.True(t, strings.HasPrefix(s.Name, EnvPrefix), s.Name)
		assert.Regexp(t, `^[A-Z][A-Z0-9_]*$`, s.Name)
		assert.False(t, seen[s.Name], "duplicate %s", s.Name)
		seen[s.Name] = true
		assert.NotEmpty(t, s.Doc, s.Name)
		assert.NotEmpty(t, s.Section, s.Name)
		if s.Secret {
			assert.True(t, strings.HasSuffix(s.Name, "_REF") || strings.HasSuffix(s.Name, "_URL"), "%s: secret variables are named *_REF or *_URL", s.Name)
		}
	}
	assert.True(t, seen[EnvVarEnvironment])
	// The 14*11 term is the provider slots: fourteen of them, eleven variables
	// each. It was 12*6 before the Stripe workstream added the credit-purchase
	// and payout slots, and then ACCOUNT_REF, SHARED_ACCOUNT, AVAILABILITY and
	// the two statement-descriptor variables to every slot. The 5 is the
	// rate-limit section: the backend, the replica count and the four
	// transport budgets, which cmd/api used to read straight from the
	// environment. The 4 is the capacity ceilings, and the 2 before it the API
	// settlement asset -- which is now 8, because the capability list, the two
	// funding-quote variables, the legal policy and the two API timeouts all
	// joined it. Each of those was read straight from the environment by
	// cmd/api, which is the one route into production that this table's whole
	// purpose is to close; test/infra now fails if another appears.
	// The trailing 7 became 8 when CP_RETENTION_SECURITY_EVENT_DAYS joined the
	// retention section: security_events is partitioned by month (00740) and
	// how many months are kept is a retention decision that had nowhere to be
	// written down.
	// The new 4 is the alerting section: a destination, its payload shape, a
	// severity floor and a delivery timeout. The 1 after it is the keyring
	// personal data is encrypted under (F-47). The 1 after the second 11 is
	// CP_AUTH_POST_LOGIN_URL: the web app is hosted on its own origin now, and
	// the API's root is a 404. The trailing 2 is the sandbox tier's payout
	// policy and sandbox gate list (ADR-0023). Before it, Metrics.OnAlert had no production caller and
	// a ledger-integrity violation reached a counter that died with the process
	// (F-118).
	// The trailing +1 is CP_AUTH_BOOTSTRAP_OPERATORS: nothing in the system
	// writes operator_roles, so a deployment that has never had an operator
	// could not get one, and the declaration that fixes it is configuration
	// rather than a route (ADR-0024, D-056).
	// The trailing +1 is CP_API_DEMO_DATA: the sandbox demo catalogue seeded
	// itself on the legal-policy declaration alone, so the deployed STAGING
	// declared that it does not seed and seeded, and the variable an operator
	// would reach for did nothing (D-086, F-144).
	assert.Equal(t, 3+8+11+8+4+6+2+5+5+4+10+4+1+2+11+1+14*11+4+1+1+8+2+1+1, len(seen))
}

func TestVars_DocumentedInDocGo(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile("doc.go")
	require.NoError(t, err)
	text := string(doc)
	for _, s := range Vars() {
		assert.Contains(t, text, s.Name, "doc.go must list %s", s.Name)
	}
}

func TestProviderMode(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"fake", "sandbox", "live"} {
		m, err := ParseProviderMode(ok)
		require.NoError(t, err)
		assert.True(t, m.IsValid())
	}
	for _, bad := range []string{"", "FAKE", "mock", "prod", "live "} {
		_, err := ParseProviderMode(bad)
		assert.Error(t, err, bad)
		assert.False(t, ProviderMode(bad).IsValid(), bad)
	}
}

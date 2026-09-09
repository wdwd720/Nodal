package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ratelimit"
)

func localConfig(t *testing.T) *config.Config {
	t.Helper()
	env := map[string]string{"CP_ENV": "LOCAL"}
	cfg, err := config.Load(context.Background(), config.ServiceAPI, config.LookupFromMap(env))
	require.NoError(t, err)
	return cfg
}

// TestRefuseFakeProviders is PART 97 at the binary: the API must fail to start
// rather than run with in-process doubles where real money is at stake. Every
// provider slot is checked, not just the ones this binary happens to use.
func TestRefuseFakeProviders(t *testing.T) {
	t.Parallel()
	for _, envName := range []string{"STAGING", "PROD"} {
		t.Run(envName, func(t *testing.T) {
			t.Parallel()
			cfg := localConfig(t)
			env, err := config.ParseEnvironment(envName)
			require.NoError(t, err)
			cfg.Env = env

			// The LOCAL defaults are fake everywhere; that alone must refuse.
			err = refuseFakeProviders(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), string(config.RuleNoFakeProviders))
			assert.Contains(t, err.Error(), "funding")
			assert.Contains(t, err.Error(), "execution")

			// One fake slot is enough.
			for name := range providerSlots(cfg) {
				_ = name
			}
			cfg2 := localConfig(t)
			cfg2.Env = env
			for _, p := range []*config.ProviderConfig{
				&cfg2.Providers.Funding, &cfg2.Providers.Wallet, &cfg2.Providers.Signing,
				&cfg2.Providers.Execution, &cfg2.Providers.MarketData, &cfg2.Providers.ChainObserver,
				&cfg2.Providers.ChainObserverFallback, &cfg2.Providers.Model, &cfg2.Providers.EventBus,
				&cfg2.Providers.Workflow, &cfg2.Providers.Archive, &cfg2.Providers.Notification,
			} {
				p.Mode = config.ProviderModeSandbox
			}
			require.NoError(t, refuseFakeProviders(cfg2), "no fake slot must be accepted")

			cfg2.Providers.Signing.Mode = config.ProviderModeFake
			err = refuseFakeProviders(cfg2)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "signing")
		})
	}
}

// TestFakeProvidersAreAllowedInDevelopmentEnvironments: the refusal is scoped
// to production-like environments, so LOCAL, TEST and DEV keep their doubles.
func TestFakeProvidersAreAllowedInDevelopmentEnvironments(t *testing.T) {
	t.Parallel()
	for _, envName := range []string{"LOCAL", "TEST", "DEV"} {
		cfg := localConfig(t)
		env, err := config.ParseEnvironment(envName)
		require.NoError(t, err)
		cfg.Env = env
		assert.NoError(t, refuseFakeProviders(cfg), envName)
	}
}

// TestRunRefusesAnInvalidConfiguration: the process exits non-zero rather than
// starting with a configuration the loader rejected.
func TestRunRefusesAnInvalidConfiguration(t *testing.T) {
	t.Parallel()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { _ = devNull.Close() }()

	env := map[string]string{"CP_ENV": "NOT_A_REAL_ENVIRONMENT"}
	assert.Equal(t, exitFailure, run(t.Context(), config.LookupFromMap(env), devNull))
}

// TestRunRefusesFakeProvidersInProduction drives the whole startup path: a
// production configuration whose providers are fake must not reach the point
// of opening a database.
func TestRunRefusesFakeProvidersInProduction(t *testing.T) {
	t.Parallel()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { _ = devNull.Close() }()

	// A PROD environment with LOCAL-shaped values: config.Validate refuses it
	// (fake providers among other rules) and run must exit non-zero.
	env := map[string]string{"CP_ENV": "PROD"}
	assert.Equal(t, exitFailure, run(t.Context(), config.LookupFromMap(env), devNull))
}

// TestWithRequestTimeoutLeavesTheStreamAlone: every ordinary request gets a
// deadline; the event stream, which is long-lived by design, does not.
func TestWithRequestTimeoutLeavesTheStreamAlone(t *testing.T) {
	t.Parallel()
	var sawDeadline []bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, ok := r.Context().Deadline()
		sawDeadline = append(sawDeadline, ok)
	})
	h := withRequestTimeout(next, time.Second)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/accounts", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/events/stream", nil))

	require.Len(t, sawDeadline, 2)
	assert.True(t, sawDeadline[0], "an ordinary request must be bounded")
	assert.False(t, sawDeadline[1], "the event stream must not be cut off by a request deadline")
}

func TestWithRequestTimeoutIsOptional(t *testing.T) {
	t.Parallel()
	deadline := true
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, deadline = r.Context().Deadline()
	})
	withRequestTimeout(next, 0).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/v1/accounts", nil))
	assert.False(t, deadline, "with no configured timeout the request is not bounded here")
}

// TestParseCapabilitiesIsNotSufficientToEnableLiveMoney: the variable only
// satisfies condition 1 of the five-condition gate check. It names capabilities
// and nothing more; the persisted, dual-approved gate row decides.
func TestParseCapabilities(t *testing.T) {
	t.Parallel()
	got := parseCapabilities(" live_funding , WITHDRAWALS ,, not-a-capability ")
	assert.Len(t, got, 2)
	_, hasFunding := got[gates.LiveFunding]
	_, hasWithdrawals := got[gates.Withdrawals]
	assert.True(t, hasFunding)
	assert.True(t, hasWithdrawals)

	assert.Empty(t, parseCapabilities(""))
	assert.Empty(t, parseCapabilities("nonsense"))
}

func TestDurationEnv(t *testing.T) {
	t.Parallel()
	lookup := config.LookupFromMap(map[string]string{
		"GOOD": "30s", "BAD": "not-a-duration", "EMPTY": "  ", "NEGATIVE": "-5s",
	})
	assert.Equal(t, 30*time.Second, durationEnv(lookup, "GOOD", time.Minute))
	assert.Equal(t, time.Minute, durationEnv(lookup, "BAD", time.Minute))
	assert.Equal(t, time.Minute, durationEnv(lookup, "EMPTY", time.Minute))
	assert.Equal(t, time.Minute, durationEnv(lookup, "NEGATIVE", time.Minute))
	assert.Equal(t, time.Minute, durationEnv(lookup, "ABSENT", time.Minute))
}

func TestProviderCatalogCoversEverySlot(t *testing.T) {
	t.Parallel()
	cfg := localConfig(t)
	catalog := providerCatalog(cfg)
	assert.Len(t, catalog, len(providerSlots(cfg)))
	for _, d := range catalog {
		assert.NotEmpty(t, d.Name, d.Role)
		assert.NotEmpty(t, d.Role)
		assert.False(t, strings.Contains(strings.ToLower(d.Name), "secret"))
	}
}

// TestOneEnvironmentVariableCannotEnableLiveMoney is the hard rule of PART 54
// at the composition root. CP_API_ENABLED_CAPABILITIES satisfies condition 1
// of the five-condition activation check and nothing else: with every
// capability named in the variable and no persisted, dual-approved gate row,
// every capability is still inactive.
func TestOneEnvironmentVariableCannotEnableLiveMoney(t *testing.T) {
	t.Parallel()
	names := make([]string, 0, len(gates.AllCapabilities()))
	for _, c := range gates.AllCapabilities() {
		names = append(names, string(c))
	}
	enabled := parseCapabilities(strings.Join(names, ","))
	require.Len(t, enabled, len(gates.AllCapabilities()), "every capability is named in the variable")

	now := time.Now().UTC()
	for _, c := range gates.AllCapabilities() {
		_, configEnabled := enabled[c]
		require.True(t, configEnabled)

		// No persisted gate row at all.
		verdict := gates.Evaluate(nil, configEnabled, now)
		assert.False(t, verdict.Active, "%s must not activate from configuration alone", c)
		assert.Equal(t, gates.ReasonNoGateRow, verdict.Reason)

		// A bootstrapped DISABLED row is likewise not enough.
		disabled := &gates.Gate{Capability: c, Environment: "PROD", State: gates.StateDisabled}
		verdict = gates.Evaluate(disabled, configEnabled, now)
		assert.False(t, verdict.Active, "%s must not activate from a DISABLED row", c)

		// Even an ACTIVE row with an effective window is refused without the
		// two distinct approvers the gate's approval chain requires.
		effective := now.Add(-time.Hour)
		active := &gates.Gate{
			Capability: c, Environment: "PROD", State: gates.StateActive,
			EffectiveAt: &effective,
		}
		verdict = gates.Evaluate(active, configEnabled, now)
		assert.False(t, verdict.Active,
			"%s must not activate without a complete approval chain (reason %q)", c, verdict.Reason)
	}
}

// TestParseRateLimit covers the "<requests>/<window>" form, the disable forms
// and everything that must be refused rather than silently defaulted.
func TestParseRateLimit(t *testing.T) {
	t.Parallel()
	def := ratelimit.Limit{Requests: 600, Window: time.Minute}
	cases := []struct {
		spec    string
		limit   ratelimit.Limit
		enabled bool
		wantErr bool
	}{
		{"", def, true, false},
		{"   ", def, true, false},
		{"600/1m", ratelimit.Limit{Requests: 600, Window: time.Minute}, true, false},
		{"50/10s", ratelimit.Limit{Requests: 50, Window: 10 * time.Second}, true, false},
		{" 5000 / 1h ", ratelimit.Limit{Requests: 5000, Window: time.Hour}, true, false},
		{"off", ratelimit.Limit{}, false, false},
		{"OFF", ratelimit.Limit{}, false, false},
		{"0", ratelimit.Limit{}, false, false},
		{"0/1m", ratelimit.Limit{}, false, false},
		{"600", ratelimit.Limit{}, false, true},
		{"abc/1m", ratelimit.Limit{}, false, true},
		{"-1/1m", ratelimit.Limit{}, false, true},
		{"600/nonsense", ratelimit.Limit{}, false, true},
		{"600/0s", ratelimit.Limit{}, false, true},
		{"600/-1m", ratelimit.Limit{}, false, true},
	}
	for _, tc := range cases {
		t.Run("spec="+tc.spec, func(t *testing.T) {
			t.Parallel()
			limit, enabled, err := parseRateLimit(tc.spec, def)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.enabled, enabled)
			assert.Equal(t, tc.limit, limit)
		})
	}
}

// TestRateLimitsAreConfigurableWithTheDocumentedDefaults: the four budgets
// come from configuration, and an unset variable keeps today's value.
func TestRateLimitsAreConfigurableWithTheDocumentedDefaults(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC())

	// Defaults.
	limits, err := rateLimits(clk, config.EnvLocal, config.LookupFromMap(map[string]string{}), ratelimit.NewMemoryStore(), true)
	require.NoError(t, err)
	require.NotNil(t, limits.General)
	require.NotNil(t, limits.Auth)
	require.NotNil(t, limits.Quote)
	require.NotNil(t, limits.Command)
	assert.Equal(t, ratelimit.Limit{Requests: 600, Window: time.Minute}, defaultRateLimits[envRateLimitGeneral])
	assert.Equal(t, ratelimit.Limit{Requests: 30, Window: time.Minute}, defaultRateLimits[envRateLimitAuth])
	assert.Equal(t, ratelimit.Limit{Requests: 120, Window: time.Minute}, defaultRateLimits[envRateLimitQuote])
	assert.Equal(t, ratelimit.Limit{Requests: 120, Window: time.Minute}, defaultRateLimits[envRateLimitCommand])

	// Overrides, including switching one off for a load run.
	limits, err = rateLimits(clk, config.EnvLocal, config.LookupFromMap(map[string]string{
		envRateLimitGeneral: "100000/1m",
		envRateLimitAuth:    "off",
		envRateLimitQuote:   "5/1s",
		envRateLimitCommand: "0",
	}), ratelimit.NewMemoryStore(), true)
	require.NoError(t, err)
	assert.NotNil(t, limits.General)
	assert.Nil(t, limits.Auth, "off means no limiter at all")
	assert.NotNil(t, limits.Quote)
	assert.Nil(t, limits.Command)

	// A malformed value is a startup error, never a silent default.
	_, err = rateLimits(clk, config.EnvLocal, config.LookupFromMap(map[string]string{
		envRateLimitQuote: "lots",
	}), ratelimit.NewMemoryStore(), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), envRateLimitQuote)
}

// TestRateLimitsCannotBeDisabledInProduction: switching a transport budget off
// is a development and load-testing affordance. STAGING and PROD refuse it at
// startup rather than running unprotected.
func TestRateLimitsCannotBeDisabledInProduction(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC())
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		for _, name := range []string{
			envRateLimitGeneral, envRateLimitAuth, envRateLimitQuote, envRateLimitCommand,
		} {
			_, err := rateLimits(clk, env, config.LookupFromMap(map[string]string{name: "off"}), ratelimit.NewMemoryStore(), true)
			require.Error(t, err, "%s %s", env, name)
			assert.ErrorIs(t, err, errRateLimitDisabledInProduction)
			assert.Contains(t, err.Error(), name)
		}
		// A configured limit is still accepted there.
		_, err := rateLimits(clk, env, config.LookupFromMap(map[string]string{
			envRateLimitGeneral: "1200/1m",
		}), ratelimit.NewMemoryStore(), true)
		assert.NoError(t, err, "%s", env)
	}
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		_, err := rateLimits(clk, env, config.LookupFromMap(map[string]string{envRateLimitGeneral: "off"}), ratelimit.NewMemoryStore(), true)
		assert.NoError(t, err, "%s", env)
	}
}

// TestLegalRouterFor_RefusesADevelopmentPolicyInProduction is the guard that
// makes the development policy safe to have at all: it exists so the internal
// economy can be exercised locally, and the one thing that must never happen
// is it being loaded where real money is.
func TestLegalRouterFor_RefusesADevelopmentPolicyInProduction(t *testing.T) {
	dev := config.LookupFromMap(map[string]string{envLegalPolicy: "DEVELOPMENT"})

	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		_, err := legalRouterFor(env, dev)
		require.Error(t, err, "%s must refuse a development legal policy", env)
		require.Contains(t, err.Error(), "may not be loaded")
	}

	for _, env := range []config.Environment{config.EnvLocal, config.EnvDev, config.EnvTest} {
		r, err := legalRouterFor(env, dev)
		require.NoError(t, err, "%s", env)
		require.NotNil(t, r)
		require.Equal(t, "legal-router-v1-local-development", r.Policy().Version)
	}
}

// TestLegalRouterFor_DefaultsToNoDetermination. Unset and CONSERVATIVE both
// mean "this deployment has decided nothing", which internal/httpapi reads as
// the fail-closed conservative policy.
func TestLegalRouterFor_DefaultsToNoDetermination(t *testing.T) {
	for _, value := range []string{"", "CONSERVATIVE", "conservative"} {
		r, err := legalRouterFor(config.EnvProd, config.LookupFromMap(map[string]string{envLegalPolicy: value}))
		require.NoError(t, err, "%q", value)
		require.Nil(t, r, "%q must leave the policy unset so the fail-closed default applies", value)
	}

	_, err := legalRouterFor(config.EnvLocal, config.LookupFromMap(map[string]string{envLegalPolicy: "PERMISSIVE"}))
	require.Error(t, err, "an unknown policy name must refuse rather than default to something")
}

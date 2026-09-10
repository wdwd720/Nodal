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

// TestRateLimitsAreConfigurableWithTheDocumentedDefaults: the four budgets
// come from configuration, and an unset variable keeps today's value.
func TestRateLimitsAreConfigurableWithTheDocumentedDefaults(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now().UTC())

	// Defaults. An empty spec takes the documented default, which is what an
	// unset variable resolves to for LOCAL and TEST.
	limits, err := rateLimits(clk, config.EnvLocal, config.RateLimitConfig{}, ratelimit.NewMemoryStore(), true)
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
	limits, err = rateLimits(clk, config.EnvLocal, config.RateLimitConfig{
		General: "100000/1m",
		Auth:    "off",
		Quote:   "5/1s",
		Command: "0",
	}, ratelimit.NewMemoryStore(), true)
	require.NoError(t, err)
	assert.NotNil(t, limits.General)
	assert.Nil(t, limits.Auth, "off means no limiter at all")
	assert.NotNil(t, limits.Quote)
	assert.Nil(t, limits.Command)

	// A malformed value is a startup error, never a silent default.
	_, err = rateLimits(clk, config.EnvLocal, config.RateLimitConfig{Quote: "lots"}, ratelimit.NewMemoryStore(), true)
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
		for name, off := range map[string]config.RateLimitConfig{
			envRateLimitGeneral: {General: "off"},
			envRateLimitAuth:    {Auth: "off"},
			envRateLimitQuote:   {Quote: "off"},
			envRateLimitCommand: {Command: "off"},
		} {
			_, err := rateLimits(clk, env, off, ratelimit.NewMemoryStore(), true)
			require.Error(t, err, "%s %s", env, name)
			assert.ErrorIs(t, err, errRateLimitDisabledInProduction)
			assert.Contains(t, err.Error(), name)
		}
		// A configured limit is still accepted there.
		_, err := rateLimits(clk, env, config.RateLimitConfig{General: "1200/1m"}, ratelimit.NewMemoryStore(), true)
		assert.NoError(t, err, "%s", env)
	}
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		_, err := rateLimits(clk, env, config.RateLimitConfig{General: "off"}, ratelimit.NewMemoryStore(), true)
		assert.NoError(t, err, "%s", env)
	}
}

// TestLegalRouterFor_RefusesADevelopmentPolicyInProduction is the guard that
// makes the development policy safe to have at all: it exists so the internal
// economy can be exercised locally, and the one thing that must never happen
// is it being loaded where real money is.
func TestLegalRouterFor_RefusesADevelopmentPolicyInProduction(t *testing.T) {
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		_, err := legalRouterFor(env, "DEVELOPMENT")
		require.Error(t, err, "%s must refuse a development legal policy", env)
		require.Contains(t, err.Error(), "may not be loaded")
	}

	for _, env := range []config.Environment{config.EnvLocal, config.EnvDev, config.EnvTest} {
		r, err := legalRouterFor(env, "DEVELOPMENT")
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
		r, err := legalRouterFor(config.EnvProd, value)
		require.NoError(t, err, "%q", value)
		require.Nil(t, r, "%q must leave the policy unset so the fail-closed default applies", value)
	}

	_, err := legalRouterFor(config.EnvLocal, "PERMISSIVE")
	require.Error(t, err, "an unknown policy name must refuse rather than default to something")
}

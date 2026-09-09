package security

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
)

// loadLocalDefaults returns a complete LOCAL configuration (defaults apply
// only in LOCAL/TEST) so a single production rule can be isolated.
func loadLocalDefaults(t *testing.T) *config.Config {
	t.Helper()
	env := map[string]string{"CP_ENV": "LOCAL"}
	cfg, err := config.Load(context.Background(), config.ServiceAPI, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err)
	return cfg
}

// TestProductionRefusesFakeProviders: PART 97 — the production binary must
// reject provider mode "fake" programmatically, not by convention.
func TestProductionRefusesFakeProviders(t *testing.T) {
	for _, envName := range []string{"PROD", "STAGING"} {
		cfg := loadLocalDefaults(t)
		env, err := config.ParseEnvironment(envName)
		require.NoError(t, err)
		cfg.Env = env
		cfg.Providers.Execution.Mode = config.ProviderModeFake
		err = cfg.Validate()
		require.Error(t, err, "%s must not accept a fake execution provider", envName)
		assert.Contains(t, err.Error(), string(config.RuleNoFakeProviders), envName)
	}
}

// TestProductionRefusesSeedAndDebugAuth: PART 98/146 — no seeded balances,
// no debug auth, no dev identity provider outside LOCAL/TEST/DEV.
func TestProductionRefusesSeedAndDebugAuth(t *testing.T) {
	cfg := loadLocalDefaults(t)
	env, err := config.ParseEnvironment("PROD")
	require.NoError(t, err)
	cfg.Env = env
	cfg.Seed.Enabled = true
	cfg.Auth.Mode = "dev"
	cfg.Auth.DebugAuthEnabled = true
	err = cfg.Validate()
	require.Error(t, err)
	msg := err.Error()
	for _, want := range []string{"Seed", "Auth"} {
		assert.True(t, strings.Contains(msg, want), "expected the %s rule in %q", want, msg)
	}
}

// TestLocalDefaultsAreNotProductionValid: the LOCAL default configuration
// (fake providers, plain secrets, no TLS) must fail production validation in
// several independent ways, so a misdeployed LOCAL config can never boot in
// PROD.
func TestLocalDefaultsAreNotProductionValid(t *testing.T) {
	cfg := loadLocalDefaults(t)
	env, err := config.ParseEnvironment("PROD")
	require.NoError(t, err)
	cfg.Env = env
	err = cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(config.RuleNoFakeProviders))
}

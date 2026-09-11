package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSeedScriptsAllowed (F-144, D-115).
//
// CP_SEED_ENABLED was required of every binary, validated by RuleNoSeed, set to
// "false" in render.yaml and read by nothing outside this package. An operator
// who reached for it changed nothing at all. This is the behaviour it has now.
func TestSeedScriptsAllowed(t *testing.T) {
	t.Parallel()

	t.Run("absent means yes in a development environment", func(t *testing.T) {
		t.Parallel()
		// `make seed` and CI's browser suite set no such variable, and always
		// worked. Requiring one here would break the recipes rather than
		// control anything.
		for _, env := range []string{"LOCAL", "local", " DEV ", "TEST"} {
			ok, err := SeedScriptsAllowed(env, LookupFromMap(map[string]string{}))
			require.NoErrorf(t, err, "%q", env)
			assert.Truef(t, ok, "%q refused a seed with no variable set", env)
		}
	})

	t.Run("false means no, which is the whole point", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{"false", "FALSE", "0", " false "} {
			ok, err := SeedScriptsAllowed("LOCAL", LookupFromMap(map[string]string{EnvVarSeedEnabled: raw}))
			require.NoErrorf(t, err, "%q", raw)
			assert.Falsef(t, ok, "CP_SEED_ENABLED=%q did not stop the seed scripts", raw)
		}
		ok, err := SeedScriptsAllowed("LOCAL", LookupFromMap(map[string]string{EnvVarSeedEnabled: "true"}))
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("a deployed environment is no whatever the variable says", func(t *testing.T) {
		t.Parallel()
		for _, env := range []string{"STAGING", "PROD"} {
			for _, raw := range []string{"true", "false", ""} {
				ok, err := SeedScriptsAllowed(env, LookupFromMap(map[string]string{EnvVarSeedEnabled: raw}))
				require.NoErrorf(t, err, "%s/%q", env, raw)
				assert.Falsef(t, ok, "%s allowed the seed scripts with CP_SEED_ENABLED=%q", env, raw)
			}
		}
	})

	t.Run("an unparseable value is an error, never a default", func(t *testing.T) {
		t.Parallel()
		_, err := SeedScriptsAllowed("LOCAL", LookupFromMap(map[string]string{EnvVarSeedEnabled: "maybe"}))
		require.Error(t, err, "the one thing worse than a switch that does nothing is one that reads no as yes")

		_, err = SeedScriptsAllowed("NOWHERE", LookupFromMap(map[string]string{}))
		require.Error(t, err)
	})

	// And the name this function reads is the one the requirements table
	// carries, so the two cannot drift apart.
	t.Run("the variable is in the table", func(t *testing.T) {
		t.Parallel()
		var found bool
		for _, v := range Vars() {
			if v.Name == EnvVarSeedEnabled {
				found = true
			}
		}
		assert.True(t, found, "%s is no longer declared, so nothing documents or hashes it", EnvVarSeedEnabled)
	})
}

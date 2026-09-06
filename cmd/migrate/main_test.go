package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveMigrateURL(t *testing.T) {
	t.Parallel()
	t.Run("explicit URL wins regardless of env", func(t *testing.T) {
		t.Parallel()
		var warn bytes.Buffer
		u, err := resolveMigrateURL(envOf(map[string]string{envMigrateURL: " postgres://m:s@db/cp?sslmode=verify-full ", envEnv: "PROD"}), &warn)
		require.NoError(t, err)
		assert.Equal(t, "postgres://m:s@db/cp?sslmode=verify-full", u)
		assert.Empty(t, warn.String())
	})
	for _, env := range []string{"", "LOCAL", "local", "TEST", " test "} {
		t.Run("fallback allowed for env "+strings.TrimSpace(env), func(t *testing.T) {
			t.Parallel()
			var warn bytes.Buffer
			u, err := resolveMigrateURL(envOf(map[string]string{envEnv: env}), &warn)
			require.NoError(t, err)
			assert.Equal(t, localDefaultURL, u)
			assert.Contains(t, warn.String(), "WARNING")
			assert.NotContains(t, warn.String(), "cp_migrate_local", "even local credentials are not echoed")
		})
	}
	for _, env := range []string{"DEV", "STAGING", "PROD", "production", "garbage"} {
		t.Run("fallback refused for env "+env, func(t *testing.T) {
			t.Parallel()
			var warn bytes.Buffer
			u, err := resolveMigrateURL(envOf(map[string]string{envEnv: env}), &warn)
			require.Error(t, err)
			assert.Empty(t, u)
			assert.Empty(t, warn.String())
		})
	}
}

func TestRun_UsageAndExitCodes(t *testing.T) {
	t.Parallel()
	env := envOf(map[string]string{envEnv: "PROD"}) // never touches a database
	var out, errb bytes.Buffer

	assert.Equal(t, exitUsage, run(nil, env, &out, &errb))
	assert.Equal(t, exitUsage, run([]string{"bogus"}, env, &out, &errb))
	assert.Equal(t, exitOK, run([]string{"help"}, env, &out, &errb))
	assert.Contains(t, out.String(), "down-to")

	// DB commands in PROD without a URL fail closed before connecting.
	errb.Reset()
	assert.Equal(t, exitFailure, run([]string{"up"}, env, &out, &errb))
	assert.Contains(t, errb.String(), envMigrateURL)

	// Version arguments are validated before any connection is attempted.
	for _, args := range [][]string{{"up-to"}, {"up-to", "x"}, {"down-to", "-1"}, {"up", "extra"}, {"status", "x"}, {"verify", "x"}} {
		errb.Reset()
		code := run(args, envOf(map[string]string{envMigrateURL: "postgres://u:p@192.0.2.1:1/x?connect_timeout=1"}), &out, &errb)
		assert.Equal(t, exitUsage, code, "%v", args)
		assert.NotContains(t, errb.String(), "192.0.2.1", "connection details are never printed")
	}
}

func TestRun_Create(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "00001_extensions.sql"), []byte("-- +goose Up\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "00002_outbox_inbox.sql"), []byte("-- +goose Up\n"), 0o644))
	env := envOf(nil)
	var out, errb bytes.Buffer

	code := run([]string{"create", "sessions", "-dir", dir}, env, &out, &errb)
	require.Equal(t, exitOK, code, errb.String())
	assert.Contains(t, out.String(), "00003_sessions.sql")
	b, err := os.ReadFile(filepath.Join(dir, "00003_sessions.sql"))
	require.NoError(t, err)
	assert.Contains(t, string(b), "-- +goose Up")
	assert.Contains(t, string(b), "-- +goose Down")
	assert.NotContains(t, string(b), "ledger history is never dropped")

	out.Reset()
	code = run([]string{"create", "-range", "financial", "-dir", dir, "ledger"}, env, &out, &errb)
	require.Equal(t, exitOK, code, errb.String())
	assert.Contains(t, out.String(), "00100_ledger.sql")
	b, err = os.ReadFile(filepath.Join(dir, "00100_ledger.sql"))
	require.NoError(t, err)
	assert.Contains(t, string(b), "SELECT 1; -- ledger history is never dropped")

	// Next in the same range increments; existing files are never overwritten.
	code = run([]string{"create", "journal", "-range", "financial", "-dir", dir}, env, &out, &errb)
	require.Equal(t, exitOK, code)
	_, err = os.Stat(filepath.Join(dir, "00101_journal.sql"))
	require.NoError(t, err)

	for _, args := range [][]string{
		{"create"},
		{"create", "Bad-Name", "-dir", dir},
		{"create", "x", "-range", "nope", "-dir", dir},
		{"create", "x", "-dir", filepath.Join(dir, "missing")},
		{"create", "a", "b", "-dir", dir},
	} {
		errb.Reset()
		code := run(args, env, &out, &errb)
		assert.NotEqual(t, exitOK, code, "%v", args)
	}
}

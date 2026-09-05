package config

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHash_StableAndExcludesSecrets(t *testing.T) {
	t.Parallel()
	base := validProdConfig(t)
	h := base.Hash()
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), h)
	assert.Equal(t, h, base.Hash(), "deterministic across calls")
	assert.Equal(t, h, base.Clone().Hash(), "deterministic across copies")
	assert.Equal(t, h, mustLoad(t, prodEnv()).Hash(), "deterministic across loads")

	// Changing where a secret lives, or (in LOCAL) the plain secret value,
	// never changes the hash: secrets are structurally excluded.
	secretMutations := map[string]func(*Config){
		"db app url ref":        func(c *Config) { c.Database.AppURL = "aws-sm://cp/prod/OTHER" },
		"db app url plain":      func(c *Config) { c.Database.AppURL = "postgres://other:pw@h/db" },
		"redis url":             func(c *Config) { c.Redis.URL = "env://OTHER_REDIS" },
		"oidc client secret":    func(c *Config) { c.Auth.ClientSecretRef = "aws-sm://cp/prod/oidc-2" },
		"provider api key":      func(c *Config) { c.Providers.Execution.APIKeyRef = "aws-sm://cp/prod/exec-2" },
		"provider webhook":      func(c *Config) { c.Providers.Funding.WebhookSecretRef = "" },
		"archive static keys":   func(c *Config) { c.Archive.AccessKeyRef = "AKIA"; c.Archive.SecretKeyRef = "plain-secret" },
		"redpanda sasl":         func(c *Config) { c.Redpanda.SASLPasswordRef = "file:///run/secrets/x" },
		"clickhouse credential": func(c *Config) { c.ClickHouse.PasswordRef = "env://CH" },
	}
	for name, mutate := range secretMutations {
		c := base.Clone()
		mutate(c)
		assert.Equal(t, h, c.Hash(), "secret mutation %q must not change the hash", name)
	}

	// Every SecretRef field is covered, not just the ones listed above.
	for path, ref := range base.secretRefs() {
		c := base.Clone()
		*c.secretRefs()[path] = *ref + "x"
		assert.Equal(t, h, c.Hash(), "secret field %s must not affect the hash", path)
	}

	nonSecretMutations := map[string]func(*Config){
		"http addr":        func(c *Config) { c.HTTP.Addr = "0.0.0.0:9090" },
		"env":              func(c *Config) { c.Env = EnvStaging },
		"build version":    func(c *Config) { c.BuildVersion = "abc123" },
		"provider mode":    func(c *Config) { c.Providers.Execution.Mode = ProviderModeSandbox },
		"provider timeout": func(c *Config) { c.Providers.Execution.Timeout++ },
		"cors":             func(c *Config) { c.HTTP.CORSOrigins = append(c.HTTP.CORSOrigins, "https://x.example.com") },
		"brokers order": func(c *Config) {
			c.Redpanda.Brokers[0], c.Redpanda.Brokers[1] = c.Redpanda.Brokers[1], c.Redpanda.Brokers[0]
		},
		"retention":         func(c *Config) { c.Retention.SecurityAuditDays++ },
		"seed":              func(c *Config) { c.Seed.Enabled = true },
		"database tls flag": func(c *Config) { c.Database.RequireTLS = false },
		"sample ratio":      func(c *Config) { c.Telemetry.TraceSampleRatio = "0.2" },
	}
	seen := map[string]string{"base": h}
	for name, mutate := range nonSecretMutations {
		c := base.Clone()
		mutate(c)
		got := c.Hash()
		assert.NotEqual(t, h, got, "non-secret mutation %q must change the hash", name)
		for other, oh := range seen {
			assert.NotEqual(t, oh, got, "mutations %q and %q collide", name, other)
		}
		seen[name] = got
	}
	assert.Equal(t, h, base.Hash(), "mutating clones never touches the original")
}

func TestHash_CanonicalJSONSortsKeys(t *testing.T) {
	t.Parallel()
	type inner struct {
		Zeta  int64
		Alpha string
	}
	type outer struct {
		Zulu  inner
		Bravo []string
		Alpha bool
	}
	b, err := canonicalJSON(outer{Zulu: inner{Zeta: 1 << 60, Alpha: "a"}, Bravo: []string{"z", "a"}, Alpha: true})
	require.NoError(t, err)
	assert.JSONEq(t, `{"Alpha":true,"Bravo":["z","a"],"Zulu":{"Alpha":"a","Zeta":1152921504606846976}}`, string(b))
	assert.Equal(t, `{"Alpha":true,"Bravo":["z","a"],"Zulu":{"Alpha":"a","Zeta":1152921504606846976}}`, string(b), "keys sorted at every level, large integers preserved")

	// The hash input itself is valid JSON containing no secret material.
	base := validProdConfig(t)
	base.Database.AppURL = "aws-sm://cp/prod/super-secret-name"
	var generic map[string]any
	require.NoError(t, json.Unmarshal(base.hashInput(), &generic))
	assert.NotContains(t, string(base.hashInput()), "super-secret-name")
	assert.Contains(t, string(base.hashInput()), `"BuildVersion"`)
}

func TestRedacted(t *testing.T) {
	t.Parallel()
	c := mustLoad(t, map[string]string{
		"CP_ENV":              "LOCAL",
		"CP_DATABASE_APP_URL": "postgres://cp_app:hunter2@127.0.0.1:5433/controlplane",
		"CP_REDIS_URL":        "env://REDIS_URL",
		"CP_AUTH_MODE":        "oidc", "CP_AUTH_ISSUER": "http://localhost:9999", "CP_AUTH_CLIENT_ID": "x",
		"CP_AUTH_CLIENT_SECRET_REF": "topsecret", "CP_AUTH_REDIRECT_URL": "http://localhost:8080/cb",
		"CP_PROVIDER_MODEL_API_KEY_REF": "file:///tmp/key",
	})
	r := c.Redacted()
	assert.Equal(t, SecretRef(RedactedMarker), r.Database.AppURL, "plain values are masked")
	assert.Equal(t, SecretRef(RedactedMarker), r.Auth.ClientSecretRef)
	assert.Equal(t, SecretRef("env://REDIS_URL"), r.Redis.URL, "references are kept")
	assert.Equal(t, SecretRef("file:///tmp/key"), r.Providers.Model.APIKeyRef)
	assert.Equal(t, c.HTTP.Addr, r.HTTP.Addr, "non-secret fields are copied")
	assert.Contains(t, string(c.Database.AppURL), "hunter2", "the original is untouched")
	assert.Equal(t, c.Hash(), r.Hash(), "redaction does not change the non-secret hash")

	b, err := json.Marshal(r)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "hunter2")
	assert.NotContains(t, string(b), "topsecret")

	// Slices are deep-copied.
	r.Redpanda.Brokers[0] = "changed"
	assert.NotEqual(t, "changed", c.Redpanda.Brokers[0])

	var nilCfg *Config
	assert.Nil(t, nilCfg.Clone())
}

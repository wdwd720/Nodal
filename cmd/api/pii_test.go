package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/pii"
)

// An optional control is only safe when something checks that it was not
// accidentally left out (the same argument TestIdentityIsGivenTheAccountCeiling
// makes for the cohort ceiling). identity.Deps.PII is nil-tolerant so a test
// can build the service without a keyring; cmd/api must never do so.
func TestIdentityIsGivenThePIIStore(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("wire.go")
	require.NoError(t, err)
	text := string(src)
	assert.Contains(t, text, "newPIIStore(",
		"nothing constructs the PII store; a verified e-mail is hashed and then discarded at every login")
	i := strings.Index(text, "identity.Deps{")
	require.Positive(t, i, "identity.Deps is no longer built in wire.go")
	block := text[i:]
	if j := strings.Index(block, "})"); j > 0 {
		block = block[:j]
	}
	assert.Contains(t, block, "PII:", "identity.Deps is built without the PII store, so login never stores personal data")
}

// The absence of a keyring is said out loud, naming the variable and the
// consequence.
func TestNoKeyringIsSaidOutLoud(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := &config.Config{Env: config.EnvLocal}
	store, err := newPIIStore(context.Background(), cfg, config.NewResolver(cfg.Env, os.LookupEnv), log)
	require.NoError(t, err, "LOCAL keeps the honest degradation; only STAGING and PROD refuse")
	assert.Nil(t, store)
	assert.Contains(t, buf.String(), "level=WARN")
	assert.Contains(t, buf.String(), "CP_PII_KEYRING_REF")
	assert.Contains(t, buf.String(), "consequence")
}

// A keyring that is set but unusable is an ERROR and no store -- never a
// store over a broken ring, and never plaintext.
func TestAnUnusableKeyringIsAnErrorAndNoStore(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := &config.Config{Env: config.EnvLocal}
	cfg.PII.Keyring = config.SecretRef(`{"active": 3, "keys": {"1": "not-base64"}}`)
	store, err := newPIIStore(context.Background(), cfg, config.NewResolver(cfg.Env, os.LookupEnv), log)
	require.NoError(t, err, "LOCAL keeps the honest degradation; only STAGING and PROD refuse")
	assert.Nil(t, store)
	assert.Contains(t, buf.String(), "level=ERROR")
	assert.Contains(t, buf.String(), "not usable")
}

// And a usable one is a store, with its versions named and its key material
// not.
func TestAUsableKeyringIsAStore(t *testing.T) {
	t.Parallel()
	k := make([]byte, pii.KeySize)
	_, err := rand.Read(k)
	require.NoError(t, err)
	b64 := base64.StdEncoding.EncodeToString(k)
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := &config.Config{Env: config.EnvLocal}
	cfg.PII.Keyring = config.SecretRef(`{"active": 1, "keys": {"1": "` + b64 + `"}}`)
	store, err := newPIIStore(context.Background(), cfg, config.NewResolver(cfg.Env, os.LookupEnv), log)
	require.NoError(t, err)
	require.NotNil(t, store)
	assert.Equal(t, 1, store.Keyring().Active())
	assert.Contains(t, buf.String(), "active_key_version=1")
	assert.NotContains(t, buf.String(), b64, "the startup line printed key material")
}

// And in STAGING or PROD every one of those three branches is a refusal
// instead (F-137). RulePIIKeyring saw the env:// reference and nothing else,
// so a STAGING whose NODAL_PII_KEYRING had never been set served and stored no
// personal data at all -- the state F-47 sat in, under a document saying it
// could not boot.
func TestADeploymentWithoutAUsableKeyringRefusesToStart(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		t.Run(string(env), func(t *testing.T) {
			t.Parallel()
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			empty := config.LookupFromMap(map[string]string{})

			unset := &config.Config{Env: env}
			_, err := newPIIStore(context.Background(), unset, config.NewResolver(env, empty), log)
			require.Error(t, err, "%s served with no keyring at all", env)
			assert.Contains(t, err.Error(), "CP_PII_KEYRING_REF")

			dangling := &config.Config{Env: env}
			dangling.PII.Keyring = config.SecretRef("env://NODAL_PII_KEYRING_NOT_SET_ANYWHERE")
			_, err = newPIIStore(context.Background(), dangling, config.NewResolver(env, empty), log)
			require.Error(t, err, "%s served on a reference that resolves to nothing", env)
			assert.Contains(t, err.Error(), "CP_PII_KEYRING_REF")

			// Resolvable and unusable is fatal too: a keyring that cannot open
			// a row is not a keyring.
			broken := config.NewResolver(env, config.LookupFromMap(map[string]string{
				"NODAL_PII_KEYRING_NOT_SET_ANYWHERE": `{"active": 3, "keys": {"1": "not-base64"}}`,
			}))
			_, err = newPIIStore(context.Background(), dangling, broken, log)
			require.Error(t, err, "%s served on a keyring that does not parse", env)
			assert.NotContains(t, err.Error(), "not-base64", "the refusal printed the keyring document")
		})
	}
}

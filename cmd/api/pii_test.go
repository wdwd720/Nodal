package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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
	assert.Nil(t, newPIIStore(context.Background(), cfg, config.NewResolver(cfg.Env, os.LookupEnv), log))
	assert.Contains(t, buf.String(), "level=WARN")
	assert.Contains(t, buf.String(), "CP_PII_KEYRING")
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
	assert.Nil(t, newPIIStore(context.Background(), cfg, config.NewResolver(cfg.Env, os.LookupEnv), log))
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
	store := newPIIStore(context.Background(), cfg, config.NewResolver(cfg.Env, os.LookupEnv), log)
	require.NotNil(t, store)
	assert.Equal(t, 1, store.Keyring().Active())
	assert.Contains(t, buf.String(), "active_key_version=1")
	assert.NotContains(t, buf.String(), b64, "the startup line printed key material")
}

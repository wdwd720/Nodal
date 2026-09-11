//go:build integration

package pii_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/pii"
)

func openApp(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set")
	}
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 4, AppName: "pii-itest"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

func key(t *testing.T) string {
	t.Helper()
	k := make([]byte, pii.KeySize)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(k)
}

func newUser(t *testing.T, d *db.DB) string {
	t.Helper()
	suffix := id.New[id.Any]().String()
	u, err := accounts.NewRepository().CreateUser(context.Background(), d, "itest", "pii-"+suffix[len(suffix)-12:], nil)
	require.NoError(t, err)
	return u.ID.String()
}

func str(s string) *string { return &s }

// The row holds ciphertext, the store reads it back, and a partial update
// keeps what it did not touch.
func TestIntegration_PersonalDataIsStoredSealedAndReadBack(t *testing.T) {
	d := openApp(t)
	ctx := context.Background()
	k1 := key(t)
	kr, err := pii.ParseKeyring(`{"active": 1, "keys": {"1": "` + k1 + `"}}`)
	require.NoError(t, err)
	store := pii.NewStore(kr)
	user := newUser(t, d)

	_, found, err := store.Read(ctx, d, user)
	require.NoError(t, err)
	require.False(t, found)

	require.NoError(t, store.Upsert(ctx, d, user, pii.Patch{Email: str("Person@Example.test"), CountryCode: str("GB")}))

	// What the database holds is not the value.
	var email, legal, dob []byte
	var version int
	require.NoError(t, d.QueryRow(ctx, `SELECT email_encrypted, legal_name_encrypted, dob_encrypted, key_version
		FROM identity_pii WHERE user_id = $1::uuid`, user).Scan(&email, &legal, &dob, &version))
	assert.NotContains(t, strings.ToLower(string(email)), "example", "the column holds the plaintext")
	assert.Nil(t, legal, "an absent value is NULL, not the encryption of the empty string")
	assert.Nil(t, dob)
	assert.Equal(t, 1, version)

	rec, found, err := store.Read(ctx, d, user)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "Person@Example.test", rec.Email, "stored as given; normalisation is the lookup hash's job, not this column's")
	assert.Equal(t, "GB", rec.CountryCode)
	assert.Equal(t, 1, rec.KeyVersion)

	// A patch to one column leaves the others as they were.
	require.NoError(t, store.Upsert(ctx, d, user, pii.Patch{LegalName: str("A. Person")}))
	rec, _, err = store.Read(ctx, d, user)
	require.NoError(t, err)
	assert.Equal(t, "Person@Example.test", rec.Email)
	assert.Equal(t, "A. Person", rec.LegalName)

	// And clearing is a pointer to the empty string, which becomes NULL.
	require.NoError(t, store.Upsert(ctx, d, user, pii.Patch{LegalName: str("")}))
	require.NoError(t, d.QueryRow(ctx, `SELECT legal_name_encrypted FROM identity_pii WHERE user_id = $1::uuid`, user).Scan(&legal))
	assert.Nil(t, legal)
}

// Rotation end to end: a ring with a new active key reads the old row,
// re-seals it, and the row then names the new version. A ring without the
// old key cannot read the row until that happens -- which is the rule for
// when a key may be removed.
func TestIntegration_RotationResealsTheRow(t *testing.T) {
	d := openApp(t)
	ctx := context.Background()
	k1, k2 := key(t), key(t)
	v1, err := pii.ParseKeyring(`{"active": 1, "keys": {"1": "` + k1 + `"}}`)
	require.NoError(t, err)
	user := newUser(t, d)
	require.NoError(t, pii.NewStore(v1).Upsert(ctx, d, user, pii.Patch{Email: str("r@example.test"), DOB: str("1990-01-01")}))

	both, err := pii.ParseKeyring(fmt.Sprintf(`{"active": 2, "keys": {"1": %q, "2": %q}}`, k1, k2))
	require.NoError(t, err)
	store := pii.NewStore(both)
	rec, _, err := store.Read(ctx, d, user)
	require.NoError(t, err)
	assert.Equal(t, 1, rec.KeyVersion, "read does not rewrite")

	require.NoError(t, store.Reseal(ctx, d, user))
	rec, _, err = store.Read(ctx, d, user)
	require.NoError(t, err)
	assert.Equal(t, 2, rec.KeyVersion)
	assert.Equal(t, "r@example.test", rec.Email)
	assert.Equal(t, "1990-01-01", rec.DOB)

	only2, err := pii.ParseKeyring(`{"active": 2, "keys": {"2": "` + k2 + `"}}`)
	require.NoError(t, err)
	rec, _, err = pii.NewStore(only2).Read(ctx, d, user)
	require.NoError(t, err, "after the reseal, the old key is no longer needed")
	assert.Equal(t, "r@example.test", rec.Email)

	// Resealing a row with no changes and no row is harmless.
	require.NoError(t, store.Reseal(ctx, d, newUser(t, d)))
}

// The application role can write and read the table; that it cannot DELETE
// is asserted with every other privilege in test/integration/migrations.
func TestIntegration_TheWrongKeyringIsAnErrorNotAnEmptyRecord(t *testing.T) {
	d := openApp(t)
	ctx := context.Background()
	a, err := pii.ParseKeyring(`{"active": 1, "keys": {"1": "` + key(t) + `"}}`)
	require.NoError(t, err)
	b, err := pii.ParseKeyring(`{"active": 1, "keys": {"1": "` + key(t) + `"}}`)
	require.NoError(t, err)
	user := newUser(t, d)
	require.NoError(t, pii.NewStore(a).Upsert(ctx, d, user, pii.Patch{Email: str("w@example.test")}))

	_, found, err := pii.NewStore(b).Read(ctx, d, user)
	require.Error(t, err, "a different key of the same version must not read as an empty record")
	assert.True(t, found)
	assert.NotContains(t, err.Error(), "example", "the error carried the value")
}

package pii

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func b64Key(t *testing.T) string {
	t.Helper()
	k := make([]byte, KeySize)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(k)
}

func ring(t *testing.T, active int, versions ...int) (*Keyring, map[int]string) {
	t.Helper()
	keys := map[int]string{}
	doc := `{"active": ` + fmt.Sprint(active) + `, "keys": {`
	for i, v := range versions {
		keys[v] = b64Key(t)
		if i > 0 {
			doc += ","
		}
		doc += fmt.Sprintf(`"%d": %q`, v, keys[v])
	}
	doc += "}}"
	kr, err := ParseKeyring(doc)
	require.NoError(t, err)
	return kr, keys
}

// A keyring that is not entirely usable is refused entirely: a half-usable
// one would write rows nothing can read.
func TestKeyringIsRefusedWhole(t *testing.T) {
	t.Parallel()
	good := b64Key(t)
	cases := map[string]string{
		"not json":            `active=1`,
		"no keys":             `{"active": 1, "keys": {}}`,
		"active not present":  `{"active": 2, "keys": {"1": "` + good + `"}}`,
		"version zero":        `{"active": 0, "keys": {"0": "` + good + `"}}`,
		"version not integer": `{"active": 1, "keys": {"one": "` + good + `"}}`,
		"key not base64":      `{"active": 1, "keys": {"1": "***"}}`,
		"key too short":       `{"active": 1, "keys": {"1": "` + base64.StdEncoding.EncodeToString([]byte("short")) + `"}}`,
		"key too long":        `{"active": 1, "keys": {"1": "` + base64.StdEncoding.EncodeToString(make([]byte, 33)) + `"}}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseKeyring(doc)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), good, "the error printed key material")
		})
	}

	kr, _ := ring(t, 2, 1, 2)
	assert.Equal(t, 2, kr.Active())
	assert.Equal(t, []int{1, 2}, kr.Versions())
}

func TestSealAndOpenRoundTrip(t *testing.T) {
	t.Parallel()
	kr, _ := ring(t, 1, 1)
	ct, v, err := kr.Seal("user-1", "email", []byte("a@example.test"))
	require.NoError(t, err)
	assert.Equal(t, 1, v)
	assert.NotContains(t, string(ct), "example", "the ciphertext contains the plaintext")

	pt, err := kr.Open("user-1", "email", 1, ct)
	require.NoError(t, err)
	assert.Equal(t, "a@example.test", string(pt))

	// Two seals of one value differ: the nonce is fresh each time, so equal
	// plaintexts are not visible as equal ciphertexts to whoever can read the
	// table.
	ct2, _, err := kr.Seal("user-1", "email", []byte("a@example.test"))
	require.NoError(t, err)
	assert.NotEqual(t, ct, ct2)
}

// The ciphertext is bound to its row, its column and its key version. Whoever
// can write the table -- a restored backup, a privileged session -- cannot
// rearrange it.
func TestCiphertextCannotBeMoved(t *testing.T) {
	t.Parallel()
	kr, _ := ring(t, 1, 1)
	ct, _, err := kr.Seal("user-1", "email", []byte("a@example.test"))
	require.NoError(t, err)

	_, err = kr.Open("user-2", "email", 1, ct)
	require.Error(t, err, "opened in another user's row")
	_, err = kr.Open("user-1", "legal_name", 1, ct)
	require.Error(t, err, "opened as another column")

	// Relabelled with a version the ring holds but did not seal it with.
	kr2, _ := ring(t, 2, 1, 2)
	ct1, _, err := kr2.Seal("user-1", "email", []byte("x"))
	require.NoError(t, err)
	_, err = kr2.Open("user-1", "email", 1, ct1)
	require.Error(t, err, "opened under a version it was not sealed with")

	// Altered by one bit.
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0x01
	_, err = kr.Open("user-1", "email", 1, tampered)
	require.Error(t, err)

	// Too short to be a ciphertext at all.
	_, err = kr.Open("user-1", "email", 1, []byte("nope"))
	require.Error(t, err)

	// A version this ring does not hold is named in the error, without any
	// key material.
	_, err = kr.Open("user-1", "email", 7, ct)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version 7")
}

// Rotation: a ring that holds the old key still opens old rows, and seals new
// ones under the active version.
func TestRotationOpensOldAndSealsNew(t *testing.T) {
	t.Parallel()
	old, keys := ring(t, 1, 1)
	ct, v, err := old.Seal("user-1", "dob", []byte("1990-01-01"))
	require.NoError(t, err)
	require.Equal(t, 1, v)

	rotated, err := ParseKeyring(`{"active": 2, "keys": {"1": "` + keys[1] + `", "2": "` + b64Key(t) + `"}}`)
	require.NoError(t, err)
	pt, err := rotated.Open("user-1", "dob", 1, ct)
	require.NoError(t, err)
	assert.Equal(t, "1990-01-01", string(pt))
	_, v2, err := rotated.Seal("user-1", "dob", []byte("1990-01-01"))
	require.NoError(t, err)
	assert.Equal(t, 2, v2)

	// And a ring that dropped the old key cannot open the old row. That is
	// the rule rotation has to respect: remove a key only once no row names
	// its version.
	dropped, err := ParseKeyring(`{"active": 2, "keys": {"2": "` + b64Key(t) + `"}}`)
	require.NoError(t, err)
	_, err = dropped.Open("user-1", "dob", 1, ct)
	require.Error(t, err)
}

func TestSealNeedsItsPlace(t *testing.T) {
	t.Parallel()
	kr, _ := ring(t, 1, 1)
	_, _, err := kr.Seal("", "email", []byte("x"))
	require.Error(t, err)
	_, _, err = kr.Seal("user-1", "", []byte("x"))
	require.Error(t, err)
}

// A nil store refuses rather than silently writing nothing: "no personal data
// was stored" must be an error the caller sees, not an absence it discovers.
func TestNilStoreRefuses(t *testing.T) {
	t.Parallel()
	var s *Store
	assert.Nil(t, NewStore(nil))
	require.Error(t, s.Upsert(t.Context(), nil, "user-1", Patch{}))
	_, _, err := s.Read(t.Context(), nil, "user-1")
	require.Error(t, err)
	require.Error(t, s.Reseal(t.Context(), nil, "user-1"))
}

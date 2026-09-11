package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A key withdrawn from the JWKS stops verifying (F-98).
//
// The cache refreshed when it was empty and when a token named an unknown kid.
// Rotation produces an unknown kid, so rotation worked. REVOCATION does not: an
// operator pulling a compromised key introduces nothing new, so no unknown kid
// ever arrives, and the withdrawn key went on verifying tokens for the life of
// the process. It self-healed only by accident, at the next ordinary rotation.
//
// That is the emergency case, and it was the one case the refresh policy could
// not serve.
func TestKeySet_AWithdrawnKeyStopsVerifyingWithinMaxAge(t *testing.T) {
	t.Parallel()

	var (
		fetches atomic.Int64
		serve   atomic.Value // the JWKS body to serve
	)
	both := `{"keys":[` + testJWK(t, "key-a") + `,` + testJWK(t, "key-b") + `]}`
	onlyB := `{"keys":[` + testJWK(t, "key-b") + `]}`
	serve.Store(both)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		body, ok := serve.Load().(string)
		require.True(t, ok)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	now := time.Now()
	const maxAge = 15 * time.Minute
	ks := newKeySet(srv.URL, srv.Client(), func() time.Time { return now }, time.Minute, maxAge,
		[]string{"RS256", "ES256"})

	// key-a is known while it is published.
	keys, err := ks.candidates(context.Background(), "key-a")
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	require.EqualValues(t, 1, fetches.Load())

	// The operator withdraws it. Nothing new appears, so nothing asks for an
	// unknown kid.
	serve.Store(onlyB)

	// Inside the window the cached set still answers, and no refetch happens.
	// This half matters: a cache that refetched on every call would pass the
	// assertion below while being a different design.
	keys, err = ks.candidates(context.Background(), "key-a")
	require.NoError(t, err)
	require.NotEmpty(t, keys)
	assert.EqualValues(t, 1, fetches.Load(), "the cache refetched inside its own window")

	// Past the window, the withdrawn key is gone.
	now = now.Add(maxAge + time.Second)
	_, err = ks.candidates(context.Background(), "key-a")
	require.Error(t, err, "a key withdrawn from the JWKS still verified")
	assert.ErrorIs(t, err, ErrUnknownKey)
	assert.EqualValues(t, 2, fetches.Load())

	// And the control: the key that is still published still works, so the
	// expiry refreshed the set rather than emptying it.
	keys, err = ks.candidates(context.Background(), "key-b")
	require.NoError(t, err)
	assert.NotEmpty(t, keys)
}

// testJWK renders one usable public JWK with the given kid.
func testJWK(t *testing.T, kid string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return string(marshalJWK(t, kid, "sig", "RS256", key.Public()))
}

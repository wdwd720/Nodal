//go:build integration

package signing_test

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/wallet/wallettest"
)

// The recovery path signs the bytes that were approved, not the bytes it is
// handed (F-67).
//
// `Sign` computes `sha256(req.UnsignedTx)` at the top and then, on the replay
// path, never used it. When a decision exists and is APPROVED but no
// `signing_results` row does -- a provider failure or a crash between the two
// transactions, which the code documents as normal recovery -- `replayLoaded`
// called `signApproved` with `req.UnsignedTx`, the bytes of *that* call.
//
// So a second call with the same `attempt_id` and different bytes signed those
// bytes with the wallet key, on an approval granted to something else. None of
// the sixteen inspector checks runs again on that path, and neither does
// `validateLinkage`, so nothing else stood between the request and the signer.
//
// The existing recovery test could not see it: it calls `s.request()` both
// times, which builds the identical transaction. This one changes the bytes,
// which is the whole point.

func TestIntegration_RecoveryRefusesDifferentBytes(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	fake, err := wallettest.New(config.EnvTest)
	require.NoError(t, err)
	s := seedChain(t, fake, defaultSeedOptions())
	svc := newService(t, fake, chainFor(s))

	// Leave the attempt in the recovering state: decided and approved, with the
	// provider never reached.
	fake.FailWith(errs.New(errs.CodeProviderUnavailable, "privy down"))
	d, signed, err := svc.Sign(ctx, s.request())
	require.Error(t, err)
	require.True(t, d.Approved)
	require.Nil(t, signed)
	require.Equal(t, 0, countRows(t, `SELECT count(*) FROM signing_results WHERE attempt_id = $1`, s.attemptID))

	// The provider is healthy again, and a second call arrives carrying a
	// different transaction under the same attempt id.
	fake.FailWith(nil)
	before := len(fake.Calls())

	other := s.request()
	tampered := append([]byte(nil), s.raw...)
	tampered[len(tampered)-1] ^= 0xff
	h := sha256.Sum256(tampered)
	other.UnsignedTx = tampered
	other.ExpectedTxHash = h[:] // internally consistent, and approved by nobody

	_, signedOther, err := svc.Sign(ctx, other)
	require.Error(t, err, "the recovery path signed bytes the decision never approved")
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "does not match the one this decision approved")
	assert.Nil(t, signedOther)
	assert.Len(t, fake.Calls(), before, "the signer was reached with unapproved bytes")
	assert.Equal(t, 0, countRows(t, `SELECT count(*) FROM signing_results WHERE attempt_id = $1`, s.attemptID))

	// The positive control, and the reason this is not simply "refuse the
	// second call": the genuine recovery must still complete. A binding that
	// refused everything would leave every provider failure permanently
	// unrecoverable, which is worse than the defect.
	d2, signed2, err := svc.Sign(ctx, s.request())
	require.NoError(t, err, "the real recovery was refused")
	assert.Equal(t, d.ID, d2.ID, "recovery completes under the same decision")
	assert.True(t, d2.Replayed)
	assert.NotEmpty(t, signed2)
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM signing_decisions WHERE attempt_id = $1`, s.attemptID),
		"no second decision")
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM signing_results WHERE attempt_id = $1`, s.attemptID))
}

// TestIntegration_ReplayOfASignedAttemptStillReturnsTheStoredBytes: once a
// result exists, the replay returns it without consulting the request's bytes
// at all -- that path never reaches the signer, so it needs no binding and must
// not gain one. Stated as a test because the fix above is a refusal, and a
// refusal in the wrong place breaks idempotency.
func TestIntegration_ReplayOfASignedAttemptStillReturnsTheStoredBytes(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	fake, err := wallettest.New(config.EnvTest)
	require.NoError(t, err)
	s := seedChain(t, fake, defaultSeedOptions())
	svc := newService(t, fake, chainFor(s))

	d, signed, err := svc.Sign(ctx, s.request())
	require.NoError(t, err)
	require.NotEmpty(t, signed)

	d2, signed2, err := svc.Sign(ctx, s.request())
	require.NoError(t, err)
	assert.Equal(t, d.ID, d2.ID)
	assert.Equal(t, signed, signed2, "a completed attempt replays the bytes it signed")
	assert.Equal(t, 1, countRows(t, `SELECT count(*) FROM signing_results WHERE attempt_id = $1`, s.attemptID))
}

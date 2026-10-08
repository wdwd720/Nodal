package devnet

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// FuzzDecode: Decode never panics; whatever it rejects is VALIDATION_FAILED
// / MALFORMED_TRANSACTION; whatever it accepts re-encodes to exactly the same
// bytes and decodes to the same transaction; and the stateless Submit checks
// and a chain's Submit never panic on it either.
func FuzzDecode(f *testing.F) {
	for _, raw := range validEncodings(f) {
		f.Add(raw)
		f.Add(raw[:len(raw)/2])
		f.Add(append(bytes.Clone(raw), 0))
	}
	f.Add([]byte{})
	f.Add([]byte("ndvt"))
	f.Add([]byte("ndvt\x01\x00\x00\x00"))
	f.Add(bytes.Repeat([]byte{0xFF}, MaxTransactionSize+1))

	c, err := NewChain(testConfig(testChainID))
	require.NoError(f, err)

	f.Fuzz(func(t *testing.T, raw []byte) {
		tx, err := Decode(raw)
		if err != nil {
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "%v", err)
			require.Equal(t, CodeMalformedTransaction, ReasonOf(err), "%v", err)
			return
		}
		enc, err := tx.Encode()
		require.NoError(t, err, "an accepted transaction must re-encode")
		require.Equal(t, raw, enc, "Encode(Decode(x)) must reproduce x")
		again, err := Decode(enc)
		require.NoError(t, err)
		require.Equal(t, tx, again)

		_ = tx.Message.Validate()
		_ = tx.VerifySignatures()
		_, _ = tx.ID()
		_ = tx.Message.RequiredSigners()
		_, _ = c.Submit(raw)
	})
}

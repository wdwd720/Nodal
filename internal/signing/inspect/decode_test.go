package inspect_test

import (
	"bytes"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/signing/inspect"
	"github.com/nodal/controlplane/internal/signing/signingtest"
)

func TestDecode_Golden(t *testing.T) {
	t.Parallel()
	s := signingtest.NewSwap()
	raw := s.Golden(inspect.VersionV0)
	tx, err := inspect.Decode(raw)
	require.NoError(t, err)
	assert.Equal(t, inspect.VersionV0, tx.Version)
	assert.Equal(t, 1, tx.NumSignatures)
	assert.Equal(t, uint8(1), tx.Header.NumRequiredSignatures)
	assert.Equal(t, s.Wallet, tx.StaticKeys[0])
	assert.Equal(t, s.Blockhash, tx.RecentBlockhash)
	assert.Len(t, tx.Instructions, 4)
	assert.Equal(t, inspect.TxHash(raw), tx.Hash)
	assert.Equal(t, len(raw), tx.Size)
	require.Len(t, tx.LookupTables, 1)
	assert.Equal(t, s.LookupTable, tx.LookupTables[0].Table)
	assert.Len(t, tx.LookupTables[0].WritableIndexes, 4, "the four pool accounts")

	legacy, err := inspect.Decode(s.Golden(inspect.VersionLegacy))
	require.NoError(t, err)
	assert.Equal(t, inspect.VersionLegacy, legacy.Version)
	assert.Empty(t, legacy.LookupTables)
}

func TestDecode_Rejects(t *testing.T) {
	t.Parallel()
	s := signingtest.NewSwap()
	golden := s.Golden(inspect.VersionV0)

	cases := map[string][]byte{
		"empty":                   {},
		"single byte":             {0x01},
		"truncated":               golden[:len(golden)/2],
		"trailing byte":           append(append([]byte(nil), golden...), 0),
		"oversized":               bytes.Repeat([]byte{0x01}, inspect.MaxTransactionSize+1),
		"v1 prefix":               append([]byte{0x81}, golden[1:]...),
		"garbage version":         append([]byte{0xC0}, golden[1:]...),
		"zero signatures claimed": append([]byte{0x00}, golden[1:]...),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := inspect.Decode(raw)
			require.Error(t, err)
			assert.ErrorIs(t, err, inspect.ErrDecode)
		})
	}
	t.Run("legacy transaction reports no lookups", func(t *testing.T) {
		t.Parallel()
		// A legacy build must not be confused by a v0 expectation.
		raw := s.Golden(inspect.VersionLegacy)
		tx, err := inspect.Decode(raw)
		require.NoError(t, err)
		assert.Empty(t, tx.LookupTables)
	})
	t.Run("a signed transaction decodes identically to its unsigned form", func(t *testing.T) {
		t.Parallel()
		raw := s.Golden(inspect.VersionLegacy)
		stx, err := solana.TransactionFromBytes(raw)
		require.NoError(t, err)
		_, err = stx.Sign(func(k solana.PublicKey) *solana.PrivateKey {
			if k.Equals(s.Wallet) {
				return &s.WalletKey
			}
			return nil
		})
		require.NoError(t, err)
		signed, err := stx.MarshalBinary()
		require.NoError(t, err)
		a, err := inspect.Decode(raw)
		require.NoError(t, err)
		b, err := inspect.Decode(signed)
		require.NoError(t, err)
		assert.Equal(t, a.Instructions, b.Instructions)
		assert.Equal(t, a.StaticKeys, b.StaticKeys)
		assert.NotEqual(t, a.Hash, b.Hash, "signatures are part of the bytes and therefore of the hash")
	})
}

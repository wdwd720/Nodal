package stableasset

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
)

// signSnapshot signs a body exactly as TakeSnapshot does.
func signSnapshot(t testing.TB, signer *SnapshotSigner, body ReserveSnapshot) SignedSnapshot {
	t.Helper()
	canonical, err := audit.CanonicalJSON(body)
	require.NoError(t, err)
	hash := sha256.Sum256(canonical)
	return SignedSnapshot{Snapshot: body, Body: canonical, Hash: hash,
		Signature: signer.sign(snapshotSigningMessage(hash)), PublicKey: signer.PublicKey()}
}

func snapshotChain(t testing.TB, signer *SnapshotSigner, n int) []SignedSnapshot {
	t.Helper()
	var out []SignedSnapshot
	prev := ""
	for i := 1; i <= n; i++ {
		body := ReserveSnapshot{
			Version: snapshotVersion, Disclaimer: SnapshotDisclaimer, StableAssetID: "sa", AssetID: "a", Chain: "sbx-solana",
			Mint: "m", Seq: strconv.Itoa(i), TakenAt: time.Date(2026, 10, 7, 12, i, 0, 0, time.UTC), PrevHash: prev,
			LedgerSupply: strconv.Itoa(1000 * i), ChainSupply: strconv.Itoa(1000 * i), PendingMint: "0", PendingBurn: "0",
			Peg: priceDoc(price(1, 0)), Reserves: []SnapshotReserve{}, Composition: []CompositionLine{},
			SupplyValue: "1", EligibleReserve: "1", CoverageBPS: "10000",
		}
		sn := signSnapshot(t, signer, body)
		prev = hex.EncodeToString(sn.Hash[:])
		out = append(out, sn)
	}
	return out
}

func requireSnapshotReason(t *testing.T, err error, reason string) {
	t.Helper()
	require.Error(t, err)
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Equal(t, reason, e.Fields["reason"])
}

// TestVerifySnapshotChain_DetectsEveryKindOfTampering: a chain verifies from
// the snapshots and the public key alone, and any edit -- to a body, a hash,
// a signature, the order, the membership, the key or the disclaimer -- fails
// verification with a stable reason.
func TestVerifySnapshotChain_DetectsEveryKindOfTampering(t *testing.T) {
	signer := NewSnapshotSigner(seed("snapshots"))
	key := signer.PublicKey()
	chain := snapshotChain(t, signer, 3)
	require.NoError(t, VerifySnapshotChain(chain, key))
	require.NoError(t, VerifySnapshotChain(nil, key), "an empty chain is trivially consistent")

	clone := func() []SignedSnapshot { return append([]SignedSnapshot(nil), chain...) }

	edited := clone()
	edited[1].Snapshot.LedgerSupply = "999999"
	requireSnapshotReason(t, VerifySnapshotChain(edited, key), SnapshotBodyNotCanonical)

	rehashedBody := clone()
	rehashedBody[1].Snapshot.ChainSupply = "1"
	b, err := audit.CanonicalJSON(rehashedBody[1].Snapshot)
	require.NoError(t, err)
	rehashedBody[1].Body = b
	requireSnapshotReason(t, VerifySnapshotChain(rehashedBody, key), SnapshotHashMismatch)

	resigned := clone()
	resigned[1].Snapshot.ChainSupply = "1"
	resigned[1].Body, _ = audit.CanonicalJSON(resigned[1].Snapshot)
	resigned[1].Hash = sha256.Sum256(resigned[1].Body)
	requireSnapshotReason(t, VerifySnapshotChain(resigned, key), SnapshotSignatureInvalid)

	swapped := clone()
	swapped[0], swapped[1] = swapped[1], swapped[0]
	require.Error(t, VerifySnapshotChain(swapped, key))

	dropped := []SignedSnapshot{chain[0], chain[2]}
	requireSnapshotReason(t, VerifySnapshotChain(dropped, key), SnapshotChainBroken)

	other := NewSnapshotSigner(seed("impostor"))
	requireSnapshotReason(t, VerifySnapshotChain(chain, other.PublicKey()), SnapshotKeyMismatch)
	forged := snapshotChain(t, other, 3)
	for i := range forged {
		forged[i].PublicKey = key // claims the real key; the signature says otherwise
	}
	requireSnapshotReason(t, VerifySnapshotChain(forged, key), SnapshotSignatureInvalid)

	noDisclaimer := clone()
	body := noDisclaimer[0].Snapshot
	body.Disclaimer = "attestation"
	noDisclaimer[0] = signSnapshot(t, signer, body)
	requireSnapshotReason(t, VerifySnapshotChain(noDisclaimer[:1], key), SnapshotNotADisclaimer)
}

func TestParseSignedSnapshot_RoundTripsWhatTakeSnapshotStores(t *testing.T) {
	signer := NewSnapshotSigner(seed("snapshots"))
	sn := snapshotChain(t, signer, 1)[0]
	parsed, err := ParseSignedSnapshot(sn.Body, sn.Hash[:], sn.Signature, sn.PublicKey)
	require.NoError(t, err)
	require.NoError(t, VerifySnapshotChain([]SignedSnapshot{parsed}, signer.PublicKey()))
	_, err = ParseSignedSnapshot(append(sn.Body, '}'), sn.Hash[:], sn.Signature, sn.PublicKey)
	require.Error(t, err)
	_, err = ParseSignedSnapshot([]byte(`{"version":"x","extra":1}`), sn.Hash[:], sn.Signature, sn.PublicKey)
	require.Error(t, err, "unknown fields are refused")
	_, err = ParseSignedSnapshot(sn.Body, sn.Hash[:31], sn.Signature, sn.PublicKey)
	require.Error(t, err)
}

// FuzzVerifySnapshot: parsing and verifying any stored bytes never panics,
// and nothing but a genuinely signed body verifies.
func FuzzVerifySnapshot(f *testing.F) {
	signer := NewSnapshotSigner(seed("snapshots"))
	sn := snapshotChain(f, signer, 1)[0]
	f.Add(sn.Body, sn.Hash[:], sn.Signature)
	f.Add([]byte(`{}`), make([]byte, 32), make([]byte, 64))
	f.Fuzz(func(t *testing.T, body, hash, sig []byte) {
		parsed, err := ParseSignedSnapshot(body, hash, sig, signer.PublicKey())
		if err != nil {
			return
		}
		if VerifySnapshotChain([]SignedSnapshot{parsed}, signer.PublicKey()) == nil {
			// Only the genuine snapshot can verify.
			if string(body) != string(sn.Body) {
				t.Fatalf("a body that was never signed verified: %q", body)
			}
		}
	})
}

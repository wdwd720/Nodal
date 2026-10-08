package sandboxchain

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
)

const seedHex = "8ba28c49ce9db1683cc3976600f4b291512c0d860ac74b76d175b3d72673f374"

func mustSeed(t *testing.T, s string) Seed {
	t.Helper()
	seed, err := ParseSeed(s)
	require.NoError(t, err)
	return seed
}

func TestParseSeed_AcceptsExactlySixtyFourHexCharacters(t *testing.T) {
	seed := mustSeed(t, seedHex)
	assert.Equal(t, seed, mustSeed(t, "  "+strings.ToUpper(seedHex)+"\n"), "case and surrounding whitespace do not matter")
	for name, bad := range map[string]string{
		"empty":          "",
		"short":          seedHex[:62],
		"long":           seedHex + "00",
		"not hex":        "zz" + seedHex[2:],
		"all zeros":      strings.Repeat("0", 64),
		"inner space":    seedHex[:32] + " " + seedHex[33:],
		"base64-looking": "i6KMSc6dsWg8w5dmAPSykVEsDYYKdrdNdNF7PXJnP3Q=",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseSeed(bad)
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// TestDerive_TheSameSeedGivesTheSameKeysInEveryBinary is the property the
// package exists for: two processes given one seed sign as one platform.
func TestDerive_TheSameSeedGivesTheSameKeysInEveryBinary(t *testing.T) {
	a, b := DeriveKeys(mustSeed(t, seedHex)), DeriveKeys(mustSeed(t, seedHex))
	acct := accounts.NewAccountID()
	assert.Equal(t, a.Issuer.PublicKey(), b.Issuer.PublicKey())
	assert.Equal(t, a.Attester.PublicKey(), b.Attester.PublicKey())
	assert.Equal(t, a.Snapshots.PublicKey(), b.Snapshots.PublicKey())
	assert.Equal(t, a.Custody.HolderAddress(acct), b.Custody.HolderAddress(acct))
}

func TestDerive_EveryPurposeHasItsOwnKey(t *testing.T) {
	seed := mustSeed(t, seedHex)
	seen := map[[32]byte]string{}
	for _, p := range []string{PurposeCustody, PurposeIssuer, PurposeSnapshots, PurposeAttester, PurposeDelegatedSigner,
		PurposeDelegationVenue + "sandbox-dex", PurposeDelegationVenue + "other-venue"} {
		k := seed.Derive(p)
		other, dup := seen[k]
		require.False(t, dup, "%s and %s derive the same key", p, other)
		seen[k] = p
		assert.NotEqual(t, [32]byte(seed), k, "a derived key is never the seed itself")
	}
}

func TestVenueSeed_IsThePrefixedPurpose(t *testing.T) {
	seed := mustSeed(t, seedHex)
	assert.Equal(t, seed.Derive(PurposeDelegationVenue+"sandbox-dex"), seed.VenueSeed("sandbox-dex"))
	assert.NotEqual(t, seed.VenueSeed("sandbox-dex"), seed.VenueSeed("sandbox-dex-2"))
}

func TestDerive_ADifferentSeedGivesDifferentKeys(t *testing.T) {
	other := []byte(seedHex)
	other[0] = '9'
	a, b := DeriveKeys(mustSeed(t, seedHex)), DeriveKeys(mustSeed(t, string(other)))
	assert.NotEqual(t, a.Attester.PublicKey(), b.Attester.PublicKey())
	assert.NotEqual(t, a.Issuer.PublicKey(), b.Issuer.PublicKey())
	acct := accounts.NewAccountID()
	assert.NotEqual(t, a.Custody.HolderAddress(acct), b.Custody.HolderAddress(acct))
}

// TestDerive_TheDerivationIsPinned holds the derivation to a known answer, so
// a refactor that silently changed it -- and with it every key of every
// existing network -- fails here first.
func TestDerive_TheDerivationIsPinned(t *testing.T) {
	got := mustSeed(t, seedHex).Derive(PurposeAttester)
	assert.Equal(t, pinnedAttesterSeed, hexOf(got[:]))
}

// pinnedAttesterSeed was computed outside Go (Python's hmac module, HMAC-SHA256
// of "nodal/sandboxchain/v1/bridge-attester" under seedHex), so the pin checks
// the construction as well as its stability.
const pinnedAttesterSeed = "ea8f6dcf1824666bc8ad4c9f0d775c24e452798d8a0a8b0527875e9f960a28d6"

func hexOf(b []byte) string { return hex.EncodeToString(b) }

package devnet

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/mr-tron/base58"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

// TestKeypair_RFC8032Vector pins the signing path to real Ed25519: RFC 8032
// section 7.1, TEST 1 (empty message).
func TestKeypair_RFC8032Vector(t *testing.T) {
	var seed [SeedSize]byte
	copy(seed[:], mustHex(t, "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60"))
	kp := NewKeypairFromSeed(seed)
	pub := kp.PublicKey()
	require.Equal(t, "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a", hex.EncodeToString(pub[:]))

	sig, err := kp.Sign(nil)
	require.NoError(t, err)
	require.Equal(t, "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065"+
		"224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b", hex.EncodeToString(sig[:]))
	require.True(t, Verify(pub, nil, sig))
	require.False(t, Verify(pub, []byte{0}, sig), "a signature must not verify for another message")
	other := testKey("other").PublicKey()
	require.False(t, Verify(other, nil, sig), "a signature must not verify for another key")
}

func TestKeypair_DeterministicFromSeed(t *testing.T) {
	a, b := testKey("same"), testKey("same")
	require.Equal(t, a.PublicKey(), b.PublicKey())
	sa, err := a.Sign([]byte("m"))
	require.NoError(t, err)
	sb, err := b.Sign([]byte("m"))
	require.NoError(t, err)
	require.Equal(t, sa, sb, "Ed25519 signing is deterministic")
	require.NotEqual(t, a.PublicKey(), testKey("different").PublicKey())
}

func TestKeypair_ZeroValueRefusesToSign(t *testing.T) {
	_, err := Keypair{}.Sign([]byte("m"))
	requireReason(t, err, errs.CodeValidationFailed, CodeKeypairUninitialized)
}

// TestKeypair_FormattingNeverLeaksThePrivateKey: whatever verb a log line or
// error message uses, only the public key is printed.
func TestKeypair_FormattingNeverLeaksThePrivateKey(t *testing.T) {
	seed := testSeed("secret")
	private := ed25519.NewKeyFromSeed(seed[:])
	kp := NewKeypairFromSeed(seed)
	secrets := []string{
		hex.EncodeToString(seed[:]),
		hex.EncodeToString(private),
		base58.Encode(private),
		base58.Encode(seed[:]),
		strings.Trim(fmt.Sprint(seed[:6]), "[]"), // how a raw field dump would render it
	}
	// %p is the interesting one: fmt reports it as a bad verb for a struct and
	// prints the raw fields without consulting Format.
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%X", "%q", "%d", "%p"} {
		for _, form := range []string{fmt.Sprintf(verb, kp), fmt.Sprintf(verb, &kp), fmt.Sprintf(verb, []Keypair{kp})} {
			for _, secret := range secrets {
				require.NotContains(t, form, secret, "verb %s leaked private material", verb)
			}
		}
	}
	require.Equal(t, "Keypair("+kp.PublicKey().String()+")", fmt.Sprintf("%v", kp))

	// Reflection-based dumpers (test diffs, debuggers) read fields directly,
	// so no field may hold key material at all: only the public key and the
	// signing closure.
	v := reflect.ValueOf(kp)
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Func:
		case reflect.Array:
			b := make([]byte, f.Len())
			for j := range b {
				b[j] = byte(f.Index(j).Uint())
			}
			pub := kp.PublicKey()
			require.Equal(t, pub[:], b, "the only byte field is the public key")
		default:
			t.Fatalf("Keypair field %s has kind %s; key material must stay inside the closure", v.Type().Field(i).Name, f.Kind())
		}
	}
}

func TestBase58_RoundTripsAndIsStrict(t *testing.T) {
	k := testKey("k").PublicKey()
	parsed, err := ParsePublicKey(k.String())
	require.NoError(t, err)
	require.Equal(t, k, parsed)

	zero := PublicKey{}
	require.Equal(t, strings.Repeat("1", 32), zero.String())
	parsedZero, err := ParsePublicKey(zero.String())
	require.NoError(t, err)
	require.True(t, parsedZero.IsZero())

	sig, err := testKey("k").Sign([]byte("m"))
	require.NoError(t, err)
	parsedSig, err := ParseSignature(sig.String())
	require.NoError(t, err)
	require.Equal(t, sig, parsedSig)

	h := Hash{1, 2, 3}
	parsedHash, err := ParseHash(h.String())
	require.NoError(t, err)
	require.Equal(t, h, parsedHash)

	bad := []string{
		"",                              // empty
		strings.Repeat("2", 45),         // longer than any 32-byte encoding
		"0" + k.String()[1:],            // '0' is not in the alphabet
		"O" + k.String()[1:],            // nor is 'O'
		k.String() + " ",                // whitespace
		base58.Encode(make([]byte, 31)), // 31 bytes
		"1" + k.String(),                // a leading '1' adds a zero byte: 33 bytes
		base58.Encode([]byte{1}),        // far too short
	}
	for _, s := range bad {
		_, err := ParsePublicKey(s)
		requireReason(t, err, errs.CodeValidationFailed, CodeInvalidBase58)
	}
	_, err = ParseSignature(k.String())
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidBase58)
	_, err = ParseSignature(strings.Repeat("z", 89))
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidBase58)
}

func TestTxID_IsTheBase58FirstSignature(t *testing.T) {
	sig, err := testKey("payer").Sign([]byte("m"))
	require.NoError(t, err)
	id := TxIDFromSignature(sig)
	require.Equal(t, TxID(base58.Encode(sig[:])), id)
	back, err := id.Signature()
	require.NoError(t, err)
	require.Equal(t, sig, back)
	_, err = TxID("not-a-txid").Signature()
	requireReason(t, err, errs.CodeValidationFailed, CodeInvalidBase58)
}

func TestChainID_Validate(t *testing.T) {
	for _, ok := range []ChainID{"sbx-solana", "sbx-ethereum", "a", "0", ChainID(strings.Repeat("z", MaxChainIDLen)), "x-1-y"} {
		require.NoError(t, ok.Validate(), "%q", ok)
	}
	for _, bad := range []ChainID{"", "SBX", "sbx_solana", "sbx solana", "sbx.eth", ChainID(strings.Repeat("z", MaxChainIDLen+1)), "\u00e9"} {
		requireReason(t, bad.Validate(), errs.CodeValidationFailed, CodeInvalidChainID)
	}
}

func TestDerivedAddresses_AreDeterministicAndSeparated(t *testing.T) {
	require.Equal(t, FeeCollectorAddress("sbx-solana"), FeeCollectorAddress("sbx-solana"))
	require.NotEqual(t, FeeCollectorAddress("sbx-solana"), FeeCollectorAddress("sbx-ethereum"))
	require.NotEqual(t, FeeCollectorAddress("sbx-solana"), BridgeEscrowAddress("sbx-solana"))
	require.False(t, BridgeEscrowAddress("sbx-solana").IsZero())
}

func TestAmountConversions_AreExact(t *testing.T) {
	maxU := ^uint64(0)
	require.Equal(t, "18446744073709551615", QuantityOf(maxU).String())
	got, err := AmountOf(QuantityOf(maxU))
	require.NoError(t, err)
	require.Equal(t, maxU, got)
	got, err = AmountOf(money.QuantityFromInt64(0))
	require.NoError(t, err)
	require.Zero(t, got)

	_, err = AmountOf(money.QuantityFromInt64(-1))
	requireReason(t, err, errs.CodeOverflow, CodeArithmeticOverflow)
	over := new(big.Int).Add(new(big.Int).SetUint64(maxU), big.NewInt(1))
	_, err = AmountOf(money.QuantityFromBigInt(over))
	requireReason(t, err, errs.CodeOverflow, CodeArithmeticOverflow)
}

func TestReasonOf_ForeignErrors(t *testing.T) {
	require.Equal(t, ErrorCode(""), ReasonOf(nil))
	require.Equal(t, ErrorCode(""), ReasonOf(fmt.Errorf("plain")))
	require.Equal(t, ErrorCode(""), ReasonOf(errs.New(errs.CodeConflict, "no reason field")))
}

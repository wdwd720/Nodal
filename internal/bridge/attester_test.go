package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
)

func seed(s string) [devnet.SeedSize]byte { return sha256.Sum256([]byte(s)) }

func pk(s string) devnet.PublicKey { return devnet.NewKeypairFromSeed(seed(s)).PublicKey() }

func bh(s string) devnet.Hash { return devnet.Hash(sha256.Sum256([]byte("blockhash/" + s))) }

const (
	srcChain devnet.ChainID = "sbx-solana"
	dstChain devnet.ChainID = "sbx-ethereum"
)

// releaseFixture is an attester, a finalized-looking lock's id and the
// release the platform decided to relay for it.
func releaseFixture(t testing.TB) (*Attester, ReleaseExpectation, devnet.Message) {
	t.Helper()
	att := NewAttester(seed("attester"))
	holder := devnet.NewKeypairFromSeed(seed("holder"))
	lock := devnet.Message{ChainID: srcChain, RecentBlockhash: bh("src"), FeePayer: holder.PublicKey(),
		Instructions: []devnet.Instruction{devnet.BridgeLock{Mint: pk("src-mint"), Owner: holder.PublicKey(), Amount: 5_000,
			DestChain: dstChain, DestOwner: holder.PublicKey(), Nonce: 7}}}
	tx, err := devnet.Sign(lock, holder)
	require.NoError(t, err)
	lockID, err := tx.ID()
	require.NoError(t, err)
	exp := ReleaseExpectation{Chain: dstChain, Mint: pk("dst-mint"), ToOwner: holder.PublicKey(), Amount: 5_000,
		SourceChain: srcChain, SourceTxID: lockID, Nonce: 7}
	msg := devnet.Message{ChainID: dstChain, RecentBlockhash: bh("dst"), FeePayer: att.PublicKey(),
		Instructions: []devnet.Instruction{exp.release(att.PublicKey())}}
	return att, exp, msg
}

func reasonCodes(t *testing.T, err error) []string {
	t.Helper()
	e, ok := errs.As(err)
	require.True(t, ok, "%v", err)
	require.Equal(t, errs.CodeSigningRejected, e.Code)
	codes, ok := e.Fields["reason_codes"].([]string)
	require.True(t, ok, "reason codes in %v", e.Fields)
	return codes
}

// TestAttester_SignsExactlyTheExpectedRelease: the attester signs the planned
// release, and what it returns decodes to exactly that release, verifiably
// signed by the bridge authority alone.
func TestAttester_SignsExactlyTheExpectedRelease(t *testing.T) {
	att, exp, msg := releaseFixture(t)
	txID, raw, err := att.SignRelease(exp, msg)
	require.NoError(t, err)
	got, err := InspectRelease(att.PublicKey(), exp, raw)
	require.NoError(t, err)
	assert.Equal(t, txID, got)
	tx, err := devnet.Decode(raw)
	require.NoError(t, err)
	require.NoError(t, tx.VerifySignatures())
	assert.Equal(t, []devnet.PublicKey{att.PublicKey()}, tx.Message.RequiredSigners())
	require.Len(t, tx.Message.Instructions, 1)
	assert.Equal(t, exp.release(att.PublicKey()), tx.Message.Instructions[0])
}

// TestInspectRelease_RefusesEveryDeviationFromThePlan: whatever differs from
// the planned release -- chain, fee payer, instruction count, the release's
// mint, recipient, amount, source chain, source transaction, nonce or
// authority, the signer set or a signature -- is refused, with the reason.
func TestInspectRelease_RefusesEveryDeviationFromThePlan(t *testing.T) {
	att, exp, msg := releaseFixture(t)
	other := pk("other")
	withRelease := func(f func(r *devnet.BridgeRelease)) func(m *devnet.Message) {
		return func(m *devnet.Message) {
			r := m.Instructions[0].(devnet.BridgeRelease)
			f(&r)
			m.Instructions = []devnet.Instruction{r}
		}
	}
	cases := []struct {
		name   string
		mutate func(m *devnet.Message)
		want   string
	}{
		{"chain", func(m *devnet.Message) { m.ChainID = srcChain }, InspectChainMismatch},
		{"fee payer", func(m *devnet.Message) { m.FeePayer = other }, InspectFeePayerMismatch},
		{"extra instruction", func(m *devnet.Message) {
			m.Instructions = append(m.Instructions, devnet.Memo{Data: []byte("x")})
		}, InspectInstructionCount},
		{"no instruction", func(m *devnet.Message) { m.Instructions = nil }, InspectMalformed},
		{"mint", withRelease(func(r *devnet.BridgeRelease) { r.Mint = other }), InspectReleaseMismatch},
		{"recipient", withRelease(func(r *devnet.BridgeRelease) { r.ToOwner = other }), InspectReleaseMismatch},
		{"amount", withRelease(func(r *devnet.BridgeRelease) { r.Amount++ }), InspectReleaseMismatch},
		{"source chain", withRelease(func(r *devnet.BridgeRelease) { r.SourceChain = "sbx-other" }), InspectReleaseMismatch},
		{"nonce", withRelease(func(r *devnet.BridgeRelease) { r.Nonce++ }), InspectReleaseMismatch},
		{"authority", withRelease(func(r *devnet.BridgeRelease) { r.Authority = other }), InspectSignerSetMismatch},
		{"another program", func(m *devnet.Message) {
			m.Instructions = []devnet.Instruction{devnet.TokenMintTo{Mint: exp.Mint, ToOwner: exp.ToOwner, Amount: exp.Amount, Authority: att.PublicKey()}}
		}, InspectReleaseMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := msg
			m.Instructions = append([]devnet.Instruction(nil), msg.Instructions...)
			c.mutate(&m)
			_, _, err := att.SignRelease(exp, m)
			require.Error(t, err)
			assert.Contains(t, reasonCodes(t, err), c.want)
		})
	}

	// A different lock (another source transaction) is another release.
	_, other2, _ := releaseFixture(t)
	other2.SourceTxID = devnet.TxID(strings.Repeat("1", len(exp.SourceTxID)))
	_, raw, err := att.SignRelease(exp, msg)
	require.NoError(t, err)
	_, err = InspectRelease(att.PublicKey(), other2, raw)
	require.Error(t, err)

	// A tampered signature, a truncated transaction and a release signed by
	// an impostor are refused by inspection of the bytes alone.
	bad := append([]byte(nil), raw...)
	bad[len(bad)-1] ^= 0x01
	_, err = InspectRelease(att.PublicKey(), exp, bad)
	require.Error(t, err)
	_, err = InspectRelease(att.PublicKey(), exp, raw[:len(raw)/2])
	assert.Equal(t, []string{InspectMalformed}, reasonCodes(t, err))
	impostor := NewAttester(seed("impostor"))
	m := msg
	m.FeePayer = impostor.PublicKey()
	m.Instructions = []devnet.Instruction{exp.release(impostor.PublicKey())}
	_, forged, err := impostor.SignRelease(exp, m)
	require.NoError(t, err, "the impostor signs its own release")
	_, err = InspectRelease(att.PublicKey(), exp, forged)
	require.Error(t, err, "but it is not the bridge authority's")
}

// TestAttester_NeverRevealsItsKey: every formatting verb and every encoding
// shows the public key only.
func TestAttester_NeverRevealsItsKey(t *testing.T) {
	s := seed("attester")
	att := NewAttester(s)
	secrets := []string{hex.EncodeToString(s[:]), fmt.Sprintf("%v", s)}
	want := "Attester(" + att.PublicKey().String() + ")"
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		got := fmt.Sprintf(verb, att)
		assert.Equal(t, want, got, verb)
	}
	b, err := json.Marshal(att)
	require.NoError(t, err)
	for _, out := range []string{string(b), fmt.Sprintf("%v", *att)} {
		for _, secret := range secrets {
			assert.NotContains(t, out, secret)
		}
	}
	// The only fields are the public key and the signing closure.
	rt := reflect.TypeOf(*att)
	require.Equal(t, 2, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		assert.False(t, rt.Field(i).IsExported(), "field %s is unexported", rt.Field(i).Name)
	}
}

// TestPlan_HashCommitsToEveryFieldAndIsTheLockMemo: changing any field of a
// plan changes its hash, and the lock the holder signs carries that hash as
// its MEMO.
func TestPlan_HashCommitsToEveryFieldAndIsTheLockMemo(t *testing.T) {
	plan := BridgePlan{Version: planVersion, LegID: "leg", RouteID: "route", AccountID: "acct", SourceChain: string(srcChain),
		DestinationChain: string(dstChain), SourceMint: pk("src-mint").String(), DestinationMint: pk("dst-mint").String(),
		SourceOwner: pk("holder").String(), DestinationOwner: pk("holder").String(), Amount: "5000", Nonce: "7",
		RecentBlockhash: bh("src").String(), BlockhashHeight: "12", QuoteExpiresAt: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	h, err := plan.Hash()
	require.NoError(t, err)
	v := reflect.ValueOf(&plan).Elem()
	for i := 0; i < v.NumField(); i++ {
		changed := plan
		f := reflect.ValueOf(&changed).Elem().Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString(f.String() + "x")
		default:
			changed.QuoteExpiresAt = plan.QuoteExpiresAt.Add(time.Second)
		}
		h2, err := changed.Hash()
		require.NoError(t, err)
		assert.NotEqual(t, h, h2, "field %s is not committed to", v.Type().Field(i).Name)
	}
	msg, lock, h3, err := plan.LockMessage()
	require.NoError(t, err)
	assert.Equal(t, h, h3)
	require.Len(t, msg.Instructions, 2)
	assert.Equal(t, lock, msg.Instructions[0])
	assert.Equal(t, devnet.Memo{Data: []byte(hex.EncodeToString(h[:]))}, msg.Instructions[1])
	assert.Equal(t, lock.Owner, msg.FeePayer, "the holder pays for its own lock")
	assert.Equal(t, uint64(5000), lock.Amount)
	assert.Equal(t, uint64(7), lock.Nonce)
	zero := plan
	zero.Amount = "0"
	_, _, _, err = zero.LockMessage()
	require.Error(t, err)
}

// FuzzInspectRelease: no input makes inspection panic, and whatever it
// accepts is exactly the planned release, signed by the bridge authority.
func FuzzInspectRelease(f *testing.F) {
	att, exp, msg := releaseFixture(f)
	_, raw, err := att.SignRelease(exp, msg)
	require.NoError(f, err)
	f.Add(raw)
	f.Add(raw[:len(raw)-1])
	f.Add([]byte{})
	f.Add([]byte("ndvt"))
	f.Fuzz(func(t *testing.T, b []byte) {
		id, err := InspectRelease(att.PublicKey(), exp, b)
		if err != nil {
			return
		}
		tx, err := devnet.Decode(b)
		require.NoError(t, err)
		require.NoError(t, tx.VerifySignatures())
		require.Len(t, tx.Message.Instructions, 1)
		require.Equal(t, exp.release(att.PublicKey()), tx.Message.Instructions[0])
		require.Equal(t, exp.Chain, tx.Message.ChainID)
		want, err := tx.ID()
		require.NoError(t, err)
		require.Equal(t, want, id)
	})
}

package stableasset

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/mr-tron/base58"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
)

func seed(label string) [32]byte { return sha256.Sum256([]byte("stableasset-test/seed/" + label)) }

func addr(label string) devnet.PublicKey {
	return devnet.PublicKey(sha256.Sum256([]byte("stableasset-test/addr/" + label)))
}

// testPlan is a valid mint plan for issuer.
func testPlan(issuer *IssuerSigner) MintPlan {
	return MintPlan{
		Version: mintPlanVersion, RequestID: "r", StableAssetID: "s", AccountID: "a", Chain: "sbx-solana",
		Mint: addr("mint").String(), Recipient: addr("holder").String(), Amount: "1000000",
		MintAuthority: issuer.PublicKey().String(), FeePayer: issuer.PublicKey().String(),
		RecentBlockhash: devnet.Hash(sha256.Sum256([]byte("blockhash"))).String(),
		ReserveAssetID:  "x", ReserveQuantity: "1000000", ReserveFactor: "10000",
		ReservePrice: priceDoc(price(1, 0)), Peg: priceDoc(price(1, 0)),
	}
}

func TestIssuerSigner_SignsExactlyThePlannedMint(t *testing.T) {
	issuer := NewIssuerSigner(seed("issuer"))
	plan := testPlan(issuer)
	msg, err := plan.Message()
	require.NoError(t, err)
	signed, err := issuer.SignMint(plan, msg)
	require.NoError(t, err)
	id, err := InspectMintTransaction(plan, signed.Raw)
	require.NoError(t, err)
	assert.Equal(t, signed.TxID, id)

	tx, err := devnet.Decode(signed.Raw)
	require.NoError(t, err)
	assert.Equal(t, []devnet.PublicKey{issuer.PublicKey()}, tx.Message.RequiredSigners(), "the mint authority alone signs")
	assert.Equal(t, issuer.PublicKey(), tx.Message.FeePayer)
	h, _, err := plan.Hash()
	require.NoError(t, err)
	assert.Equal(t, []byte(hex.EncodeToString(h[:])), tx.Message.Instructions[1].(devnet.Memo).Data, "MEMO = plan hash")

	// Signing is deterministic: the same plan and message, the same bytes.
	again, err := issuer.SignMint(plan, msg)
	require.NoError(t, err)
	assert.Equal(t, signed.Raw, again.Raw)
}

// TestInspection_RefusesEveryDeviationFromThePlan: the inspector refuses any
// transaction that is not exactly the plan's, before anything is signed.
func TestInspection_RefusesEveryDeviationFromThePlan(t *testing.T) {
	issuer := NewIssuerSigner(seed("issuer"))
	plan := testPlan(issuer)
	good, err := plan.Message()
	require.NoError(t, err)
	mintTo := good.Instructions[0].(devnet.TokenMintTo)
	other := NewIssuerSigner(seed("other"))
	mutated := func(f func(m *devnet.Message)) devnet.Message {
		m := good
		m.Instructions = append([]devnet.Instruction(nil), good.Instructions...)
		f(&m)
		return m
	}
	withMint := func(f func(i *devnet.TokenMintTo)) devnet.Message {
		return mutated(func(m *devnet.Message) {
			i := mintTo
			f(&i)
			m.Instructions[0] = i
		})
	}
	cases := map[string]struct {
		msg  devnet.Message
		code string
	}{
		"another chain":     {mutated(func(m *devnet.Message) { m.ChainID = "sbx-ethereum" }), InspectChainMismatch},
		"another blockhash": {mutated(func(m *devnet.Message) { m.RecentBlockhash = devnet.Hash{1} }), InspectBlockhashMismatch},
		"another fee payer": {mutated(func(m *devnet.Message) { m.FeePayer = other.PublicKey() }), InspectFeePayerMismatch},
		"another mint":      {withMint(func(i *devnet.TokenMintTo) { i.Mint = addr("evil-mint") }), InspectMintMismatch},
		"another recipient": {withMint(func(i *devnet.TokenMintTo) { i.ToOwner = addr("thief") }), InspectOwnerMismatch},
		"another amount":    {withMint(func(i *devnet.TokenMintTo) { i.Amount++ }), InspectAmountMismatch},
		"another authority": {withMint(func(i *devnet.TokenMintTo) { i.Authority = other.PublicKey() }), InspectAuthorityMismatch},
		"another memo":      {mutated(func(m *devnet.Message) { m.Instructions[1] = devnet.Memo{Data: []byte("not the plan")} }), InspectMemoMismatch},
		"a burn not a mint": {mutated(func(m *devnet.Message) {
			m.Instructions[0] = devnet.TokenBurn{Mint: addr("mint"), Owner: addr("holder"), Amount: 1}
		}), InspectProgramMismatch},
		"an extra instruction": {mutated(func(m *devnet.Message) { m.Instructions = append(m.Instructions, devnet.Memo{Data: []byte("x")}) }), InspectInstructionCount},
		"a second signer": {mutated(func(m *devnet.Message) {
			m.Instructions[1] = devnet.SystemTransfer{From: addr("stranger"), To: addr("holder"), Lamports: 1}
		}), InspectSignerSetMismatch},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := issuer.SignMint(plan, c.msg)
			require.Error(t, err)
			assert.Equal(t, errs.CodeSigningRejected, errs.CodeOf(err))
			e, _ := errs.As(err)
			assert.Contains(t, e.Fields["reason_codes"], c.code)
		})
	}

	// A plan naming another authority is refused before inspection.
	_, err = other.SignMint(plan, good)
	require.Error(t, err)
	e, _ := errs.As(err)
	assert.Equal(t, []string{InspectSignerNotHeld}, e.Fields["reason_codes"])

	// A correctly signed transaction for another plan does not pass as this one.
	plan2 := plan
	plan2.Amount = "2000000"
	msg2, err := plan2.Message()
	require.NoError(t, err)
	signed2, err := issuer.SignMint(plan2, msg2)
	require.NoError(t, err)
	_, err = InspectMintTransaction(plan, signed2.Raw)
	require.Error(t, err)

	// Tampered signatures are caught.
	signed, err := issuer.SignMint(plan, good)
	require.NoError(t, err)
	tampered := append([]byte(nil), signed.Raw...)
	tampered[len(tampered)-1] ^= 0x01
	_, err = InspectMintTransaction(plan, tampered)
	require.Error(t, err)
}

func TestCustodialWalletSigner_SignsOnlyTheHoldersOwnBurnsAndLocks(t *testing.T) {
	custody := NewCustodialWalletSigner(seed("custody"))
	alice, bob := accounts.AccountID(accounts.NewAccountID()), accounts.AccountID(accounts.NewAccountID())
	assert.Equal(t, custody.HolderAddress(alice), custody.HolderAddress(alice), "derivation is stable")
	assert.NotEqual(t, custody.HolderAddress(alice), custody.HolderAddress(bob))
	assert.Equal(t, custody.HolderAddress(alice), NewCustodialWalletSigner(seed("custody")).HolderAddress(alice),
		"a restart with the same master seed recovers every wallet")

	holder := custody.HolderAddress(alice)
	bh := devnet.Hash(sha256.Sum256([]byte("bh")))
	plan := BurnPlan{Version: burnPlanVersion, RequestID: "r", StableAssetID: "s", AccountID: alice.String(), Chain: "sbx-solana",
		Mint: addr("mint").String(), Holder: holder.String(), Amount: "5", FeePayer: holder.String(), RecentBlockhash: bh.String(),
		PayoutAssetID: "p", PayoutQuantity: "5", RedemptionMode: "INSTANT", Peg: priceDoc(price(1, 0))}
	msg, err := plan.Message()
	require.NoError(t, err)
	signed, err := custody.SignHolderBurn(alice, plan, msg)
	require.NoError(t, err)
	_, err = InspectBurnTransaction(plan, signed.Raw)
	require.NoError(t, err)

	_, err = custody.SignHolderBurn(bob, plan, msg)
	require.Error(t, err, "bob's wallet does not sign alice's burn")

	lock := devnet.BridgeLock{Mint: addr("mint"), Owner: holder, Amount: 5, DestChain: "sbx-ethereum", DestOwner: holder, Nonce: 9}
	planHash := sha256.Sum256([]byte("bridge plan"))
	lockMsg := devnet.Message{ChainID: "sbx-solana", RecentBlockhash: bh, FeePayer: holder,
		Instructions: []devnet.Instruction{lock, memoFor(planHash)}}
	id, raw, err := custody.SignHolderLock(alice, planHash, lock, lockMsg)
	require.NoError(t, err)
	assert.NotEmpty(t, id)
	assert.NotEmpty(t, raw)

	stolen := lock
	stolen.DestOwner = addr("thief")
	_, _, err = custody.SignHolderLock(alice, planHash, lock, devnet.Message{ChainID: "sbx-solana", RecentBlockhash: bh, FeePayer: holder,
		Instructions: []devnet.Instruction{stolen, memoFor(planHash)}})
	require.Error(t, err, "a lock to another destination is not the agreed lock")
	_, _, err = custody.SignHolderLock(bob, planHash, lock, lockMsg)
	require.Error(t, err, "bob does not own alice's lock")
}

// TestSigners_NeverRevealTheirKeys: no formatting verb, on a value or a
// pointer, prints a private key or a seed, and no signer has a field holding
// one.
func TestSigners_NeverRevealTheirKeys(t *testing.T) {
	issuerSeed, custodySeed, snapSeed := seed("issuer-k"), seed("custody-k"), seed("snapshot-k")
	issuer := NewIssuerSigner(issuerSeed)
	custody := NewCustodialWalletSigner(custodySeed)
	snaps := NewSnapshotSigner(snapSeed)
	account := accounts.AccountID(accounts.NewAccountID())
	// The per-account seed the custodian derives (and the key it expands to)
	// must not leak either.
	mac := hmac.New(sha256.New, custodySeed[:])
	_, _ = mac.Write([]byte(custodialDomain + account.String()))
	walletSeed := mac.Sum(nil)
	_ = custody.HolderAddress(account)

	secrets := [][]byte{issuerSeed[:], custodySeed[:], snapSeed[:], walletSeed,
		ed25519.NewKeyFromSeed(issuerSeed[:]), ed25519.NewKeyFromSeed(snapSeed[:]), ed25519.NewKeyFromSeed(walletSeed)}
	var forms []string
	for _, s := range secrets {
		forms = append(forms, hex.EncodeToString(s), strings.ToUpper(hex.EncodeToString(s)), base58.Encode(s), string(s),
			fmt.Sprintf("%v", s), fmt.Sprintf("%d", s))
	}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%p", "%T", "%w"}
	for _, subject := range []any{issuer, *issuer, custody, *custody, snaps, *snaps} {
		for _, verb := range verbs {
			out := fmt.Sprintf(verb, subject)
			for _, f := range forms {
				require.NotContains(t, out, f, "verb %s on %T printed a secret", verb, subject)
			}
		}
	}
	assert.Contains(t, fmt.Sprint(issuer), issuer.PublicKey().String(), "a signer formats as its public key")
	assert.NotEqual(t, custody.HolderAddress(account), devnet.PublicKey{}, "derivation works")
}

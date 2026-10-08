package delegation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
)

func plannedSwap(t *testing.T, e SwapExpectation) devnet.Message {
	t.Helper()
	m, err := e.Plan.Message(e.Blockhash)
	require.NoError(t, err)
	return m
}

func TestTheInspectorAcceptsExactlyThePlannedSwap(t *testing.T) {
	e := unitSwapExpectation(t)
	assert.Empty(t, InspectSwapMessage(e, plannedSwap(t, e)))
}

// Every field the goal names -- destination/program, instructions, amount,
// token accounts, slippage, fee payer, signer set, plan hash -- is checked on
// its own and refused with its own code.
func TestTheInspectorRefusesEveryModifiedSwap(t *testing.T) {
	e := unitSwapExpectation(t)
	cases := []struct {
		name   string
		mutate func(*devnet.Message)
		code   string
	}{
		{"another chain", func(m *devnet.Message) { m.ChainID = devnethost.ChainEthereum }, InspectChainMismatch},
		{"another blockhash", func(m *devnet.Message) { m.RecentBlockhash = uhash("old") }, InspectBlockhashMismatch},
		{"another fee payer", func(m *devnet.Message) { m.FeePayer = ukey("venue") }, InspectFeePayerMismatch},
		{"an extra instruction", func(m *devnet.Message) {
			m.Instructions = append(m.Instructions, devnet.SystemTransfer{From: ukey("wallet"), To: ukey("thief"), Lamports: 1})
		}, InspectInstructionCount},
		{"a different program", func(m *devnet.Message) {
			m.Instructions[0] = devnet.TokenBurn{Mint: ukey("mint-usd"), Owner: ukey("wallet"), Amount: 2_000_000}
		}, InspectProgramMismatch},
		{"payment to another address", func(m *devnet.Message) {
			p := m.Instructions[0].(devnet.TokenTransfer)
			p.ToOwner = ukey("thief")
			m.Instructions[0] = p
		}, InspectDestinationMismatch},
		{"delivery from another address", func(m *devnet.Message) {
			r := m.Instructions[1].(devnet.TokenTransfer)
			r.FromOwner = ukey("thief")
			m.Instructions[1] = r
		}, InspectDestinationMismatch},
		{"another pay mint", func(m *devnet.Message) {
			p := m.Instructions[0].(devnet.TokenTransfer)
			p.Mint = ukey("other-mint")
			m.Instructions[0] = p
		}, InspectTokenAccountMismatch},
		{"delivery to another wallet", func(m *devnet.Message) {
			r := m.Instructions[1].(devnet.TokenTransfer)
			r.ToOwner = ukey("thief")
			m.Instructions[1] = r
		}, InspectTokenAccountMismatch},
		{"a larger payment", func(m *devnet.Message) {
			p := m.Instructions[0].(devnet.TokenTransfer)
			p.Amount++
			m.Instructions[0] = p
		}, InspectAmountMismatch},
		{"a delivery below the slippage bound", func(m *devnet.Message) {
			r := m.Instructions[1].(devnet.TokenTransfer)
			r.Amount = 19_799_999
			m.Instructions[1] = r
		}, InspectSlippageExceeded},
		{"a delivery inside the bound but not the quote", func(m *devnet.Message) {
			r := m.Instructions[1].(devnet.TokenTransfer)
			r.Amount = 19_850_000
			m.Instructions[1] = r
		}, InspectAmountMismatch},
		{"another memo", func(m *devnet.Message) { m.Instructions[2] = devnet.Memo{Data: []byte("other plan")} }, InspectMemoMismatch},
		{"a third signer", func(m *devnet.Message) {
			p := m.Instructions[0].(devnet.TokenTransfer)
			p.FromOwner = ukey("someone")
			m.Instructions[0] = p
		}, InspectSignerSetMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := plannedSwap(t, e)
			m.Instructions = append([]devnet.Instruction(nil), m.Instructions...)
			tc.mutate(&m)
			codes := InspectSwapMessage(e, m)
			require.NotEmpty(t, codes)
			assert.Contains(t, codes, tc.code)
		})
	}
}

func TestTheInspectorRefusesAPlanThatIsNotTheStoredOne(t *testing.T) {
	e := unitSwapExpectation(t)
	m := plannedSwap(t, e)
	tampered := e
	tampered.Plan.AmountIn = "2000001"
	codes := InspectSwapMessage(tampered, m)
	assert.Contains(t, codes, InspectPlanHashMismatch)

	// A destination the capability does not allow is refused even when the
	// transaction is exactly the plan.
	outside := e
	outside.AllowedDestinations = []devnet.PublicKey{ukey("elsewhere")}
	assert.Contains(t, InspectSwapMessage(outside, m), InspectDestinationMismatch)

	// The wallet that pays must be the provider's registered key.
	wrongKey := e
	wrongKey.Wallet = ukey("other-wallet")
	assert.Contains(t, InspectSwapMessage(wrongKey, m), InspectFeePayerMismatch)

	// The planned fee must be the chain's configured fee and within the
	// signing policy's bound.
	feeChanged := e
	feeChanged.ChainFee = 11
	assert.Contains(t, InspectSwapMessage(feeChanged, m), InspectFeeMismatch)
	overPolicy := e
	overPolicy.MaxFee = 9
	assert.Contains(t, InspectSwapMessage(overPolicy, m), InspectFeeMismatch)
	w := unitWithdrawalExpectation(t)
	wm, err := w.Plan.Message(w.Blockhash)
	require.NoError(t, err)
	w.ChainFee = 11
	assert.Contains(t, InspectWithdrawalMessage(w, wm), InspectFeeMismatch)
}

func TestTheInspectorChecksSignaturesOverTheExactBytes(t *testing.T) {
	e := unitSwapExpectation(t)
	m := plannedSwap(t, e)
	wallet := devnet.NewKeypairFromSeed(useed("wallet"))
	venue := devnet.NewKeypairFromSeed(useed("venue"))
	tx, err := devnet.Sign(m, wallet, venue)
	require.NoError(t, err)
	raw, err := tx.Encode()
	require.NoError(t, err)
	txID, err := InspectSwapTransaction(e, raw)
	require.NoError(t, err)
	assert.Equal(t, tx.Signatures[0].String(), string(txID))

	// A valid signature by the wrong key is refused.
	impostor := devnet.NewKeypairFromSeed(useed("impostor"))
	bytesToSign, err := m.Bytes()
	require.NoError(t, err)
	forged, err := impostor.Sign(bytesToSign)
	require.NoError(t, err)
	bad := devnet.Transaction{Message: m, Signatures: []devnet.Signature{forged, tx.Signatures[1]}}
	badRaw, err := bad.Encode()
	require.NoError(t, err)
	_, err = InspectSwapTransaction(e, badRaw)
	require.Error(t, err)
	assert.Equal(t, errs.CodeSigningRejected, errs.CodeOf(err))

	_, err = InspectSwapTransaction(e, raw[:len(raw)-1])
	require.Error(t, err, "truncated bytes are malformed")
}

func TestTheInspectorAcceptsExactlyThePlannedWithdrawal(t *testing.T) {
	e := unitWithdrawalExpectation(t)
	m, err := e.Plan.Message(e.Blockhash)
	require.NoError(t, err)
	assert.Empty(t, InspectWithdrawalMessage(e, m))

	// A raw address substitution -- even to another destination the agent's
	// capability allows -- is refused.
	sub := m
	sub.Instructions = append([]devnet.Instruction(nil), m.Instructions...)
	p := sub.Instructions[0].(devnet.TokenTransfer)
	p.ToOwner = ukey("venue")
	sub.Instructions[0] = p
	allowsBoth := e
	allowsBoth.AllowedDestinations = []devnet.PublicKey{ukey("dest"), ukey("venue")}
	assert.Contains(t, InspectWithdrawalMessage(allowsBoth, sub), InspectDestinationMismatch)

	// An amount mutation is refused.
	more := m
	more.Instructions = append([]devnet.Instruction(nil), m.Instructions...)
	q := more.Instructions[0].(devnet.TokenTransfer)
	q.Amount = 5_000_001
	more.Instructions[0] = q
	assert.Contains(t, InspectWithdrawalMessage(e, more), InspectAmountMismatch)

	// A destination removed from the customer's allowlist is refused even
	// when the plan names it.
	removed := e
	removed.CustomerDestination = devnet.PublicKey{}
	assert.Contains(t, InspectWithdrawalMessage(removed, m), InspectDestinationMismatch)
}

func TestMessageBytesRoundTripThroughTheWireFrame(t *testing.T) {
	e := unitSwapExpectation(t)
	m := plannedSwap(t, e)
	b, err := m.Bytes()
	require.NoError(t, err)
	back, err := decodeMessage(b)
	require.NoError(t, err)
	assert.True(t, sameMessage(m, back))
	_, err = decodeMessage(append(append([]byte(nil), b...), 0))
	assert.Error(t, err, "trailing bytes are refused")
	_, err = decodeMessage(nil)
	assert.Error(t, err)
}

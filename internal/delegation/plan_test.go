package delegation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func TestComputeQuoteIsExact(t *testing.T) {
	// 2.000000 of a par stable (6 dp) for a token at 100.00 (9 dp): the mark
	// is 0.02 token = 20_000_000 base units; a 50 bp spread pays 19_900_000;
	// a 100 bp slippage bound accepts no less than 19_800_000; the notional is
	// $2.00.
	q, err := ComputeQuote(QuoteInput{AmountIn: money.QuantityFromInt64(2_000_000), PayDecimals: 6, ReceiveDecimals: 9,
		PayPrice: price(1, 0), ReceivePrice: price(10_000, 2), SpreadBPS: 50, MaxSlippageBPS: 100})
	require.NoError(t, err)
	assert.Equal(t, "20000000", q.MarkOut.String())
	assert.Equal(t, "19900000", q.AmountOut.String())
	assert.Equal(t, "19800000", q.MinAmountOut.String())
	assert.Equal(t, int64(200), q.NotionalUSD.Minor())
}

func TestComputeQuoteRoundsAgainstTheAgentNeverForIt(t *testing.T) {
	// 1 base unit of a 6-dp token at 0.333333 (6 dp quote) for a par stable
	// (6 dp): the exact mark is 0.333333 units: AmountOut rounds down, the
	// bound rounds up, the notional rounds up.
	_, err := ComputeQuote(QuoteInput{AmountIn: money.QuantityFromInt64(3), PayDecimals: 6, ReceiveDecimals: 6,
		PayPrice: price(333_333, 6), ReceivePrice: price(1, 0), SpreadBPS: 0, MaxSlippageBPS: 0})
	// 3 base units x 0.333333 is 0.999999 of a receive base unit: too small.
	require.Error(t, err)
	assert.Equal(t, ReasonAmountTooSmall, reasonOf(err))

	q, err := ComputeQuote(QuoteInput{AmountIn: money.QuantityFromInt64(3_000_000), PayDecimals: 6, ReceiveDecimals: 6,
		PayPrice: price(333_333, 6), ReceivePrice: price(1, 0), SpreadBPS: 0, MaxSlippageBPS: 0})
	require.NoError(t, err)
	assert.Equal(t, "999999", q.AmountOut.String())
	assert.Equal(t, "999999", q.MinAmountOut.String())
	assert.Equal(t, int64(100), q.NotionalUSD.Minor(), "0.999999 rounds up to a cent for limits")
}

func TestComputeQuoteRefusesASpreadWiderThanTheSlippageBound(t *testing.T) {
	_, err := ComputeQuote(QuoteInput{AmountIn: money.QuantityFromInt64(2_000_000), PayDecimals: 6, ReceiveDecimals: 9,
		PayPrice: price(1, 0), ReceivePrice: price(100, 0), SpreadBPS: 120, MaxSlippageBPS: 100})
	require.Error(t, err)
	assert.Equal(t, errs.CodeNoValidPlan, errs.CodeOf(err))
	assert.Equal(t, ReasonSlippageExceeded, reasonOf(err))
}

func TestComputeQuoteFailsClosedOnBadInputs(t *testing.T) {
	good := QuoteInput{AmountIn: money.QuantityFromInt64(1_000_000), PayDecimals: 6, ReceiveDecimals: 6,
		PayPrice: price(1, 0), ReceivePrice: price(2, 0), SpreadBPS: 10, MaxSlippageBPS: 100}
	_, err := ComputeQuote(good)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*QuoteInput){
		"zero amount":     func(in *QuoteInput) { in.AmountIn = money.QuantityFromInt64(0) },
		"zero price":      func(in *QuoteInput) { in.ReceivePrice = price(0, 0) },
		"other quote":     func(in *QuoteInput) { in.ReceivePrice.QuoteAsset = "another" },
		"spread > 100%":   func(in *QuoteInput) { in.SpreadBPS = 10_001 },
		"negative slip":   func(in *QuoteInput) { in.MaxSlippageBPS = -1 },
		"no pay price":    func(in *QuoteInput) { in.PayPrice = money.Price{} },
		"negative spread": func(in *QuoteInput) { in.SpreadBPS = -1 },
	} {
		in := good
		mutate(&in)
		_, err := ComputeQuote(in)
		assert.Error(t, err, name)
	}
}

func TestASwapPlanCompilesToTheDeliveryVersusPaymentTransaction(t *testing.T) {
	plan, h := unitSwapPlan(t)
	m, err := plan.Message(uhash("tip"))
	require.NoError(t, err)
	require.Len(t, m.Instructions, 3)
	pay := m.Instructions[0].(devnet.TokenTransfer)
	recv := m.Instructions[1].(devnet.TokenTransfer)
	memo := m.Instructions[2].(devnet.Memo)
	assert.Equal(t, ukey("wallet"), m.FeePayer)
	assert.Equal(t, devnet.TokenTransfer{Mint: ukey("mint-usd"), FromOwner: ukey("wallet"), ToOwner: ukey("venue"), Amount: 2_000_000}, pay)
	assert.Equal(t, devnet.TokenTransfer{Mint: ukey("mint-tok"), FromOwner: ukey("venue"), ToOwner: ukey("wallet"), Amount: 19_900_000}, recv)
	assert.Equal(t, memoOf(h), memo.Data)
	assert.Equal(t, []devnet.PublicKey{ukey("wallet"), ukey("venue")}, m.RequiredSigners())
	// Deterministic: the same plan and blockhash compile to the same bytes.
	again, err := plan.Message(uhash("tip"))
	require.NoError(t, err)
	assert.True(t, sameMessage(m, again))
}

func TestAWithdrawalPlanCompilesToOneTransferAndItsMemo(t *testing.T) {
	plan, h := unitWithdrawalPlan(t)
	m, err := plan.Message(uhash("tip"))
	require.NoError(t, err)
	require.Len(t, m.Instructions, 2)
	assert.Equal(t, devnet.TokenTransfer{Mint: ukey("mint-usd"), FromOwner: ukey("wallet"), ToOwner: ukey("dest"), Amount: 5_000_000},
		m.Instructions[0].(devnet.TokenTransfer))
	assert.Equal(t, memoOf(h), m.Instructions[1].(devnet.Memo).Data)
	assert.Equal(t, []devnet.PublicKey{ukey("wallet")}, m.RequiredSigners())
}

func TestAPlanHashCoversTheDestinationAndTheAmount(t *testing.T) {
	plan, h := unitWithdrawalPlan(t)
	moved := plan
	moved.DestinationAddress = ukey("attacker").String()
	h2, err := moved.Hash()
	require.NoError(t, err)
	assert.NotEqual(t, h, h2)
	bigger := plan
	bigger.Amount = "5000001"
	h3, err := bigger.Hash()
	require.NoError(t, err)
	assert.NotEqual(t, h, h3)

	sp, sh := unitSwapPlan(t)
	for _, mutate := range []func(*SwapPlan){
		func(p *SwapPlan) { p.VenueAddress = ukey("other").String() },
		func(p *SwapPlan) { p.AmountIn = "2000001" },
		func(p *SwapPlan) { p.MinAmountOut = "1" },
		func(p *SwapPlan) { p.Wallet = ukey("other").String() },
	} {
		p := sp
		mutate(&p)
		h4, err := p.Hash()
		require.NoError(t, err)
		assert.NotEqual(t, sh, h4)
	}
}

func TestAPlanThatDoesNotParseDoesNotCompile(t *testing.T) {
	for name, mutate := range map[string]func(*SwapPlan){
		"version":       func(p *SwapPlan) { p.Version = "v0" },
		"wallet":        func(p *SwapPlan) { p.Wallet = "nope" },
		"venue is self": func(p *SwapPlan) { p.VenueAddress = p.Wallet },
		"same mint":     func(p *SwapPlan) { p.ReceiveMint = p.PayMint },
		"zero in":       func(p *SwapPlan) { p.AmountIn = "0" },
		"min above out": func(p *SwapPlan) { p.MinAmountOut = "19900001" },
		"slippage":      func(p *SwapPlan) { p.MaxSlippageBPS = "10001" },
		"decimals":      func(p *SwapPlan) { p.PayDecimals = "19" },
		"chain":         func(p *SwapPlan) { p.Chain = "SBX SOLANA" },
	} {
		p, _ := unitSwapPlan(t)
		mutate(&p)
		_, err := p.Message(uhash("tip"))
		assert.Error(t, err, name)
	}
}

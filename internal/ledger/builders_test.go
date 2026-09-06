package ledger

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Base units: USDC has 6 decimals, SOL has 9.
const (
	usdc100   = 100_000_000
	usdc0_10  = 100_000
	sol0_5    = 500_000_000
	sol5Micro = 5_000 // 0.000005 SOL
)

func usd(minor int64) *money.USD {
	u := money.USDFromMinor(minor)
	return &u
}

type entryKey struct {
	ref  string
	side Side
}

func indexEntries(entries []Entry) map[entryKey]Entry {
	m := make(map[entryKey]Entry, len(entries))
	for _, e := range entries {
		m[entryKey{e.Account.key(), e.Side}] = e
	}
	return m
}

// TestSwapPosting_CanonicalExample is the swap of FINANCIAL_MODEL §2.2:
// 100 USDC -> 0.5 SOL, network fee 0.000005 SOL, platform fee 0.10 USDC.
func TestSwapPosting_CanonicalExample(t *testing.T) {
	t.Parallel()
	p, err := SwapPosting(SwapInputs{
		AccountID:           testAccount,
		FillID:              "fill-1",
		OutAsset:            testUSDC,
		OutQuantity:         q(usdc100),
		InAsset:             testSOL,
		InQuantity:          q(sol0_5),
		NetworkFeeAsset:     testSOL,
		NetworkFeeQuantity:  q(sol5Micro),
		PlatformFeeAsset:    testUSDC,
		PlatformFeeQuantity: q(usdc0_10),
		OutUSD:              usd(10000),
		InUSD:               usd(10000),
		NetworkFeeUSD:       usd(0),
		PlatformFeeUSD:      usd(10),
		OutPriceRef:         "px-usdc",
		InPriceRef:          "px-sol",
		EffectiveAt:         testEffective,
		CorrelationID:       "corr-1",
	})
	require.NoError(t, err)
	require.NoError(t, p.Validate())
	assert.Equal(t, KindTradeFill, p.Kind)
	assert.Equal(t, "fill:fill-1", p.IdempotencyKey)
	assert.Equal(t, FinancialEventReference{Type: "fill", ID: "fill-1"}, p.Reference)
	assert.Equal(t, "corr-1", p.CorrelationID)
	require.Len(t, p.Entries, 9, "Cr WALLET:USDC is one merged entry")

	got := indexEntries(p.Entries)
	want := []struct {
		ref  AccountRef
		side Side
		qty  int64
		usd  *int64
	}{
		{cust(CodeWallet, testUSDC), Credit, usdc100 + usdc0_10, ptr(int64(10010))},
		{cust(CodeTradingOutflow, testUSDC), Debit, usdc100, ptr(int64(10000))},
		{cust(CodeFeesPlatform, testUSDC), Debit, usdc0_10, ptr(int64(10))},
		{PlatformAccount(CodePlatformFeeReceivable, testUSDC), Debit, usdc0_10, ptr(int64(10))},
		{PlatformAccount(CodePlatformFeeRevenue, testUSDC), Credit, usdc0_10, ptr(int64(10))},
		{cust(CodeWallet, testSOL), Debit, sol0_5, ptr(int64(10000))},
		{cust(CodeTradingInflow, testSOL), Credit, sol0_5, ptr(int64(10000))},
		{cust(CodeWallet, testSOL), Credit, sol5Micro, ptr(int64(0))},
		{cust(CodeFeesNetwork, testSOL), Debit, sol5Micro, ptr(int64(0))},
	}
	for _, w := range want {
		e, ok := got[entryKey{w.ref.key(), w.side}]
		require.Truef(t, ok, "missing %s %s", w.side, w.ref)
		assert.Equalf(t, q(w.qty).String(), e.Quantity.String(), "%s %s quantity", w.side, w.ref)
		require.NotNil(t, e.USDValueMinor, "%s %s usd", w.side, w.ref)
		assert.Equalf(t, *w.usd, *e.USDValueMinor, "%s %s usd", w.side, w.ref)
	}
	// Per-asset totals of the example: USDC 100.20 / 100.20, SOL 0.500005 / 0.500005.
	for _, im := range assetImbalances(p.Entries) {
		assert.True(t, im.net.IsZero(), "asset %s net %s", im.asset, im.net)
		switch im.asset {
		case testUSDC:
			assert.Equal(t, "100200000", im.debits.String())
		case testSOL:
			assert.Equal(t, "500005000", im.debits.String())
		}
	}
	walletUSDC := got[entryKey{cust(CodeWallet, testUSDC).key(), Credit}]
	require.NotNil(t, walletUSDC.PriceRef)
	assert.Equal(t, "px-usdc", *walletUSDC.PriceRef, "merged entry keeps the first price ref")
	walletSOL := got[entryKey{cust(CodeWallet, testSOL).key(), Debit}]
	require.NotNil(t, walletSOL.PriceRef)
	assert.Equal(t, "px-sol", *walletSOL.PriceRef)
}

func ptr[T any](v T) *T { return &v }

func TestSwapPosting_NoFeesAndPartialUSD(t *testing.T) {
	t.Parallel()
	p, err := SwapPosting(SwapInputs{
		AccountID: testAccount, FillID: "f", OutAsset: testUSDC, OutQuantity: q(10), InAsset: testSOL, InQuantity: q(3),
		PlatformFeeAsset: testUSDC, PlatformFeeQuantity: q(1), OutUSD: usd(1000), EffectiveAt: testEffective,
	})
	require.NoError(t, err)
	require.Len(t, p.Entries, 7, "4 trade legs + 4 platform-fee legs, with the two Cr WALLET:USDC legs merged")
	got := indexEntries(p.Entries)
	merged := got[entryKey{cust(CodeWallet, testUSDC).key(), Credit}]
	assert.Equal(t, "11", merged.Quantity.String())
	assert.Nil(t, merged.USDValueMinor, "USD is dropped when a merged leg has no valuation")

	p, err = SwapPosting(SwapInputs{AccountID: testAccount, FillID: "f", OutAsset: testUSDC, OutQuantity: q(10), InAsset: testSOL, InQuantity: q(3), EffectiveAt: testEffective})
	require.NoError(t, err)
	assert.Len(t, p.Entries, 4)
	assert.Equal(t, "swap fill", p.Description)
}

func TestSwapPosting_Rejects(t *testing.T) {
	t.Parallel()
	ok := SwapInputs{AccountID: testAccount, FillID: "f", OutAsset: testUSDC, OutQuantity: q(10), InAsset: testSOL, InQuantity: q(3), EffectiveAt: testEffective}
	cases := map[string]func(in *SwapInputs){
		"zero account":         func(in *SwapInputs) { in.AccountID = accounts.AccountID{} },
		"missing fill":         func(in *SwapInputs) { in.FillID = "" },
		"same asset":           func(in *SwapInputs) { in.InAsset = in.OutAsset },
		"zero out":             func(in *SwapInputs) { in.OutQuantity = q(0) },
		"negative in":          func(in *SwapInputs) { in.InQuantity = q(-1) },
		"zero in asset":        func(in *SwapInputs) { in.InAsset = assets.AssetID{} },
		"negative fee":         func(in *SwapInputs) { in.NetworkFeeAsset = testSOL; in.NetworkFeeQuantity = q(-1) },
		"fee without asset":    func(in *SwapInputs) { in.PlatformFeeQuantity = q(1) },
		"missing effective_at": func(in *SwapInputs) { in.EffectiveAt = time.Time{} },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := ok
			f(&in)
			_, err := SwapPosting(in)
			requireCode(t, err, errs.CodeValidationFailed)
		})
	}
}

func TestFundingSettledPosting(t *testing.T) {
	t.Parallel()
	p, err := FundingSettledPosting(FundingInputs{AccountID: testAccount, DepositID: "dep-1", AssetID: testUSDC, Quantity: q(usdc100), USD: usd(10000), EffectiveAt: testEffective, CorrelationID: "c"})
	require.NoError(t, err)
	assert.Equal(t, KindFundingSettled, p.Kind)
	assert.Equal(t, "deposit:dep-1:settled", p.IdempotencyKey)
	assert.Equal(t, FinancialEventReference{Type: "deposit", ID: "dep-1"}, p.Reference)
	require.Len(t, p.Entries, 2)
	assert.Equal(t, cust(CodeWallet, testUSDC), p.Entries[0].Account)
	assert.Equal(t, Debit, p.Entries[0].Side)
	assert.Equal(t, cust(CodeCapital, testUSDC), p.Entries[1].Account)
	assert.Equal(t, Credit, p.Entries[1].Side)
	for _, e := range p.Entries {
		assert.Equal(t, "100000000", e.Quantity.String())
		require.NotNil(t, e.USDValueMinor)
		assert.Equal(t, int64(10000), *e.USDValueMinor)
	}
	net := applyPostings(p)
	assert.Equal(t, "100000000", balanceOf(net, cust(CodeWallet, testUSDC)).String())
	assert.Equal(t, "100000000", balanceOf(net, cust(CodeCapital, testUSDC)).String())

	_, err = FundingSettledPosting(FundingInputs{AccountID: testAccount, DepositID: "dep-1", AssetID: testUSDC, Quantity: q(0), EffectiveAt: testEffective})
	requireCode(t, err, errs.CodeValidationFailed)
	_, err = FundingSettledPosting(FundingInputs{DepositID: "dep-1", AssetID: testUSDC, Quantity: q(1), EffectiveAt: testEffective})
	requireCode(t, err, errs.CodeValidationFailed)
	_, err = FundingSettledPosting(FundingInputs{AccountID: testAccount, AssetID: testUSDC, Quantity: q(1), EffectiveAt: testEffective})
	requireCode(t, err, errs.CodeValidationFailed)
}

// TestFundingReversalPostings_CanonicalExample is the deficit path of §2.2:
// reversing 100 USDC when the customer holds 30 USDC.
func TestFundingReversalPostings_CanonicalExample(t *testing.T) {
	t.Parallel()
	in := FundingReversalInputs{AccountID: testAccount, DepositID: "dep-1", AssetID: testUSDC, USD: usd(10000), EffectiveAt: testEffective, Reason: "chargeback"}
	wallet, capital, deficit := cust(CodeWallet, testUSDC), cust(CodeCapital, testUSDC), cust(CodeDeficit, testUSDC)

	ps, err := FundingReversalPostings(in, q(30_000_000), q(usdc100))
	require.NoError(t, err)
	require.Len(t, ps, 2)

	t1, t2 := ps[0], ps[1]
	assert.Equal(t, KindFundingReversal, t1.Kind)
	assert.Equal(t, "deposit:dep-1:reversal", t1.IdempotencyKey)
	require.Len(t, t1.Entries, 2)
	assert.Equal(t, Entry{Account: capital, Side: Debit, Quantity: q(30_000_000), USDValueMinor: ptr(int64(3000))}, t1.Entries[0])
	assert.Equal(t, Entry{Account: wallet, Side: Credit, Quantity: q(30_000_000), USDValueMinor: ptr(int64(3000))}, t1.Entries[1])

	assert.Equal(t, KindFundingReversalDeficit, t2.Kind)
	assert.Equal(t, "deposit:dep-1:reversal_deficit", t2.IdempotencyKey)
	require.Len(t, t2.Entries, 2)
	assert.Equal(t, Entry{Account: capital, Side: Debit, Quantity: q(70_000_000), USDValueMinor: ptr(int64(7000))}, t2.Entries[0])
	assert.Equal(t, Entry{Account: deficit, Side: Credit, Quantity: q(70_000_000), USDValueMinor: ptr(int64(7000))}, t2.Entries[1])
	assert.Equal(t, "chargeback", t2.Metadata["reason"])
	assert.Equal(t, t1.Reference, t2.Reference)

	// Net effect of §2.2: WALLET −30 (to 0), CAPITAL −100, DEFICIT +70.
	net := applyPostings(ps...)
	assert.Equal(t, "-30000000", balanceOf(net, wallet).String())
	assert.Equal(t, "-100000000", balanceOf(net, capital).String())
	assert.Equal(t, "70000000", balanceOf(net, deficit).String())
}

func TestFundingReversalPostings_Edges(t *testing.T) {
	t.Parallel()
	in := FundingReversalInputs{AccountID: testAccount, DepositID: "dep-1", AssetID: testUSDC, EffectiveAt: testEffective}

	full, err := FundingReversalPostings(in, q(usdc100), q(usdc100))
	require.NoError(t, err)
	require.Len(t, full, 1)
	assert.Equal(t, KindFundingReversal, full[0].Kind)
	assert.Nil(t, full[0].Metadata)
	assert.Nil(t, full[0].Entries[0].USDValueMinor, "no USD when none supplied")

	more, err := FundingReversalPostings(in, q(usdc100*3), q(usdc100))
	require.NoError(t, err)
	require.Len(t, more, 1)
	assert.Equal(t, "100000000", more[0].Entries[0].Quantity.String(), "never reverses more than amount")

	empty, err := FundingReversalPostings(in, q(0), q(usdc100))
	require.NoError(t, err)
	require.Len(t, empty, 1)
	assert.Equal(t, KindFundingReversalDeficit, empty[0].Kind)
	assert.Equal(t, "100000000", empty[0].Entries[1].Quantity.String())

	// USD allocation is exact: 33 minor over 1/3 covered splits 11 / 22.
	in.USD = usd(33)
	third, err := FundingReversalPostings(in, q(1), q(3))
	require.NoError(t, err)
	require.Len(t, third, 2)
	assert.Equal(t, int64(11), *third[0].Entries[0].USDValueMinor)
	assert.Equal(t, int64(22), *third[1].Entries[0].USDValueMinor)

	_, err = FundingReversalPostings(in, q(-1), q(usdc100))
	requireCode(t, err, errs.CodeValidationFailed)
	_, err = FundingReversalPostings(in, q(10), q(0))
	requireCode(t, err, errs.CodeValidationFailed)
	_, err = FundingReversalPostings(FundingReversalInputs{AccountID: testAccount, AssetID: testUSDC, EffectiveAt: testEffective}, q(10), q(1))
	requireCode(t, err, errs.CodeValidationFailed)
}

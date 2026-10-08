package margin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
)

func testFacilities() (econ.Facility, econ.Facility) {
	return econ.Facility{ID: econ.NewFacilityID(), Kind: ledger.FacilityLendingPool},
		econ.Facility{ID: econ.NewFacilityID(), Kind: ledger.FacilityInsuranceFund}
}

func testMeta() PostingMeta {
	return PostingMeta{Key: "k", RefType: "margin_test", RefID: "r", At: t0}
}

// TestProp_EveryTemplateBalancesPerPartyAndIsSimulatedOnly: for any
// positive amounts, every posting this package builds passes the ledger's
// own validation -- balanced per asset and, because every kind is
// simulated-only, per party.
func TestProp_EveryTemplateBalancesPerPartyAndIsSimulatedOnly(t *testing.T) {
	acct := accounts.AccountID(econ.NewIntentID())
	quote, base := assets.AssetID(econ.NewIntentID()), assets.AssetID(econ.NewIntentID())
	pool, fund := testFacilities()
	rapid.Check(t, func(rt *rapid.T) {
		q := money.QuantityFromInt64(rapid.Int64Range(1, 1<<50).Draw(rt, "q"))
		proceeds := money.QuantityFromInt64(rapid.Int64Range(0, 1<<50).Draw(rt, "proceeds"))
		fee, err := proceeds.MulDiv(money.QuantityFromInt64(rapid.Int64Range(0, 10_000).Draw(rt, "feeShare")), money.QuantityFromInt64(10_000), money.RoundFloor)
		if err != nil {
			rt.Fatal(err)
		}
		builders := map[string]func() (ledger.Posting, error){
			"post":     func() (ledger.Posting, error) { return CollateralMovePosting(testMeta(), acct, base, q, MovePost) },
			"release":  func() (ledger.Posting, error) { return CollateralMovePosting(testMeta(), acct, base, q, MoveRelease) },
			"borrow":   func() (ledger.Posting, error) { return BorrowPosting(testMeta(), acct, pool, quote, q) },
			"repay":    func() (ledger.Posting, error) { return RepayPosting(testMeta(), acct, pool, quote, q) },
			"interest": func() (ledger.Posting, error) { return InterestPosting(testMeta(), acct, pool, quote, q) },
			"trade":    func() (ledger.Posting, error) { return SpotBuyPosting(testMeta(), acct, quote, q, base, q.Add(q)) },
			"liquidation": func() (ledger.Posting, error) {
				return LiquidationPosting(testMeta(), acct, base, q, quote, proceeds, fee)
			},
			"deficit":           func() (ledger.Posting, error) { return DeficitCoveragePosting(testMeta(), acct, pool, fund, quote, q) },
			"deficit repayment": func() (ledger.Posting, error) { return DeficitRepaymentPosting(testMeta(), acct, fund, quote, q) },
			"pool funding":      func() (ledger.Posting, error) { return FacilityFundingPosting(testMeta(), pool, quote, q) },
			"fund funding":      func() (ledger.Posting, error) { return FacilityFundingPosting(testMeta(), fund, quote, q) },
		}
		for name, build := range builders {
			p, err := build()
			if err != nil {
				rt.Fatalf("%s: %v", name, err)
			}
			if !p.Kind.IsSimulatedOnly() {
				rt.Fatalf("%s uses %s, which is not simulated-only", name, p.Kind)
			}
			if err := p.Validate(); err != nil {
				rt.Fatalf("%s does not balance: %v", name, err)
			}
		}
	})
}

func TestPostings_TemplatesMatchTheModel(t *testing.T) {
	acct := accounts.AccountID(econ.NewIntentID())
	quote, base := assets.AssetID(econ.NewIntentID()), assets.AssetID(econ.NewIntentID())
	pool, fund := testFacilities()
	q := money.QuantityFromInt64(100)

	p, err := BorrowPosting(testMeta(), acct, pool, quote, q)
	require.NoError(t, err)
	assert.Equal(t, ledger.KindMarginBorrow, p.Kind)
	assert.Equal(t, []ledger.Code{ledger.CodeWallet, ledger.CodeMarginLoan, ledger.CodeLoanReceivable, ledger.CodePoolCash}, codes(p))

	p, err = DeficitCoveragePosting(testMeta(), acct, pool, fund, quote, q)
	require.NoError(t, err)
	assert.Equal(t, ledger.KindDeficitCoverage, p.Kind)
	assert.Equal(t, []ledger.Code{ledger.CodeMarginLoan, ledger.CodeDeficit, ledger.CodePoolCash, ledger.CodeLoanReceivable,
		ledger.CodeDeficitReceivable, ledger.CodeInsuranceFundCash}, codes(p))

	p, err = LiquidationPosting(testMeta(), acct, base, q, quote, money.QuantityFromInt64(50), money.QuantityFromInt64(1))
	require.NoError(t, err)
	assert.Equal(t, ledger.KindLiquidation, p.Kind)
	assert.Equal(t, ledger.CodeMarginCollateral, p.Entries[0].Account.Code, "the sold quantity leaves the collateral it was held in")

	_, err = LiquidationPosting(testMeta(), acct, base, q, quote, money.QuantityFromInt64(1), money.QuantityFromInt64(2))
	require.Error(t, err, "a fee larger than the proceeds")

	p, err = DeficitRepaymentPosting(testMeta(), acct, fund, quote, q)
	require.NoError(t, err)
	assert.Equal(t, ledger.KindMarginRepay, p.Kind)
	assert.Equal(t, []ledger.Code{ledger.CodeDeficit, ledger.CodeWallet, ledger.CodeInsuranceFundCash, ledger.CodeDeficitReceivable}, codes(p),
		"the exact inverse of the fund's half of DEFICIT_COVERAGE")
	_, err = DeficitRepaymentPosting(testMeta(), acct, pool, quote, q)
	require.Error(t, err, "a deficit is repaid to an insurance fund, not to the pool")
	_, err = BorrowPosting(testMeta(), acct, pool, quote, money.Quantity{})
	require.Error(t, err, "a zero borrow")
	_, err = FacilityFundingPosting(testMeta(), econ.Facility{ID: econ.NewFacilityID(), Kind: ledger.FacilityBridge}, quote, q)
	require.Error(t, err, "only the pool and the fund are funded here")
}

// TestPostings_TheLiquidationFeeIsPlatformFeeRevenue (F13): the fee's two
// sides are the chart's own pair for an explicit fee inside a trade (model
// §5): the customer pays FEES_PLATFORM out of its WALLET, the platform books
// PLATFORM_FEE_RECEIVABLE against PLATFORM_FEE_REVENUE. No facility appears
// in a liquidation posting at all: nothing is booked as insurance-fund
// capital.
func TestPostings_TheLiquidationFeeIsPlatformFeeRevenue(t *testing.T) {
	acct := accounts.AccountID(econ.NewIntentID())
	quote, base := assets.AssetID(econ.NewIntentID()), assets.AssetID(econ.NewIntentID())
	fee := money.QuantityFromInt64(7)
	p, err := LiquidationPosting(testMeta(), acct, base, money.QuantityFromInt64(100), quote, money.QuantityFromInt64(500), fee)
	require.NoError(t, err)
	require.NoError(t, p.Validate())
	type leg struct {
		owner ledger.OwnerType
		code  ledger.Code
		side  ledger.Side
	}
	var feeLegs []leg
	for _, e := range p.Entries {
		assert.NotEqual(t, ledger.OwnerFacility, e.Account.OwnerType, "no facility takes part in a liquidation sale")
		if e.Account.AssetID == quote && e.Quantity.Equal(fee) {
			feeLegs = append(feeLegs, leg{e.Account.OwnerType, e.Account.Code, e.Side})
		}
	}
	assert.Equal(t, []leg{
		{ledger.OwnerCustomer, ledger.CodeFeesPlatform, ledger.Debit},
		{ledger.OwnerCustomer, ledger.CodeWallet, ledger.Credit},
		{ledger.OwnerPlatform, ledger.CodePlatformFeeReceivable, ledger.Debit},
		{ledger.OwnerPlatform, ledger.CodePlatformFeeRevenue, ledger.Credit},
	}, feeLegs)

	none, err := LiquidationPosting(testMeta(), acct, base, money.QuantityFromInt64(100), quote, money.QuantityFromInt64(500), money.Quantity{})
	require.NoError(t, err)
	for _, e := range none.Entries {
		assert.Equal(t, ledger.OwnerCustomer, e.Account.OwnerType, "no fee, no fee legs")
	}
}

// TestEffectJournalShapes_EveryMarginEffectKindIsBound: every effect kind the
// service posts under has exactly one journal shape, and a posting of the
// wrong kind or reference type is refused before anything is written.
func TestEffectJournalShapes_EveryMarginEffectKindIsBound(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range EffectJournalShapes() {
		assert.False(t, seen[s.EffectKind], "%s bound twice", s.EffectKind)
		seen[s.EffectKind] = true
		assert.True(t, s.JournalKind.IsSimulatedOnly(), "%s posts a simulated-only kind", s.EffectKind)
	}
	p := ledger.Posting{Kind: ledger.KindMarginRepay, Reference: ledger.FinancialEventReference{Type: refRepayment}}
	require.NoError(t, checkEffectJournal("REPAY", p))
	require.Error(t, checkEffectJournal("BORROW", p), "a repayment journal cannot apply a borrow")
	p.Reference.Type = refDeficitRepayment
	require.Error(t, checkEffectJournal("REPAY", p), "the reference type is part of the shape")
	require.Error(t, checkEffectJournal("UNHEARD_OF", p))
}

func codes(p ledger.Posting) []ledger.Code {
	out := make([]ledger.Code, 0, len(p.Entries))
	for _, e := range p.Entries {
		out = append(out, e.Account.Code)
	}
	return out
}

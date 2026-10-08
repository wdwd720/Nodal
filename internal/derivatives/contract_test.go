package derivatives

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

func TestSpec_TickValueIsAWholeNumberOfSettlementUnits(t *testing.T) {
	s := perpSpec(t)
	assert.Equal(t, "5000", s.TickValue.String(), "0.50 × 0.01 BTC = $0.005 = 5000 micro-dollars")
	v, err := s.Value(q(1), q(6_500_050))
	require.NoError(t, err)
	assert.Equal(t, "650005000", v.String(), "one contract at $65 000.50 is $650.005")
	v, err = s.Value(q(-3), q(6_500_000))
	require.NoError(t, err)
	assert.Equal(t, "-1950000000", v.String(), "a short's value is negative")
}

func TestSpec_RefusesWhatCannotSettleExactly(t *testing.T) {
	usd, btc := testAssets()
	ins := func(terms instruments.Terms) instruments.Instrument {
		return instruments.Instrument{ID: instruments.NewInstrumentID(), Type: terms.Kind, SettlementAssetID: usd.ID, Status: assets.StatusActive}
	}

	terms := testTerms(instruments.TypePerpetual, usd, btc)
	terms.ContractMultiplier = q(1) // one satoshi: a tick is 0.005 micro-dollars
	_, err := NewSpec(ins(terms), terms, usd, btc)
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	terms = testTerms(instruments.TypeOption, usd, btc)
	k := q(6_500_010) // not a multiple of the 50 tick
	terms.StrikeMantissa = &k
	_, err = NewSpec(ins(terms), terms, usd, btc)
	require.Error(t, err, "a strike off the tick grid")

	terms = testTerms(instruments.TypeFuture, usd, btc)
	terms.SettlementStyle = instruments.SettlePhysical
	_, err = NewSpec(ins(terms), terms, usd, btc)
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))

	real := usd
	real.Kind, real.ValueDomain = assets.KindSPLToken, valuedomain.SelfCustodialCrypto
	terms = testTerms(instruments.TypePerpetual, real, btc)
	_, err = NewSpec(ins(terms), terms, real, btc)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "derivatives settle only in SIMULATED value")

	terms = testTerms(instruments.TypePerpetual, usd, btc)
	terms.Kind = instruments.TypeSpotPair
	_, err = NewSpec(ins(terms), terms, usd, btc)
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
}

func TestSpec_PricesOffTheTickGridAreRefused(t *testing.T) {
	s := perpSpec(t)
	_, err := s.Value(q(1), q(6_500_010))
	require.Error(t, err)
	assert.False(t, s.OnTick(q(-50)))
	assert.True(t, s.OnTick(q(0)))
	up, err := s.AlignToTick(q(6_500_010), money.RoundCeil)
	require.NoError(t, err)
	assert.Equal(t, "6500050", up.String())
	down, err := s.AlignToTick(q(6_500_010), money.RoundFloor)
	require.NoError(t, err)
	assert.Equal(t, "6500000", down.String())
	_, err = s.AlignToTick(q(6_500_010), money.RoundExact)
	require.Error(t, err)
}

func TestSpec_ValueAnyPriceRoundsOnceInTheNamedMode(t *testing.T) {
	s := perpSpec(t)
	// 1 contract at 65000.505 (scale 3) = 0.01 × 65000.505 = 650.00505 USD = 650005050 micro.
	v, err := s.ValueAnyPrice(q(1), q(65_000_505), 3, money.RoundExact)
	require.NoError(t, err)
	assert.Equal(t, "650005050", v.String())
	// At scale 5 the value has a fractional micro-dollar: 650.0050505 USD.
	_, err = s.ValueAnyPrice(q(1), q(6_500_050_505), 5, money.RoundExact)
	require.Error(t, err)
	up, err := s.ValueAnyPrice(q(1), q(6_500_050_505), 5, money.RoundCeil)
	require.NoError(t, err)
	assert.Equal(t, "650005051", up.String())
	down, err := s.ValueAnyPrice(q(1), q(6_500_050_505), 5, money.RoundFloor)
	require.NoError(t, err)
	assert.Equal(t, "650005050", down.String())
}

func TestRescale(t *testing.T) {
	v, err := Rescale(q(12_345), 2, 4, money.RoundExact)
	require.NoError(t, err)
	assert.Equal(t, "1234500", v.String())
	_, err = Rescale(q(12_345), 4, 2, money.RoundExact)
	require.Error(t, err)
	v, err = Rescale(q(12_350), 4, 2, money.RoundHalfEven)
	require.NoError(t, err)
	assert.Equal(t, "124", v.String())
}

func TestSpec_QuantityMustBeWholeLotsAboveTheMinimum(t *testing.T) {
	usd, btc := testAssets()
	terms := testTerms(instruments.TypePerpetual, usd, btc)
	terms.LotSize, terms.MinQuantity = q(5), q(10)
	s := specFor(t, terms, usd, btc)
	require.NoError(t, s.CheckQuantity(q(15)))
	require.Error(t, s.CheckQuantity(q(5)), "below the minimum")
	require.Error(t, s.CheckQuantity(q(12)), "not a whole number of lots")
	require.Error(t, s.CheckQuantity(q(0)))
}

func TestEnumListsAreExactlyTheMigrationsLists(t *testing.T) {
	assert.Equal(t, []PositionStatus{"OPEN", "CLOSED", "SETTLED"}, AllPositionStatuses())
	assert.Equal(t, []FillSide{"BUY", "SELL"}, AllFillSides())
	assert.Equal(t, []FillSource{"PAPER", "BOOK", "EXERCISE", "ASSIGNMENT", "EXPIRY"}, AllFillSources())
	assert.Equal(t, []CashFlowKind{"REALIZED", "VARIATION_MARGIN", "FUNDING", "FUTURE_SETTLEMENT", "OPTION_PREMIUM",
		"OPTION_EXERCISE", "OPTION_ASSIGNMENT"}, AllCashFlowKinds())
	assert.Equal(t, []SettlementKind{"FUTURE", "OPTION"}, AllSettlementKinds())
	assert.Equal(t, []FundingDirection{"PAY", "RECEIVE"}, AllFundingDirections())
	assert.Equal(t, []ExerciseKind{"HOLDER", "AUTOMATIC"}, AllExerciseKinds())
}

func TestPositionTransitionsAreOpenToFlatOnly(t *testing.T) {
	for _, from := range AllPositionStatuses() {
		for _, to := range AllPositionStatuses() {
			want := from == PositionOpen && to != PositionOpen
			assert.Equal(t, want, CanTransitionPosition(from, to), "%s -> %s", from, to)
		}
	}
}

func TestEveryCashFlowKindPostsWithASimulatedOnlyJournalKind(t *testing.T) {
	for _, k := range AllCashFlowKinds() {
		jk, ok := JournalKindFor(k)
		require.True(t, ok, "%s has no journal kind", k)
		assert.True(t, jk.IsSimulatedOnly(), "%s posts with %s", k, jk)
	}
	_, ok := JournalKindFor("NOT_A_KIND")
	assert.False(t, ok)
	assert.Equal(t, ledger.KindFundingPayment, mustJournal(t, FlowFunding))
	assert.Equal(t, ledger.KindOptionSettlement, mustJournal(t, FlowOptionAssignment))
}

func mustJournal(t *testing.T, k CashFlowKind) ledger.Kind {
	t.Helper()
	jk, ok := JournalKindFor(k)
	require.True(t, ok)
	return jk
}

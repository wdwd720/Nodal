package margin

import (
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/money"
)

// feeLedgerKey is the fee ledger's schedule key pattern (its database CHECK).
var feeLedgerKey = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:/-]{0,127}$`)

// TestLiquidationFeeSchedule_DisclosesThePolicysFeeAndSaysItRoundsUp: the
// schedule is a valid fee-ledger schedule in the numeraire, charging exactly
// the policy's rate as its percentage fee and nothing else, its key is a
// valid key that names the numeraire and the rate, and every disclosure it
// produces states that the fee rounds in the platform's favour.
func TestLiquidationFeeSchedule_DisclosesThePolicysFeeAndSaysItRoundsUp(t *testing.T) {
	numeraire := assets.NewAssetID()
	p := DefaultPolicy()
	s := LiquidationFeeSchedule(p, numeraire, 6)
	require.NoError(t, s.Validate())
	assert.Equal(t, p.LiquidationFeeBPS, s.PercentageFeeBPS)
	assert.Equal(t, numeraire, s.QuoteAsset)
	assert.Zero(t, s.SpreadBPS)
	assert.True(t, s.FixedFee.IsZero() && s.MinFee.IsZero() && s.MaxTotalFee.IsZero())
	assert.Equal(t, money.RoundCeil, s.Rounding.PercentageFee)
	assert.True(t, s.Rounding.AllowPlatformFavourable)
	key := LiquidationFeeScheduleKey(numeraire, p.LiquidationFeeBPS)
	assert.Regexp(t, feeLedgerKey, key)
	assert.Contains(t, key, numeraire.String())
	assert.NotEqual(t, key, LiquidationFeeScheduleKey(numeraire, p.LiquidationFeeBPS+1), "another rate is another key")

	q, err := s.Quote(liquidationFeeInput(numeraire, 6, money.QuantityFromInt64(798_000_000), time.Unix(0, 0)))
	require.NoError(t, err)
	d := q.Disclosure()
	assert.True(t, d.Rounding.PlatformFavourableAllowed && d.Rounding.AnyFavoursPlatform, "the disclosure says the fee rounds up")
	assert.Equal(t, "7980000", d.Fees.Total.String(), "1% of 798 proceeds")
	assert.Equal(t, "798000000", d.NotionalAtReference.String(), "the disclosed notional is the proceeds")
}

// TestProp_TheDisclosedLiquidationFeeIsThePlansFee: for any proceeds, rate
// and numeraire precision, the fee-ledger disclosure of a liquidation charges
// exactly the fee PlanLiquidation computes -- ceil(proceeds x fee bps) -- as
// its platform fee and nothing else, its notional is the proceeds, it
// credits the proceeds less the fee, and it verifies against its schedule.
func TestProp_TheDisclosedLiquidationFeeIsThePlansFee(t *testing.T) {
	numeraire := assets.NewAssetID()
	rapid.Check(t, func(rt *rapid.T) {
		p := DefaultPolicy()
		p.LiquidationFeeBPS = money.BPS(rapid.Int64Range(0, 9_999).Draw(rt, "bps"))
		decimals := uint8(rapid.IntRange(0, 18).Draw(rt, "decimals"))
		proceeds := money.QuantityFromInt64(rapid.Int64Range(1, 1<<62).Draw(rt, "proceeds"))
		want, err := mulBPS(proceeds, p.LiquidationFeeBPS, money.RoundCeil)
		require.NoError(rt, err)
		s := LiquidationFeeSchedule(p, numeraire, decimals)
		in := liquidationFeeInput(numeraire, decimals, proceeds, time.Unix(1_700_000_000, 0))
		q, err := s.Quote(in)
		require.NoError(rt, err)
		require.NoError(rt, fees.VerifyQuote(s, in, q))
		require.NoError(rt, fees.VerifyDisclosure(s, q.Disclosure()))
		c := feeledger.ChargesOf(q.Disclosure())
		if !c.Platform.Equal(want) || !c.Strategy.IsZero() || !c.Spread.IsZero() || !c.Venue.IsZero() || !c.Network.IsZero() {
			rt.Fatalf("disclosed %+v, the plan charges %s", c, want)
		}
		if !q.NotionalAtReference.Equal(proceeds) || !q.TotalCredit.Equal(proceeds.Sub(want)) {
			rt.Fatalf("notional %s and credit %s for proceeds %s and fee %s", q.NotionalAtReference, q.TotalCredit, proceeds, want)
		}
	})
}

package nativemarket

import (
	"math/big"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func qs(s string) money.Quantity {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad quantity literal " + s)
	}
	return money.QuantityFromBigInt(v)
}

// A market shaped like a plausible launch: 1,000,000,000 tokens at six
// decimals in the pool, priced against a 30,000-Credit virtual reserve (also
// six decimals). Numbers this size are the point — the arithmetic has to be
// exact at 10^15 units, not just at 100.
func launchCurve() Curve {
	return Curve{
		VirtualCreditReserve: qs("30000000000"),      // 30,000 Credits
		InitialAssetReserve:  qs("1000000000000000"), // 1e9 tokens
	}
}

func launchState() State {
	return State{RealCreditReserve: q(0), AssetReserve: launchCurve().InitialAssetReserve}
}

func noFees() Fees   { return Fees{} }
func someFees() Fees { return Fees{PlatformBPS: 100, CreatorBPS: 50} } // 1% + 0.5%

// ---------------------------------------------------------------------------
// Shape
// ---------------------------------------------------------------------------

func TestCurve_ValidateRejectsAPoolThatCannotPrice(t *testing.T) {
	require.NoError(t, launchCurve().Validate())
	require.Error(t, Curve{InitialAssetReserve: q(1)}.Validate(),
		"a zero virtual reserve makes the first buy infinitely cheap")
	require.Error(t, Curve{VirtualCreditReserve: q(1)}.Validate())
	require.Error(t, Curve{VirtualCreditReserve: q(-1), InitialAssetReserve: q(1)}.Validate())
}

func TestFees_CannotExceedTheCap(t *testing.T) {
	require.NoError(t, someFees().Validate())
	require.NoError(t, Fees{PlatformBPS: 500, CreatorBPS: 500}.Validate())
	require.Error(t, Fees{PlatformBPS: 500, CreatorBPS: 501}.Validate(),
		"a market that can charge more than 10% is a mechanism for taking a holder's position")
	require.Error(t, Fees{PlatformBPS: -1}.Validate())
}

func TestState_InvariantHoldsAtLaunch(t *testing.T) {
	c, s := launchCurve(), launchState()
	require.True(t, s.HoldsInvariant(c))
	require.NoError(t, s.Validate(c))

	// A pool claiming more units than were ever minted into it is incoherent.
	bad := s
	bad.AssetReserve = c.InitialAssetReserve.Add(q(1))
	require.Error(t, bad.Validate(c))
}

// ---------------------------------------------------------------------------
// A single trade
// ---------------------------------------------------------------------------

func TestQuoteBuy_ConservesCreditsAndMovesThePriceUp(t *testing.T) {
	c, s := launchCurve(), launchState()
	spend := qs("100000000") // 100 Credits

	fill, err := QuoteBuy(c, s, spend, someFees())
	require.NoError(t, err)

	require.Equal(t, Buy, fill.Side)
	require.Equal(t, spend.String(), fill.CreditsIn.String())
	require.Equal(t, "1000000", fill.PlatformFee.String(), "1% of 100 Credits")
	require.Equal(t, "500000", fill.CreatorFee.String(), "0.5% of 100 Credits")
	require.Equal(t, "98500000", fill.CreditsToPool.String())

	// Conservation, checked here as well as inside the quote.
	sum := fill.CreditsToPool.Add(fill.PlatformFee).Add(fill.CreatorFee)
	require.Equal(t, spend.String(), sum.String())

	require.True(t, fill.AssetsOut.IsPositive())
	require.Equal(t, fill.StateBefore.AssetReserve.Sub(fill.AssetsOut).String(),
		fill.StateAfter.AssetReserve.String(),
		"units leaving the pool must equal units the user receives")
	require.Equal(t, fill.CreditsToPool.String(), fill.StateAfter.RealCreditReserve.String())

	require.True(t, fill.StateAfter.HoldsInvariant(c))
	require.Positive(t, fill.SpotAfter.Cmp(fill.SpotBefore), "buying must move the price up")
}

func TestQuoteSell_ReturnsCreditsAndMovesThePriceDown(t *testing.T) {
	c, s := launchCurve(), launchState()
	buy, err := QuoteBuy(c, s, qs("100000000"), noFees())
	require.NoError(t, err)

	sell, err := QuoteSell(c, buy.StateAfter, buy.AssetsOut, noFees())
	require.NoError(t, err)

	require.Equal(t, Sell, sell.Side)
	require.True(t, sell.CreditsOut.IsPositive())
	require.True(t, sell.StateAfter.HoldsInvariant(c))
	require.Negative(t, sell.SpotAfter.Cmp(sell.SpotBefore), "selling must move the price down")

	// Round-tripping must never be profitable, even with no fees at all: the
	// rounding is what stops it. This is the "no value from rounding" property
	// of PART LXXVIII at its sharpest, because with zero fees rounding is the
	// only thing standing in the way.
	require.LessOrEqual(t, sell.CreditsOut.Cmp(qs("100000000")), 0,
		"buying and immediately selling must not return more Credits than were paid")
}

func TestQuoteBuy_RejectsNonPositiveAmounts(t *testing.T) {
	c, s := launchCurve(), launchState()

	_, err := QuoteBuy(c, s, q(0), noFees())
	require.Error(t, err)
	_, err = QuoteBuy(c, s, q(-5), noFees())
	require.Error(t, err, "a negative buy would be a withdrawal wearing a trade's clothes")
}

// TestQuoteBuy_FeesCanNeverConsumeAnEntireTrade is a consequence of rounding
// fees DOWN, and is worth pinning because rounding them up would make the
// smallest trades pay 100% fee and receive nothing.
func TestQuoteBuy_FeesCanNeverConsumeAnEntireTrade(t *testing.T) {
	c, s := launchCurve(), launchState()
	for _, amt := range []int64{1, 2, 9, 99, 10_000} {
		fill, err := QuoteBuy(c, s, q(amt), Fees{PlatformBPS: 700, CreatorBPS: 300}) // the maximum
		require.NoError(t, err, "a %d-unit buy at the maximum fee must still transact", amt)
		require.True(t, fill.CreditsToPool.IsPositive(),
			"a %d-unit buy paid %s in fees and put nothing into the market", amt,
			fill.PlatformFee.Add(fill.CreatorFee))
		require.True(t, fill.AssetsOut.IsPositive())
	}
}

// TestQuoteBuy_RejectsBuysTooSmallToMoveTheCurve covers the other end: on a
// market whose units are expensive, a sub-unit buy must be refused rather than
// silently taking the Credits and returning nothing.
func TestQuoteBuy_RejectsBuysTooSmallToMoveTheCurve(t *testing.T) {
	// A pool with very few, very expensive units.
	c := Curve{VirtualCreditReserve: qs("1000000000000"), InitialAssetReserve: q(1_000)}
	s := State{RealCreditReserve: q(0), AssetReserve: q(1_000)}

	_, err := QuoteBuy(c, s, q(1), noFees())
	require.Error(t, err)
	require.Contains(t, err.Error(), "rounds to zero",
		"a buy that would yield no units must be refused, not silently swallowed")
}

func TestQuoteSell_RejectsMoreUnitsThanTheCurveEverSold(t *testing.T) {
	c, s := launchCurve(), launchState()
	// The pool is full: nobody holds anything, so any sell is fabricated supply.
	_, err := QuoteSell(c, s, q(1), noFees())
	require.Error(t, err)
	require.Contains(t, err.Error(), "more into the pool than the curve ever sold")
}

func TestQuote_RefusesToTradeAgainstABrokenState(t *testing.T) {
	c := launchCurve()
	broken := State{RealCreditReserve: q(0), AssetReserve: q(1)} // K is nowhere near held
	_, err := QuoteBuy(c, broken, qs("1000000"), noFees())
	require.Error(t, err)
	require.Equal(t, errs.CodeInternal, errs.CodeOf(err))
	require.Contains(t, err.Error(), "invariant")
}

func TestFill_SlippageIsReportedNotDiscovered(t *testing.T) {
	c, s := launchCurve(), launchState()
	small, err := QuoteBuy(c, s, qs("1000000"), noFees()) // 1 Credit
	require.NoError(t, err)
	large, err := QuoteBuy(c, s, qs("10000000000"), noFees()) // 10,000 Credits
	require.NoError(t, err)

	require.Greater(t, large.SlippageBPS(), small.SlippageBPS(),
		"a larger order must report more slippage; that is the cost being disclosed")
	require.GreaterOrEqual(t, small.SlippageBPS(), money.BPS(0))
}

// ---------------------------------------------------------------------------
// Properties over sequences
// ---------------------------------------------------------------------------

// simulate walks a random sequence of buys and sells, asserting the two
// load-bearing properties after every single trade.
func simulate(t *testing.T, seed uint64, fees Fees, steps int) {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b9))
	c, s := launchCurve(), launchState()
	k := c.K()

	// held is what users collectively hold; a sell can never exceed it.
	held := big.NewInt(0)
	paidIn := big.NewInt(0)  // Credits users have put in
	paidOut := big.NewInt(0) // Credits users have taken out
	feesTaken := big.NewInt(0)

	for i := 0; i < steps; i++ {
		buy := held.Sign() == 0 || rng.IntN(2) == 0
		if buy {
			// 0.01 to 5,000 Credits.
			amt := q(int64(rng.Uint64N(5_000_000_000) + 10_000))
			fill, err := QuoteBuy(c, s, amt, fees)
			if err != nil {
				// Only legitimate refusals are acceptable here.
				require.Contains(t, err.Error(), "rounds to zero", "unexpected buy refusal at step %d: %v", i, err)
				continue
			}
			s = fill.StateAfter
			held.Add(held, fill.AssetsOut.BigInt())
			paidIn.Add(paidIn, fill.CreditsIn.BigInt())
			feesTaken.Add(feesTaken, fill.PlatformFee.BigInt())
			feesTaken.Add(feesTaken, fill.CreatorFee.BigInt())
		} else {
			// Sell a random fraction of what is held.
			denom := int64(rng.IntN(8) + 1)
			amt := new(big.Int).Quo(held, big.NewInt(denom))
			if amt.Sign() == 0 {
				amt = new(big.Int).Set(held)
			}
			fill, err := QuoteSell(c, s, money.QuantityFromBigInt(amt), fees)
			if err != nil {
				require.Contains(t, err.Error(), "releases no Credits",
					"unexpected sell refusal at step %d: %v", i, err)
				continue
			}
			s = fill.StateAfter
			held.Sub(held, amt)
			paidOut.Add(paidOut, fill.CreditsOut.BigInt())
			feesTaken.Add(feesTaken, fill.PlatformFee.BigInt())
			feesTaken.Add(feesTaken, fill.CreatorFee.BigInt())
		}

		require.False(t, s.RealCreditReserve.IsNegative(),
			"step %d: the real Credit reserve went negative", i)
		require.NoError(t, s.Validate(c), "step %d", i)
		product := new(big.Int).Mul(s.Effective(c).BigInt(), s.AssetReserve.BigInt())
		require.GreaterOrEqual(t, product.Cmp(k), 0,
			"step %d: the constant product fell below K", i)

		// Supply conservation: what the pool holds plus what users hold is
		// exactly what was minted into the curve. No trade may create a unit.
		total := new(big.Int).Add(s.AssetReserve.BigInt(), held)
		require.Equal(t, 0, total.Cmp(c.InitialAssetReserve.BigInt()),
			"step %d: asset units were created or destroyed", i)
	}

	// Value conservation across the whole run: everything users paid in is
	// still accounted for as reserve, payouts or fees. Nothing appeared.
	accounted := new(big.Int).Add(s.RealCreditReserve.BigInt(), paidOut)
	accounted.Add(accounted, feesTaken)
	require.Equal(t, 0, accounted.Cmp(paidIn),
		"Credits were created or destroyed: paid in %s, accounted %s", paidIn, accounted)
}

// TestProp_RealReserveNeverGoesNegative is the property the file header proves
// by arithmetic; this exercises it against sequences the proof is supposed to
// cover.
func TestProp_RealReserveNeverGoesNegative(t *testing.T) {
	for seed := uint64(1); seed <= 25; seed++ {
		simulate(t, seed, noFees(), 200)
	}
}

func TestProp_InvariantAndConservationHoldWithFees(t *testing.T) {
	for seed := uint64(100); seed <= 120; seed++ {
		simulate(t, seed, someFees(), 200)
	}
}

func TestProp_MaximumFeesDoNotBreakConservation(t *testing.T) {
	for seed := uint64(200); seed <= 210; seed++ {
		simulate(t, seed, Fees{PlatformBPS: 700, CreatorBPS: 300}, 150)
	}
}

// TestProp_FullLiquidationLeavesTheReserveSolvent is PART LXXVIII's "break
// during full liquidation": every holder sells everything at once.
func TestProp_FullLiquidationLeavesTheReserveSolvent(t *testing.T) {
	c, s := launchCurve(), launchState()
	held := big.NewInt(0)
	paidIn := big.NewInt(0)

	for i := 0; i < 40; i++ {
		fill, err := QuoteBuy(c, s, qs("500000000"), noFees()) // 500 Credits each
		require.NoError(t, err)
		s = fill.StateAfter
		held.Add(held, fill.AssetsOut.BigInt())
		paidIn.Add(paidIn, fill.CreditsIn.BigInt())
	}

	fill, err := QuoteSell(c, s, money.QuantityFromBigInt(held), noFees())
	require.NoError(t, err, "the pool must be able to buy back everything it sold")
	require.False(t, fill.StateAfter.RealCreditReserve.IsNegative())
	require.LessOrEqual(t, fill.CreditsOut.BigInt().Cmp(paidIn), 0,
		"liquidating everything cannot return more than was ever paid in")
	require.True(t, fill.StateAfter.HoldsInvariant(c))

	// And the pool is back to holding every unit it started with.
	require.Equal(t, c.InitialAssetReserve.String(), fill.StateAfter.AssetReserve.String())
}

// TestProp_ManyTinyTradesCannotExtractValue is the rounding attack: grind the
// market with the smallest possible round trips and see whether the attacker
// ends up ahead.
func TestProp_ManyTinyTradesCannotExtractValue(t *testing.T) {
	c, s := launchCurve(), launchState()
	// Seed the pool so a sell is possible at all.
	seed, err := QuoteBuy(c, s, qs("1000000000"), noFees())
	require.NoError(t, err)
	s = seed.StateAfter
	held := seed.AssetsOut

	spent := new(big.Int).Set(seed.CreditsIn.BigInt())
	received := big.NewInt(0)

	for i := 0; i < 2_000; i++ {
		buy, err := QuoteBuy(c, s, q(10_000), noFees()) // 0.01 Credits
		if err != nil {
			continue
		}
		s = buy.StateAfter
		held = held.Add(buy.AssetsOut)
		spent.Add(spent, buy.CreditsIn.BigInt())

		sell, err := QuoteSell(c, s, buy.AssetsOut, noFees())
		if err != nil {
			continue
		}
		s = sell.StateAfter
		held = held.Sub(buy.AssetsOut)
		received.Add(received, sell.CreditsOut.BigInt())
	}

	final, err := QuoteSell(c, s, held, noFees())
	require.NoError(t, err)
	received.Add(received, final.CreditsOut.BigInt())

	require.LessOrEqual(t, received.Cmp(spent), 0,
		"2,000 tiny round trips extracted %s Credits more than they paid",
		new(big.Int).Sub(received, spent))
	require.True(t, final.StateAfter.HoldsInvariant(c))
}

// FuzzCurve_NeverBreaksTheInvariant throws arbitrary amounts at the curve and
// requires that every accepted trade leaves a coherent, solvent pool and every
// rejected one leaves the state untouched.
func FuzzCurve_NeverBreaksTheInvariant(f *testing.F) {
	f.Add(int64(1_000_000), int64(500_000), uint8(0), int64(100))
	f.Add(int64(1), int64(1), uint8(1), int64(0))
	f.Add(int64(1<<62), int64(1<<40), uint8(0), int64(999))

	c := launchCurve()
	f.Fuzz(func(t *testing.T, buyAmt, sellFrac int64, mode uint8, feeBps int64) {
		if feeBps < 0 || feeBps > int64(MaxTotalFeeBPS) {
			t.Skip()
		}
		fees := Fees{PlatformBPS: money.BPS(feeBps)}
		s := launchState()
		k := c.K()
		held := big.NewInt(0)

		if buyAmt > 0 {
			if fill, err := QuoteBuy(c, s, q(buyAmt), fees); err == nil {
				s = fill.StateAfter
				held.Add(held, fill.AssetsOut.BigInt())
			}
		}
		if mode%2 == 1 && held.Sign() > 0 && sellFrac > 0 {
			amt := new(big.Int).Quo(held, big.NewInt(sellFrac))
			if amt.Sign() > 0 {
				if fill, err := QuoteSell(c, s, money.QuantityFromBigInt(amt), fees); err == nil {
					s = fill.StateAfter
					held.Sub(held, amt)
				}
			}
		}

		require.False(t, s.RealCreditReserve.IsNegative())
		require.NoError(t, s.Validate(c))
		product := new(big.Int).Mul(s.Effective(c).BigInt(), s.AssetReserve.BigInt())
		require.GreaterOrEqual(t, product.Cmp(k), 0, "constant product fell below K")
		total := new(big.Int).Add(s.AssetReserve.BigInt(), held)
		require.Equal(t, 0, total.Cmp(c.InitialAssetReserve.BigInt()))
	})
}

// TestCurve_ExtremeSizesDoNotOverflow covers PART LXXII items 17 and 20.
func TestCurve_ExtremeSizesDoNotOverflow(t *testing.T) {
	c, s := launchCurve(), launchState()

	// A whale buying an absurd amount: allowed, priced, and it cannot take
	// more units than the pool holds.
	huge := qs("999999999999999999999999999999")
	fill, err := QuoteBuy(c, s, huge, someFees())
	require.NoError(t, err)
	require.LessOrEqual(t, fill.AssetsOut.Cmp(c.InitialAssetReserve), 0,
		"no buy may take more units than the pool ever held")
	require.True(t, fill.StateAfter.HoldsInvariant(c))
	require.True(t, fill.StateAfter.AssetReserve.Sign() >= 0)

	// And selling it all back returns at most what was paid.
	back, err := QuoteSell(c, fill.StateAfter, fill.AssetsOut, someFees())
	require.NoError(t, err)
	require.LessOrEqual(t, back.CreditsOut.Cmp(huge), 0)
	require.False(t, back.StateAfter.RealCreditReserve.IsNegative())
}

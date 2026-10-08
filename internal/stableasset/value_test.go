package stableasset

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/money"
)

func price(mantissa int64, scale int32) money.Price {
	return money.Price{Mantissa: money.QuantityFromInt64(mantissa), Scale: scale, QuoteAsset: "q", Source: "s",
		At: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
}

func qty(n int64) money.Quantity { return money.QuantityFromInt64(n) }

func TestValue_ExactArithmeticAndRendering(t *testing.T) {
	a := unitsValue(qty(1_500_000), 6) // 1.5
	b := unitsValue(qty(25), 2)        // 0.25
	assert.Equal(t, "1.750000", a.Add(b).String())
	assert.Equal(t, "1.250000", a.Sub(b).String())
	assert.Equal(t, 1, a.Cmp(b))
	assert.Equal(t, 0, unitsValue(qty(100), 2).Cmp(unitsValue(qty(1_000_000), 6)))
	assert.Equal(t, "1.5000000000", a.times(price(10_000, 4)).String())
	assert.Equal(t, "1.3500000000", a.timesBPS(9_000).String())

	usd, err := unitsValue(qty(1_234_567), 6).ToUSD(money.RoundDown)
	require.NoError(t, err)
	assert.Equal(t, "1.23", usd.String())
	usd, err = unitsValue(qty(1_234_567), 6).ToUSD(money.RoundUp)
	require.NoError(t, err)
	assert.Equal(t, "1.24", usd.String())
	_, err = unitsValue(money.QuantityFromInt64(1).Mul(pow10(30)), 0).ToUSD(money.RoundDown)
	require.Error(t, err, "beyond the USD range fails, never wraps")

	v, err := parseDecimalValue("12.0340")
	require.NoError(t, err)
	assert.Equal(t, 0, v.Cmp(unitsValue(qty(12_034), 3)))
	_, err = parseDecimalValue("1.")
	require.Error(t, err)
}

func TestRatios_CoverageRoundsDownAndDeviationRoundsUp(t *testing.T) {
	assert.Equal(t, money.BPS(9_999), ratioBPS(unitsValue(qty(99_999), 5), unitsValue(qty(1), 0)))
	assert.Equal(t, money.OneHundredPercent, ratioBPS(unitsValue(qty(5), 0), Value{}), "nothing to cover is full coverage")
	assert.Equal(t, money.BPS(0), ratioBPS(Value{}, unitsValue(qty(1), 0)))
	assert.Equal(t, money.BPS(1), deviationBPS(price(1_000_001, 6), price(1, 0)), "a hair off the peg is one basis point, never zero")
	assert.Equal(t, money.BPS(500), deviationBPS(price(95, 2), price(1, 0)))
	assert.Equal(t, money.BPS(0), deviationBPS(price(100, 2), price(1, 0)))
}

func TestCapAtPar_ACashEquivalentIsNeverWorthMoreThanADollar(t *testing.T) {
	assert.Equal(t, 0, Value{Coef: capAtPar(price(101, 2)).Mantissa}.Cmp(Value{Coef: qty(1)}))
	below := capAtPar(price(99, 2))
	assert.Equal(t, "99", below.Mantissa.String())
	assert.Equal(t, int32(2), below.Scale)
}

func TestRequiredReserve_RefusesUselessInputs(t *testing.T) {
	_, err := requiredReserve(qty(1), 6, price(1, 0), 6, price(1, 0), 0)
	require.Error(t, err, "a zero factor backs nothing")
	_, err = requiredReserve(qty(1), 6, price(1, 0), 6, price(0, 0), 10_000)
	require.Error(t, err, "a zero price backs nothing")
	r, err := requiredReserve(qty(100), 6, price(1, 0), 6, price(1, 0), 9_800)
	require.NoError(t, err)
	assert.Equal(t, "103", r.String(), "100 / 0.98 = 102.04, rounded up")
}

// TestProp_RequiredReserveIsTheSmallestCoveringPayment: for any quantity,
// decimals, peg, price and factor, the required reserve covers the issued
// value at the peg and one base unit less does not.
func TestProp_RequiredReserveIsTheSmallestCoveringPayment(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		q := qty(rapid.Int64Range(1, 1_000_000_000_000).Draw(rt, "q"))
		sd := uint8(rapid.IntRange(0, 18).Draw(rt, "stable_decimals"))
		rd := uint8(rapid.IntRange(0, 18).Draw(rt, "reserve_decimals"))
		peg := price(rapid.Int64Range(1, 5_000_000).Draw(rt, "peg"), int32(rapid.IntRange(0, 8).Draw(rt, "peg_scale")))
		p := price(rapid.Int64Range(1, 5_000_000).Draw(rt, "price"), int32(rapid.IntRange(0, 8).Draw(rt, "price_scale")))
		f := money.BPS(rapid.Int64Range(1, 10_000).Draw(rt, "factor"))

		r, err := requiredReserve(q, sd, peg, rd, p, f)
		require.NoError(rt, err)
		target := unitsValue(q, sd).times(peg)
		covers := func(x money.Quantity) bool { return unitsValue(x, rd).times(p).timesBPS(f).Cmp(target) >= 0 }
		require.True(rt, covers(r))
		require.True(rt, r.IsPositive())
		require.False(rt, covers(r.Sub(qty(1))), "a smaller payment would under-back the issuance")
	})
}

// TestProp_ParPayoutNeverOverpays: a redemption pays at most the peg value of
// what it retires, and one base unit more would overpay. This is the Go half
// of SQLSTATE SV005.
func TestProp_ParPayoutNeverOverpays(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		q := qty(rapid.Int64Range(1, 1_000_000_000_000).Draw(rt, "q"))
		sd := uint8(rapid.IntRange(0, 18).Draw(rt, "stable_decimals"))
		rd := uint8(rapid.IntRange(0, 18).Draw(rt, "reserve_decimals"))
		peg := price(rapid.Int64Range(1, 5_000_000).Draw(rt, "peg"), int32(rapid.IntRange(0, 8).Draw(rt, "peg_scale")))

		p, err := parPayout(q, sd, peg, rd)
		require.NoError(rt, err)
		value := unitsValue(q, sd).times(peg)
		require.True(rt, unitsValue(p, rd).Cmp(value) <= 0)
		require.True(rt, unitsValue(p.Add(qty(1)), rd).Cmp(value) > 0)
	})
}

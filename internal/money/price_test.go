package money

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var priceTime = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// solUSDC is 150.123456 USDC per SOL.
func solUSDC(t testing.TB) Price {
	t.Helper()
	p, err := NewPrice(q64(150123456), 6, "USDC", "jupiter", priceTime)
	require.NoError(t, err)
	return p
}

func TestPrice_Validate(t *testing.T) {
	valid := Price{Mantissa: q64(150123456), Scale: 6, QuoteAsset: "USDC", Source: "jupiter", At: priceTime}
	require.NoError(t, valid.Validate())

	zeroPrice := valid
	zeroPrice.Mantissa = Quantity{}
	require.NoError(t, zeroPrice.Validate(), "a zero mantissa is valid")

	edge := valid
	edge.Scale = MaxPriceScale
	require.NoError(t, edge.Validate())
	edge.Scale = 0
	require.NoError(t, edge.Validate())

	cases := []struct {
		name   string
		mutate func(p *Price)
	}{
		{"negative scale", func(p *Price) { p.Scale = -1 }},
		{"scale too large", func(p *Price) { p.Scale = MaxPriceScale + 1 }},
		{"scale max int32", func(p *Price) { p.Scale = math.MaxInt32 }},
		{"negative mantissa", func(p *Price) { p.Mantissa = q64(-1) }},
		{"empty quote asset", func(p *Price) { p.QuoteAsset = "" }},
		{"blank quote asset", func(p *Price) { p.QuoteAsset = "  " }},
		{"empty source", func(p *Price) { p.Source = "" }},
		{"blank source", func(p *Price) { p.Source = "\t" }},
		{"zero time", func(p *Price) { p.At = time.Time{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.mutate(&p)
			require.ErrorIs(t, p.Validate(), ErrInvalidPrice)
			_, err := NewPrice(p.Mantissa, p.Scale, p.QuoteAsset, p.Source, p.At)
			require.ErrorIs(t, err, ErrInvalidPrice)
			_, err = Notional(q64(1), 9, p, 6, RoundHalfEven)
			require.ErrorIs(t, err, ErrInvalidPrice)
		})
	}
	var zero Price
	require.ErrorIs(t, zero.Validate(), ErrInvalidPrice)
}

func TestPriceFromDecimalString(t *testing.T) {
	cases := []struct {
		in       string
		mantissa string
		scale    int32
	}{
		{"150.123456", "150123456", 6},
		{"150", "150", 0},
		{"0", "0", 0},
		{"0.00000001", "1", 8},
		{"1.50", "150", 2},
		{"000.5", "5", 1},
		{"1." + strings.Repeat("0", 37) + "1", "1" + strings.Repeat("0", 37) + "1", 38},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			p, err := PriceFromDecimalString(tc.in, "USDC", "test", priceTime)
			require.NoError(t, err)
			require.Equal(t, tc.mantissa, p.Mantissa.String())
			require.Equal(t, tc.scale, p.Scale)
			require.Equal(t, "USDC", p.QuoteAsset)
			require.Equal(t, "test", p.Source)
			require.Equal(t, priceTime, p.At)
			require.NoError(t, p.Validate())
		})
	}

	_, err := PriceFromDecimalString("-1.5", "USDC", "test", priceTime)
	require.ErrorIs(t, err, ErrInvalidPrice)
	_, err = PriceFromDecimalString("1."+strings.Repeat("0", 39), "USDC", "test", priceTime)
	require.ErrorIs(t, err, ErrInvalidPrice)
	_, err = PriceFromDecimalString("abc", "USDC", "test", priceTime)
	require.ErrorIs(t, err, ErrInvalidPrice)
	require.ErrorIs(t, err, ErrInvalidFormat)
	_, err = PriceFromDecimalString("1.", "USDC", "test", priceTime)
	require.ErrorIs(t, err, ErrInvalidPrice)
	_, err = PriceFromDecimalString("1.5", "", "test", priceTime)
	require.ErrorIs(t, err, ErrInvalidPrice)
	_, err = PriceFromDecimalString("1.5", "USDC", "test", time.Time{})
	require.ErrorIs(t, err, ErrInvalidPrice)
	_, err = PriceFromDecimalString("1"+strings.Repeat("0", 128), "USDC", "test", priceTime)
	require.ErrorIs(t, err, ErrInvalidPrice)
	require.ErrorIs(t, err, ErrOverflow)
}

func TestPrice_String(t *testing.T) {
	require.Equal(t, "150.123456 USDC", solUSDC(t).String())
	p := Price{Mantissa: q64(150), Scale: 0, QuoteAsset: "USD"}
	require.Equal(t, "150 USD", p.String())
	p = Price{Mantissa: q64(5), Scale: 3, QuoteAsset: "USDT"}
	require.Equal(t, "0.005 USDT", p.String())
	p = Price{Mantissa: q64(150), Scale: 40, QuoteAsset: "USDC"}
	require.Equal(t, "150e-40 USDC", p.String())
}

func TestNotional_Solana(t *testing.T) {
	const solDecimals, usdcDecimals = 9, 6
	p := solUSDC(t)

	// 1.5 SOL × 150.123456 = 225.185184 USDC, exact in micro-USDC.
	oneAndHalfSOL := q64(1_500_000_000)
	for _, mode := range allModes {
		n, err := Notional(oneAndHalfSOL, solDecimals, p, usdcDecimals, mode)
		require.NoError(t, err, mode)
		require.Equal(t, "225185184", n.String(), mode)
		require.Equal(t, "225.185184", n.ToDecimalString(usdcDecimals), mode)
	}

	// 0.5 SOL = 75.061728 USDC exactly.
	n, err := Notional(q64(500_000_000), solDecimals, p, usdcDecimals, RoundExact)
	require.NoError(t, err)
	require.Equal(t, "75061728", n.String())

	// 1 lamport = 0.000000150123456 USDC, below one micro-USDC.
	oneLamport := q64(1)
	for _, tc := range []struct {
		mode RoundingMode
		want string
	}{
		{RoundDown, "0"}, {RoundFloor, "0"}, {RoundHalfEven, "0"}, {RoundHalfUp, "0"}, {RoundUp, "1"}, {RoundCeil, "1"},
	} {
		n, err := Notional(oneLamport, solDecimals, p, usdcDecimals, tc.mode)
		require.NoError(t, err, tc.mode)
		require.Equal(t, tc.want, n.String(), tc.mode)
	}
	_, err = Notional(oneLamport, solDecimals, p, usdcDecimals, RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)

	// 1234567 lamports × 150123456 = 185337464703552 / 10^9 = 185337.464703552 micro-USDC.
	odd := q64(1_234_567)
	for _, tc := range []struct {
		mode RoundingMode
		want string
	}{
		{RoundDown, "185337"},
		{RoundFloor, "185337"},
		{RoundHalfEven, "185337"},
		{RoundHalfUp, "185337"},
		{RoundUp, "185338"},
		{RoundCeil, "185338"},
	} {
		n, err := Notional(odd, solDecimals, p, usdcDecimals, tc.mode)
		require.NoError(t, err, tc.mode)
		require.Equal(t, tc.want, n.String(), tc.mode)
	}
	// Selling the same amount: signed semantics differ between Down/Floor and Up/Ceil.
	for _, tc := range []struct {
		mode RoundingMode
		want string
	}{
		{RoundDown, "-185337"},
		{RoundFloor, "-185338"},
		{RoundHalfEven, "-185337"},
		{RoundHalfUp, "-185337"},
		{RoundUp, "-185338"},
		{RoundCeil, "-185337"},
	} {
		n, err := Notional(odd.Neg(), solDecimals, p, usdcDecimals, tc.mode)
		require.NoError(t, err, tc.mode)
		require.Equal(t, tc.want, n.String(), tc.mode)
	}

	// 2 SOL at 1.5 USDC (mantissa 15, scale 1) = 3 USDC.
	cheap, err := NewPrice(q64(15), 1, "USDC", "test", priceTime)
	require.NoError(t, err)
	n, err = Notional(q64(2_000_000_000), solDecimals, cheap, usdcDecimals, RoundExact)
	require.NoError(t, err)
	require.Equal(t, "3000000", n.String())

	// Zero quantity and zero price both give zero.
	n, err = Notional(Quantity{}, solDecimals, p, usdcDecimals, RoundExact)
	require.NoError(t, err)
	require.True(t, n.IsZero())
	zeroPrice, err := NewPrice(Quantity{}, 6, "USDC", "test", priceTime)
	require.NoError(t, err)
	n, err = Notional(oneAndHalfSOL, solDecimals, zeroPrice, usdcDecimals, RoundExact)
	require.NoError(t, err)
	require.True(t, n.IsZero())

	// A whole u64 of lamports never overflows.
	// 18446744073709551615 × 150123456 = 2769288972292796628654181440; / 10^9 = 2769288972292796628.654181440
	u64Max := mustQ(t, "18446744073709551615")
	n, err = Notional(u64Max, solDecimals, p, usdcDecimals, RoundDown)
	require.NoError(t, err)
	require.Equal(t, "2769288972292796628", n.String())
	n, err = Notional(u64Max, solDecimals, p, usdcDecimals, RoundHalfEven)
	require.NoError(t, err)
	require.Equal(t, "2769288972292796629", n.String())
}

func TestNotional_PositiveExponentIsExact(t *testing.T) {
	// 1 USDC (6 decimals) at 1.50 (scale 2) in an 18-decimal quote asset:
	// exponent = 18 - 6 - 2 = 10, so no division happens at all.
	p, err := NewPrice(q64(150), 2, "WETH", "test", priceTime)
	require.NoError(t, err)
	for _, mode := range allModes {
		n, err := Notional(q64(1_000_000), 6, p, 18, mode)
		require.NoError(t, err, mode)
		require.Equal(t, "1500000000000000000", n.String(), mode)
	}
	// Exponent exactly zero: qty × mantissa.
	p0, err := NewPrice(q64(7), 0, "X", "test", priceTime)
	require.NoError(t, err)
	n, err := Notional(q64(3), 4, p0, 4, RoundExact)
	require.NoError(t, err)
	require.Equal(t, "21", n.String())
}

func TestNotional_ScaleInvariance(t *testing.T) {
	prices := []struct {
		mantissa string
		scale    int32
	}{
		{"150123456", 6}, {"1501234560", 7}, {"15012345600", 8}, {"150123456000", 9}, {"150123456" + strings.Repeat("0", 32), 38},
	}
	qty := q64(1_234_567)
	for _, mode := range inexactModes {
		var want string
		for i, pr := range prices {
			p, err := NewPrice(mustQ(t, pr.mantissa), pr.scale, "USDC", "test", priceTime)
			require.NoError(t, err)
			n, err := Notional(qty, 9, p, 6, mode)
			require.NoError(t, err)
			if i == 0 {
				want = n.String()
				continue
			}
			require.Equal(t, want, n.String(), "%s scale %d", mode, pr.scale)
		}
	}
}

func TestNotional_Errors(t *testing.T) {
	p := solUSDC(t)
	_, err := Notional(q64(1), 9, p, 6, roundingUnset)
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
	bad := p
	bad.Source = ""
	_, err = Notional(q64(1_500_000_000), 9, bad, 6, RoundHalfEven)
	require.ErrorIs(t, err, ErrInvalidPrice)
	var zero Price
	_, err = Notional(q64(1), 9, zero, 6, RoundHalfEven)
	require.ErrorIs(t, err, ErrInvalidPrice)
}

func TestQuoteQuantityToUSD(t *testing.T) {
	cases := []struct {
		q        string
		decimals uint8
		mode     RoundingMode
		want     string
	}{
		{"225185184", 6, RoundHalfEven, "225.19"}, // 22518.5184 cents
		{"225185184", 6, RoundDown, "225.18"},
		{"225185184", 6, RoundUp, "225.19"},
		{"-225185184", 6, RoundHalfEven, "-225.19"},
		{"-225185184", 6, RoundDown, "-225.18"},
		{"-225185184", 6, RoundFloor, "-225.19"},
		{"-225185184", 6, RoundCeil, "-225.18"},
		{"1000000", 6, RoundExact, "1.00"},
		{"5", 0, RoundExact, "5.00"},
		{"-5", 0, RoundExact, "-5.00"},
		{"123", 1, RoundExact, "12.30"},
		{"12345", 2, RoundExact, "123.45"},
		{"1", 18, RoundHalfEven, "0.00"},
		{"1", 18, RoundUp, "0.01"},
		{"5000", 6, RoundHalfEven, "0.00"}, // 0.5 cent tie -> even 0
		{"5000", 6, RoundHalfUp, "0.01"},
		{"15000", 6, RoundHalfEven, "0.02"}, // 1.5 cents -> even 2
		{"0", 18, RoundExact, "0.00"},
		{"92233720368547758070000", 6, RoundExact, "92233720368547758.07"}, // MaxInt64 cents exactly
		{"-92233720368547758080000", 6, RoundExact, "-92233720368547758.08"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s@%d_%s", tc.q, tc.decimals, tc.mode), func(t *testing.T) {
			got, err := QuoteQuantityToUSD(mustQ(t, tc.q), tc.decimals, tc.mode)
			require.NoError(t, err)
			require.Equal(t, tc.want, got.String())
		})
	}

	_, err := QuoteQuantityToUSD(q64(225185184), 6, RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
	_, err = QuoteQuantityToUSD(mustQ(t, "1"+strings.Repeat("0", 30)), 6, RoundDown)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = QuoteQuantityToUSD(mustQ(t, "92233720368547758080000"), 6, RoundExact) // MaxInt64 + 1 cents
	require.ErrorIs(t, err, ErrOverflow)
	_, err = QuoteQuantityToUSD(q64(math.MaxInt64), 0, RoundExact)
	require.ErrorIs(t, err, ErrOverflow)
	_, err = QuoteQuantityToUSD(q64(1), 6, roundingUnset)
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
	_, err = QuoteQuantityToUSD(q64(1), 2, roundingUnset) // no rounding needed, mode still checked
	require.ErrorIs(t, err, ErrInvalidRoundingMode)
}

func TestUSDToQuoteQuantity(t *testing.T) {
	got, err := USDToQuoteQuantity(mustUSD(t, "1.00"), 6, RoundExact)
	require.NoError(t, err)
	require.Equal(t, "1000000", got.String())
	got, err = USDToQuoteQuantity(mustUSD(t, "1.00"), 18, RoundExact)
	require.NoError(t, err)
	require.Equal(t, "1000000000000000000", got.String())
	got, err = USDToQuoteQuantity(mustUSD(t, "-0.01"), 6, RoundExact)
	require.NoError(t, err)
	require.Equal(t, "-10000", got.String())
	got, err = USDToQuoteQuantity(mustUSD(t, "1.23"), 2, RoundExact)
	require.NoError(t, err)
	require.Equal(t, "123", got.String())
	got, err = USDToQuoteQuantity(MinUSD(), 6, RoundExact)
	require.NoError(t, err)
	require.Equal(t, "-92233720368547758080000", got.String())

	got, err = USDToQuoteQuantity(mustUSD(t, "1.00"), 0, RoundExact)
	require.NoError(t, err)
	require.Equal(t, "1", got.String())
	got, err = USDToQuoteQuantity(mustUSD(t, "1.50"), 0, RoundHalfEven)
	require.NoError(t, err)
	require.Equal(t, "2", got.String())
	got, err = USDToQuoteQuantity(mustUSD(t, "1.50"), 0, RoundDown)
	require.NoError(t, err)
	require.Equal(t, "1", got.String())
	got, err = USDToQuoteQuantity(mustUSD(t, "1.25"), 1, RoundHalfEven)
	require.NoError(t, err)
	require.Equal(t, "12", got.String())
	_, err = USDToQuoteQuantity(mustUSD(t, "1.50"), 0, RoundExact)
	require.ErrorIs(t, err, ErrPrecisionLoss)
	_, err = USDToQuoteQuantity(mustUSD(t, "1.50"), 6, roundingUnset)
	require.ErrorIs(t, err, ErrInvalidRoundingMode)

	// Round trip through the quote asset is exact for >= 2 decimals.
	for _, s := range []string{"0.00", "0.01", "-0.01", "1234.56", "92233720368547758.07", "-92233720368547758.08"} {
		u := mustUSD(t, s)
		for _, d := range []uint8{2, 6, 9, 18} {
			q, err := USDToQuoteQuantity(u, d, RoundExact)
			require.NoError(t, err)
			back, err := QuoteQuantityToUSD(q, d, RoundExact)
			require.NoError(t, err)
			require.Equal(t, u, back)
		}
	}
}

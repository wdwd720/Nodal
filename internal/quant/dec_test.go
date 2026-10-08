package quant

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

const fortyDigits = "0.0000000000000000000000000000000000000001" // 1 ulp

func TestParseDecCanonical(t *testing.T) {
	cases := map[string]string{
		"0":               "0",
		"-0":              "0",
		"-0.000":          "0",
		"007":             "7",
		"1.50":            "1.5",
		"-1.50":           "-1.5",
		"123.456":         "123.456",
		"0.1":             "0.1",
		"-00.010":         "-0.01",
		fortyDigits:       fortyDigits,
		"-" + fortyDigits: "-" + fortyDigits,
		// Digits beyond the 40th are accepted when they are zeros.
		"1.000000000000000000000000000000000000000000000":              "1",
		"2.71828182845904523536028747135266249775720":                  "2.7182818284590452353602874713526624977572",
		strings.Repeat("9", 128):                                       strings.Repeat("9", 128),
		"-" + strings.Repeat("9", 128) + "." + strings.Repeat("9", 40): "-" + strings.Repeat("9", 128) + "." + strings.Repeat("9", 40),
	}
	for in, want := range cases {
		v, err := ParseDec(in)
		require.NoError(t, err, in)
		require.Equal(t, want, v.String(), in)
		back, err := ParseDec(v.String())
		require.NoError(t, err)
		require.True(t, back.Equal(v))
	}
}

func TestParseDecRejects(t *testing.T) {
	invalid := []string{
		"", "-", "+1", "1.", ".5", "1e3", "1E3", " 1", "1 ", "1\n", "--1", "1.2.3", "1,000", "1_000",
		"NaN", "Inf", "-Inf", "0x10", "١", "1.-5", "0.5x", strings.Repeat("1", 1025),
	}
	for _, s := range invalid {
		_, err := ParseDec(s)
		requireCode(t, err, errs.CodeValidationFailed)
		_, err = ParseDecRound(s, money.RoundHalfEven)
		requireCode(t, err, errs.CodeValidationFailed)
	}
	// A non-zero 41st digit is precision loss unless a mode is given.
	_, err := ParseDec("0.00000000000000000000000000000000000000005")
	requireCode(t, err, errs.CodePrecisionLoss)
	// Magnitude bound.
	_, err = ParseDec("1" + strings.Repeat("0", 128))
	requireCode(t, err, errs.CodeOverflow)
	_, err = ParseDecRound(strings.Repeat("9", 128)+"."+strings.Repeat("9", 41), money.RoundUp)
	requireCode(t, err, errs.CodeOverflow)
	// Invalid modes are rejected even when no rounding is needed.
	_, err = ParseDecRound("1", 0)
	requireCode(t, err, errs.CodeValidationFailed)
	_, err = ParseDecRound("1", 99)
	requireCode(t, err, errs.CodeValidationFailed)
}

func TestParseDecRoundModes(t *testing.T) {
	half := "0.00000000000000000000000000000000000000005"   // exactly ½ ulp
	above := "0.000000000000000000000000000000000000000051" // just above ½ ulp
	tie15 := "0.00000000000000000000000000000000000000015"  // 1.5 ulp
	cases := []struct {
		in   string
		mode money.RoundingMode
		want string
	}{
		{half, money.RoundHalfEven, "0"},
		{half, money.RoundHalfUp, fortyDigits},
		{half, money.RoundDown, "0"},
		{half, money.RoundUp, fortyDigits},
		{half, money.RoundFloor, "0"},
		{half, money.RoundCeil, fortyDigits},
		{"-" + half, money.RoundFloor, "-" + fortyDigits},
		{"-" + half, money.RoundCeil, "0"},
		{"-" + half, money.RoundHalfUp, "-" + fortyDigits},
		{"-" + half, money.RoundHalfEven, "0"},
		{above, money.RoundHalfEven, fortyDigits},
		{tie15, money.RoundHalfEven, "0.0000000000000000000000000000000000000002"},
		{"-" + tie15, money.RoundHalfEven, "-0.0000000000000000000000000000000000000002"},
	}
	for _, tc := range cases {
		v, err := ParseDecRound(tc.in, tc.mode)
		require.NoError(t, err)
		require.Equal(t, tc.want, v.String(), "%s %s", tc.in, tc.mode)
	}
	_, err := ParseDecRound(half, money.RoundExact)
	requireCode(t, err, errs.CodePrecisionLoss)
}

func TestDecZeroValueAndImmutability(t *testing.T) {
	var z Dec
	require.Equal(t, "0", z.String())
	require.True(t, z.IsZero())
	require.Equal(t, 0, z.Sign())
	require.True(t, z.Equal(DecFromInt64(0)))
	requireDecEqual(t, DecFromInt64(3), z.Add(DecFromInt64(3)))
	requireDecEqual(t, Dec{}, z.Mul(DecFromInt64(3)))
	_, err := DecFromInt64(1).Quo(z, money.RoundHalfEven)
	requireCode(t, err, errs.CodeValidationFailed)

	a, b := d(t, "1.25"), d(t, "-3.5")
	as, bs := a.String(), b.String()
	_ = a.Add(b)
	_ = a.Sub(b)
	_ = a.Mul(b)
	_ = a.Neg()
	_ = b.Abs()
	_, _ = a.Quo(b, money.RoundDown)
	_, _ = a.MulRound(b, money.RoundUp)
	_, _ = a.Round(1, money.RoundCeil)
	_, _ = a.ToQuantity(6, money.RoundFloor)
	_ = a.Max(b)
	_ = a.Min(b)
	require.Equal(t, as, a.String())
	require.Equal(t, bs, b.String())
}

func TestDecArithmeticTable(t *testing.T) {
	requireDecEqual(t, d(t, "4.75"), d(t, "1.25").Add(d(t, "3.5")))
	requireDecEqual(t, d(t, "-2.25"), d(t, "1.25").Sub(d(t, "3.5")))
	requireDecEqual(t, d(t, "4.375"), d(t, "1.25").Mul(d(t, "3.5")))
	requireDecEqual(t, d(t, "-4.375"), d(t, "-1.25").Mul(d(t, "3.5")))
	requireDecEqual(t, d(t, "3.5"), d(t, "-3.5").Abs())
	requireDecEqual(t, d(t, "-1.25"), d(t, "1.25").Neg())
	require.Equal(t, -1, d(t, "1.25").Cmp(d(t, "3.5")))
	require.Equal(t, 1, d(t, "-1").Cmp(d(t, "-2")))
	requireDecEqual(t, d(t, "-2"), d(t, "-1").Min(d(t, "-2")))
	requireDecEqual(t, d(t, "-1"), d(t, "-1").Max(d(t, "-2")))
	require.True(t, d(t, "-0.1").IsNegative())
	require.True(t, d(t, "0.1").IsPositive())

	// Mul rounds half-even at the 40th digit: 1e-20 × 5e-21 = ½ ulp → 0,
	// 1e-20 × 1.5e-20 = 1.5 ulp → 2 ulps.
	tiny := d(t, "0.00000000000000000001")
	requireDecEqual(t, Dec{}, tiny.Mul(d(t, "0.000000000000000000005")))
	requireDecEqual(t, ulps(2), tiny.Mul(d(t, "0.000000000000000000015")))
	r, err := tiny.MulRound(d(t, "0.000000000000000000005"), money.RoundUp)
	require.NoError(t, err)
	requireDecEqual(t, ulps(1), r)
	_, err = tiny.MulRound(d(t, "0.000000000000000000005"), money.RoundExact)
	requireCode(t, err, errs.CodePrecisionLoss)
	_, err = tiny.MulRound(tiny, 0)
	requireCode(t, err, errs.CodeValidationFailed)

	thirds := map[money.RoundingMode]string{
		money.RoundDown:     "0.3333333333333333333333333333333333333333",
		money.RoundUp:       "0.3333333333333333333333333333333333333334",
		money.RoundHalfEven: "0.3333333333333333333333333333333333333333",
		money.RoundFloor:    "0.3333333333333333333333333333333333333333",
		money.RoundCeil:     "0.3333333333333333333333333333333333333334",
	}
	for mode, want := range thirds {
		q, err := DecFromInt64(1).Quo(DecFromInt64(3), mode)
		require.NoError(t, err)
		require.Equal(t, want, q.String(), mode.String())
		q, err = DecFromInt64(-1).Quo(DecFromInt64(3), mode)
		require.NoError(t, err)
		if mode == money.RoundFloor {
			require.Equal(t, "-0.3333333333333333333333333333333333333334", q.String())
		}
		if mode == money.RoundCeil {
			require.Equal(t, "-0.3333333333333333333333333333333333333333", q.String())
		}
	}
	q, err := DecFromInt64(2).Quo(DecFromInt64(3), money.RoundHalfUp)
	require.NoError(t, err)
	require.Equal(t, "0.6666666666666666666666666666666666666667", q.String())
	q, err = DecFromInt64(1).Quo(DecFromInt64(8), money.RoundExact)
	require.NoError(t, err)
	require.Equal(t, "0.125", q.String())
	_, err = DecFromInt64(1).Quo(DecFromInt64(3), money.RoundExact)
	requireCode(t, err, errs.CodePrecisionLoss)
	_, err = DecFromInt64(1).Quo(Dec{}, money.RoundDown)
	requireCode(t, err, errs.CodeValidationFailed)
}

func TestRoundAndStringFixed(t *testing.T) {
	cases := []struct {
		in    string
		scale int32
		mode  money.RoundingMode
		want  string
		fixed string
	}{
		{"1.005", 2, money.RoundHalfEven, "1", "1.00"},
		{"1.015", 2, money.RoundHalfEven, "1.02", "1.02"},
		{"1.005", 2, money.RoundHalfUp, "1.01", "1.01"},
		{"-1.005", 2, money.RoundHalfUp, "-1.01", "-1.01"},
		{"-1.001", 2, money.RoundFloor, "-1.01", "-1.01"},
		{"-1.009", 2, money.RoundCeil, "-1", "-1.00"},
		{"1.5", 0, money.RoundHalfEven, "2", "2"},
		{"2.5", 0, money.RoundHalfEven, "2", "2"},
		{"-0.004", 2, money.RoundHalfEven, "0", "0.00"},
		{"0.123456789", 40, money.RoundExact, "0.123456789", "0.1234567890000000000000000000000000000000"},
	}
	for _, tc := range cases {
		r, err := d(t, tc.in).Round(tc.scale, tc.mode)
		require.NoError(t, err)
		require.Equal(t, tc.want, r.String(), "%s Round(%d, %s)", tc.in, tc.scale, tc.mode)
		s, err := d(t, tc.in).StringFixed(tc.scale, tc.mode)
		require.NoError(t, err)
		require.Equal(t, tc.fixed, s)
	}
	v := d(t, "1.25")
	same, err := v.Round(45, money.RoundExact)
	require.NoError(t, err)
	requireDecEqual(t, v, same)
	_, err = v.Round(-1, money.RoundDown)
	requireCode(t, err, errs.CodeValidationFailed)
	_, err = v.Round(1, money.RoundExact)
	requireCode(t, err, errs.CodePrecisionLoss)
	_, err = v.Round(1, 0)
	requireCode(t, err, errs.CodeValidationFailed)
	_, err = v.StringFixed(41, money.RoundDown)
	requireCode(t, err, errs.CodeValidationFailed)
}

func TestConversions(t *testing.T) {
	require.Equal(t, "-42", DecFromInt64(-42).String())
	require.Equal(t, "0.0001", DecFromBPS(1).String())
	require.Equal(t, "-0.0025", DecFromBPS(-25).String())
	require.Equal(t, "1", DecFromBPS(money.OneHundredPercent).String())

	v, err := DecFromQuantity(money.QuantityFromInt64(1_500_000_000), 9, money.RoundExact)
	require.NoError(t, err)
	require.Equal(t, "1.5", v.String())
	v, err = DecFromQuantity(money.QuantityFromInt64(-1), 6, money.RoundExact)
	require.NoError(t, err)
	require.Equal(t, "-0.000001", v.String())
	// 45 decimals: digits beyond the 40th need a mode.
	q, err := money.ParseQuantity("123456")
	require.NoError(t, err)
	_, err = DecFromQuantity(q, 45, money.RoundExact)
	requireCode(t, err, errs.CodePrecisionLoss)
	v, err = DecFromQuantity(q, 45, money.RoundHalfEven)
	require.NoError(t, err)
	require.Equal(t, "0.0000000000000000000000000000000000000001", v.String())
	_, err = DecFromQuantity(q, 9, 0)
	requireCode(t, err, errs.CodeValidationFailed)

	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	p, err := money.PriceFromDecimalString("150.123456", "USDC", "test", at)
	require.NoError(t, err)
	v, err = DecFromPrice(p)
	require.NoError(t, err)
	require.Equal(t, "150.123456", v.String())
	_, err = DecFromPrice(money.Price{Mantissa: money.QuantityFromInt64(1), Scale: 2})
	requireCode(t, err, errs.CodeValidationFailed)

	got, err := d(t, "1.5").ToQuantity(9, money.RoundExact)
	require.NoError(t, err)
	require.Equal(t, "1500000000", got.String())
	got, err = d(t, "1.2345").ToQuantity(2, money.RoundHalfEven)
	require.NoError(t, err)
	require.Equal(t, "123", got.String())
	_, err = d(t, "1.2345").ToQuantity(2, money.RoundExact)
	requireCode(t, err, errs.CodePrecisionLoss)
	got, err = d(t, "-1.2345").ToQuantity(2, money.RoundFloor)
	require.NoError(t, err)
	require.Equal(t, "-124", got.String())
	got, err = d(t, "1.5").ToQuantity(45, money.RoundExact)
	require.NoError(t, err)
	require.Equal(t, "15"+strings.Repeat("0", 44), got.String())

	pr, err := d(t, "150.1234565").ToPrice(6, "USDC", "model:bsm", at, money.RoundHalfEven)
	require.NoError(t, err)
	require.Equal(t, "150.123456 USDC", pr.String())
	back, err := DecFromPrice(pr)
	require.NoError(t, err)
	require.Equal(t, "150.123456", back.String())
	_, err = d(t, "-1").ToPrice(2, "USDC", "model:bsm", at, money.RoundHalfEven)
	requireCode(t, err, errs.CodeValidationFailed)
	_, err = d(t, "1").ToPrice(39, "USDC", "model:bsm", at, money.RoundHalfEven)
	requireCode(t, err, errs.CodeValidationFailed)
	_, err = d(t, "1").ToPrice(2, "", "model:bsm", at, money.RoundHalfEven)
	requireCode(t, err, errs.CodeValidationFailed)
}

func TestToInt64(t *testing.T) {
	cases := []struct {
		in   string
		mode money.RoundingMode
		want int64
	}{
		{"2.5", money.RoundHalfEven, 2},
		{"3.5", money.RoundHalfEven, 4},
		{"2.5", money.RoundHalfUp, 3},
		{"-2.5", money.RoundHalfUp, -3},
		{"-2.5", money.RoundFloor, -3},
		{"-2.5", money.RoundCeil, -2},
		{"-2.5", money.RoundDown, -2},
		{"0.0000000000000000000000000000000000000001", money.RoundUp, 1},
		{"42", money.RoundExact, 42},
		{"9223372036854775807", money.RoundExact, 9223372036854775807},
		{"-9223372036854775808", money.RoundExact, -9223372036854775808},
	}
	for _, tc := range cases {
		got, err := d(t, tc.in).ToInt64(tc.mode)
		require.NoError(t, err, tc.in)
		require.Equal(t, tc.want, got, "%s %s", tc.in, tc.mode)
		requireDecEqual(t, DecFromInt64(got), DecFromInt64(tc.want))
	}
	_, err := d(t, "9223372036854775808").ToInt64(money.RoundExact)
	requireCode(t, err, errs.CodeOverflow)
	_, err = d(t, "9223372036854775807.5").ToInt64(money.RoundHalfUp)
	requireCode(t, err, errs.CodeOverflow)
	_, err = d(t, "1.5").ToInt64(money.RoundExact)
	requireCode(t, err, errs.CodePrecisionLoss)
	_, err = d(t, "1").ToInt64(0)
	requireCode(t, err, errs.CodeValidationFailed)
}

func TestUnitAndPowers(t *testing.T) {
	require.Equal(t, "1"+strings.Repeat("0", 40), unit().String())
	require.Equal(t, "1", decPow10(0).String())
	require.Equal(t, "0.0000000000000001", decPow10(-16).String())
	require.Equal(t, "1000000000000000000", decPow10(18).String())
	require.Equal(t, fortyDigits, decPow10(-40).String())
	// unit() returns a fresh value each time.
	u := unit()
	u.SetInt64(7)
	require.Equal(t, "1"+strings.Repeat("0", 40), unit().String())
}

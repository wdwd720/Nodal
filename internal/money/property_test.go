package money

import (
	"errors"
	"math"
	"math/big"
	"testing"

	"pgregory.net/rapid"
)

// Generators.

func genMode(t *rapid.T) RoundingMode {
	return rapid.SampledFrom(allModes).Draw(t, "mode")
}

func genInexactMode(t *rapid.T) RoundingMode {
	return rapid.SampledFrom(inexactModes).Draw(t, "mode")
}

// genQuantity draws a signed integer of up to maxDigits digits.
func genQuantity(t *rapid.T, label string, maxDigits int) Quantity {
	s := rapid.StringMatching(`-?[0-9]{1,`+itoa(maxDigits)+`}`).Draw(t, label)
	q, err := ParseQuantity(s)
	if err != nil {
		t.Fatalf("ParseQuantity(%q): %v", s, err)
	}
	return q
}

func genNonZeroQuantity(t *rapid.T, label string, maxDigits int) Quantity {
	for {
		q := genQuantity(t, label, maxDigits)
		if !q.IsZero() {
			return q
		}
	}
}

func itoa(n int) string { return big.NewInt(int64(n)).String() }

// Properties.

func TestProp_USDAddCommutative(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := USDFromMinor(rapid.Int64().Draw(t, "a"))
		b := USDFromMinor(rapid.Int64().Draw(t, "b"))
		ab, errAB := a.Add(b)
		ba, errBA := b.Add(a)
		if (errAB == nil) != (errBA == nil) {
			t.Fatalf("asymmetric errors: %v vs %v", errAB, errBA)
		}
		// Overflow must occur exactly when the exact sum leaves int64.
		exact := new(big.Int).Add(big.NewInt(a.Minor()), big.NewInt(b.Minor()))
		if errAB != nil {
			if !errors.Is(errAB, ErrOverflow) {
				t.Fatalf("unexpected error %v", errAB)
			}
			if exact.IsInt64() {
				t.Fatalf("%s + %s reported overflow but exact sum %s fits", a, b, exact)
			}
			return
		}
		if !exact.IsInt64() {
			t.Fatalf("%s + %s = %s should have overflowed", a, b, ab)
		}
		if ab != ba || ab.Minor() != exact.Int64() {
			t.Fatalf("%s + %s: got %s and %s, want %s", a, b, ab, ba, exact)
		}
	})
}

func TestProp_USDAddSubInverse(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := USDFromMinor(rapid.Int64().Draw(t, "a"))
		b := USDFromMinor(rapid.Int64().Draw(t, "b"))
		sum, err := a.Add(b)
		if err != nil {
			return
		}
		back, err := sum.Sub(b)
		if err != nil {
			t.Fatalf("(%s + %s) - %s: %v", a, b, b, err)
		}
		if back != a {
			t.Fatalf("(%s + %s) - %s = %s", a, b, b, back)
		}
		diff, err := a.Sub(b)
		if err != nil {
			exact := new(big.Int).Sub(big.NewInt(a.Minor()), big.NewInt(b.Minor()))
			if exact.IsInt64() {
				t.Fatalf("%s - %s reported overflow but fits", a, b)
			}
			return
		}
		back, err = diff.Add(b)
		if err != nil || back != a {
			t.Fatalf("(%s - %s) + %s = %s, %v", a, b, b, back, err)
		}
	})
}

func TestProp_USDStringParseRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		u := USDFromMinor(rapid.Int64().Draw(t, "minor"))
		s := u.String()
		back, err := ParseUSD(s)
		if err != nil {
			t.Fatalf("ParseUSD(%q): %v", s, err)
		}
		if back != u {
			t.Fatalf("round trip %s -> %q -> %s", u, s, back)
		}
		for _, mode := range allModes {
			r, err := ParseUSDRound(s, mode)
			if err != nil || r != u {
				t.Fatalf("ParseUSDRound(%q, %s) = %s, %v", s, mode, r, err)
			}
		}
		if back.String() != s {
			t.Fatalf("String not canonical: %q vs %q", back.String(), s)
		}
	})
}

func TestProp_USDMulBPSMatchesQuantity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		u := USDFromMinor(rapid.Int64().Draw(t, "minor"))
		bps := BPS(rapid.Int64Range(-30_000, 30_000).Draw(t, "bps"))
		mode := genMode(t)
		got, err := u.MulBPS(bps, mode)
		want, wantErr := QuantityFromInt64(u.Minor()).MulBPSChecked(bps, mode)
		if wantErr != nil {
			if !errors.Is(err, ErrPrecisionLoss) {
				t.Fatalf("USD.MulBPS: %v, Quantity: %v", err, wantErr)
			}
			return
		}
		if !want.BigInt().IsInt64() {
			if !errors.Is(err, ErrOverflow) {
				t.Fatalf("expected overflow, got %s, %v", got, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("USD.MulBPS: %v", err)
		}
		if got.Minor() != want.BigInt().Int64() {
			t.Fatalf("%s × %d (%s): USD %s vs Quantity %s", u, bps, mode, got, want)
		}
	})
}

func TestProp_QuantityAddSubInverse(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := genQuantity(t, "a", 80)
		b := genQuantity(t, "b", 80)
		if got := a.Add(b).Sub(b); !got.Equal(a) {
			t.Fatalf("(%s + %s) - %s = %s", a, b, b, got)
		}
		if got := a.Sub(b).Add(b); !got.Equal(a) {
			t.Fatalf("(%s - %s) + %s = %s", a, b, b, got)
		}
		if got := a.Add(b); !got.Equal(b.Add(a)) {
			t.Fatalf("%s + %s not commutative", a, b)
		}
		if got := a.Neg().Neg(); !got.Equal(a) {
			t.Fatalf("--%s = %s", a, got)
		}
		if got := a.Add(a.Neg()); !got.IsZero() {
			t.Fatalf("%s + -%s = %s", a, a, got)
		}
	})
}

func TestProp_QuantityStringParseRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		q := genQuantity(t, "q", 127)
		s := q.String()
		back, err := ParseQuantity(s)
		if err != nil || !back.Equal(q) {
			t.Fatalf("ParseQuantity(%q) = %s, %v", s, back, err)
		}
		if back.String() != s {
			t.Fatalf("String not canonical: %q vs %q", back.String(), s)
		}
		var viaJSON Quantity
		b, err := q.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err := viaJSON.UnmarshalJSON(b); err != nil || !viaJSON.Equal(q) {
			t.Fatalf("JSON round trip of %s: %s, %v", q, viaJSON, err)
		}
		var viaScan Quantity
		v, _ := q.Value()
		if err := viaScan.Scan(v); err != nil || !viaScan.Equal(q) {
			t.Fatalf("SQL round trip of %s: %s, %v", q, viaScan, err)
		}
	})
}

func TestProp_RoundingMonotonic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		num := genQuantity(t, "num", 60)
		den := genNonZeroQuantity(t, "den", 30)

		res := map[RoundingMode]Quantity{}
		for _, mode := range inexactModes {
			r, err := num.Div(den, mode)
			if err != nil {
				t.Fatalf("%s / %s (%s): %v", num, den, mode, err)
			}
			res[mode] = r
		}
		down, up := res[RoundDown], res[RoundUp]
		floor, ceil := res[RoundFloor], res[RoundCeil]
		he, hu := res[RoundHalfEven], res[RoundHalfUp]

		// Magnitude ordering: |down| <= |half*| <= |up|.
		for _, mid := range []Quantity{he, hu} {
			if down.Abs().Cmp(mid.Abs()) > 0 || mid.Abs().Cmp(up.Abs()) > 0 {
				t.Fatalf("%s / %s: |down|=%s |mid|=%s |up|=%s", num, den, down.Abs(), mid.Abs(), up.Abs())
			}
		}
		// Signed ordering: floor <= everything <= ceil.
		for mode, r := range res {
			if floor.Cmp(r) > 0 || r.Cmp(ceil) > 0 {
				t.Fatalf("%s / %s: %s=%s outside [floor=%s, ceil=%s]", num, den, mode, r, floor, ceil)
			}
		}
		// Neighboring integers: up and down differ by at most one unit, as do ceil and floor.
		if d := up.Abs().Sub(down.Abs()); d.Sign() < 0 || d.Cmp(q64(1)) > 0 {
			t.Fatalf("%s / %s: |up|-|down| = %s", num, den, d)
		}
		if d := ceil.Sub(floor); d.Sign() < 0 || d.Cmp(q64(1)) > 0 {
			t.Fatalf("%s / %s: ceil-floor = %s", num, den, d)
		}
		// Every result is within one unit of the exact quotient: |r*den - num| < |den|.
		for mode, r := range res {
			diff := r.Mul(den).Sub(num).Abs()
			if diff.Cmp(den.Abs()) >= 0 {
				t.Fatalf("%s / %s (%s) = %s is not within one unit", num, den, mode, r)
			}
		}
		// Nearest modes are within half a unit: 2*|r*den - num| <= |den|.
		for _, mode := range []RoundingMode{RoundHalfEven, RoundHalfUp} {
			twice := res[mode].Mul(den).Sub(num).Abs().Mul(q64(2))
			if twice.Cmp(den.Abs()) > 0 {
				t.Fatalf("%s / %s (%s) = %s is not nearest", num, den, mode, res[mode])
			}
		}
		// RoundExact succeeds exactly when den divides num, and then all modes agree.
		exact, err := num.Div(den, RoundExact)
		divisible := new(big.Int).Rem(num.BigInt(), den.BigInt()).Sign() == 0
		if divisible {
			if err != nil {
				t.Fatalf("%s / %s exact: %v", num, den, err)
			}
			for mode, r := range res {
				if !r.Equal(exact) {
					t.Fatalf("%s / %s divisible but %s=%s != %s", num, den, mode, r, exact)
				}
			}
		} else if !errors.Is(err, ErrPrecisionLoss) {
			t.Fatalf("%s / %s not divisible: want ErrPrecisionLoss, got %v", num, den, err)
		}
	})
}

func TestProp_DecimalStringRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		q := genQuantity(t, "q", 100)
		decimals := rapid.SampledFrom([]uint8{0, 1, 2, 6, 8, 9, 18, 38, rapid.Uint8().Draw(t, "extra")}).Draw(t, "decimals")
		s := q.ToDecimalString(decimals)
		for _, mode := range allModes {
			back, err := QuantityFromDecimalString(s, decimals, mode)
			if err != nil {
				t.Fatalf("QuantityFromDecimalString(%q, %d, %s): %v", s, decimals, mode, err)
			}
			if !back.Equal(q) {
				t.Fatalf("round trip %s -> %q -> %s (decimals %d, %s)", q, s, back, decimals, mode)
			}
		}
		back, _ := QuantityFromDecimalString(s, decimals, RoundExact)
		if back.ToDecimalString(decimals) != s {
			t.Fatalf("ToDecimalString not canonical: %q vs %q", back.ToDecimalString(decimals), s)
		}
	})
}

func TestProp_NotionalScaleInvariance(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		qty := genQuantity(t, "qty", 30)
		mantissa := genQuantity(t, "mantissa", 30).Abs()
		scale := int32(rapid.IntRange(0, 30).Draw(t, "scale"))
		shift := rapid.IntRange(0, int(MaxPriceScale)-int(scale)).Draw(t, "shift")
		baseDecimals := uint8(rapid.IntRange(0, 18).Draw(t, "baseDecimals"))
		quoteDecimals := uint8(rapid.IntRange(0, 18).Draw(t, "quoteDecimals"))
		mode := genMode(t)

		p1, err := NewPrice(mantissa, scale, "USDC", "test", priceTime)
		if err != nil {
			t.Fatal(err)
		}
		p2, err := NewPrice(mantissa.ScaleUp(uint8(shift)), scale+int32(shift), "USDC", "test", priceTime)
		if err != nil {
			t.Fatal(err)
		}
		n1, err1 := Notional(qty, baseDecimals, p1, quoteDecimals, mode)
		n2, err2 := Notional(qty, baseDecimals, p2, quoteDecimals, mode)
		if (err1 == nil) != (err2 == nil) {
			t.Fatalf("asymmetric errors: %v vs %v", err1, err2)
		}
		if err1 != nil {
			if !errors.Is(err1, ErrPrecisionLoss) || mode != RoundExact {
				t.Fatalf("unexpected error %v (%s)", err1, mode)
			}
			return
		}
		if !n1.Equal(n2) {
			t.Fatalf("scale %d vs %d: %s vs %s", scale, scale+int32(shift), n1, n2)
		}
		// The result must equal qty × mantissa × 10^exp computed independently.
		exp := int(quoteDecimals) - int(baseDecimals) - int(scale)
		exact := new(big.Int).Mul(qty.BigInt(), mantissa.BigInt())
		if exp >= 0 {
			exact.Mul(exact, pow10(exp))
			if n1.BigInt().Cmp(exact) != 0 {
				t.Fatalf("positive exponent must be exact: %s vs %s", n1, exact)
			}
			return
		}
		den := pow10(-exp)
		diff := new(big.Int).Sub(new(big.Int).Mul(n1.BigInt(), den), exact)
		if diff.CmpAbs(den) >= 0 {
			t.Fatalf("result %s not within one unit of %s / %s", n1, exact, den)
		}
	})
}

func TestProp_MulBPSIdentityAndLinearity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		q := genQuantity(t, "q", 60)
		mode := genMode(t)
		full, err := q.MulBPSChecked(OneHundredPercent, mode)
		if err != nil || !full.Equal(q) {
			t.Fatalf("%s × 100%% (%s) = %s, %v", q, mode, full, err)
		}
		zero, err := q.MulBPSChecked(0, mode)
		if err != nil || !zero.IsZero() {
			t.Fatalf("%s × 0 (%s) = %s, %v", q, mode, zero, err)
		}
		u := USDFromMinor(rapid.Int64().Draw(t, "minor"))
		uf, err := u.MulBPS(OneHundredPercent, mode)
		if err != nil || uf != u {
			t.Fatalf("%s × 100%% (%s) = %s, %v", u, mode, uf, err)
		}
		// Doubling the basis points doubles an exact result.
		bps := BPS(rapid.Int64Range(-5_000, 5_000).Draw(t, "bps"))
		one, err := q.MulBPSChecked(bps, RoundExact)
		if err != nil {
			return
		}
		two, err := q.MulBPSChecked(2*bps, RoundExact)
		if err != nil || !two.Equal(one.Mul(q64(2))) {
			t.Fatalf("linearity: %s × %d = %s, × %d = %s, %v", q, bps, one, 2*bps, two, err)
		}
	})
}

func TestProp_USDQuoteRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		u := USDFromMinor(rapid.Int64().Draw(t, "minor"))
		d := uint8(rapid.IntRange(2, 30).Draw(t, "decimals"))
		q, err := USDToQuoteQuantity(u, d, RoundExact)
		if err != nil {
			t.Fatalf("USDToQuoteQuantity(%s, %d): %v", u, d, err)
		}
		back, err := QuoteQuantityToUSD(q, d, RoundExact)
		if err != nil || back != u {
			t.Fatalf("QuoteQuantityToUSD(%s, %d) = %s, %v; want %s", q, d, back, err, u)
		}
		// With fewer decimals than cents the conversion may round; it must never overflow for u.
		coarse := uint8(rapid.IntRange(0, 1).Draw(t, "coarse"))
		cq, err := USDToQuoteQuantity(u, coarse, genInexactMode(t))
		if err != nil {
			t.Fatalf("coarse conversion: %v", err)
		}
		if cq.BigInt().Cmp(big.NewInt(math.MinInt64)) < 0 || cq.BigInt().Cmp(big.NewInt(math.MaxInt64)) > 0 {
			t.Fatalf("coarse quantity %s out of int64 range", cq)
		}
	})
}
